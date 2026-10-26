package screen

import (
	"bytes"
	"fmt"
	"slices"
	"testing"
	"time"

	"agtd.sh/internal/protocol"
)

// predictRig is a painter with alt scrollback and the wheel prediction on
// a 10-row alternate screen whose transcript is the top 7 rows (an input
// box from row 8, so the field's BoxTop is 8), showing "line NN" rows from
// a top the test moves, as an agent that scrolls by step rows a tick does.
type predictRig struct {
	t   *testing.T
	p   *Painter
	now time.Time
	seq uint32
	top int
}

const predictEnd = 7

func newPredictRig(t *testing.T) *predictRig {
	r := &predictRig{t: t, p: New(), now: time.Unix(1e9, 0)}
	r.p.SetAltScrollback(1000)
	r.p.SetPredictScroll(100 * time.Millisecond)
	r.p.SetClock(func() time.Time { return r.now })
	return r
}

func (r *predictRig) rows(top int) []protocol.FrameRow {
	rows := make([]protocol.FrameRow, 10)
	for y := range rows {
		vt := ""
		switch {
		case y < predictEnd:
			vt = fmt.Sprintf("line %02d", top+y)
		case y == 8:
			vt = "> "
		case y == 9:
			vt = "status"
		}
		rows[y] = protocol.FrameRow{Index: uint16(y), VT: []byte(vt)}
	}
	return rows
}

// show paints the agent's screen with top at the top, returning what the
// painter wrote.
func (r *predictRig) show(top int) []byte {
	r.seq++
	r.top = top
	f := &protocol.Frame{Seq: r.seq, Full: r.seq == 1, Alternate: true, Cols: 20, Lines: 10, Rows: r.rows(top),
		Field: &protocol.Field{Found: true, Alternate: true, BoxTop: 8}}
	return r.p.Apply(f, nil)
}

// shown is the text the terminal shows on row y, as far as the painter
// knows: the guess while one is drawn, the agent's row otherwise.
func (r *predictRig) shown(y int) string {
	if r.p.shown != nil {
		return string(r.p.shown[y])
	}
	return string(r.p.Row(y))
}

func wheelUp() []byte   { return []byte("\x1b[<64;5;3M") }
func wheelDown() []byte { return []byte("\x1b[<65;5;3M") }

// started is a rig whose agent has scrolled from line 0 to line 20, so
// the record holds lines 0 to 19.
func started(t *testing.T) *predictRig {
	r := newPredictRig(t)
	for top := 0; top <= 20; top++ {
		r.show(top)
	}
	return r
}

// A tick is drawn at once: the transcript moves by a row, filled from the
// record, the rows under it are left alone, and the agent's frame that
// follows to the same place changes nothing that is shown.
func TestPredictDrawsATickAtOnce(t *testing.T) {
	r := started(t)
	paint := r.p.Wheel(wheelUp())
	if !bytes.Contains(paint, []byte("line 19")) || bytes.Contains(paint, []byte("status")) {
		t.Fatalf("the tick drew %q", paint)
	}
	if got := r.shown(0); got != "line 19" {
		t.Fatalf("row 0 shows %q", got)
	}
	if got := r.shown(6); got != "line 25" {
		t.Fatalf("row 6 shows %q", got)
	}
	r.show(19)
	if st := r.p.PredictStats(); st.Guesses != 1 || st.TakenBack != 0 || st.Corrections != 0 {
		t.Fatalf("after the agent followed: %+v", st)
	}
	if r.p.shown != nil || !r.p.PredictDue().IsZero() {
		t.Fatal("a guess the agent followed is still drawn or pending")
	}
	// Down again, from what was seen at the live end.
	r.p.Wheel(wheelDown())
	if got := r.shown(6); got != "line 26" {
		t.Fatalf("down: row 6 shows %q", got)
	}
	r.show(20)
	if st := r.p.PredictStats(); st.TakenBack != 0 || st.Corrections != 0 {
		t.Fatalf("after down: %+v", st)
	}
}

// Ticks before the agent answers add up, and a frame that has moved part
// of the way leaves the rest drawn.
func TestPredictTicksAddUp(t *testing.T) {
	r := started(t)
	for range 3 {
		r.p.Wheel(wheelUp())
	}
	if got := r.shown(0); got != "line 17" {
		t.Fatalf("three ticks: row 0 shows %q", got)
	}
	r.show(19)
	if got := r.shown(0); got != "line 17" || r.p.pr.debt != -2 {
		t.Fatalf("one row answered: row 0 shows %q, debt %d", got, r.p.pr.debt)
	}
	r.show(17)
	if st := r.p.PredictStats(); st.TakenBack != 0 || st.Corrections != 0 || r.p.shown != nil {
		t.Fatalf("answered: %+v", st)
	}
}

