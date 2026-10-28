package screen

import (
	"fmt"
	"strings"
	"testing"

	"agtd.sh/internal/protocol"
)

// view is a screen of height rows: the transcript lines from top, then
// the bottom rows (an input box, status rows).
func view(lines []string, top, height int, bottom ...string) [][]byte {
	rows := make([][]byte, height)
	for i := 0; i < height-len(bottom) && top+i < len(lines); i++ {
		if top+i >= 0 {
			rows[i] = []byte(lines[top+i])
		}
	}
	for i, b := range bottom {
		rows[height-len(bottom)+i] = []byte(b)
	}
	return rows
}

func numbered(n int) []string {
	var out []string
	for i := 1; i <= n; i++ {
		out = append(out, fmt.Sprintf("row %03d", i))
	}
	return out
}

func recorded(t *Transcript) string {
	var out []string
	for _, r := range t.Rows(0) {
		out = append(out, string(r))
	}
	return strings.Join(out, ",")
}

func TestTranscriptShiftsRecordOnce(t *testing.T) {
	lines := numbered(30)
	box := []string{"────", "❯ ", "────", "status 1s"}
	tr := NewTranscript(0)
	// The viewport scrolls down the transcript one or two rows at a time,
	// the status row changing under it; the user scrolls back up and down
	// again; nothing is recorded twice.
	tops := []int{0, 1, 3, 4, 2, 0, 2, 4, 6, 7, 9}
	for i, top := range tops {
		tr.Update(view(lines, top, 10, append(box[:3:3], fmt.Sprintf("status %ds", i))...), 6)
	}
	if got, want := recorded(tr), strings.Join(lines[:9], ","); got != want {
		t.Fatalf("recorded %s, want %s", got, want)
	}
	if tr.Jumps != 0 {
		t.Fatalf("%d jumps", tr.Jumps)
	}
}

func TestTranscriptInPlaceChangesRecordNothing(t *testing.T) {
	lines := numbered(6)
	tr := NewTranscript(0)
	for i := range 5 {
		rows := view(lines, 0, 8, "spinner "+strings.Repeat(".", i), "❯ ")
		tr.Update(rows, 6)
	}
	if tr.Recorded() != 0 || tr.Jumps != 0 {
		t.Fatalf("recorded %d, jumps %d", tr.Recorded(), tr.Jumps)
	}
}

func TestTranscriptJumpBackIsFound(t *testing.T) {
	lines := numbered(40)
	box := []string{"────", "❯ "}
	tr := NewTranscript(0)
	for top := 0; top <= 30; top++ {
		tr.Update(view(lines, top, 10, box...), 8)
	}
	before := tr.Recorded()
	// A jump far back to rows already recorded is found; the jump to the
	// end again is new content, which starts where the record ends.
	tr.Update(view(lines, 5, 10, box...), 8)
	tr.Update(view(lines, 30, 10, box...), 8)
	tr.Update(view(lines, 32, 10, box...), 8)
	if tr.Recorded() != before+2 || tr.Jumps != 1 {
		t.Fatalf("recorded %d after %d, jumps %d: %s", tr.Recorded(), before, tr.Jumps, recorded(tr))
	}
	if got, want := recorded(tr), strings.Join(lines[:32], ","); got != want {
		t.Fatalf("recorded %s", got)
	}
}

// A screen of new content records what the old one still showed, and a
// few rows the program repeats (its banner) are not a jump back.
func TestTranscriptNewContentFlushes(t *testing.T) {
	banner := []string{"banner 1", "banner 2", "banner 3", "", "! cmd"}
	box := []string{"────", "❯ "}
	tr := NewTranscript(0)
	tr.Update(view(banner, 0, 10, box...), 8)
	tr.Update(view(append(append([]string{}, banner...), "out 1", "out 2"), 0, 10, box...), 8)
	// Cleared: the banner again on an empty transcript.
	tr.Update(view(banner[:3], 0, 10, box...), 8)
	if got := recorded(tr); got != "banner 1,banner 2,banner 3,,! cmd,out 1,out 2" {
		t.Fatalf("recorded %q", got)
	}
	if tr.Jumps != 1 {
		t.Fatalf("%d jumps", tr.Jumps)
	}
}

