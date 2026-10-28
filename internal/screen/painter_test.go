package screen

import (
	"fmt"
	"strings"
	"testing"

	"agtd.sh/internal/protocol"
)

func TestPainterFullAndPartialFrames(t *testing.T) {
	p := New()
	full := &protocol.Frame{Seq: 1, Full: true, Cols: 10, Lines: 3, CursorRow: 0, CursorCol: 2,
		Prefix: []byte("\x1b[?2004h\x1b[3g\x1b[9G\x1bH\x1b[H"),
		Rows:   []protocol.FrameRow{{Index: 0, VT: []byte("$ ")}, {Index: 1}, {Index: 2, VT: []byte("\x1b[0m\x1b[31mx")}}}
	out := string(p.Apply(full, nil))
	want := syncBegin + "\x1b[?2004h\x1b[3g\x1b[9G\x1bH\x1b[H" +
		"\x1b[1;1H" + rowStart + "$ " + "\x1b[2;1H" + rowStart + "\x1b[3;1H" + rowStart + "\x1b[0m\x1b[31mx" +
		"\x1b[1;3H" + syncEnd
	if out != want {
		t.Fatalf("full frame:\n got %q\nwant %q", out, want)
	}
	if p.Seq() != 1 || string(p.Row(0)) != "$ " || p.Row(1) != nil || p.Row(5) != nil {
		t.Fatalf("cache: seq %d rows %q %q", p.Seq(), p.Row(0), p.Row(1))
	}
	// A partial frame paints only its rows, keeps the prefix and resets a
	// mode the new prefix no longer sets. The overlay ends with the echo's
	// cursor, so the frame's cursor (which lags the keys in flight) is not
	// written after it.
	part := &protocol.Frame{Seq: 2, Cols: 10, Lines: 3, CursorRow: 1, CursorCol: 0, Prefix: []byte("\x1b[?25l\x1b[3g\x1b[H"),
		Rows: []protocol.FrameRow{{Index: 1, VT: []byte("hi")}}, Keyboard: []byte("\x1b[=7;1u"), Effects: []byte("\a"), CursorShape: 6}
	out = string(p.Apply(part, []byte("OVERLAY")))
	want = syncBegin + "\x1b[?2004l" + "\x1b[?25l\x1b[3g\x1b[H" + "\x1b[2;1H" + rowStart + "hi" + "\x1b[=7;1u" + "\a" + "OVERLAY" + "\x1b[6 q" + syncEnd
	if out != want {
		t.Fatalf("partial frame:\n got %q\nwant %q", out, want)
	}
	if string(p.Row(0)) != "$ " || string(p.Row(1)) != "hi" {
		t.Fatalf("cache after partial: %q %q", p.Row(0), p.Row(1))
	}
	// The shape is only written when it changes, back to the default too.
	out = string(p.Apply(&protocol.Frame{Seq: 3, Cols: 10, Lines: 3, CursorShape: 6}, nil))
	if strings.Contains(out, " q") {
		t.Fatalf("shape repeated: %q", out)
	}
	out = string(p.Apply(&protocol.Frame{Seq: 4, Cols: 10, Lines: 3}, nil))
	if !strings.Contains(out, "\x1b[0 q") {
		t.Fatalf("shape not reset: %q", out)
	}
}

// While keys are in flight the echo's overlay places the cursor where the
// typing will be; the program's cursor in the frame is behind by those
// keys and must not be placed after it. Without an overlay the frame's
// cursor stands.
func TestPainterLeavesTheCursorToTheOverlay(t *testing.T) {
	p := New()
	p.Apply(&protocol.Frame{Seq: 1, Full: true, Cols: 20, Lines: 2}, nil)
	frame := &protocol.Frame{Seq: 2, Cols: 20, Lines: 2, CursorRow: 1, CursorCol: 3}
	overlay := "\x1b[2;1Htyping\x1b[2;7H"
	out := string(p.Apply(frame, []byte(overlay)))
	if !strings.HasSuffix(out, overlay+syncEnd) {
		t.Fatalf("the frame's cursor was placed after the overlay: %q", out)
	}
	out = string(p.Apply(&protocol.Frame{Seq: 3, Cols: 20, Lines: 2, CursorRow: 1, CursorCol: 3}, nil))
	if !strings.HasSuffix(out, "\x1b[2;4H"+syncEnd) {
		t.Fatalf("no overlay: the frame's cursor should stand: %q", out)
	}
}