// A guess the agent does not follow is taken back once its time is up:
// two round trips and predictWait after the tick, or predictSettle after
// a frame built once the worker had read the tick.
func TestPredictTakesBackWhatTheAgentIgnores(t *testing.T) {
	r := started(t)
	r.p.Wheel(wheelUp())
	if due := r.p.PredictDue(); due != r.now.Add(200*time.Millisecond+predictWait) {
		t.Fatalf("due %v after the tick", due.Sub(r.now))
	}
	r.now = r.now.Add(100 * time.Millisecond)
	if paint := r.p.PredictExpire(); paint != nil {
		t.Fatalf("taken back early: %q", paint)
	}
	// Frames that have not moved (the agent drew something else): the
	// first was built before the worker read the ack that followed the
	// tick, so it proves nothing; the second after it, so the agent gets
	// predictSettle more.
	for acked := r.seq; acked <= r.seq+1; acked++ {
		r.p.Apply(&protocol.Frame{Seq: acked + 1, Alternate: true, Cols: 20, Lines: 10, Acked: acked, HasAcked: true,
			Rows:  []protocol.FrameRow{{Index: 9, VT: fmt.Appendf(nil, "status %d", acked)}},
			Field: &protocol.Field{Found: true, Alternate: true, BoxTop: 8}}, nil)
		if acked == r.seq {
			if due := r.p.PredictDue(); due != r.now.Add(100*time.Millisecond+predictWait) {
				t.Fatalf("due %v after a frame that proves nothing", due.Sub(r.now))
			}
		}
	}
	if due := r.p.PredictDue(); due != r.now.Add(predictSettle) {
		t.Fatalf("due %v after the answering frame", due.Sub(r.now))
	}
	r.now = r.now.Add(predictSettle)
	paint := r.p.PredictExpire()
	if !bytes.Contains(paint, []byte("line 20")) || r.shown(0) != "line 20" {
		t.Fatalf("taken back: %q, row 0 %q", paint, r.shown(0))
	}
	if st := r.p.PredictStats(); st.TakenBack != 1 {
		t.Fatalf("%+v", st)
	}
}

// Two guesses in a row taken back pause the guessing; two gestures that
// would have been right resume it.
func TestPredictPausesAfterTwoMisses(t *testing.T) {
	r := started(t)
	for range 2 {
		r.p.Wheel(wheelUp())
		r.now = r.now.Add(time.Second)
		r.p.PredictExpire()
	}
	if !r.p.pr.paused {
		t.Fatal("not paused after two misses")
	}
	if paint := r.p.Wheel(wheelUp()); paint != nil {
		t.Fatalf("drew while paused: %q", paint)
	}
	// The agent follows this tick and the next: they would have been
	// right.
	r.show(19)
	r.now = r.now.Add(time.Second)
	r.p.PredictExpire()
	r.p.Wheel(wheelUp())
	r.show(18)
	r.now = r.now.Add(time.Second)
	r.p.PredictExpire()
	if r.p.pr.paused {
		t.Fatal("still paused after two gestures that would have been right")
	}
	if paint := r.p.Wheel(wheelUp()); paint == nil {
		t.Fatal("resumed but drew nothing")
	}
}

// The rows per tick are learned from what the agent does.
func TestPredictLearnsTheStep(t *testing.T) {
	r := started(t)
	for top := 21; top <= 40; top++ {
		r.show(top)
	}
	for range 6 {
		r.p.Wheel(wheelUp())
		r.show(r.top - 3)
		r.now = r.now.Add(time.Second)
		r.p.PredictExpire()
	}
	if s := r.p.step(); s != 3 {
		t.Fatalf("step %d after the agent moved 3 a tick (%.2f)", s, r.p.pr.stepF)
	}
	r.p.Wheel(wheelUp())
	if got, want := r.shown(0), fmt.Sprintf("line %02d", r.top-3); got != want {
		t.Fatalf("row 0 shows %q, want %q", got, want)
	}
}

