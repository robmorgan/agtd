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
// in Claude Code and inserts a newline in Codex). Under frames, Enter on a
// prompt is predicted as the submit, the field emptying at once (submits
// says when; a frame that shows otherwise, a newline or a menu's choice,
// is adopted like any other unpredicted drawing). Any other key (Tab, Esc,
// Ctrl-C, Up, Down, a mouse click, ...) is sent at once and not predicted;
// the echo then pauses until the field is seen again. Input that
// edits nothing (a wheel scroll, so a fullscreen agent scrolls its
// transcript, mouse motion, a focus change, a key release, the terminal's
// CSI reply to the agent's query) is passed on without pausing. While keys
// are unconfirmed the attachment's output is held (dropped), since the
// agent's own redraws would fight the drawing; once the field confirms every
// key, the screen is repainted from a snapshot inside synchronized output,
// so the agent's own rendering, identical in the field, appears in one step.
//
// The echo keeps the states it predicted, oldest first, each with the key
// that produced it. An observed field that equals one of them, text and
// cursor, confirms the keys up to it, provided the key had been sent when
// the field was asked for (the worker reads the field before any input
// sent after the ask, so a later state can only match by coincidence); the rest are replayed over the
// observed field, since the agent may have placed the cursor or wrapped a
// word differently. A field that equals none means the agent did something
// unpredicted (a popup took a key, a different wrap), so the observed field
// is adopted and the predictions dropped: one visible correction, as mosh
// makes. A field asked for before an unpredicted key or a resize describes
// a field that may be gone, so it is ignored and the field asked for again.
//
// Keys the echo does not know (function keys, mouse reports, the terminal's
// replies to the agent's queries) are sent as they came, unpredicted; key
// releases change nothing in the field and go with the keys around them.
//
// After a reattach, keys sent on the lost connection may not have reached
// the agent. The next field says which of them it drew; the rest are sent
// again, before the keys that were still batched and any typed meanwhile,
// which wait for that answer. A key that reached the agent but that it had
// not drawn by then (it waits fieldSettle for output to settle) is sent
// twice: the worker does not acknowledge input, so the field is the only
// evidence.

