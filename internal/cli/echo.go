package cli

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"github.com/rivo/uniseg"

	"agtd.sh/internal/protocol"
)

// Local echo.
//
// Over a slow link every keystroke costs a round trip before the agent's
// echo of it comes back. With local echo, a key is drawn at once in the
// agent's own input field, exactly where the agent will draw it, and sent to
// the agent as typed (batched for at most flushAfter). The drawing is a
// prediction of what the agent will show once the keys reach it, and the
// agent stays the authority: the worker reports what the field looks like
// (protocol.Field, found on its libghostty screen; see ARCHITECTURE.md,
// "Local echo"), and the prediction is checked against it.
//
// Only keys every editor handles the same way are predicted: printable
// text, Backspace, Delete, Left, Right and the Shift-Enter and Alt-Enter
// newlines (Ctrl-J and Ctrl-Enter differ between agents: Ctrl-Enter submits
// in Claude Code and inserts a newline in Codex). Any other key (Enter, Tab,
// Esc, Ctrl-C, Up, Down, ...) is sent at once and not predicted; the echo
// then pauses until the field is seen again. While keys
// are unconfirmed the attachment's output is held (dropped), since the
// agent's own redraws would fight the drawing; once the field confirms every
// key, the screen is repainted from a snapshot inside synchronized output,
// so the agent's own rendering, identical in the field, appears in one step.
//
// The echo keeps the states it predicted, oldest first, each with the key
// that produced it. An observed field that equals one of them confirms the
// keys up to it; the rest are replayed over the observed field, since the
// agent may have placed the cursor or wrapped a word differently. A field
// that equals none means the agent did something unpredicted (a popup took
// a key, a different wrap), so the observed field is adopted and the
// predictions dropped: one visible correction, as mosh makes.

const (
	// echoFlushAfter is the default for [composer] flush_after: how long
	// predicted keys wait for company before they go to the agent.
	echoFlushAfter = 50 * time.Millisecond
	// echoProbeAfter is how long after the last flush (or the last
	// output, when idle) the field is asked for.
	echoProbeAfter = 150 * time.Millisecond
	// echoMaxStates bounds the unconfirmed predictions; past it the echo
	// drops them and lets keys through raw, so an agent that stopped
	// reading never pins a stale drawing.
	echoMaxStates = 512

	syncBegin = "\x1b[?2026h"
	syncEnd   = "\x1b[?2026l"
)

// gcell is one grapheme cluster and the cells it takes.
type gcell struct {
	s string
	w int
}

func graphemes(s string) []gcell {
	var out []gcell
	g := uniseg.NewGraphemes(s)
	for g.Next() {
		w := g.Width()
		if w < 1 {
			// A control character or a combining mark on its own; it
			// takes a cell when the agent draws it.
			w = 1
		}
		out = append(out, gcell{s: g.Str(), w: w})
	}
	return out
}

func cellsWidth(cells []gcell) int {
	w := 0
	for _, c := range cells {
		w += c.w
	}
	return w
}

func cellsString(cells []gcell) string {
	var b strings.Builder
	for _, c := range cells {
		b.WriteString(c.s)
	}
	return b.String()
}

// echoText is the predicted contents of the field: logical lines (split at
// newlines the user inserted) and the cursor, as a line and a grapheme index.
type echoText struct {
	lines     [][]gcell
	line, col int
}

func (t *echoText) clone() echoText {
	c := echoText{line: t.line, col: t.col, lines: make([][]gcell, len(t.lines))}
	for i, l := range t.lines {
		c.lines[i] = append([]gcell(nil), l...)
	}
	return c
}

func (t *echoText) insert(s string) {
	if len(t.lines) == 0 {
		t.lines = [][]gcell{nil}
	}
	parts := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	for i, part := range parts {
		if i > 0 {
			t.newline()
		}
		cells := graphemes(part)
		line := t.lines[t.line]
		line = append(line[:t.col], append(cells, line[t.col:]...)...)
		t.lines[t.line] = line
		t.col += len(cells)
	}
}

func (t *echoText) newline() {
	if len(t.lines) == 0 {
		t.lines = [][]gcell{nil}
	}
	line := t.lines[t.line]
	rest := append([]gcell(nil), line[t.col:]...)
	t.lines[t.line] = line[:t.col]
	t.lines = append(t.lines[:t.line+1], append([][]gcell{rest}, t.lines[t.line+1:]...)...)
	t.line++
	t.col = 0
}

