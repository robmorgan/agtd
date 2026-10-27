package worker

import (
	"bytes"
	"fmt"
	"os"
	"slices"
	"time"

	"agtd.sh/internal/protocol"
	"agtd.sh/internal/screen"
	"go.mitchellh.com/libghostty"
)

// The record of an agent's transcript on the alternate screen
// (CapAttachAltRecord, docs/designs/alt-screen-scrollback.md, Phase 3).
//
// An agent such as Claude Code repaints its transcript on the alternate
// screen by absolute position, so the rows that leave its top are gone
// from the terminal. The worker looks at the screen after every feed, at
// each end of a synchronized-output block in it, and a screen.Transcript
// finds how far the content moved and records the rows that left. The
// record lives on the owner goroutine; frames carry where the screen is in
// it and the rows a client does not hold yet, so a client's wheel
// prediction has the transcript above the screen from its first frame,
// including what scrolled by while nobody was attached.
//
// Only agents whose field profile says they draw a transcript there are
// recorded (fieldProfile.records): an editor or a pager on the alternate
// screen is not a transcript.

const (
	// altRecordRows bounds the record: about 1 MiB at 100 bytes a row.
	altRecordRows = 10000
	// altSeedRows is how many of the newest rows the first frame after an
	// attach carries, enough for the prediction's copy (decision 11)
	// without making that frame slow on a slow link.
	altSeedRows = 2000
	// altFrameRows bounds the rows a later frame carries; more recorded
	// between two frames leave the client's copy starting after them.
	altFrameRows = frameMaxHistory
)

// altDetectEvery is the least time between two searches for the agent's
// input box (1.5 ms each on 200x50, BENCHMARKS.md), which say where the
// transcript ends. One runs when the rows under the box change, at most
// this often, and once more after they stop. A variable for tests.
var altDetectEvery = 100 * time.Millisecond

// altRecord is the record of the alternate screen the agent is on.
type altRecord struct {
	t *screen.Transcript
	// end is how many rows from the top are the transcript, from the last
	// search for the box (0: not found), and bottom the rows from end
	// down at that search: while they stay the same, so does the box.
	end        int
	bottom     [][]byte
	detectedAt time.Time
	// detectArmed is whether a timer will look again for a box search the
	// rate limit put off.
	detectArmed bool
}

// syncEnd ends a synchronized-output block.
var syncEnd = []byte("\x1b[?2026l")

// feedRecorded feeds data through the shadow terminal, looking at the
// screen for the record at each end of a synchronized block in it and at
// its end: a look inside a block sees half a repaint, and two repaints in
// one read would hide the rows between them. The read that enters the
// alternate screen is cut too, since it may hold many repaints.
func (s *ownerState) feedRecorded(data []byte) {
	if s.altRecord == nil && s.altChecked {
		// A program on the alternate screen that is not recorded: the
		// look only notices it leaving.
		s.afterFeed(s.terminal.feed(data))
		s.lookAlternate()
		return
	}
	for len(data) > 0 {
		n := len(data)
		if i := bytes.Index(data, syncEnd); i >= 0 {
			n = i + len(syncEnd)
		}
		s.afterFeed(s.terminal.feed(data[:n]))
		s.lookAlternate()
		data = data[n:]
	}
}

// lookAlternate takes the screen into the record when the agent is on the
// alternate screen, starting a record when it got there, and drops the
// record when it left. A failure drops the record: clients go back to
// inferring their own.
func (s *ownerState) lookAlternate() {
	if err := s.lookAlternateErr(); err != nil {
		fmt.Fprintf(os.Stderr, "session worker: transcript record: %v\n", err)
		s.altRecord, s.altChecked = nil, true
	}
}

func (s *ownerState) lookAlternateErr() error {
	term := s.terminal.term
	active, err := term.ActiveScreen()
	if err != nil {
		return err
	}
	if active != libghostty.ScreenAlternate {
		s.altRecord, s.altChecked = nil, false
		return nil
	}
	if s.altRecord == nil {
		// Whether the program is one to record is decided once each time
		// it enters the alternate screen.
		if s.altChecked {
			return nil
		}
		s.altChecked = true
		if prof, _ := s.fieldProfile(); !prof.records {
			return nil
		}
		s.altRecord = &altRecord{t: screen.NewTranscript(altRecordRows)}
	}
	if syncing, err := term.Mode(libghostty.ModeSyncOutput); err != nil || syncing {
		return err
	}
	cols, err := term.Cols()
	if err != nil {
		return err
	}
	lines, err := term.Rows()
	if err != nil {
		return err
	}
	rows, err := frameRows(term, cols, lines)
	if err != nil {
		return err
	}
	rec := s.altRecord
	if err := s.detectTranscriptEnd(rec, rows, time.Now()); err != nil {
		return err
	}
	rec.t.Update(rows, rec.end)
	return nil
}

// detectTranscriptEnd searches for the agent's input box when the rows
// under it changed since the last search, at most every altDetectEvery; a
// search put off runs from a timer.
func (s *ownerState) detectTranscriptEnd(rec *altRecord, rows [][]byte, now time.Time) error {
	if rec.bottom != nil && len(rows) == rec.end+len(rec.bottom) && rowsEqual(rows[rec.end:], rec.bottom) {
		return nil
	}
	if wait := altDetectEvery - now.Sub(rec.detectedAt); rec.bottom != nil && wait > 0 {
		if !rec.detectArmed && s.owner != nil {
			rec.detectArmed = true
			time.AfterFunc(wait, func() {
				s.owner.post(func(s *ownerState) {
					if s.altRecord == rec {
						rec.detectArmed = false
						s.lookAlternate()
					}
				})
			})
		}
		return nil
	}
	f, err := s.field()
	if err != nil {
		return err
	}
	rec.end = 0
	if f != nil && f.Found && f.Alternate && f.BoxTop > 0 && int(f.BoxTop) <= len(rows) {
		rec.end = int(f.BoxTop) - 1
	}
	rec.bottom, rec.detectedAt = slices.Clone(rows[rec.end:]), now
	return nil
}

// frameRecord is the record for a frame to st: where the screen is, and
// the rows st has not been sent, the newest altSeedRows the first time.
// Nil when nothing is recorded.
func (s *ownerState) frameRecord(st *frameState) *protocol.FrameRecord {
	rec := s.altRecord
	if rec == nil {
		st.recordOf = nil
		return nil
	}
	t := rec.t
	r := &protocol.FrameRecord{Pos: t.Pos(), Mark: t.Recorded(), Jumps: uint32(t.Jumps), End: uint16(rec.end)}
	from := max(st.recordMark, r.Mark-altFrameRows)
	if st.recordOf != t {
		r.New, st.recordOf = true, t
		from = r.Mark - altSeedRows
	}
	// The rows are copied out: the record moves its rows down when it
	// drops the oldest, after this frame is built and before it is sent.
	r.Rows = slices.Clone(t.Rows(from))
	st.recordMark = r.Mark
	return r
}

// altRecordHandoff carries the record across a handoff.
type altRecordHandoff struct {
	Transcript screen.TranscriptState
	End        int
}

func (s *ownerState) altRecordHandoff() *altRecordHandoff {
	if s.altRecord == nil {
		return nil
	}
	return &altRecordHandoff{Transcript: s.altRecord.t.State(), End: s.altRecord.end}
}

func restoreAltRecord(h *altRecordHandoff) *altRecord {
	if h == nil {
		return nil
	}
	return &altRecord{t: screen.RestoreTranscript(altRecordRows, h.Transcript), end: h.End}
}
