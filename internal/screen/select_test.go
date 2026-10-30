package screen

import (
	"fmt"
	"testing"
	"time"

	"agtd.sh/internal/protocol"
)

func TestHighlightRow(t *testing.T) {
	for _, c := range []struct {
		row    string
		x0, x1 int
		want   string
	}{
		{"abcdef", 1, 3, "a\x1b[7mbcd\x1b[0mef"},
		// Empty cells in the selection become spaces; past it they stay
		// a cursor move.
		{"\x1b[2Cab\x1b[3Cc", 0, 4, "\x1b[7m  ab \x1b[0m\x1b[2Cc"},
		// A row shorter than the selection is padded.
		{"ab", 0, 4, "\x1b[7mab   \x1b[0m"},
		{"ab", 3, 4, "ab\x1b[C\x1b[7m  \x1b[0m"},
		// The row's own pen is put back after the selection, and an SGR
		// inside it is followed by the selection's again.
		{"\x1b[0m\x1b[2mabcdef", 1, 2, "\x1b[0m\x1b[2ma\x1b[7mbc\x1b[0m\x1b[2mdef"},
		{"ab\x1b[1mcd", 1, 2, "a\x1b[7mb\x1b[1m\x1b[7mc\x1b[0m\x1b[1md"},
		// A wide character is one grapheme over two cells.
		{"a界b", 1, 2, "a\x1b[7m界\x1b[0mb"},
	} {
		if got := string(highlightRow([]byte(c.row), c.x0, c.x1, []byte("\x1b[7m"))); got != c.want {
			t.Errorf("highlightRow(%q, %d, %d) = %q, want %q", c.row, c.x0, c.x1, got, c.want)
		}
	}
}

func TestUnselect(t *testing.T) {
	p := New()
	p.sel.inverse = true
	if got := string(p.unselect([]byte("\x1b[0m\x1b[2;7mab\x1b[7mcd"))); got != "\x1b[0m\x1b[2mabcd" {
		t.Errorf("inverse: %q", got)
	}
	p.sel.inverse, p.sel.bg = false, "48;2;38;79;120"
	if got := string(p.unselect([]byte("\x1b[0m\x1b[48;2;38;79;120mab\x1b[38;5;1;48;2;38;79;120mc"))); got != "\x1b[0mab\x1b[38;5;1mc" {
		t.Errorf("background: %q", got)
	}
}

func TestMouseReports(t *testing.T) {
	got := mouseReports([]byte("x\x1b[<0;10;12M\x1b[<32;14;12M\x1b[<0;14;12mq\x1b[<64;1;1M\x1b[<1;2"))
	want := []mouseReport{{0, 10, 12, true}, {32, 14, 12, true}, {0, 14, 12, false}, {64, 1, 1, true}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// selectRig is a painter with the selection prediction on over a 20x10
// screen of "row N" lines from an agent with SGR button-event tracking on,
// which draws the selections the test tells it to in reverse video.
type selectRig struct {
	t   *testing.T
	p   *Painter
	now time.Time
	seq uint32
}

func newSelectRig(t *testing.T) *selectRig {
	r := &selectRig{t: t, p: New(), now: time.Unix(1e9, 0)}
	r.p.SetPredictSelection(100 * time.Millisecond)
	r.p.SetClock(func() time.Time { return r.now })
	r.show(nil)
	return r
}

// show paints the agent's screen with spans (row to inclusive columns)
// selected.
func (r *selectRig) show(spans map[int][2]int) {
	r.seq++
	f := &protocol.Frame{Seq: r.seq, Full: r.seq == 1, Alternate: true, Cols: 20, Lines: 10}
	if r.seq == 1 {
		f.Prefix = []byte("\x1b[?1002h\x1b[?1006h\x1b[H")
	}
	for y := range 10 {
		vt := []byte(fmt.Sprintf("row %d", y))
		if sp, ok := spans[y]; ok {
			vt = highlightRow(vt, sp[0], sp[1], []byte("\x1b[7m"))
		}
		f.Rows = append(f.Rows, protocol.FrameRow{Index: uint16(y), VT: vt})
	}
	r.p.Apply(f, nil)
}

// gesture drags from row 2 to row 5 and releases, the agent answering
// with spans after each report (nil: it draws nothing), and returns
// whether the painter drew a guess.
func (r *selectRig) gesture(spans map[int][2]int) bool {
	before := r.p.SelectStats().Guessed
	for _, in := range []string{"\x1b[<0;3;3M", "\x1b[<32;6;4M", "\x1b[<32;8;6M", "\x1b[<0;8;6m"} {
		r.p.Select([]byte(in))
		r.show(spans)
	}
	r.now = r.now.Add(time.Second)
	r.p.PredictExpire()
	r.show(nil)
	return r.p.SelectStats().Guessed > before
}

// Two wrong guesses in a row pause the guessing until two gestures in a
// row would have been right.
func TestSelectPauses(t *testing.T) {
	r := newSelectRig(t)
	right := map[int][2]int{2: {2, 19}, 3: {0, 19}, 4: {0, 19}, 5: {0, 7}}
	// The first gesture's own answers teach the pen and bounds, and the
	// rest of it is guessed.
	r.gesture(right)
	if !r.gesture(right) {
		t.Fatal("not guessed once learned")
	}
	if st := r.p.SelectStats(); st.Right != 2 || st.Wrong != 0 {
		t.Fatalf("after right guesses: %+v", st)
	}
	wrong := map[int][2]int{2: {0, 19}, 3: {0, 19}, 4: {0, 19}, 5: {0, 19}}
	r.gesture(wrong)
	r.gesture(wrong)
	if r.gesture(right) {
		t.Fatalf("guessed after two wrong guesses: %+v", r.p.SelectStats())
	}
	if r.gesture(right) {
		t.Fatal("guessed after one gesture that would have been right")
	}
	if !r.gesture(right) {
		t.Fatalf("not guessed again after two that would have been right: %+v", r.p.SelectStats())
	}
}