func TestPainterScrollAndResize(t *testing.T) {
	p := New()
	p.Apply(&protocol.Frame{Seq: 1, Full: true, Cols: 5, Lines: 3, Rows: []protocol.FrameRow{{Index: 0, VT: []byte("a")}, {Index: 1, VT: []byte("b")}, {Index: 2, VT: []byte("c")}}}, nil)
	// Two rows scrolled away: their final contents painted in place, two
	// line feeds at the bottom, the cache shifted, and the new bottom rows
	// painted.
	out := string(p.Apply(&protocol.Frame{Seq: 2, Cols: 5, Lines: 3, Scrolled: 2, History: [][]byte{[]byte("A"), []byte("B")}, Rows: []protocol.FrameRow{{Index: 1, VT: []byte("d")}, {Index: 2, VT: []byte("e")}}}, nil))
	if !strings.HasPrefix(out, syncBegin+"\x1b[1;1H"+rowStart+"A"+"\x1b[2;1H"+rowStart+"B"+"\x1b[3;1H\n\n\x1b[2;1H") {
		t.Fatalf("scroll: %q", out)
	}
	// More scrolled than a screen (a full frame): the rows without
	// contents go first, as they were; the rest a screen at a time.
	q := New()
	q.Apply(&protocol.Frame{Seq: 1, Full: true, Cols: 5, Lines: 2}, nil)
	out = string(q.Apply(&protocol.Frame{Seq: 2, Full: true, Cols: 5, Lines: 2, Scrolled: 6, History: [][]byte{[]byte("w"), []byte("x"), []byte("y")}, Rows: []protocol.FrameRow{{Index: 0, VT: []byte("n")}, {Index: 1}}}, nil))
	if !strings.HasPrefix(out, syncBegin+"\x1b[2;1H\n\n\n"+"\x1b[1;1H"+rowStart+"w"+"\x1b[2;1H"+rowStart+"x"+"\x1b[2;1H\n\n"+"\x1b[1;1H"+rowStart+"y"+"\x1b[2;1H\n") {
		t.Fatalf("full scroll: %q", out)
	}
	if string(p.Row(0)) != "c" || string(p.Row(1)) != "d" || string(p.Row(2)) != "e" {
		t.Fatalf("cache after scroll: %q %q %q", p.Row(0), p.Row(1), p.Row(2))
	}
	// Scrolling the whole screen away blanks the cache.
	p.Apply(&protocol.Frame{Seq: 3, Cols: 5, Lines: 3, Scrolled: 3}, nil)
	if p.Row(0) != nil || p.Row(2) != nil {
		t.Fatal("cache not blank after scrolling everything")
	}
	// Another size clears the screen and the cache.
	out = string(p.Apply(&protocol.Frame{Seq: 4, Full: true, Cols: 6, Lines: 2, Rows: []protocol.FrameRow{{Index: 0, VT: []byte("z")}}}, nil))
	if !strings.Contains(out, "\x1b[2J") || string(p.Row(0)) != "z" || p.Row(2) != nil {
		t.Fatalf("resize: %q rows %q", out, p.Row(0))
	}
}

