package worker

import (
	"bytes"
	"fmt"
	"sync/atomic"
	"time"

	"agtd.sh/internal/protocol"
	"go.mitchellh.com/libghostty"
)

// Screen frames (CapAttachFrames).
//
// A frame client gets the screen as rows instead of the raw output. Its
// attachment keeps a frameState: the rows it was last sent, and the modes,
// keyboard state, colours and cursor shape, so a frame carries only what
// changed. At most frameWindow frames are in flight: the handler builds the
// next when the screen changed since the last (dirty) and the client has
// acknowledged all but the newest, at least frameFloor apart, so a slow
// link gets fewer, fresher frames rather than a queue of stale output, and
// nothing is ever dropped. A frame is built on the owner goroutine at a
// sequence boundary (doAtGround), so it never shows half a sequence.
//
// A row is the VT the formatter writes for it: self-contained, from a
// default pen, so the client paints it alone by absolute position. One
// formatter run over the screen, split at the CRLFs the formatter puts
// between rows, costs 19 µs for 200x50 (BENCHMARKS.md).

const (
	// frameFloor is the least time between two frames to one client.
	frameFloor = 4 * time.Millisecond
	// frameWindow is how many frames may be unacknowledged at once. One
	// would make a change that lands just after a frame went wait a whole
	// round trip (a shell echoes a typed word in several writes); two lets
	// it follow at once, and a client still falls behind by at most two.
	frameWindow = 2
	// frameMaxHistory bounds the scrolled rows a frame carries the final
	// contents of; a burst that scrolled more between two frames leaves
	// the client's scrollback with the rest as they were last seen (or
	// blank, beyond a screen).
	frameMaxHistory = 1000
	// frameFieldAfterInput is how long after a client's input its frames
	// carry the input field even while output is still flowing, so keys
	// typed during a streaming answer are confirmed.
	frameFieldAfterInput = 500 * time.Millisecond
)

// frameState is what one frame client has been sent.
type frameState struct {
	seq         uint32
	cols, lines uint16
	alternate   bool
	// rows is the VT last sent for each row; nil is blank.
	rows                     [][]byte
	prefix, keyboard, colors []byte
	shape                    uint8
	// history is the scrollback row count at the last frame, which says
	// how many rows scrolled off since.
	history uint
	// full forces the next frame to carry every row (a snapshot request).
	full bool
	// effects are bells and notifications since the last frame.
	effects []byte
	// askField makes the next frame carry the field (AttachField).
	askField bool
	// wantField is the client's standing request for the field
	// (FrameAck.Field), set by the handler.
	wantField atomic.Bool
	// dirty is whether the screen changed since the last frame; set on
	// the owner goroutine, read by the handler, cleared by a build.
	dirty atomic.Bool
	// wake (one slot, never blocks) tells the handler a frame may be due.
	wake chan struct{}
	// rs reads the cursor and its shape without copying the screen twice.
	rs     *libghostty.RenderState
	closed bool
}

func newFrameState() (*frameState, error) {
	rs, err := libghostty.NewRenderState()
	if err != nil {
		return nil, err
	}
	return &frameState{rs: rs, wake: make(chan struct{}, 1)}, nil
}

// close releases the render state; a build after it fails.
func (st *frameState) close() {
	if !st.closed {
		st.closed = true
		st.rs.Close()
	}
}

// changed marks the screen changed and wakes the handler.
func (st *frameState) changed() {
	st.dirty.Store(true)
	select {
	case st.wake <- struct{}{}:
	default:
	}
}

// noteFrameOutput marks every frame client's screen changed.
func (s *ownerState) noteFrameOutput() {
	for _, a := range s.attachments {
		if a.frames != nil {
			a.frames.changed()
		}
	}
}

// frameEffects queues the bells and notifications of a feed for frame
// clients to relay, as the raw output would have carried them.
func (s *ownerState) frameEffects(fx terminalEffects) {
	if fx.bells == 0 && len(fx.notifications) == 0 {
		return
	}
	var b []byte
	for range fx.bells {
		b = append(b, '\a')
	}
	for _, n := range fx.notifications {
		b = fmt.Appendf(b, "\x1b]777;notify;%s;%s\x1b\\", n.Title, n.Body)
	}
	for _, a := range s.attachments {
		if a.frames != nil {
			a.frames.effects = append(a.frames.effects, b...)
		}
	}
}