func (t *echoText) backspace() {
	switch {
	case len(t.lines) == 0:
	case t.col > 0:
		line := t.lines[t.line]
		t.lines[t.line] = append(line[:t.col-1], line[t.col:]...)
		t.col--
	case t.line > 0:
		prev := t.lines[t.line-1]
		t.col = len(prev)
		t.lines[t.line-1] = append(prev, t.lines[t.line]...)
		t.lines = append(t.lines[:t.line], t.lines[t.line+1:]...)
		t.line--
	}
}

func (t *echoText) delete() {
	switch {
	case len(t.lines) == 0:
	case t.col < len(t.lines[t.line]):
		line := t.lines[t.line]
		t.lines[t.line] = append(line[:t.col], line[t.col+1:]...)
	case t.line+1 < len(t.lines):
		t.lines[t.line] = append(t.lines[t.line], t.lines[t.line+1]...)
		t.lines = append(t.lines[:t.line+1], t.lines[t.line+2:]...)
	}
}

func (t *echoText) left() {
	switch {
	case t.col > 0:
		t.col--
	case t.line > 0:
		t.line--
		t.col = len(t.lines[t.line])
	}
}

func (t *echoText) right() {
	switch {
	case len(t.lines) == 0:
	case t.col < len(t.lines[t.line]):
		t.col++
	case t.line+1 < len(t.lines):
		t.line++
		t.col = 0
	}
}

// echoRow is one display row of the field: its cells, which logical line
// and grapheme it starts at, and whether a space was dropped at its end.
type echoRow struct {
	cells   []gcell
	line    int
	start   int
	dropped bool
}

func (r echoRow) text() string { return rowText(r.cells, 0) }

// rowText is a row's text with trailing spaces trimmed, keeping at least
// keep cells of it: on the cursor's row, spaces typed before the cursor
// are text, as the worker reports them (field.go).
func rowText(cells []gcell, keep int) string {
	end, w := len(cells), cellsWidth(cells)
	for end > 0 && cells[end-1].s == " " && w-1 >= keep {
		end--
		w--
	}
	return cellsString(cells[:end])
}

// echoLayout is the field as the agent would draw it: display rows and the
// cursor's row and column (cells from the text column).
type echoLayout struct {
	rows       []echoRow
	cRow, cCol int
}

// wrapLine breaks one logical line into rows of at most width cells the way
// the agents do (docs/designs/smooth-remote-attach.md): greedily, breaking
// after the last space that fits, a word longer than a row broken where the
// row ends. A space that falls on a break is dropped (Claude Code, Codex)
// or kept, wrapping like any character (pi, keepSpace).
func wrapLine(line []gcell, width int, keepSpace bool) []echoRow {
	if width < 1 {
		width = 1
	}
	var rows []echoRow
	row := echoRow{start: 0}
	w := 0
	lastSpace := -1 // index in row.cells of the last space
	flush := func(next int, dropped bool) {
		row.dropped = dropped
		rows = append(rows, row)
		row = echoRow{start: next}
		w = 0
		lastSpace = -1
	}
	for i := 0; i < len(line); i++ {
		c := line[i]
		if w+c.w <= width {
			row.cells = append(row.cells, c)
			w += c.w
			if c.s == " " {
				lastSpace = len(row.cells) - 1
			}
			continue
		}
		// c does not fit.
		switch {
		case c.s == " " && !keepSpace:
			// Dropped at the break; the next word starts a row.
			flush(i+1, true)
		case lastSpace >= 0 && c.s != " ":
			// Break after the last space: it stays at the end of this
			// row (trimmed from its text, kept for the cursor) when the
			// agent keeps spaces, and is dropped otherwise.
			carry := append([]gcell(nil), row.cells[lastSpace+1:]...)
			next := row.start + lastSpace + 1
			if keepSpace {
				row.cells = row.cells[:lastSpace+1]
				flush(next, false)
			} else {
				row.cells = row.cells[:lastSpace]
				flush(next, true)
			}
			row.cells = carry
			w = cellsWidth(carry)
			i--
		default:
			flush(i, false)
			i--
		}
	}
	rows = append(rows, row)
	return rows
}