func TestParseModes(t *testing.T) {
	modes := parseModes([]byte("\x1b[?1049h\x1b[?2004h\x1b[4h\x1b[?25l\x1b[3g\x1b[9G\x1bH\x1b[H"))
	if set, ok := modes["?25"]; !modes["?1049"] || !modes["?2004"] || !modes["4"] || !ok || set || len(modes) != 4 {
		t.Fatalf("modes %v", modes)
	}
	if got := defaultMode("?2004", true); got != "\x1b[?2004l" {
		t.Fatalf("back from set %q", got)
	}
	if got := defaultMode("?25", false); got != "\x1b[?25h" {
		t.Fatalf("back from reset %q", got)
	}
	// A hidden cursor the next prefix no longer mentions is shown again.
	p := New()
	p.Apply(&protocol.Frame{Seq: 1, Full: true, Cols: 4, Lines: 1, Prefix: []byte("\x1b[?25l\x1b[H")}, nil)
	if out := string(p.Apply(&protocol.Frame{Seq: 2, Cols: 4, Lines: 1, Prefix: []byte("\x1b[H")}, nil)); !strings.Contains(out, "\x1b[?25h") {
		t.Fatalf("cursor not shown again: %q", out)
	}
	// Only changes are written: the alternate screen is entered once, not
	// again when the cursor hides (a repeat would clear it), and the tab
	// stops go only when they change.
	p = New()
	p.Apply(&protocol.Frame{Seq: 1, Full: true, Cols: 4, Lines: 1, Prefix: []byte("\x1b[?1049h\x1b[3g\x1b[H")}, nil)
	out := string(p.Apply(&protocol.Frame{Seq: 2, Cols: 4, Lines: 1, Prefix: []byte("\x1b[?1049h\x1b[?25l\x1b[3g\x1b[H")}, nil))
	if strings.Contains(out, "1049") || strings.Contains(out, "\x1b[3g") || !strings.Contains(out, "\x1b[?25l") {
		t.Fatalf("repeated modes written: %q", out)
	}
	modes, rest := splitPrefix([]byte("\x1b[?1049h\x1b[3g\x1b[9G\x1bH\x1b[H"))
	if len(modes) != 1 || string(rest) != "\x1b[3g\x1b[9G\x1bH\x1b[H" {
		t.Fatalf("split %v %q", modes, rest)
	}
}

func TestResetSetsMouseModesAgain(t *testing.T) {
	p := New()
	prefix := []byte("\x1b[?1000h\x1b[?1006h\x1b[?1049h\x1b[H")
	p.Apply(&protocol.Frame{Seq: 1, Full: true, Cols: 4, Lines: 1, Prefix: prefix}, nil)
	p.Reset()
	out := string(p.Apply(&protocol.Frame{Seq: 2, Full: true, Cols: 4, Lines: 1, Prefix: prefix}, nil))
	if !strings.Contains(out, "\x1b[?1000h") || !strings.Contains(out, "\x1b[?1006h") || strings.Contains(out, "?1049h") {
		t.Fatalf("after Reset: %q", out)
	}
}

// Under alt scrollback the alternate screen is never entered, rows leaving
// the top of the transcript are scrolled into the scrollback, and detach
// leaves the cursor under the screen.
func TestPainterAltScrollback(t *testing.T) {
	p := New()
	p.SetAltScrollback()
	if p.Leave() != nil {
		t.Fatal("Leave before any frame")
	}
	screen := func(top int) []protocol.FrameRow {
		var rows []protocol.FrameRow
		for y := range 9 {
			rows = append(rows, protocol.FrameRow{Index: uint16(y), VT: []byte(fmt.Sprintf("row %02d", top+y))})
		}
		return append(rows, protocol.FrameRow{Index: 9, VT: []byte("> ")})
	}
	out := string(p.Apply(&protocol.Frame{Seq: 1, Full: true, Alternate: true, Cols: 10, Lines: 10, Prefix: []byte("\x1b[?1049h\x1b[?2004h\x1b[H"), Rows: screen(0)}, nil))
	if strings.Contains(out, "1049") || !strings.Contains(out, "?2004h") {
		t.Fatalf("first frame: %q", out)
	}
	// The transcript moves up two rows: rows 00 and 01 are painted at the
	// top and scrolled off with line feeds, then every row is repainted.
	out = string(p.Apply(&protocol.Frame{Seq: 2, Alternate: true, Cols: 10, Lines: 10, Rows: screen(2)[:9]}, nil))
	if !strings.Contains(out, "\x1b[1;1H\x1b[0m\x1b[Krow 00\x1b[2;1H\x1b[0m\x1b[Krow 01\x1b[10;1H\n\n\x1b[1;1H") || !strings.Contains(out, "\x1b[10;1H\x1b[0m\x1b[K> ") {
		t.Fatalf("shift: %q", out)
	}
	if string(p.Leave()) != "\x1b[10;1H\r\n" {
		t.Fatalf("Leave %q", p.Leave())
	}
	// Back on the primary screen nothing is recorded or switched.
	out = string(p.Apply(&protocol.Frame{Seq: 3, Full: true, Cols: 10, Lines: 10, Prefix: []byte("\x1b[H"), Rows: []protocol.FrameRow{{Index: 0, VT: []byte("$ ")}}}, nil))
	if strings.Contains(out, "1049") || p.Leave() != nil {
		t.Fatalf("primary: %q, Leave %q", out, p.Leave())
	}
}

