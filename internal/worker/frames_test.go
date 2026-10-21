package worker

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"agtd.sh/internal/protocol"
	"agtd.sh/internal/screen"
	"agtd.sh/internal/session"
	"go.mitchellh.com/libghostty"
)

// frameRig is a source terminal with one frame attachment, and a client
// terminal the frames are painted into through the painter.
type frameRig struct {
	t      *testing.T
	src    *terminalState
	s      *ownerState
	a      *ownerAttachment
	client *libghostty.Terminal
	p      *screen.Painter
	last   *protocol.Frame
	paint  []byte
}

func newFrameRig(t *testing.T, cols, rows uint16) *frameRig {
	t.Helper()
	// Scrollback caps are reached at different rows by two terminals, so
	// the rig has more than the streams fill.
	src, err := newTerminalState(cols, rows, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	s := newOwnerState("frames", nil, src)
	a := &ownerAttachment{kind: session.AttachmentAttach, lastUsed: time.Now().Add(-time.Hour)}
	if a.frames, err = newFrameState(); err != nil {
		t.Fatal(err)
	}
	s.attachments["attach-1"] = a
	client, err := libghostty.NewTerminal(libghostty.WithSize(cols, rows), libghostty.WithMaxScrollbackBytes(64<<20))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.frames.close(); client.Close() })
	r := &frameRig{t: t, src: src, s: s, a: a, client: client, p: screen.New()}
	// What the CLI writes before the first frame: a clear and the history.
	history, err := src.historySnapshot(allScrollback)
	if err != nil {
		t.Fatal(err)
	}
	client.VTWrite(append([]byte("\x1b[2J\x1b[H"), history...))
	return r
}

// feed gives the source output through the owner's path (effects, dirty).
func (r *frameRig) feed(data string) {
	r.t.Helper()
	if err := r.s.publishScanned([]byte(data)); err != nil {
		r.t.Fatal(err)
	}
}

// frame builds the next frame and paints it into the client.
func (r *frameRig) frame() *protocol.Frame {
	r.t.Helper()
	f, err := r.s.buildFrame(r.a, time.Now())
	if err != nil {
		r.t.Fatal(err)
	}
	r.paint = r.p.Apply(f, nil)
	r.client.VTWrite(r.paint)
	r.last = f
	return f
}

// same checks the client shows what the source does: every written cell
// with its style, the cursor, the active screen and the scrollback. Blank
// cells are not compared: the formatter writes a gap before later text
// with the pen of the cell before it, and nothing for a row of cells
// erased with a background colour, so those carry no style or the wrong
// one, in snapshots as in frames (ARCHITECTURE.md lists this).
func (r *frameRig) same(what string) {
	r.t.Helper()
	if sh, ch := must(r.src.term.ScrollbackRows()), must(r.client.ScrollbackRows()); sh != ch {
		paint := r.paint
		if len(paint) > 1500 {
			paint = paint[:1500]
		}
		r.t.Fatalf("%s: scrollback rows %d in the source, %d in the client (frame %d: full %v, scrolled %d, %d history rows, %d rows, prefix %q)\npainted: %q", what, sh, ch, r.last.Seq, r.last.Full, r.last.Scrolled, len(r.last.History), len(r.last.Rows), r.last.Prefix, paint)
	}
	wantCells, _, _, _, _ := cellsOf(r.src.term)
	gotCells, _, _, _, _ := cellsOf(r.client)
	if wantCells != gotCells {
		var y int
		fmt.Sscanf(firstDiff(wantCells, gotCells), "line %d:", &y)
		r.t.Fatalf("%s: screens differ at %s\nframe VT %d: %q\npainter row %d: %q", what, firstDiff(wantCells, gotCells), y, frameRow(r.last, y), y, r.p.Row(y))
	}
	wx, wy := must(r.src.term.CursorX()), must(r.src.term.CursorY())
	gx, gy := must(r.client.CursorX()), must(r.client.CursorY())
	if wx != gx || wy != gy {
		r.t.Fatalf("%s: cursor want %d,%d got %d,%d", what, wx, wy, gx, gy)
	}
	if must(r.src.term.ActiveScreen()) != must(r.client.ActiveScreen()) {
		r.t.Fatalf("%s: active screen differs", what)
	}
	if must(r.src.term.ActiveScreen()) == libghostty.ScreenPrimary {
		want, got := formatContent(r.src.term, libghostty.FormatterFormatPlain, true), formatContent(r.client, libghostty.FormatterFormatPlain, true)
		if want != got {
			r.t.Fatalf("%s: scrollback and screen differ at %s (%d vs %d history rows)", what, firstDiff(want, got), must(r.src.term.ScrollbackRows()), must(r.client.ScrollbackRows()))
		}
	}
	for _, m := range []libghostty.Mode{libghostty.ModeBracketedPaste, libghostty.ModeCursorVisible, libghostty.ModeWraparound} {
		if must(r.src.term.Mode(m)) != must(r.client.Mode(m)) {
			r.t.Fatalf("%s: mode %v differs", what, m)
		}
	}
}