// buildFrame builds the next frame for a: every row when something made
// the last ones useless (the first frame, a resize, a switch of screens, a
// snapshot request), otherwise the rows that changed, after the rows the
// primary screen scrolled into history. Run on the owner goroutine at a
// sequence boundary.
func (s *ownerState) buildFrame(a *ownerAttachment, now time.Time) (*protocol.Frame, error) {
	st := a.frames
	if st == nil || st.closed {
		return nil, fmt.Errorf("the attachment has ended")
	}
	term := s.terminal.term
	cols, err := term.Cols()
	if err != nil {
		return nil, err
	}
	lines, err := term.Rows()
	if err != nil {
		return nil, err
	}
	screen, err := term.ActiveScreen()
	if err != nil {
		return nil, err
	}
	alternate := screen == libghostty.ScreenAlternate
	full := st.full || st.seq == 0 || cols != st.cols || lines != st.lines || alternate != st.alternate
	rows, err := frameRows(term, cols, lines)
	if err != nil {
		return nil, err
	}
	// The scrollback is the primary screen's: on the alternate screen it
	// reads as empty and nothing scrolls into it, so its count is kept
	// from the last frame on the primary screen.
	history := st.history
	if !alternate {
		if history, err = term.ScrollbackRows(); err != nil {
			return nil, err
		}
	}
	scrolled := 0
	if !full && !alternate {
		scrolled = scrolledRows(st.rows, rows, history, st.history)
		if scrolled >= int(lines) {
			full, scrolled = true, 0
		}
	}
	var scrolledVT [][]byte
	// A full frame scrolls what grew since the last count: the attach
	// set it to what Attached.Snapshot covered, and a return from the
	// alternate screen finds it unchanged.
	if scrolled > 0 || (full && !alternate && !st.alternate && history > st.history) {
		// The rows that scrolled off since the last frame, as they are
		// now, at most a screen of the most recent.
		n := scrolled
		if full {
			// A full frame after more than a screen scrolled: the
			// client scrolls it all, with the contents of as many of
			// the rows as a frame carries.
			scrolled = min(int(history-st.history), 65535)
			n = min(scrolled, frameMaxHistory)
		}
		if n > 0 {
			if scrolledVT, err = historyRows(term, cols, history, n); err != nil {
				return nil, err
			}
		}
	}
	st.history = history
	switch {
	case full:
		st.rows = make([][]byte, lines)
	case scrolled > 0:
		copy(st.rows, st.rows[scrolled:])
		for i := int(lines) - scrolled; i < int(lines); i++ {
			st.rows[i] = nil
		}
	}
	f := &protocol.Frame{Seq: st.seq + 1, Full: full, Alternate: alternate, Cols: cols, Lines: lines, Scrolled: uint16(scrolled), History: scrolledVT}
	for y, row := range rows {
		if full || !bytes.Equal(row, st.rows[y]) {
			f.Rows = append(f.Rows, protocol.FrameRow{Index: uint16(y), VT: row})
			st.rows[y] = row
		}
	}
	prefix, keyboard, err := frameExtras(term)
	if err != nil {
		return nil, err
	}
	if full || !bytes.Equal(prefix, st.prefix) {
		f.Prefix, st.prefix = prefix, prefix
	}
	if full || !bytes.Equal(keyboard, st.keyboard) {
		f.Keyboard, st.keyboard = keyboard, keyboard
	}
	colors, err := changedColors(term)
	if err != nil {
		return nil, err
	}
	if full || !bytes.Equal(colors, st.colors) {
		f.Colors, st.colors = colors, colors
	}
	if err := st.rs.Update(term); err != nil {
		return nil, err
	}
	cur, err := st.rs.Cursor()
	if err != nil {
		return nil, err
	}
	f.CursorRow, f.CursorCol = cur.ViewportY, cur.ViewportX
	if !cur.ViewportHasValue {
		if f.CursorRow, err = term.CursorY(); err != nil {
			return nil, err
		}
		if f.CursorCol, err = term.CursorX(); err != nil {
			return nil, err
		}
	}
	if f.CursorShape, err = frameCursorShape(st.rs, term); err != nil {
		return nil, err
	}
	st.shape = f.CursorShape
	f.Effects, st.effects = st.effects, nil
	// The field costs a scan of the screen: carried when the client asked
	// for it outright, or stands to see it and the program has settled,
	// or just typed (its keys are what the client waits to see confirmed).
	if st.askField || (st.wantField.Load() && (now.Sub(s.lastOutputAt) >= fieldSettle || now.Sub(a.lastUsed) <= frameFieldAfterInput)) {
		if f.Field, err = s.field(); err != nil {
			return nil, err
		}
		st.askField = false
	}
	st.seq, st.cols, st.lines, st.alternate, st.full = f.Seq, cols, lines, alternate, false
	st.dirty.Store(false)
	return f, nil
}