// A screen bigger than the terminal it is painted into, before the frames
// at the new size, keeps its bottom rows (the input box) and each row's
// first cells.
func TestBackdropCutsToFit(t *testing.T) {
	p := New()
	p.Apply(&protocol.Frame{Seq: 1, Full: true, Cols: 12, Lines: 3, Rows: []protocol.FrameRow{
		{Index: 0, VT: []byte("transcript")},
		{Index: 1, VT: []byte("────────────")},
		{Index: 2, VT: []byte("> typed text")},
	}}, nil)
	got := string(p.Backdrop(5, 2))
	want := cup(0, 0) + rowStart + "─────\x1b[0m" + cup(1, 0) + rowStart + "> typ\x1b[0m"
	if got != want {
		t.Fatalf("backdrop %q, want %q", got, want)
	}
	if got := string(p.Backdrop(12, 3)); !strings.Contains(got, "transcript") || !strings.Contains(got, "> typed text") {
		t.Fatalf("backdrop that fits %q", got)
	}
}

func TestCropRow(t *testing.T) {
	for _, c := range []struct{ row, want string }{
		{"abc", "abc"},
		{"abcdefgh", "abcd\x1b[0m"},
		{"\x1b[1mab\x1b[0mcdef", "\x1b[1mab\x1b[0mcd\x1b[0m"},
		// The formatter's empty cells.
		{"a\x1b[2Cbcdef", "a\x1b[2Cb\x1b[0m"},
		{"a\x1b[5Cb", "a\x1b[0m"},
		// A wide character that would straddle the cut is left out.
		{"abc世界", "abc\x1b[0m"},
		{"ab世界", "ab世\x1b[0m"},
		// A link open at the cut is ended.
		{"\x1b]8;;https://x\x1b\\abcdef\x1b]8;;\x1b\\", "\x1b]8;;https://x\x1b\\abcd\x1b[0m\x1b]8;;\x1b\\"},
		{"\x1b]8;;https://x\x1b\\ab\x1b]8;;\x1b\\cdef", "\x1b]8;;https://x\x1b\\ab\x1b]8;;\x1b\\cd\x1b[0m"},
	} {
		if got := string(cropRow([]byte(c.row), 4)); got != c.want {
			t.Errorf("cropRow(%q) = %q, want %q", c.row, got, c.want)
		}
	}
}

// The echo's drawing of typed keys covers rows the frames painted; the
// first frame without it repaints them, since the agent may have moved
// what was under it (pi moves its input box up to open a menu).
func TestOverdrawnRowsRepaint(t *testing.T) {
	p := New()
	rows := func(vt ...string) []protocol.FrameRow {
		var out []protocol.FrameRow
		for i, s := range vt {
			out = append(out, protocol.FrameRow{Index: uint16(i), VT: []byte(s)})
		}
		return out
	}
	p.Apply(&protocol.Frame{Seq: 1, Full: true, Cols: 10, Lines: 3, Rows: rows("one", "two", "box")}, nil)
	p.Apply(&protocol.Frame{Seq: 2, Cols: 10, Lines: 3, Rows: rows("one", "box", "menu")}, []byte(cup(2, 0)+"/"))
	got := string(p.Apply(&protocol.Frame{Seq: 3, Cols: 10, Lines: 3, CursorRow: 1, CursorCol: 1}, nil))
	for _, row := range []string{"one", "box", "menu"} {
		if !strings.Contains(got, row) {
			t.Fatalf("row %q not repainted after the overlay: %q", row, got)
		}
	}
	if got := string(p.Apply(&protocol.Frame{Seq: 4, Cols: 10, Lines: 3, CursorRow: 1, CursorCol: 1}, nil)); strings.Contains(got, "menu") {
		t.Fatalf("repainted again: %q", got)
	}
	// Typed keys drawn straight to the terminal count too.
	p.Overdrawn()
	if got := string(p.Apply(&protocol.Frame{Seq: 5, Cols: 10, Lines: 3, CursorRow: 1, CursorCol: 1}, nil)); !strings.Contains(got, "menu") {
		t.Fatalf("not repainted after Overdrawn: %q", got)
	}
}