// layout lays the text out for a field.
func layout(t echoText, f *protocol.Field) echoLayout {
	var lay echoLayout
	lines := t.lines
	if len(lines) == 0 {
		lines = [][]gcell{nil}
	}
	for li, line := range lines {
		rows := wrapLine(line, int(f.Width), f.WrapKeepsSpace)
		for ri, r := range rows {
			r.line = li
			if li == t.line && t.col >= r.start {
				end := r.start + len(r.cells)
				last := ri == len(rows)-1
				switch {
				case t.col < end, t.col == end && (last || r.dropped):
					// On this row: before its end, at the end of the
					// last row, or before the space dropped at its end.
					lay.cRow = len(lay.rows)
					lay.cCol = cellsWidth(line[r.start:t.col])
				case t.col == end:
					// At the start of the next row.
					lay.cRow = len(lay.rows) + 1
					lay.cCol = 0
				}
			}
			lay.rows = append(lay.rows, r)
		}
	}
	return lay
}

// texts is the rows as the worker would report them: trailing spaces
// trimmed, except those before the cursor on its row.
func (l echoLayout) texts() []string {
	out := make([]string, len(l.rows))
	for i, r := range l.rows {
		keep := 0
		if i == l.cRow {
			keep = l.cCol
		}
		out[i] = rowText(r.cells, keep)
	}
	return out
}

// textFromField is the text the field shows, each display row taken as a
// line of its own (a wrap and a newline look the same, and laying rows out
// one by one draws them the same), with the cursor where the field says.
func textFromField(f *protocol.Field) echoText {
	t := echoText{}
	for _, row := range f.Rows {
		t.lines = append(t.lines, graphemes(row))
	}
	if len(t.lines) == 0 {
		t.lines = [][]gcell{nil}
	}
	t.line = min(max(int(f.Row)-int(f.Top), 0), len(t.lines)-1)
	col := int(f.Col) - int(f.TextCol)
	w := 0
	for i, c := range t.lines[t.line] {
		if w >= col {
			t.col = i
			return t
		}
		w += c.w
	}
	t.col = len(t.lines[t.line])
	return t
}

// echoState is a prediction: the field after key, as rows and a cursor.
type echoState struct {
	rows       []string
	cRow, cCol int
	key        keyEvent
}

type echoConfig struct {
	flushAfter time.Duration
}

// echo is the local echo of one attachment. Everything runs on the attach
// loop's goroutine.
type echo struct {
	cfg echoConfig
	now func() time.Time

	keys keyDecoder
	// field is the latest field the worker reported, nil until one is
	// found. text is the predicted contents; states the predictions not
	// yet confirmed, oldest first.
	field  *protocol.Field
	text   echoText
	states []echoState
	// paused: an unpredicted key went to the agent, so nothing is drawn
	// until the field is seen again.
	paused bool

	// pending is the batch of keys not yet sent; flushDue when it goes.
	pending  []byte
	flushDue time.Time
	// probeDue is when the field is asked for next; probing while an ask
	// is outstanding. quietDue is the ask after output goes quiet.
	probeDue, quietDue time.Time
	probing            bool
	// snapshotDue: an AttachSnapshot was asked for along with the field,
	// to repaint once the field confirms the keys.
	snapshotDue bool
	// dropped is the output held since the last repaint, and dirty that
	// the screen may differ from the agent's for another reason (a
	// prediction the field did not confirm): either calls for a repaint.
	dropped int
	dirty   bool
	// painted: the drawing is on screen; offset is how many rows it
	// scrolled the screen by (a box growing past the bottom).
	painted bool
	offset  int
	// resend: after a reattach, the keys the next field proves missing
	// are sent again.
	resend bool
	timer  *time.Timer
}

func newEcho(cfg echoConfig) *echo {
	if cfg.flushAfter <= 0 {
		cfg.flushAfter = echoFlushAfter
	}
	e := &echo{cfg: cfg, now: time.Now, timer: time.NewTimer(time.Hour)}
	e.timer.Stop()
	// The field is asked for once the attach's output has been quiet for
	// a moment, whether or not any follows the snapshot: the prompt may
	// already be in it, with nothing more to come until a key is typed.
	e.quietDue = e.now().Add(echoProbeAfter)
	e.schedule()
	return e
}

