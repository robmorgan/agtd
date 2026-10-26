package screen

import (
	"fmt"
	"strings"
	"testing"
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
