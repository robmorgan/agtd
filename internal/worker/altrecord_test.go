package worker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agtd.sh/internal/protocol"
	"agtd.sh/internal/screen"
)

// claudeScroll is the claude-scroll stream (testdata/streams) up to its
// last mark, "cleared": after it Claude Code exits, leaving the alternate
// screen.
func claudeScroll(t *testing.T) []byte {
	t.Helper()
	var marks map[string]int
	if err := json.Unmarshal(must(os.ReadFile(filepath.Join("testdata", "streams", "claude-scroll.marks.json"))), &marks); err != nil {
		t.Fatal(err)
	}
	for _, st := range loadStreams(t) {
		if st.name == "claude-scroll" {
			return st.data[:marks["cleared"]]
		}
	}
	t.Fatal("no claude-scroll stream (testdata/streams/claude-scroll.vt.gz)")
	return nil
}

// detectEveryChange searches for the input box at every change under it
// for the rest of the test: a replay runs minutes of repaints in
// milliseconds, so a wall-clock limit would hold a box search's answer
// over dozens of them (TestAltRecordDetectsAtMostEvery covers the limit).
func detectEveryChange(t *testing.T) {
	old := altDetectEvery
	altDetectEvery = 0
	t.Cleanup(func() { altDetectEvery = old })
}

// shellRowsIn checks rows hold "shell row 001" to "shell row 120" once
// each, in order.
func shellRowsIn(t *testing.T, what string, rows [][]byte) {
	t.Helper()
	seen := map[int]int{}
	last, order := 0, true
	for _, r := range rows {
		line := plainRow(r)
		var n int
		if i := strings.Index(line, "shell row "); i >= 0 {
			if _, err := fmt.Sscanf(line[i:], "shell row %d", &n); err == nil {
				seen[n]++
				order = order && n > last
				last = n
			}
		}
	}
	var missing, dup []int
	for n := 1; n <= 120; n++ {
		switch {
		case seen[n] == 0:
			missing = append(missing, n)
		case seen[n] > 1:
			dup = append(dup, n)
		}
	}
	if len(missing) > 0 || len(dup) > 0 || !order {
		t.Errorf("%s: missing %v, duplicated %v, in order %v", what, missing, dup, order)
	}
}

func TestAltRecordClaudeScroll(t *testing.T) {
	// Value: protects=the worker records Claude Code's transcript as it
	// scrolls with nobody attached, each row once, in order, and the
	// first frame of a later attach seeds the client's copy with it;
	// fails_when=the worker looks inside a synchronized block, misses
	// the block ends inside a read, records an unprofiled program, or
	// the seed is cut or misnumbered; why_new=the worker record is new
	// (Phase 3); seam=none; no_claim=the client's guesses over it
	// (TestFramesPredictWheelClaudeScroll).
	data := claudeScroll(t)
	for _, c := range []struct {
		name   string
		pieces [][]byte
	}{
		{"one read", [][]byte{data}},
		{"16 KiB reads", chunks(data, 16384)},
		{"2 KiB reads", chunks(data, 2048)},
	} {
		t.Run(c.name, func(t *testing.T) {
			detectEveryChange(t)
			r := newFrameRig(t, streamCols, streamRows)
			r.s.agentName = "claude"
			r.p.SetPredictScroll(0)
			for _, p := range c.pieces {
				r.feed(string(p))
			}
			rec := r.s.altRecord
			if rec == nil {
				t.Fatal("nothing recorded")
			}
			shellRowsIn(t, "worker", rec.t.Rows(0))
			if rec.end < 20 {
				t.Errorf("transcript ends at row %d", rec.end)
			}
			// An attach after it all: the first frame seeds the copy.
			r.a.frames.record = true
			f := r.frame()
			if f.Record == nil || !f.Record.New || f.Record.Mark != rec.t.Recorded() || int(f.Record.End) != rec.end {
				t.Fatalf("first frame's record: %+v", f.Record)
			}
			if f2 := r.frame(); f2.Record == nil || f2.Record.New || len(f2.Record.Rows) != 0 {
				t.Fatalf("second frame's record: %+v", f2.Record)
			}
			if n := r.p.Recorded(); n != rec.t.Recorded() {
				t.Fatalf("the client's copy holds %d rows, the worker's %d", n, rec.t.Recorded())
			}
		})
	}
}