// timerC fires when tick is due; nil for no echo.
func (e *echo) timerC() <-chan time.Time {
	if e == nil {
		return nil
	}
	return e.timer.C
}

func (e *echo) active() bool { return e.field != nil && e.field.Found && !e.paused }

// holding reports that output must be dropped: keys are drawn that the
// agent has not been seen to draw yet.
func (e *echo) holding() bool { return e != nil && len(e.states) > 0 }

// output notes n bytes of output: dropped while holding, otherwise they
// may have moved the field, so it is asked for again once output pauses.
func (e *echo) output(n int) {
	if e.holding() {
		e.dropped += n
		return
	}
	e.quietDue = e.now().Add(echoProbeAfter)
	e.schedule()
}

// input takes typed bytes (hotkeys already removed). It returns what to
// send now and what to draw.
func (e *echo) input(data []byte) (send, paint []byte) {
	for _, ev := range e.keys.push(data) {
		if !e.active() || len(e.states) >= echoMaxStates {
			e.pending = append(e.pending, ev.raw...)
			send = append(send, e.flush()...)
			continue
		}
		if !e.predict(ev) {
			// Unpredicted: it goes now, with everything before it,
			// and the drawing stops meaning anything.
			e.pending = append(e.pending, ev.raw...)
			send = append(send, e.flush()...)
			e.pause()
			continue
		}
		lay := layout(e.text, e.field)
		e.states = append(e.states, echoState{rows: lay.texts(), cRow: lay.cRow, cCol: lay.cCol, key: ev})
		e.pending = append(e.pending, ev.raw...)
		if e.flushDue.IsZero() {
			e.flushDue = e.now().Add(e.cfg.flushAfter)
		}
		paint = e.paint(lay)
	}
	e.schedule()
	return send, paint
}

// predict applies a key to the text if it is one every editor handles the
// same way, and reports whether it was.
func (e *echo) predict(ev keyEvent) bool {
	switch {
	case ev.isPaste:
		e.text.insert(ev.paste)
	case ev.printable():
		e.text.insert(string(ev.r))
	case ev.code == keyEnter && (ev.mods == modShift || ev.mods == modAlt):
		// Shift-Enter reaches the echo only from a terminal with the
		// kitty keyboard protocol; elsewhere it is a plain Enter and
		// goes to the agent unpredicted, which submits.
		e.text.newline()
	case ev.code == keyBackspace && ev.mods == 0:
		e.text.backspace()
	case ev.code == keyDelete && ev.mods == 0:
		e.text.delete()
	case ev.code == keyLeft && ev.mods == 0:
		e.text.left()
	case ev.code == keyRight && ev.mods == 0:
		e.text.right()
	default:
		return false
	}
	return true
}

// pause stops drawing until the field is seen again: the agent's output
// flows, and its own rendering takes over from the drawing.
func (e *echo) pause() {
	e.paused = true
	e.states = nil
	if e.painted {
		e.dirty = true
	}
	e.probeDue = e.now().Add(echoProbeAfter)
}

// flush returns the pending keys, clearing them, and arranges for the
// field to be asked for after them.
func (e *echo) flush() []byte {
	out := e.pending
	e.pending = nil
	e.flushDue = time.Time{}
	if len(out) > 0 {
		e.probeDue = e.now().Add(echoProbeAfter)
	}
	return out
}

// tick runs what is due: a flush, a probe.
func (e *echo) tick(now time.Time) (send []byte, reqs []*protocol.Request) {
	if !e.flushDue.IsZero() && !now.Before(e.flushDue) {
		send = e.flush()
	}
	if !e.probing {
		due := !e.probeDue.IsZero() && !now.Before(e.probeDue)
		quiet := !e.quietDue.IsZero() && !now.Before(e.quietDue)
		if due || quiet {
			reqs = e.probe()
		}
	}
	e.schedule()
	return send, reqs
}

// probe asks for the field, and for a snapshot too when a repaint is owed
// once the field confirms the keys.
func (e *echo) probe() []*protocol.Request {
	e.probing = true
	e.probeDue, e.quietDue = time.Time{}, time.Time{}
	reqs := []*protocol.Request{{AttachField: protocol.Empty}}
	if e.dropped > 0 || e.dirty || e.painted {
		e.snapshotDue = true
		reqs = append(reqs, &protocol.Request{AttachSnapshot: protocol.Empty})
	}
	return reqs
}