// Under alt scrollback a look inside a synchronized-output block (half a
// repaint, from a program that left mode 2026 set past the worker's hold)
// is painted but not recorded; the first look after the block records the
// move it made.
func TestPainterAltScrollbackSkipsSyncedLooks(t *testing.T) {
	// Value: protects=the terminal's scrollback gets each transcript row
	// once, not half a repaint read as a jump; fails_when=altFrame records
	// frames while ?2026 is set; why_new=TestPainterAltScrollback never
	// sets mode 2026; seam=none; no_claim=the worker's frameSyncHold
	// (TestFrameHoldsForSyncOutput).
	p := New()
	p.SetAltScrollback()
	screen := func(top int) []protocol.FrameRow {
		var rows []protocol.FrameRow
		for y := range 9 {
			rows = append(rows, protocol.FrameRow{Index: uint16(y), VT: []byte(fmt.Sprintf("row %02d", top+y))})
		}
		return append(rows, protocol.FrameRow{Index: 9, VT: []byte("> ")})
	}
	p.Apply(&protocol.Frame{Seq: 1, Full: true, Alternate: true, Cols: 10, Lines: 10, Prefix: []byte("\x1b[?1049h\x1b[H"), Rows: screen(0)}, nil)
	out := string(p.Apply(&protocol.Frame{Seq: 2, Alternate: true, Cols: 10, Lines: 10, Prefix: []byte("\x1b[?1049h\x1b[?2026h\x1b[H"), Rows: screen(2)[:9]}, nil))
	if strings.Contains(out, "row 00") || strings.Contains(out, "\n") {
		t.Fatalf("a synced look was recorded: %q", out)
	}
	out = string(p.Apply(&protocol.Frame{Seq: 3, Alternate: true, Cols: 10, Lines: 10, Prefix: []byte("\x1b[?1049h\x1b[H")}, nil))
	if !strings.Contains(out, "row 00\x1b[2;1H\x1b[0m\x1b[Krow 01\x1b[10;1H\n\n") {
		t.Fatalf("the move made inside the block was not recorded after it: %q", out)
	}
}

func TestPainterAltScrollbackDoesNotPushTheSeed(t *testing.T) {
	// Value: protects=under alt scrollback, the worker's seed of a new
	// record fills the painter's copy but is not scrolled into the
	// terminal, where each reattach would add it again, while rows
	// recorded after it are; fails_when=altFrame pushes Follow's rows
	// for a new record; why_new=Frame.Record is new; seam=none;
	// no_claim=seeding the terminal's scrollback (a later design item).
	p := New()
	p.SetAltScrollback()
	rows := func(top int) []protocol.FrameRow {
		out := make([]protocol.FrameRow, 4)
		for y := range out {
			out[y] = protocol.FrameRow{Index: uint16(y), VT: fmt.Appendf(nil, "line %02d", top+y)}
		}
		return out
	}
	rec := func(top int, isNew bool, from int) *protocol.FrameRecord {
		r := &protocol.FrameRecord{New: isNew, Pos: int64(top), Mark: int64(top), End: 4}
		for n := from; n < top; n++ {
			r.Rows = append(r.Rows, fmt.Appendf(nil, "line %02d", n))
		}
		return r
	}
	out := string(p.Apply(&protocol.Frame{Seq: 1, Full: true, Alternate: true, Cols: 10, Lines: 4, Rows: rows(10), Record: rec(10, true, 0), HasRecord: true}, nil))
	if strings.Contains(out, "line 05") || p.Recorded() != 10 {
		t.Fatalf("seed pushed (recorded %d): %q", p.Recorded(), out)
	}
	out = string(p.Apply(&protocol.Frame{Seq: 2, Alternate: true, Cols: 10, Lines: 4, Rows: rows(12), Record: rec(12, false, 10), HasRecord: true}, nil))
	if !strings.Contains(out, "line 10") || !strings.Contains(out, "line 11") {
		t.Fatalf("recorded rows not pushed: %q", out)
	}
}