func TestAltRecordOnlyForAgents(t *testing.T) {
	// Value: protects=an editor or pager on the alternate screen is not
	// recorded, and an agent's record ends when it leaves the alternate
	// screen and starts anew, as new to clients, when it comes back;
	// fails_when=the profile check is skipped or the record outlives the
	// screen; why_new=the worker record is new; seam=none; no_claim=none.
	r := newFrameRig(t, 40, 10)
	r.s.command = "/usr/bin/vim"
	r.feed("\x1b[?1049h\x1b[Hfile line 1")
	if r.s.altRecord != nil {
		t.Fatal("recorded vim")
	}
	r.feed("\x1b[?1049l")
	r.s.agentName = "claude"
	r.feed("\x1b[?1049h\x1b[Hline 1")
	first := r.s.altRecord
	if first == nil {
		t.Fatal("did not record claude")
	}
	r.a.frames.record = true
	if f := r.frame(); f.Record == nil || !f.Record.New {
		t.Fatalf("first frame's record: %+v", f.Record)
	}
	r.feed("\x1b[?1049l$ ")
	if r.s.altRecord != nil {
		t.Fatal("the record outlived the alternate screen")
	}
	if f := r.frame(); f.Record != nil {
		t.Fatalf("a primary frame carries a record: %+v", f.Record)
	}
	r.feed("\x1b[?1049h\x1b[Hline 1")
	if r.s.altRecord == nil || r.s.altRecord == first {
		t.Fatal("no new record on coming back")
	}
	if f := r.frame(); f.Record == nil || !f.Record.New {
		t.Fatalf("the new record is not new to the client: %+v", f.Record)
	}
}

// rowsNumbered are rows "row N" for N in [from, to).
func rowsNumbered(from, to int64) [][]byte {
	var out [][]byte
	for n := from; n < to; n++ {
		out = append(out, fmt.Appendf(nil, "row %d", n))
	}
	return out
}

func TestAltRecordFrameRows(t *testing.T) {
	// Value: protects=the seed is the newest altSeedRows, a later frame
	// carries only rows the client lacks and at most altFrameRows, and
	// the client's copy stays numbered like the worker's across all
	// three; fails_when=frameRecord resends rows, uncaps a burst, or
	// numbers the seed wrong; why_new=frameRecord is new; seam=the record
	// is filled through Follow instead of a screen; no_claim=what a
	// screen records (TestAltRecordClaudeScroll).
	s := newOwnerState("rows", nil, nil)
	t0 := screen.NewTranscript(altRecordRows)
	t0.Follow(&protocol.FrameRecord{Pos: 3000, Mark: 3000, Rows: rowsNumbered(0, 3000)})
	s.altRecord = &altRecord{t: t0, end: 30}
	st := &frameState{record: true}
	client := screen.NewTranscript(altRecordRows)
	check := func(what string, wantNew bool, wantRows int) {
		t.Helper()
		r := s.frameRecord(st)
		if r.New != wantNew || len(r.Rows) != wantRows || r.End != 30 {
			t.Fatalf("%s: new %v, %d rows, end %d", what, r.New, len(r.Rows), r.End)
		}
		client.Follow(r)
		if client.Recorded() != t0.Recorded() || string(client.Rows(client.Recorded() - 1)[0]) != string(t0.Rows(t0.Recorded() - 1)[0]) {
			t.Fatalf("%s: the copy ends at %d, the record at %d", what, client.Recorded(), t0.Recorded())
		}
	}
	check("seed", true, altSeedRows)
	check("nothing new", false, 0)
	t0.Follow(&protocol.FrameRecord{Pos: 3500, Mark: 3500, Rows: rowsNumbered(3000, 3500)})
	check("500 more", false, 500)
	t0.Follow(&protocol.FrameRecord{Pos: 5000, Mark: 5000, Rows: rowsNumbered(3500, 5000)})
	check("a burst", false, altFrameRows)
	if client.Oldest() != 4000 {
		t.Fatalf("after the burst the copy starts at %d", client.Oldest())
	}
	s.altRecord = nil
	if r := s.frameRecord(st); r != nil || st.recordOf != nil {
		t.Fatalf("a record without one: %+v", r)
	}
}