// schedule arms the timer for the earliest due time.
func (e *echo) schedule() {
	var due time.Time
	for _, t := range []time.Time{e.flushDue, e.probeDue, e.quietDue} {
		if t.IsZero() || (e.probing && t != e.flushDue) {
			continue
		}
		if due.IsZero() || t.Before(due) {
			due = t
		}
	}
	if !e.timer.Stop() {
		select {
		case <-e.timer.C:
		default:
		}
	}
	if !due.IsZero() {
		e.timer.Reset(max(due.Sub(e.now()), 0))
	}
}

// fieldAnswer takes the worker's answer. It returns keys to send again (after
// a reattach, the ones the field proves missing) and what to draw.
func (e *echo) fieldAnswer(f *protocol.Field) (send, paint []byte) {
	e.probing = false
	e.paused = false
	e.offset = 0
	resend := e.resend
	e.resend = false
	if !f.Found {
		e.field = nil
		e.states = nil
		e.schedule()
		return nil, nil
	}
	// Which prediction has the agent reached? The states were replayed
	// from the previous field, so that field's rows are the state before
	// the oldest of them.
	matched := -1
	for i := len(e.states) - 1; i >= 0; i-- {
		if equalRows(e.states[i].rows, f.Rows) {
			matched = i
			break
		}
	}
	var remaining []echoState
	switch {
	case matched >= 0:
		remaining = e.states[matched+1:]
	case len(e.states) > 0 && e.field != nil && equalRows(e.field.Rows, f.Rows):
		remaining = e.states
	default:
		// Nothing predicted matches what the agent drew: take the
		// agent's word for it.
		if len(e.states) > 0 && e.painted {
			e.dirty = true
		}
		remaining = nil
	}
	// Replay what is still owed over what the agent shows, so a cursor
	// or a wrap the agent placed differently is predicted from there on.
	e.field = f
	e.text = textFromField(f)
	e.states = nil
	for _, st := range remaining {
		if !e.predict(st.key) {
			break
		}
		lay := layout(e.text, e.field)
		e.states = append(e.states, echoState{rows: lay.texts(), cRow: lay.cRow, cCol: lay.cCol, key: st.key})
		if resend {
			send = append(send, st.key.raw...)
		}
	}
	if len(e.states) > 0 {
		paint = e.paint(layout(e.text, e.field))
		e.probeDue = e.now().Add(echoProbeAfter)
	}
	e.schedule()
	return send, paint
}

