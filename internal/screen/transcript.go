package screen

import (
	"bytes"
	"slices"
	"strings"

	"agtd.sh/internal/protocol"
)

// Transcript records the rows that leave the top of a program's viewport on
// the alternate screen, where nothing scrolls in terminal terms: a program
// such as Claude Code repaints its transcript by absolute position, so a
// row that moves up is a repaint and a row that leaves the top is gone.
// Comparing consecutive screens finds how far the content moved; the rows
// that moved off the top are appended to the record once each, so a
// terminal's own scrollback can be fed from it
// (docs/designs/alt-screen-scrollback.md).
//
// Rows are VT as the formatter writes them, each self-contained from a
// default pen; nil is a blank row. They are compared by RowKey, since the
// formatter writes an empty cell as a cursor move and a space as a space,
// and a program's diff renderer leaves either behind depending on what was
// on screen before: the same visible row comes in different bytes.
//
// The logical row numbers count from the top row of the first screen seen.
// pos is the logical number of the current screen's top row, mark the first
// row not yet recorded; pos <= mark always, so the record is the contiguous
// rows [dropped, mark).
type Transcript struct {
	prev      [][]byte
	prevKeys  []string
	prevEnd   int
	pos, mark int64
	rows      [][]byte
	keys      []string
	dropped   int64
	max       int
	// Jumps counts screens that changed wholesale to content the record
	// does not hold (a return to the end after scrolling back is one):
	// rows that scrolled past between two looks may be lost there.
	Jumps int
}

// NewTranscript makes a record holding at most max rows (the oldest are
// dropped).
func NewTranscript(max int) *Transcript { return &Transcript{max: max} }

// Recorded is how many rows were ever recorded.
func (t *Transcript) Recorded() int64 { return t.mark }

// Pos is the logical number of the last screen's top row.
func (t *Transcript) Pos() int64 { return t.pos }

// Oldest is the logical number of the oldest row the record still holds.
func (t *Transcript) Oldest() int64 { return t.dropped }

// Rows returns the recorded rows from logical row from on, as many as the
// record still holds.
func (t *Transcript) Rows(from int64) [][]byte {
	from = max(from, t.dropped)
	if from >= t.mark {
		return nil
	}
	return t.rows[from-t.dropped:]
}

// Update takes the next screen and returns the rows it recorded, oldest
// first. end is how many rows from the top are the program's transcript,
// as far as the caller knows (the rows above its input box); 0 for all of
// them. The returned rows are cur's or prev's own slices; the caller must
// not change them.
func (t *Transcript) Update(cur [][]byte, end int) [][]byte {
	if end <= 0 || end > len(cur) {
		end = len(cur)
	}
	prev, old, prevEnd := t.prev, t.prevKeys, t.prevEnd
	keys := make([]string, len(cur))
	for i, r := range cur {
		keys[i] = RowKey(r)
	}
	t.prev, t.prevKeys, t.prevEnd = cur, keys, end
	if prev == nil {
		return nil
	}
	if slices.Equal(old, keys) {
		return nil
	}
	n := min(len(old), len(keys))
	still := 0
	if len(old) == len(keys) {
		still = sameRun(old, keys)
	}
	if k, run := Shift(old, keys); k != 0 && run > still {
		var out [][]byte
		if k > 0 {
			out = t.record(prev[:k], old[:k])
		}
		t.pos += int64(k)
		return out
	}
	if still*2 >= n {
		// Changed in place: a spinner, a streaming tail, the input box.
		return nil
	}
	// A wholesale change: a jump (to the end, far back, /clear), or more
	// moved between two looks than a screen holds. The rows both screens
	// share at the bottom (the input box, the status rows) are not
	// transcript. If the new screen's rows above them are recorded, that
	// is where it is.
	tail := 0
	for tail < n && old[len(old)-1-tail] == keys[len(keys)-1-tail] {
		tail++
	}
	if at, ok := t.lookup(keys, min(end, len(keys)-tail)); ok {
		t.pos = at
		return nil
	}
	// New content: what the old screen's transcript held above the shared
	// rows is recorded, since it will not be seen again, with at most one
	// of its trailing blank rows, and the new screen starts after it.
	stop := min(prevEnd, len(prev)-tail)
	for stop > 1 && old[stop-1] == "" && old[stop-2] == "" {
		stop--
	}
	out := t.record(prev[:stop], old[:stop])
	t.pos = t.mark
	t.Jumps++
	return out
}