func TestAltRecordSurvivesHandoff(t *testing.T) {
	// Value: protects=`agtd upgrade` keeps the record, through the JSON
	// handoff state, and a client reattaching to the new image gets it as
	// a new seed; fails_when=handoffState drops AltRecord or the restore
	// misnumbers it; why_new=the record is new; seam=the handoff's JSON
	// alone, not the exec (handoff_test.go covers that); no_claim=none.
	r := newFrameRig(t, streamCols, streamRows)
	r.s.agentName = "claude"
	r.feed(string(claudeScroll(t)[:30000]))
	if r.s.altRecord == nil || r.s.altRecord.t.Recorded() == 0 {
		t.Fatal("nothing recorded")
	}
	data, err := json.Marshal(&handoffState{Version: handoffVersion, AltRecord: r.s.altRecordHandoff()})
	if err != nil {
		t.Fatal(err)
	}
	var st handoffState
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	got := restoreAltRecord(st.AltRecord)
	want := r.s.altRecord
	if got.t.Recorded() != want.t.Recorded() || got.t.Pos() != want.t.Pos() || got.end != want.end {
		t.Fatalf("restored %d rows at %d (end %d), had %d at %d (end %d)", got.t.Recorded(), got.t.Pos(), got.end, want.t.Recorded(), want.t.Pos(), want.end)
	}
	for i, row := range want.t.Rows(0) {
		if string(got.t.Rows(0)[i]) != string(row) {
			t.Fatalf("row %d differs", i)
		}
	}
	if restoreAltRecord(nil) != nil {
		t.Fatal("restored a record from an older image")
	}
}

// BenchmarkAltRecordFeed feeds the claude-scroll stream (Claude Code at
// 120x40, 117 KiB) through the owner in 4 KiB reads, with the record and
// without it, for what the looks, the block-end cuts and the box searches
// cost per stream.
func BenchmarkAltRecordFeed(b *testing.B) {
	var data []byte
	for _, st := range loadStreams(b) {
		if st.name == "claude-scroll" {
			data = st.data
		}
	}
	if data == nil {
		b.Fatal("no claude-scroll stream")
	}
	reads := chunks(data, 4096)
	for _, agent := range []string{"", "claude"} {
		name := "unrecorded"
		if agent != "" {
			name = "recorded"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				b.StopTimer()
				src, err := newTerminalState(streamCols, streamRows, 64<<20)
				if err != nil {
					b.Fatal(err)
				}
				s := newOwnerState("bench", nil, src)
				s.agentName = agent
				b.StartTimer()
				for _, p := range reads {
					if err := s.publishScanned(p); err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				src.close()
				b.StartTimer()
			}
		})
	}
}

func TestAltRecordDetectsAtMostEvery(t *testing.T) {
	// Value: protects=the input box is searched for (1.5 ms) only when the
	// rows under it changed, at most every altDetectEvery, so a streaming
	// agent does not pay for one per repaint; fails_when=the bottom rows
	// are not compared or the limit is ignored; why_new=the record is new;
	// seam=times passed in, the put-off search's timer is not run (the rig
	// has no owner goroutine); no_claim=the timer firing.
	r := newFrameRig(t, 40, 10)
	r.s.agentName = "claude"
	r.feed("\x1b[?1049h\x1b[Hline 1\r\nline 2")
	rec := r.s.altRecord
	if rec == nil || rec.bottom == nil {
		t.Fatal("no first search")
	}
	t0 := rec.detectedAt
	rows := func(last string) [][]byte {
		rs := make([][]byte, 10)
		rs[0], rs[9] = []byte("line 1"), []byte(last)
		return rs
	}
	same := slices.Clone(rec.bottom)
	if len(same) == 0 {
		t.Fatal("no rows under the box")
	}
	// The rows under the box as searched: no search, however late.
	base := make([][]byte, 10-len(same))
	if err := r.s.detectTranscriptEnd(rec, append(base, same...), t0.Add(time.Hour)); err != nil || rec.detectedAt != t0 {
		t.Fatalf("searched again for the same rows (%v)", err)
	}
	// Changed, within the limit: put off.
	if err := r.s.detectTranscriptEnd(rec, rows("status 2"), t0.Add(altDetectEvery/2)); err != nil || rec.detectedAt != t0 {
		t.Fatalf("searched within the limit (%v)", err)
	}
	// Changed, past it: searched.
	now := t0.Add(altDetectEvery)
	if err := r.s.detectTranscriptEnd(rec, rows("status 3"), now); err != nil || !rec.detectedAt.Equal(now) {
		t.Fatalf("did not search past the limit (%v)", err)
	}
}