// firstDiff describes the first line on which two texts differ.
func firstDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(w) || i < len(g); i++ {
		var a, b string
		if i < len(w) {
			a = w[i]
		}
		if i < len(g) {
			b = g[i]
		}
		if a != b {
			return fmt.Sprintf("line %d:\nwant %q\ngot  %q", i, a, b)
		}
	}
	return "nowhere"
}

func frameRow(f *protocol.Frame, y int) []byte {
	for _, row := range f.Rows {
		if int(row.Index) == y {
			return row.VT
		}
	}
	return nil
}

// The recorded streams, painted frame by frame through the painter, leave
// the client's screen, cursor and scrollback as the source's.
func TestFramesReplayStreams(t *testing.T) {
	for _, st := range loadStreams(t) {
		t.Run(st.name, func(t *testing.T) {
			r := newFrameRig(t, streamCols, streamRows)
			frames, full, sent := 0, 0, 0
			const chunk = 2048
			for i := 0; i < len(st.data); i += chunk {
				r.feed(string(st.data[i:min(i+chunk, len(st.data))]))
				f := r.frame()
				frames++
				if f.Full {
					full++
				}
				for _, row := range f.Rows {
					sent += len(row.VT)
				}
				r.same(st.name)
			}
			t.Logf("%s: %d frames (%d full), %d row bytes for %d bytes of output", st.name, frames, full, sent, len(st.data))
		})
	}
}

// Rows the primary screen scrolls into history reach the client's own
// scrollback, below and at the scrollback cap; the alternate screen never
// scrolls anything.
func TestFramesScrollIntoHistory(t *testing.T) {
	r := newFrameRig(t, 20, 4)
	if f := r.frame(); !f.Full || f.Scrolled != 0 || len(f.Rows) != 4 {
		t.Fatalf("first frame %+v", f)
	}
	r.feed("one\r\ntwo\r\nthree\r\nfour\r\nfive\r\n")
	f := r.frame()
	if f.Full || f.Scrolled != 2 || len(f.History) != 2 || string(f.History[0]) != "one" || string(f.History[1]) != "two" {
		t.Fatalf("after five lines: full %v scrolled %d history %q", f.Full, f.Scrolled, f.History)
	}
	r.same("five lines")
	// More than a screen's worth at once is a full repaint.
	r.feed(strings.Repeat("x\r\n", 10))
	if f = r.frame(); !f.Full {
		t.Fatalf("ten lines: not full: %+v", f)
	}
	r.same("ten lines")
	// At the cap the row count stops growing; the shift is found from the
	// rows themselves.
	capped, err := newTerminalState(20, 4, 1)
	if err != nil {
		t.Fatal(err)
	}
	old := [][]byte{[]byte("a"), []byte("b"), []byte("c"), []byte("d")}
	if k := scrolledRows(old, [][]byte{[]byte("c"), []byte("d"), []byte("e"), nil}, 5, 5); k != 2 {
		t.Fatalf("shift of 2 found %d", k)
	}
	if k := scrolledRows(old, [][]byte{[]byte("a"), []byte("b"), []byte("c"), []byte("z")}, 5, 5); k != 0 {
		t.Fatalf("a changed bottom row is not a scroll: %d", k)
	}
	if k := scrolledRows([][]byte{nil, nil, nil, nil}, [][]byte{nil, nil, nil, []byte("t")}, 5, 5); k != 0 {
		t.Fatalf("blank rows do not count as a scroll: %d", k)
	}
	if k := scrolledRows(old, [][]byte{[]byte("b"), []byte("c"), []byte("d"), []byte("e")}, 9, 5); k != 4 {
		t.Fatalf("history growth wins: %d", k)
	}
	_ = capped
	// The alternate screen.
	r.feed("\x1b[?1049h")
	if f = r.frame(); !f.Full || !f.Alternate || !bytes.Contains(f.Prefix, []byte("?1049h")) {
		t.Fatalf("alternate screen: %+v", f)
	}
	r.same("alternate")
	r.feed(strings.Repeat("y\r\n", 6))
	if f = r.frame(); f.Scrolled != 0 {
		t.Fatalf("the alternate screen scrolled %d", f.Scrolled)
	}
	r.same("alternate scrolled")
	r.feed("\x1b[?1049l")
	if f = r.frame(); !f.Full || f.Alternate || bytes.Contains(f.Prefix, []byte("?1049h")) {
		t.Fatalf("back to primary: %+v", f)
	}
	r.same("primary again")
}