func TestTranscriptBounded(t *testing.T) {
	lines := numbered(50)
	tr := NewTranscript(10)
	for top := 0; top <= 40; top++ {
		tr.Update(view(lines, top, 10, "❯ "), 9)
	}
	if tr.Recorded() != 40 || len(tr.Rows(0)) != 10 || string(tr.Rows(0)[0]) != "row 031" {
		t.Fatalf("recorded %d, held %d: %s", tr.Recorded(), len(tr.Rows(0)), recorded(tr))
	}
	if got := tr.Rows(38); len(got) != 2 || string(got[0]) != "row 039" {
		t.Fatalf("rows from 38: %q", got)
	}
}

func TestRowKey(t *testing.T) {
	for _, c := range []struct{ a, b string }{
		{"     shell row 010", "\x1b[5Cshell\x1b[1Crow\x1b[1C010"},
		{"x\x1b[Cy", "x y"},
		{"abc   ", "abc"},
		{"\x1b[1mabc\x1b[0m", "\x1b[1mabc"},
	} {
		if RowKey([]byte(c.a)) != RowKey([]byte(c.b)) {
			t.Errorf("RowKey(%q) = %q, RowKey(%q) = %q", c.a, RowKey([]byte(c.a)), c.b, RowKey([]byte(c.b)))
		}
	}
	if RowKey([]byte("\x1b[38;2;1;2;3mabc")) == RowKey([]byte("abc")) {
		t.Error("a colour difference is lost")
	}
}

// A row's cursor-forward count comes from the daemon: one too large to
// parse, or huge, is bounded, never a panic or a huge key.
func TestRowKeyBoundsCursorForward(t *testing.T) {
	// Value: protects=a malformed frame row cannot crash agt or make it
	// allocate gigabytes; fails_when=RowKey repeats an unbounded count;
	// why_new=only the formatter's small counts were seen; seam=none;
	// no_claim=what a daemon sends.
	for _, row := range []string{"a\x1b[99999999999999999999Cb", "a\x1b[1000000000Cb"} {
		if k := RowKey([]byte(row)); len(k) > maxCols+2 {
			t.Fatalf("RowKey(%q) is %d bytes", row, len(k))
		}
	}
	if k := RowKey([]byte("a\x1b[3Cb")); k != "a   b" {
		t.Fatalf("RowKey kept %q", k)
	}
}

// rowsOf is lines[from:to] as rows.
func rowsOf(lines []string, from, to int) [][]byte {
	var out [][]byte
	for _, l := range lines[from:to] {
		out = append(out, []byte(l))
	}
	return out
}