// Follow takes the next screen's place in a record kept elsewhere (the
// worker's, protocol.FrameRecord) instead of inferring it, and returns the
// rows it added, oldest first. A new record, or one that went back,
// replaces this one; rows that never arrived (more were recorded between
// two frames than a frame carries) leave the copy starting after them.
func (t *Transcript) Follow(r *protocol.FrameRecord) [][]byte {
	if r.New || r.Mark < t.mark {
		*t = Transcript{max: t.max}
	}
	from := r.Mark - int64(len(r.Rows))
	if from > t.mark || len(t.rows) == 0 {
		t.rows, t.keys = t.rows[:0], t.keys[:0]
		t.dropped, t.mark = from, from
	}
	t.pos, t.Jumps = r.Pos, int(r.Jumps)
	skip := t.mark - from
	if skip >= int64(len(r.Rows)) {
		return nil
	}
	out := r.Rows[skip:]
	for _, row := range out {
		t.keys = append(t.keys, RowKey(row))
	}
	t.rows = append(t.rows, out...)
	t.mark = r.Mark
	t.trim()
	return out
}

// TranscriptState is a Transcript's state, for a worker handing its
// session to a new image.
type TranscriptState struct {
	Pos, Mark, Dropped int64
	Jumps              int
	Rows               [][]byte
	// Prev is the last screen seen and PrevEnd its transcript's height,
	// which the next screen is compared with.
	Prev    [][]byte
	PrevEnd int
}

// State returns t's state, with its own row lists: t moves its rows down
// in place when it drops the oldest, and the state may be read on another
// goroutine after t goes on. The rows' bytes are never changed, so they
// are shared.
func (t *Transcript) State() TranscriptState {
	return TranscriptState{Pos: t.pos, Mark: t.mark, Dropped: t.dropped, Jumps: t.Jumps, Rows: slices.Clone(t.rows), Prev: slices.Clone(t.prev), PrevEnd: t.prevEnd}
}

// RestoreTranscript makes a record holding at most max rows from a state
// State returned. A state that does not add up (its rows are not
// [Dropped, Mark)) restores as an empty record.
func RestoreTranscript(max int, st TranscriptState) *Transcript {
	t := &Transcript{max: max}
	if st.Mark-st.Dropped != int64(len(st.Rows)) || st.Pos > st.Mark {
		return t
	}
	t.pos, t.mark, t.dropped, t.Jumps, t.rows, t.prevEnd = st.Pos, st.Mark, st.Dropped, st.Jumps, st.Rows, st.PrevEnd
	t.keys = make([]string, len(st.Rows))
	for i, r := range st.Rows {
		t.keys[i] = RowKey(r)
	}
	if st.Prev != nil {
		t.prev = st.Prev
		t.prevKeys = make([]string, len(st.Prev))
		for i, r := range st.Prev {
			t.prevKeys[i] = RowKey(r)
		}
	}
	t.trim()
	return t
}

// record appends the rows at logical pos onward that are not recorded yet,
// and drops the oldest beyond max.
func (t *Transcript) record(rows [][]byte, keys []string) [][]byte {
	skip := t.mark - t.pos
	if skip >= int64(len(rows)) {
		return nil
	}
	if skip < 0 {
		skip = 0
	}
	out := rows[skip:]
	t.rows = append(t.rows, out...)
	t.keys = append(t.keys, keys[skip:]...)
	t.mark = t.pos + int64(len(rows))
	t.trim()
	return out
}

// trim drops the oldest rows beyond max, a quarter of max at a time, so a
// full record is not copied on every frame that scrolls.
func (t *Transcript) trim() {
	if t.max > 0 && len(t.rows) > t.max+t.max/4 {
		drop := len(t.rows) - t.max
		t.rows = t.rows[:copy(t.rows, t.rows[drop:])]
		t.keys = t.keys[:copy(t.keys, t.keys[drop:])]
		clear(t.rows[len(t.rows):cap(t.rows)])
		t.dropped += int64(drop)
	}
}