// Modes, keyboard state, the cursor's shape, colours and effects ride in
// frames only when they change, and the client mirrors them.
func TestFramesCarryTerminalState(t *testing.T) {
	r := newFrameRig(t, 20, 4)
	r.frame()
	r.feed("\x1b[?2004h\x1b[?25l")
	f := r.frame()
	if !bytes.Contains(f.Prefix, []byte("?2004h")) || !bytes.Contains(f.Prefix, []byte("?25l")) || len(f.Rows) != 0 {
		t.Fatalf("modes: %+v", f)
	}
	r.same("modes on")
	r.feed("\x1b[?2004l")
	if f = r.frame(); !bytes.Contains(f.Prefix, []byte("?25l")) || bytes.Contains(f.Prefix, []byte("?2004")) {
		t.Fatalf("mode off: %q", f.Prefix)
	}
	r.same("paste off")
	if must(r.client.Mode(libghostty.ModeBracketedPaste)) {
		t.Fatal("the client kept bracketed paste on")
	}
	r.feed("\x1b[?25h\x1b[>7u\x1b[>4;2m")
	if f = r.frame(); !bytes.Contains(f.Keyboard, []byte("=7;1u")) || !bytes.Contains(f.Keyboard, []byte(">4;2m")) {
		t.Fatalf("keyboard: %q", f.Keyboard)
	}
	if flags := must(r.client.KittyKeyboardFlags()); flags != 7 {
		t.Fatalf("client kitty flags %v", flags)
	}
	if f = r.frame(); f.Keyboard != nil || f.Prefix != nil {
		t.Fatalf("unchanged state repeated: %+v", f)
	}
	r.feed("\x1b[6 q\a\x1b]777;notify;Title;Body\x1b\\")
	if f = r.frame(); f.CursorShape != 6 || string(f.Effects) != "\a\x1b]777;notify;Title;Body\x1b\\" {
		t.Fatalf("shape and effects: %d %q", f.CursorShape, f.Effects)
	}
	_, _, _, shape, _ := cellsOf(r.client)
	if shape != libghostty.CursorVisualStyleBar {
		t.Fatalf("client cursor shape %v", shape)
	}
	r.feed("\x1b]11;#102030\x1b\\")
	if f = r.frame(); !bytes.Contains(f.Colors, []byte("11;")) {
		t.Fatalf("colours: %q", f.Colors)
	}
	// A resize is a full frame of the new size.
	if err := r.src.resize(30, 6, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := r.client.Resize(30, 6, 0, 0); err != nil {
		t.Fatal(err)
	}
	if f = r.frame(); !f.Full || f.Cols != 30 || f.Lines != 6 {
		t.Fatalf("resize: %+v", f)
	}
	r.same("resized")
}

// The field rides in a frame when asked for outright, or when the client
// wants it and the program has settled or the client just typed.
func TestFramesCarryTheField(t *testing.T) {
	r := newFrameRig(t, 40, 5)
	r.feed("$ ")
	if f := r.frame(); f.Field != nil {
		t.Fatalf("field without a request: %+v", f.Field)
	}
	r.a.frames.askField = true
	r.feed("x")
	if f := r.frame(); f.Field == nil || !f.Field.Found || f.Field.Rows[0] != "x" {
		t.Fatalf("asked field: %+v", f.Field)
	}
	r.a.frames.wantField.Store(true)
	r.s.lastOutputAt = time.Now()
	r.feed("y")
	if f := r.frame(); f.Field != nil {
		t.Fatalf("field while output flows: %+v", f.Field)
	}
	r.a.lastUsed = time.Now()
	r.feed("z")
	if f := r.frame(); f.Field == nil || f.Field.Rows[0] != "xyz" {
		t.Fatalf("field after typing: %+v", f.Field)
	}
	r.a.lastUsed = time.Now().Add(-time.Minute)
	r.s.lastOutputAt = time.Now().Add(-time.Second)
	if f := r.frame(); f.Field == nil {
		t.Fatal("no field once settled")
	}
}

// The history snapshot carries the scrollback rows and leaves the screen
// blank for the first frame; under the alternate screen it is the primary
// screen's.
func TestHistorySnapshot(t *testing.T) {
	src, err := newTerminalState(10, 3, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if h, err := src.historySnapshot(allScrollback); err != nil || h != nil {
		t.Fatalf("empty history: %q %v", h, err)
	}
	src.feed([]byte("one\r\ntwo\r\nthree\r\nfour\r\n"))
	h, err := src.historySnapshot(allScrollback)
	if err != nil || string(h) != "one\r\ntwo\r\n\r\n\r\n" {
		t.Fatalf("history: %q %v", h, err)
	}
	if h, err = src.historySnapshot(1); err != nil || string(h) != "two\r\n\r\n\r\n" {
		t.Fatalf("one row: %q %v", h, err)
	}
	src.feed([]byte("\x1b[?1049h"))
	if h, err = src.historySnapshot(allScrollback); err != nil || string(h) != "one\r\ntwo\r\n\r\n\r\n" {
		t.Fatalf("under the alternate screen: %q %v", h, err)
	}
	client, err := libghostty.NewTerminal(libghostty.WithSize(10, 3), libghostty.WithMaxScrollbackBytes(1<<20))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	client.VTWrite(append([]byte("\x1b[2J\x1b[H"), h...))
	if rows := must(client.ScrollbackRows()); rows != 2 {
		t.Fatalf("client scrollback rows %d", rows)
	}
	if screen, err := formatScreen(client, libghostty.FormatterFormatPlain, false, 0); err != nil || strings.TrimSpace(string(screen)) != "" {
		t.Fatalf("screen not blank: %q %v", screen, err)
	}
}

// readFrame reads responses until a frame.
func (c *client) readFrame() *protocol.Frame {
	c.t.Helper()
	for {
		resp := c.read()
		if resp.AttachFrame != nil {
			return resp.AttachFrame
		}
		if resp.Error != nil || resp.SessionEnded != nil || resp.EndOfStream != nil {
			c.t.Fatalf("no frame: %+v", resp)
		}
	}
}

// expectNoFrame checks nothing arrives for a while.
func (c *client) expectNoFrame(d time.Duration) {
	c.t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(d))
	defer c.conn.SetReadDeadline(time.Time{})
	if resp, err := protocol.ReadResponse(c.r); err == nil {
		c.t.Fatalf("unexpected response %+v", resp)
	}
}

func (c *client) ack(seq uint32, field bool) {
	c.t.Helper()
	c.send(&protocol.Request{AttachFrameAck: &protocol.FrameAck{Seq: seq, Field: field}})
}

func frameText(f *protocol.Frame) string {
	var b strings.Builder
	for _, r := range f.Rows {
		b.Write(r.VT)
		b.WriteByte('\n')
	}
	return b.String()
}

// Through the worker: a frame client gets the scrollback in Attached and
// frames it acknowledges; the next waits for the acknowledgement and a
// change; a snapshot request and a resize are full frames; AttachField puts
// the field in the next frame; the alternate screen is a full frame; the
// session's end sends the last screen.
func TestFrameClientThroughTheWorker(t *testing.T) {
	const script = `stty -echo; read go; printf 'hello\r\n'; read x; printf 'again\r\n'; read y; printf '\033[?1049h\033[Htop'; read z; printf '\033[?1049l'; read w; printf 'bye'; exit 0`
	h := startWorkerWith(t, script)
	c, attached := h.attachWith(protocol.AttachSession{Features: []string{protocol.CapAttachFrames, protocol.CapAttachField, protocol.CapAttachSizeOwner}})
	if !attached.HasFeature(protocol.CapAttachFrames) {
		t.Fatalf("frames not negotiated: %v", attached.Features)
	}
	f := c.readFrame()
	if !f.Full || f.Seq != 1 || int(f.Lines) != len(f.Rows) {
		t.Fatalf("first frame: full %v seq %d rows %d/%d", f.Full, f.Seq, len(f.Rows), f.Lines)
	}
	c.ack(f.Seq, false)
	c.input("go\n")
	if f = c.readFrame(); f.Full || f.Seq != 2 || !strings.Contains(frameText(f), "hello") {
		t.Fatalf("hello frame: full %v seq %d %q", f.Full, f.Seq, frameText(f))
	}
	// Two frames may be unacknowledged; the next output makes the second.
	c.input("x\n")
	if f = c.readFrame(); f.Seq != 3 || !strings.Contains(frameText(f), "again") {
		t.Fatalf("again frame: seq %d %q", f.Seq, frameText(f))
	}
	c.ack(f.Seq, false)
	// A snapshot request is a full frame.
	c.send(&protocol.Request{AttachSnapshot: protocol.Empty})
	if f = c.readFrame(); !f.Full || f.Seq != 4 {
		t.Fatalf("snapshot frame: full %v seq %d", f.Full, f.Seq)
	}
	c.ack(f.Seq, false)
	// The field, when asked for.
	c.send(&protocol.Request{AttachField: protocol.Empty})
	if f = c.readFrame(); f.Field == nil {
		t.Fatalf("no field in frame %d", f.Seq)
	}
	c.ack(f.Seq, false)
	// A resize is a full frame of the new size.
	c.send(&protocol.Request{AttachResize: &protocol.Geometry{Cols: 100, Rows: 30}})
	if f = c.readFrame(); !f.Full || f.Cols != 100 || f.Lines != 30 {
		t.Fatalf("resize frame: full %v %dx%d", f.Full, f.Cols, f.Lines)
	}
	c.ack(f.Seq, false)
	// The alternate screen and the return from it are full frames.
	c.input("y\n")
	if f = c.readFrame(); !f.Full || !f.Alternate || !strings.Contains(frameText(f), "top") {
		t.Fatalf("alternate screen: full %v alternate %v %q", f.Full, f.Alternate, frameText(f))
	}
	c.ack(f.Seq, false)
	c.input("z\n")
	if f = c.readFrame(); !f.Full || f.Alternate || !strings.Contains(frameText(f), "again") {
		t.Fatalf("back on the primary screen: full %v alternate %v %q", f.Full, f.Alternate, frameText(f))
	}
	c.ack(f.Seq, false)
	// The session ends: the last screen, then the end.
	c.input("w\n")
	sawBye := false
	for {
		resp := c.read()
		if resp.AttachFrame != nil && strings.Contains(frameText(resp.AttachFrame), "bye") {
			sawBye = true
			continue
		}
		if resp.SessionEnded != nil {
			break
		}
		if resp.Error != nil || resp.EndOfStream != nil {
			t.Fatalf("unexpected end: %+v", resp)
		}
	}
	if !sawBye {
		t.Fatal("the last screen was not sent before the end")
	}
	h.waitExit()
}

// A frame client that stops acknowledging gets no more frames while the
// session floods, and nothing is queued for it: when it acknowledges, one
// frame with the screen as it is now follows. Another frame client keeps
// getting frames meanwhile, at its own pace.
func TestFrameClientsDrainAtTheirOwnPace(t *testing.T) {
	const script = `stty -echo; read go; head -c 3000000 /dev/zero | tr '\0' x | fold -w 100; printf 'FLOODED\r\n'; read l; printf 'LATER\r\n'; read m; exit 0`
	h := startWorkerWith(t, script)
	slow, _ := h.attachWith(protocol.AttachSession{Features: []string{protocol.CapAttachFrames}})
	fast, _ := h.attachWith(protocol.AttachSession{Features: []string{protocol.CapAttachFrames}})
	f := slow.readFrame()
	slow.ack(f.Seq, false)
	g := fast.readFrame()
	fast.ack(g.Seq, false)
	// The flood: the slow client acknowledges nothing after its first
	// frame, so it gets the window's worth and no more; the fast one
	// acknowledges everything and sees the end.
	fast.input("go\n")
	f = slow.readFrame()
	f = slow.readFrame()
	frames := 0
	for {
		g = fast.readFrame()
		frames++
		fast.ack(g.Seq, false)
		if strings.Contains(frameText(g), "FLOODED") {
			break
		}
		if frames > 100000 {
			t.Fatal("the flood never ended")
		}
	}
	t.Logf("the fast client took %d frames for 3 MB of output", frames)
	slow.expectNoFrame(300 * time.Millisecond)
	// One frame, the current screen, when the slow client catches up.
	slow.ack(f.Seq, false)
	if f = slow.readFrame(); f.Seq != 4 || !strings.Contains(frameText(f), "FLOODED") {
		t.Fatalf("after the acknowledgement: seq %d %q", f.Seq, frameText(f))
	}
	slow.ack(f.Seq, false)
	slow.expectNoFrame(200 * time.Millisecond)
	// Both go on.
	fast.input("l\n")
	if f = slow.readFrame(); !strings.Contains(frameText(f), "LATER") {
		t.Fatalf("slow client after the flood: %q", frameText(f))
	}
	if g = fast.readFrame(); !strings.Contains(frameText(g), "LATER") {
		t.Fatalf("fast client after the flood: %q", frameText(g))
	}
	slow.ack(f.Seq, false)
	fast.ack(g.Seq, false)
	fast.input("m\n")
	fast.expectEnd()
	slow.expectEnd()
	h.waitExit()
}