// frameRows formats the visible screen's rows, one formatter run split at
// the CRLFs between rows, padded with blank (nil) rows to lines.
func frameRows(term *libghostty.Terminal, cols, lines uint16) ([][]byte, error) {
	sel, err := screenSelection(term, cols, lines)
	if err != nil {
		return nil, err
	}
	out, err := formatRowsOnly(term, sel, nil)
	if err != nil {
		return nil, err
	}
	rows := make([][]byte, lines)
	for i, part := range bytes.Split(out, crlf) {
		if i >= int(lines) {
			break
		}
		if len(part) > 0 {
			rows[i] = part
		}
	}
	return rows, nil
}

// historyRows formats the last n rows of scrollback (history is the count).
func historyRows(term *libghostty.Terminal, cols uint16, history uint, n int) ([][]byte, error) {
	first, err := term.GridRef(libghostty.Point{Tag: libghostty.PointTagHistory, Y: uint32(int(history) - n)})
	if err != nil {
		return nil, err
	}
	last, err := term.GridRef(libghostty.Point{Tag: libghostty.PointTagHistory, X: cols - 1, Y: uint32(history - 1)})
	if err != nil {
		return nil, err
	}
	out, err := formatRowsOnly(term, &libghostty.Selection{Start: *first, End: *last}, nil)
	if err != nil {
		return nil, err
	}
	rows := make([][]byte, n)
	for i, part := range bytes.Split(out, crlf) {
		if i >= n {
			break
		}
		if len(part) > 0 {
			rows[i] = part
		}
	}
	return rows, nil
}

// screenSelection selects the whole visible screen.
func screenSelection(term *libghostty.Terminal, cols, lines uint16) (*libghostty.Selection, error) {
	first, err := term.GridRef(libghostty.Point{Tag: libghostty.PointTagActive})
	if err != nil {
		return nil, err
	}
	last, err := term.GridRef(libghostty.Point{Tag: libghostty.PointTagActive, X: cols - 1, Y: uint32(lines) - 1})
	if err != nil {
		return nil, err
	}
	return &libghostty.Selection{Start: *first, End: *last}, nil
}

// formatRowsOnly formats sel as VT with no state before or after the rows
// beyond extras.
func formatRowsOnly(term *libghostty.Terminal, sel *libghostty.Selection, extras []libghostty.FormatterOption) ([]byte, error) {
	opts := append([]libghostty.FormatterOption{
		libghostty.WithFormatterSelection(sel),
		libghostty.WithFormatterFormat(libghostty.FormatterFormatVT),
		libghostty.WithFormatterTrim(false),
		libghostty.WithFormatterUnwrap(false),
	}, extras...)
	f, err := libghostty.NewFormatter(term, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create formatter: %w", err)
	}
	defer f.Close()
	return f.Format()
}

// frameExtras reads the state a client must mirror: the modes and tab
// stops (prefix, as the formatter writes them before a screen, ending with
// CUP home) and the keyboard state (modifyOtherKeys, kitty keyboard flags,
// as it writes them after one). Each comes from formatting one cell with
// the extras, less that cell's own bytes.
func frameExtras(term *libghostty.Terminal) (prefix, keyboard []byte, err error) {
	cell, err := term.GridRef(libghostty.Point{Tag: libghostty.PointTagActive})
	if err != nil {
		return nil, nil, err
	}
	sel := &libghostty.Selection{Start: *cell, End: *cell}
	base, err := formatRowsOnly(term, sel, nil)
	if err != nil {
		return nil, nil, err
	}
	modes, err := formatRowsOnly(term, sel, []libghostty.FormatterOption{libghostty.WithFormatterExtraModes(true), libghostty.WithFormatterExtraTabstops(true)})
	if err != nil {
		return nil, nil, err
	}
	kb, err := formatRowsOnly(term, sel, []libghostty.FormatterOption{libghostty.WithFormatterExtraKeyboard(true), libghostty.WithFormatterExtraKittyKeyboard(true)})
	if err != nil {
		return nil, nil, err
	}
	if !bytes.HasSuffix(modes, base) || !bytes.HasPrefix(kb, base) {
		return nil, nil, fmt.Errorf("the formatter's extras do not surround the cell")
	}
	prefix = bytes.Clone(modes[:len(modes)-len(base)])
	keyboard = bytes.Clone(kb[len(base):])
	return prefix, keyboard, nil
}