// A row the agent draws over the transcript while scrolled (a "Jump to
// bottom" on its last row) is pinned once two guesses in a row moved it
// wrong, and left as the agent drew it from then on.
func TestPredictPinsTheAgentsChrome(t *testing.T) {
	r := started(t)
	marked := func(top int) {
		r.seq++
		r.top = top
		rows := r.rows(top)
		rows[predictEnd-1].VT = append(rows[predictEnd-1].VT, []byte("  Jump to bottom")...)
		r.p.Apply(&protocol.Frame{Seq: r.seq, Alternate: true, Cols: 20, Lines: 10, Rows: rows,
			Field: &protocol.Field{Found: true, Alternate: true, BoxTop: 8}}, nil)
	}
	for i := range 3 {
		r.p.Wheel(wheelUp())
		marked(r.top - 1)
		if pinned := r.p.pr.pinned[predictEnd-1]; pinned != (i >= 1) {
			t.Fatalf("after guess %d: pinned %v", i+1, pinned)
		}
	}
	r.p.Wheel(wheelUp())
	if got := r.shown(predictEnd - 1); got != fmt.Sprintf("line %02d  Jump to bottom", r.top+predictEnd-1) {
		t.Fatalf("the pinned row shows %q", got)
	}
	if r.p.pr.paused {
		t.Fatal("chrome paused the guessing")
	}
}

// Nothing is guessed for the wheel outside the transcript, with a
// modifier, at the top of what the record holds, or off the alternate
// screen; leaving the alternate screen drops a guess.
func TestPredictOnlyWhereItCan(t *testing.T) {
	r := started(t)
	for _, in := range []string{"\x1b[<64;5;9M", "\x1b[<68;5;3M", "\x1b[<0;5;3M", "x"} {
		if paint := r.p.Wheel([]byte(in)); paint != nil {
			t.Fatalf("%q drew %q", in, paint)
		}
	}
	r.show(1)
	r.p.Wheel(wheelUp())
	if r.shown(0) != "line 00" {
		t.Fatalf("one row up from line 1: %q", r.shown(0))
	}
	if paint := r.p.Wheel(wheelUp()); paint != nil || r.p.pr.debt != -1 {
		t.Fatalf("past the record's start: %q, debt %d", paint, r.p.pr.debt)
	}
	r.seq++
	r.p.Apply(&protocol.Frame{Seq: r.seq, Full: true, Cols: 20, Lines: 10, Rows: r.rows(0)}, nil)
	if r.p.shown != nil || r.p.pr.debt != 0 || r.p.alt != nil {
		t.Fatal("the primary screen kept the guess")
	}
	if paint := r.p.Wheel(wheelUp()); paint != nil {
		t.Fatalf("drew on the primary screen: %q", paint)
	}
}

func TestWheelTicks(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []int64
	}{
		{"\x1b[<64;1;1M", []int64{-1}},
		{"\x1b[<65;10;7M\x1b[<65;10;7Mx\x1b[<64;3;2m", []int64{1, 1, -1}},
		{"\x1b[<65;10;8M", nil}, // under the transcript
		{"\x1b[<69;10;2M", nil}, // shift
		{"\x1b[<0;10;2M", nil},  // a click
		{"\x1b[<64;10;2", nil},  // cut short
		{"\x1b[<6x4;1;1M", nil}, // malformed
		{"\x1b[A\x1b[<64;1;1M", []int64{-1}},
	} {
		if got := wheelTicks([]byte(c.in), predictEnd); !slices.Equal(got, c.want) {
			t.Errorf("%q: %v, want %v", c.in, got, c.want)
		}
	}
}

// The backdrop is the screen as shown, a guess included, while the
// terminal is on its primary screen, and nothing on the alternate one.
func TestBackdrop(t *testing.T) {
	if New().Backdrop() != nil {
		t.Fatal("a backdrop before anything was painted")
	}
	r := started(t)
	b := r.p.Backdrop()
	if !bytes.Contains(b, []byte("line 20")) || !bytes.Contains(b, []byte("status")) {
		t.Fatalf("backdrop %q", b)
	}
	r.p.Wheel(wheelUp())
	if b := r.p.Backdrop(); !bytes.Contains(b, []byte("line 19")) {
		t.Fatalf("backdrop under a guess %q", b)
	}
	p := New()
	p.Apply(&protocol.Frame{Seq: 1, Full: true, Alternate: true, Cols: 20, Lines: 2, Prefix: []byte("\x1b[?1049h"),
		Rows: []protocol.FrameRow{{Index: 0, VT: []byte("x")}}}, nil)
	if b := p.Backdrop(); b != nil {
		t.Fatalf("a backdrop on the alternate screen: %q", b)
	}
}