const (
	// echoProbeAfter is how long after the last flush (or the last
	// output, when idle) the field is asked for.
	echoProbeAfter = 150 * time.Millisecond
	// echoIncompleteAfter is how long an incomplete escape sequence or
	// paste waits for the rest before it goes as it is.
	echoIncompleteAfter = 50 * time.Millisecond
	// echoHeldMax bounds the keys held while a resend is owed; past it
	// the echo gives up the resend and sends them, as when the overlay
	// opens.
	echoHeldMax = pasteMax
	// echoMaxStates bounds the unconfirmed predictions; past it the echo
	// drops them and lets keys through raw, so an agent that stopped
	// reading never pins a stale drawing.
	echoMaxStates = 512
	// echoFocusWindow is how long after a repaint the terminal's answer
	// to it turning focus reporting back on is expected.
	echoFocusWindow = 500 * time.Millisecond

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

// clear empties the text, as a submit does.
func (t *echoText) clear() {
	t.lines = [][]gcell{nil}
	t.line, t.col = 0, 0
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
	// sent: the key has gone to the agent, not just into the batch.
	// asked: it had gone when the field was last asked for, so the field
	// can show it.
	sent, asked bool
	// seenFrom: under frames, the first ack a frame must have been built
	// after for its field to show the key (the one after the last ack
	// sent before it).
	seenFrom uint32
}

// at reports whether the field shows this state: its text and its cursor.
func (st echoState) at(f *protocol.Field) bool {
	return equalRows(st.rows, f.Rows) && st.cRow == int(f.Row)-int(f.Top) && st.cCol == int(f.Col)-int(f.TextCol)
}

// sameField reports whether two fields show the same text and cursor.
func sameField(a, b *protocol.Field) bool {
	return equalRows(a.Rows, b.Rows) && int(a.Row)-int(a.Top) == int(b.Row)-int(b.Top) && int(a.Col)-int(a.TextCol) == int(b.Col)-int(b.TextCol)
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
	// decodeDue is when what the decoder holds (an incomplete sequence,
	// a paste without its end yet) goes as it is.
	decodeDue time.Time
	// probeDue is when the field is asked for next; probing while an ask
	// is outstanding. quietDue is the ask after output goes quiet.
	probeDue, quietDue time.Time
	probing            bool
	// epoch counts the unpredicted keys and resizes, after which a field
	// asked for earlier is stale; probeEpoch is the epoch of the last ask.
	epoch, probeEpoch int
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
	// frames: the screen arrives as frames (attach-frames), and the
	// field inside them. Output is never held, since the frame's rows
	// show through once the keys are confirmed; while they are not, the
	// predicted rows are painted over each frame (overlay), and a probe
	// asks for the field only, never a snapshot.
	frames bool
	// ackSeq is the last frame acknowledged, and staleBefore the first
	// ack a frame must have been built after for its field not to predate
	// an unpredicted key or a resize: under frames the field is in every
	// frame, not an answer to a probe.
	ackSeq, staleBefore uint32
	// resend: after a reattach, the keys the next field proves missing
	// are sent again; until it answers, every key waits in pending
	// unpredicted (held), so that none overtakes them.
	resend bool
	held   bool
	// focus is the last focus report sent to the agent, and focusUntil
	// the end of the window after a repaint. Some terminals (Ghostty)
	// report the focus whenever focus reporting is turned on, and a
	// repaint of a focus-reporting agent's snapshot turns it on again
	// (twice for a fullscreen agent: the primary screen's modes, then its
	// own). A report that repeats the last one inside that window is
	// such an answer and stays here; a change, or a repeat at any other
	// time (a new program turning focus reporting on), goes.
	focus      string
	focusUntil time.Time
	timer      *time.Timer
	// rest is the screen under frames as it last was with the field
	// empty: its rows and its field. An agent may lay the screen out
	// differently while there is text (pi moves its box up over a menu
	// of slash commands), so the key that empties the field again is
	// drawn over this screen, with the box back where it was (restoring)
	// until the agent's field replaces it.
	rest      *echoRest
	restoring bool
}

// echoRest is the screen with the field empty.
type echoRest struct {
	field *protocol.Field
	rows  [][]byte
}

// wheelReport reports that raw is an SGR mouse report of the plain wheel
// (button 64 to 67), which the painter predicts (screen.Painter.Wheel).
func wheelReport(raw []byte) bool {
	return len(raw) > 5 && string(raw[:4]) == "\x1b[<6" && raw[4] >= '4' && raw[4] <= '7' && raw[5] == ';'
}

// fieldEmpty reports that a field shows no text.
func fieldEmpty(f *protocol.Field) bool {
	for _, r := range f.Rows {
		if r != "" {
			return false
		}
	}
	return true
}

// sameBox reports whether two fields have their box in the same place.
func sameBox(a, b *protocol.Field) bool {
	return a.Lines == b.Lines && a.Cols == b.Cols && a.Top == b.Top && a.BoxTop == b.BoxTop && a.BoxBottom == b.BoxBottom
}

// screenShown notes the screen a frame left (its rows), with its field:
// one with the field empty and nothing predicted is the rest screen,
// which any other must still show above the box, where it was or moved
// up with the box, else the agent has written since and it is dropped.
func (e *echo) screenShown(f *protocol.Field, rows [][]byte) {
	if e == nil || !e.frames || f == nil || !f.Found {
		return
	}
	if fieldEmpty(f) {
		if !e.holding() {
			e.rest = &echoRest{field: f, rows: rows}
		}
		return
	}
	if e.rest != nil && !e.rest.above(f, rows) {
		// The drawing stops painting the dropped screen; the frames
		// correct the box it left.
		e.rest, e.restoring = nil, false
	}
}

// above reports whether rows show the rest screen above f's box.
func (r *echoRest) above(f *protocol.Field, rows [][]byte) bool {
	if len(rows) != len(r.rows) {
		return false
	}
	n, shift := int(f.BoxTop), int(r.field.BoxTop)-int(f.BoxTop)
	same := func(d int) bool {
		for y := range n {
			if y+d < 0 || y+d >= len(r.rows) || !bytes.Equal(rows[y], r.rows[y+d]) {
				return false
			}
		}
		return true
	}
	return same(0) || (shift != 0 && same(shift))
}

func newEcho(cfg echoConfig) *echo {
	e := &echo{cfg: cfg, now: time.Now, timer: time.NewTimer(time.Hour)}
	e.keys.forwardAll = true
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
// send now and what to draw: the drawing after the last key, which also
// makes any scrolling the keys before it called for.
func (e *echo) input(data []byte) (send, paint []byte) {
	drawn := false
	for _, ev := range e.keys.push(data) {
		s, d := e.key(ev)
		send = append(send, s...)
		drawn = drawn || d
	}
	if drawn && len(e.states) > 0 {
		paint = e.paint(layout(e.text, e.field))
	}
	e.noteHeld()
	e.schedule()
	return send, paint
}

// key takes one key: it is held back (a repeated focus report), held for a
// resend, sent, or predicted and batched. It returns what to send now and whether the text changed.
func (e *echo) key(ev keyEvent) (send []byte, drawn bool) {
	if ev.focus {
		if r := string(ev.raw); r == e.focus && e.now().Before(e.focusUntil) {
			return nil, false
		}
		e.focus = string(ev.raw)
	}
	if e.resend {
		e.pending = append(e.pending, ev.raw...)
		e.held = true
		if len(e.pending) > echoHeldMax {
			// Too much to hold for an answer that is not coming soon:
			// the resend is given up, and everything goes in order.
			e.resend = false
			return e.resumeHeld(), false
		}
		return nil, false
	}
	if ev.code == keyPassive {
		// A wheel scroll, mouse motion, a focus change, a key
		// release or a query reply edits no text, so the drawing
		// stands and the field need not be asked for. It goes in
		// order behind the keys still batched, or at once; a wheel
		// tick takes the batch with it, since the painter draws its
		// guess now and times the agent's answer from now.
		if e.flushDue.IsZero() {
			return ev.raw, false
		}
		e.pending = append(e.pending, ev.raw...)
		if wheelReport(ev.raw) {
			return e.flush(), false
		}
		return nil, false
	}
	if !e.active() || len(e.states) >= echoMaxStates {
		e.pending = append(e.pending, ev.raw...)
		return e.flush(), false
	}
	if !e.predict(ev) {
		// Unpredicted: it goes now, with everything before it, and
		// the drawing stops meaning anything.
		e.pending = append(e.pending, ev.raw...)
		send = e.flush()
		e.pause()
		return send, false
	}
	if e.rest != nil && !e.restoring && len(e.text.lines) == 1 && len(e.text.lines[0]) == 0 && !sameBox(e.rest.field, e.field) {
		// Emptied: the screen goes back to how it was when it last was.
		e.field, e.restoring = e.rest.field, true
	}
	lay := layout(e.text, e.field)
	e.states = append(e.states, echoState{rows: lay.texts(), cRow: lay.cRow, cCol: lay.cCol, key: ev})
	send = e.queue(ev.raw)
	if ev.code == keyEnter && ev.mods == 0 {
		// A submit ends the batch: nothing is typed after it that
		// would share the flush.
		send = append(send, e.flush()...)
	}
	return send, true
}

// noteHeld arms the expiry of what the decoder holds: bytes that wait for
// the rest of a sequence or a paste go as they are if nothing follows them
// for echoIncompleteAfter.
func (e *echo) noteHeld() {
	if e.keys.held() {
		e.decodeDue = e.now().Add(echoIncompleteAfter)
	} else {
		e.decodeDue = time.Time{}
	}
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
		// goes to the agent as one.
		e.text.newline()
	case ev.code == keyEnter && ev.mods == 0 && !ev.lineFeed() && e.frames && e.submits():
		// The prompt goes to the agent and the field empties. Only
		// under frames: in raw mode the hold would drop the agent's
		// answer until the field was probed.
		e.text.clear()
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

// lineFeed reports a bare LF, which the decoder names Enter: Ctrl-J, or a
// Shift-Enter the terminal was set to send as one (Ghostty's
// `keybind = shift+enter=text:\n`). It is a newline in Claude Code, not a
// submit, and not the same in every agent, so it goes unpredicted.
func (ev keyEvent) lineFeed() bool { return len(ev.raw) == 1 && ev.raw[0] == '\n' }

// submits reports that Enter on the text as it stands would submit it in
// Claude Code, Codex and pi alike: there is text, it does not end with a
// backslash (a continuation), it is not a slash command (a menu may be
// open, and Enter picks from it) and the word at the cursor is not an @
// mention (a completion may be open). Anywhere else Enter goes
// unpredicted, as every unpredicted key does.
func (e *echo) submits() bool {
	t := &e.text
	if len(t.lines) == 0 || t.line >= len(t.lines) {
		return false
	}
	text := 0
	for _, line := range t.lines {
		text += len(line)
	}
	if text == 0 {
		return false
	}
	if first := t.lines[0]; len(first) > 0 && first[0].s == "/" {
		return false
	}
	if last := t.lines[len(t.lines)-1]; len(last) > 0 && last[len(last)-1].s == "\\" {
		return false
	}
	line := t.lines[t.line]
	start := min(t.col, len(line))
	for start > 0 && line[start-1].s != " " {
		start--
	}
	return start >= min(t.col, len(line)) || line[start].s != "@"
}

// pause stops drawing until the field is seen again: the agent's output
// flows, and its own rendering takes over from the drawing.
func (e *echo) pause() {
	e.paused, e.restoring = true, false
	e.epoch++
	e.staleBefore = e.ackSeq + 1
	e.states = nil
	if e.painted {
		e.dirty = true
	}
	e.probeDue = e.now().Add(echoProbeAfter)
}

// queue adds a predicted key to the batch, which goes after flushAfter, or
// at once when there is no batching.
func (e *echo) queue(raw []byte) (send []byte) {
	e.pending = append(e.pending, raw...)
	if e.cfg.flushAfter <= 0 {
		return e.flush()
	}
	if e.flushDue.IsZero() {
		e.flushDue = e.now().Add(e.cfg.flushAfter)
	}
	return nil
}

// flush returns the pending keys, clearing them, and arranges for the
// field to be asked for after them. While a resend is owed it returns
// nothing: the keys wait for the field that settles it.
func (e *echo) flush() []byte {
	e.flushDue = time.Time{}
	if e.resend {
		return nil
	}
	out := e.pending
	e.pending = nil
	if len(out) > 0 {
		for i := range e.states {
			if !e.states[i].sent {
				e.states[i].sent = true
				e.states[i].seenFrom = e.ackSeq + 1
			}
		}
		e.probeDue = e.now().Add(echoProbeAfter)
	}
	return out
}

// tick runs what is due: a flush, a probe.
func (e *echo) tick(now time.Time) (send []byte, reqs []*protocol.Request) {
	if !e.flushDue.IsZero() && !now.Before(e.flushDue) {
		send = e.flush()
	}
	if !e.decodeDue.IsZero() && !now.Before(e.decodeDue) {
		// Expired bytes are never keys the echo predicts.
		for _, ev := range e.keys.expire() {
			s, _ := e.key(ev)
			send = append(send, s...)
		}
		e.noteHeld()
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
	e.probeEpoch = e.epoch
	for i := range e.states {
		e.states[i].asked = e.states[i].sent
	}
	e.probeDue, e.quietDue = time.Time{}, time.Time{}
	reqs := []*protocol.Request{{AttachField: protocol.Empty}}
	if !e.frames && (e.dropped > 0 || e.dirty || e.painted) {
		e.snapshotDue = true
		reqs = append(reqs, &protocol.Request{AttachSnapshot: protocol.Empty})
	}
	return reqs
}

// overlay is the drawing to put over a frame: the predicted rows while
// keys are in flight, ending with the cursor where the prediction puts it
// (the painter leaves it there rather than at the frame's, which is behind
// by those keys); nothing once the frame shows them itself.
func (e *echo) overlay() []byte {
	if e == nil || !e.frames || !e.holding() || !e.active() {
		return nil
	}
	return e.paintRows(layout(e.text, e.field))
}

// schedule arms the timer for the earliest due time.
func (e *echo) schedule() {
	var due time.Time
	times := []time.Time{e.flushDue, e.decodeDue}
	if !e.probing {
		times = append(times, e.probeDue, e.quietDue)
	}
	for _, t := range times {
		if t.IsZero() {
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
	if e.probeEpoch != e.epoch {
		// Asked for before an unpredicted key or a resize: the field it
		// describes may be gone. pause and resized asked for another.
		e.probeEpoch = e.epoch
		e.schedule()
		return nil, nil
	}
	return e.reconcile(f)
}

// frameField takes the field in a frame the worker built after reading
// the ack of frame acked (Frame.AckedBy), so it shows the keys sent before
// that ack: those can confirm a prediction, and a field from before an
// unpredicted key or a resize is ignored.
//
// A frame may also show keys it is not proven to: one built before that
// ack but after the keys arrived. A field that equals only such a state
// proves nothing either way, so it is ignored too. Either way the field
// is asked for again, and each ask makes a frame, so one past both bounds
// follows.
func (e *echo) frameField(f *protocol.Field, acked uint32) (send, paint []byte) {
	e.probing = false
	ignore := acked < e.staleBefore
	if !ignore {
		proven, unproven := false, false
		for i := range e.states {
			st := &e.states[i]
			st.asked = st.sent && acked >= st.seenFrom
			if st.at(f) {
				proven = proven || st.asked
				unproven = unproven || (st.sent && !st.asked)
			}
		}
		ignore = unproven && !proven
	}
	if ignore {
		if e.probeDue.IsZero() {
			e.probeDue = e.now().Add(echoProbeAfter)
		}
		e.schedule()
		return nil, nil
	}
	return e.reconcile(f)
}

// frameAcked notes that frame seq was acknowledged.
func (e *echo) frameAcked(seq uint32) {
	if e != nil {
		e.ackSeq = seq
	}
}

// reconcile takes a field that can show the keys marked asked.
func (e *echo) reconcile(f *protocol.Field) (send, paint []byte) {
	e.paused = false
	e.offset = 0
	resend := e.resend
	e.resend = false
	if !f.Found {
		e.field = nil
		e.states = nil
		if resend {
			send = e.resumeHeld()
		}
		e.schedule()
		return send, nil
	}
	// Which prediction has the agent reached? The states were replayed
	// from the previous field, so that field is the state before the
	// oldest of them. Text alone is not enough: Left and Right change
	// only the cursor.
	matched := -1
	for i := len(e.states) - 1; i >= 0; i-- {
		if e.states[i].asked && e.states[i].at(f) {
			matched = i
			break
		}
	}
	var remaining []echoState
	switch {
	case matched >= 0:
		remaining = e.states[matched+1:]
	case len(e.states) > 0 && e.field != nil && sameField(e.field, f):
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
	e.field, e.restoring = f, false
	e.text = textFromField(f)
	e.states = nil
	for _, st := range remaining {
		if !e.predict(st.key) {
			break
		}
		lay := layout(e.text, e.field)
		next := echoState{rows: lay.texts(), cRow: lay.cRow, cCol: lay.cCol, key: st.key, sent: st.sent, seenFrom: st.seenFrom}
		if resend && st.sent {
			// Sent on the lost connection, and not drawn: it never
			// arrived. Those still batched go below, after it.
			send = append(send, st.key.raw...)
			next.seenFrom = e.ackSeq + 1
		}
		e.states = append(e.states, next)
	}
	if resend {
		send = append(send, e.resumeHeld()...)
	}
	if len(e.states) > 0 {
		paint = e.paint(layout(e.text, e.field))
		e.probeDue = e.now().Add(echoProbeAfter)
	}
	e.schedule()
	return send, paint
}

// resumeHeld sends the keys held for a resend, which were not predicted:
// if any were typed while it was owed, the drawing no longer shows the
// field, so it pauses until the field is seen again.
func (e *echo) resumeHeld() []byte {
	held := e.held
	e.held = false
	send := e.flush()
	if held {
		e.pause()
	}
	return send
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
	e.repainted()
	return []byte(syncBegin + attachRepaintSequence + string(data) + syncEnd)
}

// repainted notes a snapshot painted over the session's screen (the echo's
// repaint, a resync, the overlay closing), opening the focus window.
func (e *echo) repainted() {
	if e != nil {
		e.focusUntil = e.now().Add(echoFocusWindow)
	}
}

// suspend ends the echo for now (the overlay opens, the attachment ends):
// pending keys go, with what the decoder holds, and predictions are
// dropped.
func (e *echo) suspend() (send []byte) {
	// Keys a reattach left unconfirmed are not sent again: the field
	// that would say which never arrived.
	e.resend, e.held = false, false
	// What the decoder holds goes too, rather than in front of the next
	// key typed after the overlay, which has a decoder of its own.
	e.pending = append(e.pending, e.keys.reset()...)
	e.noteHeld()
	send = e.flush()
	e.states = nil
	e.paused, e.restoring = false, false
	e.painted = false
	e.offset = 0
	e.schedule()
	return send
}

// resized: the field moved; ask again once the agent has redrawn.
func (e *echo) resized() {
	e.epoch++
	e.staleBefore = e.ackSeq + 1
	e.field, e.rest, e.restoring = nil, nil, false
	e.states = nil
	e.painted = false
	e.offset = 0
	e.quietDue = e.now().Add(echoProbeAfter)
	e.schedule()
}

// reattached: the screen was repainted from the new attachment's snapshot;
// the field is asked for at once, and the keys it proves missing are sent
// again. Whatever was asked of the lost attachment will not be answered. A
// resend still owed from an earlier reattach stays owed, with the keys held
// for it.
func (e *echo) reattached() {
	e.resend = e.resend || len(e.states) > 0
	if e.resend {
		e.flushDue = time.Time{}
	}
	// A focus report sent before the stream was lost may never have
	// arrived: the next one goes, whatever it repeats.
	e.focus = ""
	e.painted, e.dropped, e.dirty, e.offset = false, 0, false, 0
	// The screen remembered with the field empty was the old attachment's.
	e.rest, e.restoring = nil, false
	e.probeDue = e.now()
	e.probing, e.snapshotDue = false, false
	// The new attachment's frames count from 1, and each shows whatever
	// reached the agent on the lost one.
	e.ackSeq, e.staleBefore = 0, 0
	for i := range e.states {
		e.states[i].seenFrom = 0
	}
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
	out := append([]byte(syncBegin), e.paintRows(lay)...)
	return append(out, syncEnd...)
}

// paintRows is paint without the synchronized-output wrapper, for
// composing over a frame.
func (e *echo) paintRows(lay echoLayout) []byte {
	f := e.field
	lines := int(f.Lines)
	var b bytes.Buffer
	if e.restoring {
		for y, row := range e.rest.rows {
			b.WriteString(cup(y, 0) + ansiReset + "\x1b[K")
			b.Write(row)
		}
	}
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
	e.painted = true
	return b.Bytes()
}