func equalRows(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// snapshotAnswer takes a snapshot asked for by a probe: once nothing is
// owed to the drawing, it is the repaint that shows the agent's own screen.
func (e *echo) snapshotAnswer(data []byte) (paint []byte) {
	if !e.snapshotDue {
		return nil
	}
	e.snapshotDue = false
	if e.holding() {
		// Keys are still in flight; the next probe asks again.
		return nil
	}
	e.dropped, e.dirty, e.painted, e.offset = 0, false, false, 0
	return []byte(syncBegin + attachClearSequence + string(data) + syncEnd)
}

// suspend ends the echo for now (the overlay opens, the attachment ends):
// pending keys go, predictions are dropped.
func (e *echo) suspend() (send []byte) {
	send = e.flush()
	e.states = nil
	e.paused = false
	e.painted = false
	e.offset = 0
	e.schedule()
	return send
}

// resized: the field moved; ask again once the agent has redrawn.
func (e *echo) resized() {
	e.field = nil
	e.states = nil
	e.painted = false
	e.offset = 0
	e.quietDue = e.now().Add(echoProbeAfter)
	e.schedule()
}

// reattached: the screen was repainted from the new attachment's snapshot;
// the field is asked for at once, and the keys it proves missing are sent
// again.
func (e *echo) reattached() {
	e.resend = len(e.states) > 0
	e.painted, e.dropped, e.dirty, e.offset = false, 0, false, 0
	e.probeDue = e.now()
	e.probing = false
	e.schedule()
}

// unconfirmed is how many predicted keys the agent has not been seen to
// draw.
func (e *echo) unconfirmed() int {
	if e == nil {
		return 0
	}
	return len(e.states)
}

// Drawing.
//
// The drawing is composed from the field's chrome: the rows the worker sent
// as VT bytes are put back as they were, then the box is drawn over them
// at its predicted size, its rules moved the way the agent moves them, and
// the text in the agent's own pen. The whole update is one write inside
// synchronized output.

func cup(row, col int) string { return fmt.Sprintf("\x1b[%d;%dH", row+1, col+1) }

// paint draws the predicted field.
func (e *echo) paint(lay echoLayout) []byte {
	f := e.field
	lines := int(f.Lines)
	var b bytes.Buffer
	b.WriteString(syncBegin)
	// Put the chrome back as it was.
	chrome := map[int][]byte{}
	first, last := int(f.BoxTop), int(f.BoxBottom)
	for _, r := range f.Chrome {
		chrome[int(r.Index)] = r.VT
		first, last = min(first, int(r.Index)), max(last, int(r.Index))
	}
	n, m := len(lay.rows), len(f.Rows)
	growth := n - m
	top := int(f.Top)
	if f.GrowsUp {
		top -= growth
	}
	// Rows pushed past the bottom scroll the screen, as the agent's own
	// growth does on the primary screen.
	if !f.GrowsUp {
		if over := last + growth - e.offset - (lines - 1); over > 0 {
			b.WriteString(cup(lines-1, 0) + strings.Repeat("\n", over))
			e.offset += over
		}
	}
	at := func(row int) string { return cup(row-e.offset, 0) }
	for y := first; y <= last; y++ {
		if y-e.offset < 0 {
			continue
		}
		b.WriteString(at(y) + ansiReset + string(chrome[y]) + ansiReset + "\x1b[K")
	}
	// Move what the box pushes: the rows above it, when it grows up; the
	// rows below it, when it grows down.
	if growth != 0 {
		if f.GrowsUp {
			for y := int(f.BoxTop); y < int(f.Top); y++ {
				if y-growth-e.offset >= 0 {
					b.WriteString(at(y-growth) + ansiReset + string(chrome[y]) + ansiReset + "\x1b[K")
				}
			}
			for y := int(f.BoxTop); y < int(f.BoxTop)-growth && y <= last; y++ {
				// Shrinking uncovers rows the agent will fill; blank
				// until it does.
				b.WriteString(at(y) + ansiReset + "\x1b[2K")
			}
		} else {
			for y := last; y >= int(f.Top)+m; y-- {
				if y+growth <= lines-1+e.offset {
					b.WriteString(at(y+growth) + ansiReset + string(chrome[y]) + ansiReset + "\x1b[K")
				}
			}
			for y := last + growth + 1; y <= last; y++ {
				b.WriteString(at(y) + ansiReset + "\x1b[2K")
			}
		}
	}
	// The text rows.
	textCol, width := int(f.TextCol), int(f.Width)
	for i, r := range lay.rows {
		y := top + i
		if y-e.offset < 0 || y-e.offset >= lines {
			continue
		}
		b.WriteString(at(y) + ansiReset + "\x1b[2K")
		if i == 0 {
			b.WriteString(string(chrome[int(f.Top)]) + ansiReset)
		}
		b.WriteString(cup(y-e.offset, textCol) + string(f.Style))
		cells := r.cells
		used := 0
		if f.InverseCursor && i == lay.cRow {
			// The agent paints its own cursor: the cell under it, or a
			// space past the end, in inverse video.
			at, found := 0, false
			for j, c := range cells {
				if at == lay.cCol {
					b.WriteString(cellsString(cells[:j]) + "\x1b[7m" + c.s + "\x1b[27m" + cellsString(cells[j+1:]))
					used = cellsWidth(cells)
					found = true
					break
				}
				at += c.w
			}
			if !found {
				b.WriteString(cellsString(cells) + "\x1b[7m \x1b[27m")
				used = cellsWidth(cells) + 1
			}
		} else {
			b.WriteString(cellsString(cells))
			used = cellsWidth(cells)
		}
		if used < width {
			fmt.Fprintf(&b, "\x1b[%dX", width-used)
		}
		b.WriteString(ansiReset)
	}
	b.WriteString(cup(top+lay.cRow-e.offset, textCol+lay.cCol))
	b.WriteString(syncEnd)
	e.painted = true
	return b.Bytes()
}