// frameCursorShape is the DECSCUSR shape to mirror: bar or underline, with
// their blinking variants, or 0 for a block, which is also what a terminal
// shows when no program asked for a shape (the client keeps its default).
func frameCursorShape(rs *libghostty.RenderState, term *libghostty.Terminal) (uint8, error) {
	shape, err := rs.CursorVisualStyle()
	if err != nil {
		return 0, err
	}
	var n uint8
	switch shape {
	case libghostty.CursorVisualStyleBar:
		n = 6
	case libghostty.CursorVisualStyleUnderline:
		n = 4
	default:
		return 0, nil
	}
	if blinking, _ := term.Mode(libghostty.ModeCursorBlinking); blinking {
		n--
	}
	return n, nil
}

// scrolledRows is how many rows the primary screen scrolled into history
// since the last frame: the growth of the scrollback, exact while it is
// growing. At the scrollback's cap the count stops growing (and may fall,
// as pages are dropped), so then the rows are compared shifted: the
// smallest shift under which every row still on screen matches its old
// place, with something on at least one of them, is the scroll. A screen
// that did not change at all did not scroll.
func scrolledRows(old, rows [][]byte, history, previous uint) int {
	lines := len(rows)
	if history > previous {
		return min(int(history-previous), lines)
	}
	if len(old) != lines || rowsEqual(old, rows) {
		return 0
	}
	for k := 1; k < lines; k++ {
		match, content := true, false
		for i := 0; i < lines-k; i++ {
			if !bytes.Equal(rows[i], old[i+k]) {
				match = false
				break
			}
			if len(rows[i]) > 0 {
				content = true
			}
		}
		if match && content {
			return k
		}
	}
	return 0
}

func rowsEqual(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

// historySnapshot is what a frame client writes into its cleared terminal
// before its first frame: up to scrollback rows of the primary screen's
// scrollback (allScrollback for all of it), each ended by CRLF, then the
// line feeds that push them all off the screen into the terminal's own
// scrollback, leaving the screen blank for the frame. Nil without any.
func (s *terminalState) historySnapshot(scrollback int) ([]byte, error) {
	term := s.term
	if scrollback == 0 {
		return nil, nil
	}
	screen, err := term.ActiveScreen()
	if err != nil {
		return nil, err
	}
	if screen == libghostty.ScreenAlternate {
		// The primary screen's scrollback is only reachable from the
		// primary screen: format a copy that has left the alternate one.
		data, err := term.Snapshot()
		if err != nil {
			return nil, err
		}
		dec, err := libghostty.NewSnapshotDecoderBytes(data)
		if err != nil {
			return nil, err
		}
		defer dec.Close()
		cp, err := dec.Decode()
		if err != nil {
			return nil, err
		}
		defer cp.Close()
		cp.VTWrite([]byte("\x1b[?1049l\x1b[?1047l\x1b[?47l"))
		term = cp
	}
	// Formatting reads the scrollback: compress again at the next quiet
	// tick (see format).
	s.compressed = false
	history, err := term.ScrollbackRows()
	if err != nil {
		return nil, err
	}
	if history == 0 {
		return nil, nil
	}
	n := history
	if scrollback > 0 && uint(scrollback) < history {
		n = uint(scrollback)
	}
	cols, err := term.Cols()
	if err != nil {
		return nil, err
	}
	rows, err := term.Rows()
	if err != nil {
		return nil, err
	}
	first, err := term.GridRef(libghostty.Point{Tag: libghostty.PointTagHistory, Y: uint32(history - n)})
	if err != nil {
		return nil, err
	}
	last, err := term.GridRef(libghostty.Point{Tag: libghostty.PointTagHistory, X: cols - 1, Y: uint32(history - 1)})
	if err != nil {
		return nil, err
	}
	out, err := formatRowsOnly(term, &libghostty.Selection{Start: *first, End: *last}, nil)
	if err != nil {
		return nil, err
	}
	return append(out, bytes.Repeat(crlf, int(rows))...), nil
}