func TestTranscriptFollowsAWorkersRecord(t *testing.T) {
	// Value: protects=a client copy of the worker's record holds each row
	// once, in order, from the seed on, and starts over where it cannot
	// be contiguous; fails_when=Follow appends rows it holds, misnumbers
	// them, or keeps a copy across a gap, a new record or one that went
	// back; why_new=Follow is new; seam=none; no_claim=the worker's side
	// (internal/worker/altrecord_test.go).
	lines := numbered(30)
	tr := NewTranscript(0)
	// The seed: rows 5 to 10 of a record 10 long, the screen at 12.
	if out := tr.Follow(&protocol.FrameRecord{New: true, Pos: 12, Mark: 10, Jumps: 1, Rows: rowsOf(lines, 5, 10)}); len(out) != 5 {
		t.Fatalf("seed added %d rows", len(out))
	}
	if tr.Oldest() != 5 || tr.Recorded() != 10 || tr.Pos() != 12 || tr.Jumps != 1 {
		t.Fatalf("oldest %d, recorded %d, pos %d, jumps %d", tr.Oldest(), tr.Recorded(), tr.Pos(), tr.Jumps)
	}
	if got := string(tr.Rows(7)[0]); got != lines[7] {
		t.Fatalf("row 7 is %q", got)
	}
	// Rows it holds already are not added again; nothing new is nothing.
	if out := tr.Follow(&protocol.FrameRecord{Pos: 13, Mark: 12, Rows: rowsOf(lines, 8, 12)}); len(out) != 2 || string(out[0]) != lines[10] {
		t.Fatalf("overlap added %q", out)
	}
	if out := tr.Follow(&protocol.FrameRecord{Pos: 11, Mark: 12}); out != nil || tr.Pos() != 11 {
		t.Fatalf("no rows added %q, pos %d", out, tr.Pos())
	}
	if got, want := recorded(tr), strings.Join(lines[5:12], ","); got != want {
		t.Fatalf("record %s", got)
	}
	// A gap: the copy starts after it.
	tr.Follow(&protocol.FrameRecord{Pos: 25, Mark: 20, Rows: rowsOf(lines, 18, 20)})
	if tr.Oldest() != 18 || tr.Recorded() != 20 || len(tr.Rows(0)) != 2 {
		t.Fatalf("after a gap: oldest %d, recorded %d", tr.Oldest(), tr.Recorded())
	}
	// A record that went back (the agent's screen started anew without a
	// frame between) and a new one both replace the copy.
	tr.Follow(&protocol.FrameRecord{Pos: 3, Mark: 3, Rows: rowsOf(lines, 0, 3)})
	if tr.Oldest() != 0 || tr.Recorded() != 3 {
		t.Fatalf("went back: oldest %d, recorded %d", tr.Oldest(), tr.Recorded())
	}
	tr.Follow(&protocol.FrameRecord{New: true, Pos: 4, Mark: 4, Rows: rowsOf(lines, 20, 24)})
	if got := recorded(tr); got != strings.Join(lines[20:24], ",") {
		t.Fatalf("new record: %s", got)
	}
}

func TestTranscriptFollowKeepsItsBound(t *testing.T) {
	// Value: protects=a client following a long run keeps at most its
	// bound; fails_when=Follow skips trim; why_new=Follow is new;
	// seam=none; no_claim=none.
	lines := numbered(100)
	tr := NewTranscript(20)
	for m := 10; m <= 100; m += 10 {
		tr.Follow(&protocol.FrameRecord{Pos: int64(m), Mark: int64(m), Rows: rowsOf(lines, m-10, m)})
	}
	if n := len(tr.Rows(0)); n > 25 || tr.Recorded() != 100 || string(tr.Rows(99)[0]) != lines[99] {
		t.Fatalf("%d rows held, recorded %d", n, tr.Recorded())
	}
}

func TestTranscriptStateRestores(t *testing.T) {
	// Value: protects=a record handed to a new worker image carries on as
	// if never handed over, and a state that does not add up restores
	// empty rather than misnumbered; fails_when=State or
	// RestoreTranscript drop a field or skip the keys the next Update
	// compares; why_new=both are new; seam=none; no_claim=the JSON in the
	// handoff (internal/worker/altrecord_test.go).
	lines := numbered(40)
	a := NewTranscript(0)
	for top := 0; top <= 10; top++ {
		a.Update(view(lines, top, 12, "> "), 11)
	}
	b := RestoreTranscript(0, a.State())
	for top := 11; top <= 20; top++ {
		a.Update(view(lines, top, 12, "> "), 11)
		b.Update(view(lines, top, 12, "> "), 11)
	}
	if recorded(a) != recorded(b) || a.Pos() != b.Pos() || a.Recorded() != b.Recorded() || a.Jumps != b.Jumps {
		t.Fatalf("restored record went its own way:\n%s\n%s", recorded(a), recorded(b))
	}
	st := a.State()
	st.Mark++
	if c := RestoreTranscript(0, st); c.Recorded() != 0 || c.Rows(0) != nil {
		t.Fatalf("a state that does not add up restored %d rows", c.Recorded())
	}
}