// lookup finds the screen's rows above limit in the record, newest first:
// three consecutive rows with content locate it, and the rows from there
// down to limit must match the record until either ends, or for at least
// half of them (a program may draw over its last row while scrolled, as
// Claude Code's "Jump to bottom" does). A program that repeats a few rows
// (its banner after /clear) is not taken for a jump back. It returns the
// logical number the screen's top row would have.
func (t *Transcript) lookup(cur []string, limit int) (int64, bool) {
	i := -1
	for y := 0; y+2 < limit; y++ {
		if cur[y] != "" && cur[y+1] != "" && cur[y+2] != "" {
			i = y
			break
		}
	}
	if i < 0 {
		return 0, false
	}
	for j := len(t.keys) - 3; j >= 0; j-- {
		if t.keys[j] != cur[i] || t.keys[j+1] != cur[i+1] || t.keys[j+2] != cur[i+2] {
			continue
		}
		y := i
		for y < limit && j+y-i < len(t.keys) && cur[y] == t.keys[j+y-i] {
			y++
		}
		if y == limit || j+y-i == len(t.keys) || (y-i)*2 >= limit-i {
			return t.dropped + int64(j) - int64(i), true
		}
	}
	return 0, false
}

// Shift finds how far the content moved between two screens: the shift k
// (positive when it moved up) under which the longest run of consecutive
// rows has cur[i] == old[i+k], and that run's length. A run counts only if
// it is minShiftRun rows or longer and holds at least two rows that occur
// once in old, since rules, blank separators and repeated bullets match at
// many shifts and a few such rows line up by chance (Claude Code's screen
// after /clear matched its last one at a shift of one, two rows long).
// Ties go to the smaller shift. 0, 0 when no shift qualifies.
func Shift(old, cur []string) (k, run int) {
	counts := make(map[string]int, len(old))
	for _, r := range old {
		if r != "" {
			counts[r]++
		}
	}
	for d := 1; d < len(old); d++ {
		for _, s := range []int{d, -d} {
			if r := shiftRun(old, cur, s, counts); r > run {
				k, run = s, r
			}
		}
	}
	return k, run
}

// minShiftRun is the shortest run of rows that shows a shift. A transcript
// that moved by k leaves the transcript's height less k rows matching (13
// or more for Claude Code at 40 rows in the recorded stream); a larger move
// is read as a jump.
const minShiftRun = 4

// shiftRun is the longest qualifying run of cur[i] == old[i+s].
func shiftRun(old, cur []string, s int, counts map[string]int) int {
	best, length, content, unique := 0, 0, 0, 0
	for i := max(0, -s); i < len(cur) && i+s < len(old); i++ {
		if cur[i] != old[i+s] {
			length, content, unique = 0, 0, 0
			continue
		}
		length++
		if cur[i] != "" {
			content++
			if counts[cur[i]] == 1 {
				unique++
			}
		}
		if length >= minShiftRun && content >= 2 && unique >= 2 && length > best {
			best = length
		}
	}
	return best
}

// sameRun is the longest run of rows equal at the same place.
func sameRun(old, cur []string) int {
	best, length := 0, 0
	for i := 0; i < len(cur) && i < len(old); i++ {
		if cur[i] == old[i] {
			length++
			best = max(best, length)
		} else {
			length = 0
		}
	}
	return best
}

// RowKey is a row's VT with empty cells and spaces made the same: cursor
// moves forward (CSI n C) become spaces, and trailing spaces and pen resets
// go. "" is a blank row.
func RowKey(row []byte) string {
	if bytes.IndexByte(row, 0x1b) < 0 {
		return strings.TrimRight(string(row), " ")
	}
	var b strings.Builder
	for i := 0; i < len(row); i++ {
		if row[i] == 0x1b {
			seq := row[i : i+escapeLen(row[i:])]
			if n, ok := cufCount(seq); ok {
				b.WriteString(strings.Repeat(" ", n))
				i += len(seq) - 1
				continue
			}
		}
		b.WriteByte(row[i])
	}
	k := b.String()
	for {
		t := strings.TrimRight(k, " ")
		t = strings.TrimSuffix(t, "\x1b[0m")
		if t == k {
			return k
		}
		k = t
	}
}
