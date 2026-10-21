package cli

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

	"agtd.sh/internal/config"
	"agtd.sh/internal/protocol"
	"agtd.sh/internal/transport"
)

// echoFlushAfter is the batching window the tests type with: the configured
// default.
const echoFlushAfter = config.DefaultFlushAfter

// The wrap rules against what the agents drew (the probe measurements in
// docs/designs/smooth-remote-attach.md and the field fixtures in
// internal/worker).

var fieldText = strings.Repeat("the quick brown fox jumps over the lazy dog ", 4)

func rowsOf(text string, width int, keepSpace bool) []string {
	var out []string
	for _, r := range wrapLine(graphemes(text), width, keepSpace) {
		out = append(out, r.text())
	}
	return out
}

func TestWrapLikeTheAgents(t *testing.T) {
	k := func(n int) string { return strings.Repeat("k", n) }
	m := func(n int) string { return strings.Repeat("m", n) }
	for _, c := range []struct {
		name      string
		text      string
		width     int
		keepSpace bool
		want      []string
	}{
		{"claude words", fieldText, 116, false, []string{
			"the quick brown fox jumps over the lazy dog the quick brown fox jumps over the lazy dog the quick brown fox jumps",
			"over the lazy dog the quick brown fox jumps over the lazy dog"}},
		{"codex words", fieldText, 117, false, []string{
			"the quick brown fox jumps over the lazy dog the quick brown fox jumps over the lazy dog the quick brown fox jumps",
			"over the lazy dog the quick brown fox jumps over the lazy dog"}},
		{"pi words", fieldText, 119, true, []string{
			"the quick brown fox jumps over the lazy dog the quick brown fox jumps over the lazy dog the quick brown fox jumps over",
			"the lazy dog the quick brown fox jumps over the lazy dog"}},
		{"claude long word", k(130), 116, false, []string{k(116), k(14)}},
		{"codex long word", k(130), 117, false, []string{k(117), k(13)}},
		{"pi long word", k(130), 119, true, []string{k(119), k(11)}},
		// A space that falls on the break is dropped by Claude Code and
		// Codex, kept by pi (the probe's boundary cases).
		{"claude full row then space", m(116) + " zzkkkkk", 116, false, []string{m(116), "zzkkkkk"}},
		{"claude word split then space", m(117) + " zzkkkkk", 116, false, []string{m(116), "m zzkkkkk"}},
		{"codex full row then space", m(117) + " zzkkkkk", 117, false, []string{m(117), "zzkkkkk"}},
		{"pi full row then space", m(119) + " zzkkkkk", 119, true, []string{m(119), " zzkkkkk"}},
		{"pi space fits then word wraps", m(118) + " zzkkkkk", 119, true, []string{m(118), "zzkkkkk"}},
		{"wide characters", "wide 日本語 👍 end", 116, false, []string{"wide 日本語 👍 end"}},
		{"empty", "", 116, false, []string{""}},
	} {
		if got := rowsOf(c.text, c.width, c.keepSpace); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

// claudeField is Claude Code's field with rows in it and the cursor at the
// end of the last.
func claudeField(rows []string) *protocol.Field {
	last := rows[len(rows)-1]
	return &protocol.Field{Found: true, Cols: 120, Lines: 40, Alternate: true, Row: 36 + uint16(len(rows)-1), Col: 2 + uint16(cellsWidth(graphemes(last))),
		Top: 36, BoxTop: 35, BoxBottom: 37,
		TextCol: 2, Width: 116, GrowsUp: true, Rows: rows, Style: []byte("\x1b[0m"),
		Chrome: []protocol.ScreenRow{{Index: 35, VT: []byte(strings.Repeat("─", 120))}, {Index: 36, VT: []byte("❯ \x1b[2mTry it\x1b[0m")}, {Index: 37, VT: []byte(strings.Repeat("─", 120))}}}
}

func typeKeys(e *echo, s string) (send, paint []byte) {
	for _, r := range s {
		sd, pt := e.input([]byte(string(r)))
		send = append(send, sd...)
		if pt != nil {
			paint = pt
		}
	}
	return send, paint
}

func TestLayoutCursorFollowsTheAgent(t *testing.T) {
	f := claudeField([]string{""})
	var text echoText
	text.insert(fieldText)
	lay := layout(text, f)
	if lay.cRow != 1 || lay.cCol != 62 {
		t.Fatalf("after the text: cursor at row %d col %d, want 1, 62", lay.cRow, lay.cCol)
	}
	text = echoText{}
	text.insert(strings.Repeat("k", 130))
	if lay = layout(text, f); lay.cRow != 1 || lay.cCol != 14 {
		t.Fatalf("long word: cursor at row %d col %d, want 1, 14", lay.cRow, lay.cCol)
	}
	for range 14 {
		text.backspace()
	}
	if lay = layout(text, f); len(lay.rows) != 1 || lay.cRow != 0 || lay.cCol != 116 {
		t.Fatalf("after backspacing across the wrap: %d rows, cursor at row %d col %d", len(lay.rows), lay.cRow, lay.cCol)
	}
	for range 5 {
		text.left()
	}
	text.insert("Q")
	if lay = layout(text, f); len(lay.rows) != 2 || lay.cRow != 0 || lay.cCol != 112 || lay.rows[1].text() != "k" {
		t.Fatalf("after inserting mid-row: %q, cursor at row %d col %d", lay.texts(), lay.cRow, lay.cCol)
	}
	text = echoText{}
	text.insert("one")
	text.newline()
	text.newline()
	text.insert("three")
	if lay = layout(text, f); !reflect.DeepEqual(lay.texts(), []string{"one", "", "three"}) || lay.cRow != 2 || lay.cCol != 5 {
		t.Fatalf("empty line: %q, cursor at row %d col %d", lay.texts(), lay.cRow, lay.cCol)
	}
	// Backspace at the start of a line joins it to the one before.
	text.line, text.col = 2, 0
	text.backspace()
	if lay = layout(text, f); !reflect.DeepEqual(lay.texts(), []string{"one", "three"}) || lay.cRow != 1 || lay.cCol != 0 {
		t.Fatalf("joined: %q, cursor at row %d col %d", lay.texts(), lay.cRow, lay.cCol)
	}
}

func TestTextFromFieldPlacesTheCursor(t *testing.T) {
	f := claudeField([]string{"hello", "日本語x"})
	f.Row, f.Col, f.Top = 37, 8, 36 // on the second row, after 日本語
	text := textFromField(f)
	if text.line != 1 || text.col != 3 || cellsString(text.lines[1]) != "日本語x" {
		t.Fatalf("text %+v", text)
	}
}

// A typed key is drawn at once, sent after flushAfter, the field asked for
// after that, and the screen repainted once the field confirms it.
func TestEchoFlow(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	e := newEcho(echoConfig{flushAfter: 50 * time.Millisecond})
	e.now = func() time.Time { return now }
	if send, paint := typeKeys(e, "x"); send == nil || paint != nil {
		t.Fatalf("without a field, keys go raw: send %q, paint %q", send, paint)
	}
	e.fieldAnswer(claudeField([]string{""}))

	send, paint := typeKeys(e, "hi")
	if len(send) != 0 || !bytes.Contains(paint, []byte("hi")) || !bytes.HasPrefix(paint, []byte(syncBegin)) || !bytes.HasSuffix(paint, []byte(syncEnd)) {
		t.Fatalf("typing: send %q, paint %q", send, paint)
	}
	if !e.holding() || e.unconfirmed() != 2 {
		t.Fatalf("holding %v, unconfirmed %d", e.holding(), e.unconfirmed())
	}
	e.output(100) // the agent's redraw, held
	if send, reqs := e.tick(now.Add(49 * time.Millisecond)); send != nil || reqs != nil {
		t.Fatalf("too early: send %q, reqs %v", send, reqs)
	}
	send, reqs := e.tick(now.Add(50 * time.Millisecond))
	if string(send) != "hi" || reqs != nil {
		t.Fatalf("flush: send %q, reqs %v", send, reqs)
	}
	now = now.Add(50 * time.Millisecond)
	send, reqs = e.tick(now.Add(echoProbeAfter))
	if send != nil || len(reqs) != 2 || reqs[0].AttachField == nil || reqs[1].AttachSnapshot == nil {
		t.Fatalf("probe: send %q, reqs %v", send, reqs)
	}
	// The agent has drawn one of the two keys: the other is still owed.
	if send, paint := e.fieldAnswer(claudeField([]string{"h"})); send != nil || paint == nil || e.unconfirmed() != 1 {
		t.Fatalf("partial confirmation: send %q, paint %q, unconfirmed %d", send, paint, e.unconfirmed())
	}
	if paint := e.snapshotAnswer([]byte("SNAP")); paint != nil {
		t.Fatalf("a snapshot while keys are owed must not repaint: %q", paint)
	}
	// Then both.
	e.fieldAnswer(claudeField([]string{"hi"}))
	if e.holding() {
		t.Fatal("still holding after the field confirmed everything")
	}
	reqs = e.probe()
	if paint := e.snapshotAnswer([]byte("SNAP")); string(paint) != syncBegin+attachRepaintSequence+"SNAP"+syncEnd {
		t.Fatalf("repaint %q", paint)
	}
	if e.dropped != 0 || e.painted {
		t.Fatalf("dropped %d, painted %v after the repaint", e.dropped, e.painted)
	}
}

func TestEchoUnpredictedKeysGoAtOnceAndPause(t *testing.T) {
	e := newEcho(echoConfig{flushAfter: echoFlushAfter})
	e.fieldAnswer(claudeField([]string{""}))
	typeKeys(e, "ab")
	send, paint := e.input([]byte("\r"))
	if string(send) != "ab\r" || paint != nil || !e.paused || e.holding() {
		t.Fatalf("enter: send %q, paint %q, paused %v, holding %v", send, paint, e.paused, e.holding())
	}
	// While paused, keys go raw.
	if send, paint := typeKeys(e, "c"); string(send) != "c" || paint != nil {
		t.Fatalf("paused: send %q, paint %q", send, paint)
	}
	// The next field asked for resumes the echo.
	e.probe()
	e.fieldAnswer(claudeField([]string{""}))
	if send, paint := typeKeys(e, "d"); send != nil || paint == nil {
		t.Fatalf("resumed: send %q, paint %q", send, paint)
	}
	// Value: protects=keys typed before an unpredicted key in the same
	// read are not drawn after it; fails_when=input paints whenever a
	// key was drawn, even after the pause dropped the predictions;
	// why_new=the cases above type the unpredicted key in a read of its
	// own; seam=none; no_claim=what the agent then draws.
	// Typed in one read, the Enter submits the text before it: drawing
	// that text after it would show what the agent has just cleared.
	e.probe()
	e.fieldAnswer(claudeField([]string{""}))
	if send, paint := e.input([]byte("ef\r")); string(send) != "def\r" || paint != nil || !e.paused {
		t.Fatalf("one read: send %q, paint %q, paused %v", send, paint, e.paused)
	}
}

// A fullscreen agent (Claude Code, Codex) turns on mouse reporting and
// scrolls its transcript on the wheel: input that edits nothing goes on as
// it arrived, in order, and leaves the drawing standing. Clicks and keys the
// decoder has no name for go too, and pause the echo.
func TestEchoForwardsInputThatIsNotText(t *testing.T) {
	passive := []string{
		"\x1b[<64;10;5M", // SGR wheel up
		"\x1b[<65;10;5M", // SGR wheel down
		"\x1b[<35;12;7M", // SGR motion, no button
		"\x1b[M`*%",      // X10 wheel up
		"\x1b[96;10;5M",  // urxvt (1015) wheel up
		"\x1b[97;1:3u",   // kitty key release
		"\x1b[12;40R",    // cursor position report
		"\x1b[?62;22c",   // device attributes
		"\x1b[?2026;2$y", // mode report
		"\x1b[?1u",       // kitty keyboard flags
		"\x1b[>1;10;0c",  // secondary device attributes
		"\x1b[0n",        // status report
		"\x1b[8;24;80t",  // window size report
	}
	for _, in := range passive {
		e := newEcho(echoConfig{flushAfter: echoFlushAfter})
		if send, _ := e.input([]byte(in)); string(send) != in {
			t.Errorf("%q without a field: send %q", in, send)
		}
		e.fieldAnswer(claudeField([]string{""}))
		typeKeys(e, "ab")
		// Behind the batched keys, not ahead of them.
		if send, paint := e.input([]byte(in)); send != nil || paint != nil || e.paused || e.unconfirmed() != 2 {
			t.Errorf("%q while typing: send %q, paint %q, paused %v, unconfirmed %d", in, send, paint, e.paused, e.unconfirmed())
		}
		if send := e.flush(); string(send) != "ab"+in {
			t.Errorf("%q: flushed %q", in, send)
		}
		// Nothing batched: at once.
		if send, _ := e.input([]byte(in)); string(send) != in || e.paused {
			t.Errorf("%q idle: send %q, paused %v", in, send, e.paused)
		}
	}
	for _, in := range []string{
		"\x1b[<0;10;5M",          // SGR left press
		"\x1b[<0;10;5m",          // SGR left release
		"\x1b[M #%",              // X10 left press
		"\x1b[32;10;5M",          // urxvt (1015) left press
		"\x1b[0;10;5M",           // urxvt report with a button below 32
		"\x1b[<99999999999;1;1M", // SGR button out of range
		"\x1bOP",                 // F1
		"\x1b[15~",               // F5
		"\x1b[2~",                // Insert
		"\x1b[\x01",              // not a CSI sequence: ESC [, then Ctrl-A
		"\x1b[<;10;5M",           // SGR report with no button
	} {
		e := newEcho(echoConfig{flushAfter: echoFlushAfter})
		if send, paint := e.input([]byte(in)); string(send) != in || paint != nil {
			t.Errorf("%q without a field: send %q, paint %q", in, send, paint)
		}
		e.fieldAnswer(claudeField([]string{""}))
		typeKeys(e, "ab")
		if send, _ := e.input([]byte(in)); string(send) != "ab"+in || !e.paused {
			t.Errorf("%q: send %q, paused %v", in, send, e.paused)
		}
		// Paused, it still goes at once.
		if send, paint := e.input([]byte(in)); string(send) != in || paint != nil || !e.paused {
			t.Errorf("%q paused: send %q, paint %q, paused %v", in, send, paint, e.paused)
		}
	}
}

// The same input against the echo's other states: one read mixing typing
// and a wheel scroll, a scroll while paused, a click after the batch went
// but before the agent confirmed it, and both at the prediction cap.
func TestEchoInputThatIsNotTextInEveryState(t *testing.T) {
	const wheel, click = "\x1b[<64;10;5M", "\x1b[<0;10;5M"

	e := newEcho(echoConfig{flushAfter: echoFlushAfter})
	e.fieldAnswer(claudeField([]string{""}))
	in := "a" + wheel + "b"
	if send, _ := e.input([]byte(in)); send != nil || e.paused || e.unconfirmed() != 2 {
		t.Fatalf("mixed read: send %q, paused %v, unconfirmed %d", send, e.paused, e.unconfirmed())
	}
	if send := e.flush(); string(send) != in {
		t.Fatalf("mixed read: flushed %q", send)
	}

	e = newEcho(echoConfig{flushAfter: echoFlushAfter})
	e.fieldAnswer(claudeField([]string{""}))
	e.input([]byte("\r"))
	due := e.probeDue
	if send, paint := e.input([]byte(wheel)); string(send) != wheel || paint != nil || !e.paused || e.probeDue != due {
		t.Fatalf("paused: send %q, paint %q, paused %v, probe moved %v", send, paint, e.paused, e.probeDue != due)
	}

	e = newEcho(echoConfig{flushAfter: echoFlushAfter})
	e.fieldAnswer(claudeField([]string{""}))
	typeKeys(e, "ab")
	e.flush()
	if send, _ := e.input([]byte(click)); string(send) != click || !e.paused || e.unconfirmed() != 0 || !e.dirty {
		t.Fatalf("click while holding: send %q, paused %v, unconfirmed %d, dirty %v", send, e.paused, e.unconfirmed(), e.dirty)
	}

	// A scroll while the keys are unconfirmed: it goes at once and the
	// drawing stands; the agent's scrolled screen is held with the rest of
	// its output and painted from a snapshot once the keys are confirmed.
	e = newEcho(echoConfig{flushAfter: echoFlushAfter})
	e.fieldAnswer(claudeField([]string{""}))
	typeKeys(e, "ab")
	e.flush()
	if send, paint := e.input([]byte(wheel)); string(send) != wheel || paint != nil || e.paused || e.unconfirmed() != 2 || !e.holding() {
		t.Fatalf("scroll while holding: send %q, paint %q, paused %v, unconfirmed %d", send, paint, e.paused, e.unconfirmed())
	}
	e.output(500)
	if reqs := e.probe(); len(reqs) != 2 || reqs[1].AttachSnapshot == nil {
		t.Fatalf("probe after held output: %v", reqs)
	}
	e.fieldAnswer(claudeField([]string{"ab"}))
	if paint := e.snapshotAnswer([]byte("SNAP")); string(paint) != syncBegin+attachRepaintSequence+"SNAP"+syncEnd {
		t.Fatalf("repaint after the scroll: %q", paint)
	}

	// While the field is being asked for, input that edits nothing goes at
	// once and leaves the question outstanding.
	e = newEcho(echoConfig{flushAfter: echoFlushAfter})
	e.fieldAnswer(claudeField([]string{""}))
	e.probe()
	if send, _ := e.input([]byte("\x1b[I")); string(send) != "\x1b[I" || !e.probing || e.paused {
		t.Fatalf("focus while probing: send %q, probing %v, paused %v", send, e.probing, e.paused)
	}

	// A click while the field is being asked for makes the answer stale:
	// it describes the field before the click, so the echo stays paused
	// and asks again.
	e = newEcho(echoConfig{flushAfter: echoFlushAfter})
	e.fieldAnswer(claudeField([]string{""}))
	e.probe()
	e.input([]byte(click))
	if _, paint := e.fieldAnswer(claudeField([]string{""})); paint != nil || !e.paused || e.probeDue.IsZero() {
		t.Fatalf("click while probing: paint %q, paused %v", paint, e.paused)
	}

	// Opening the overlay sends what is batched, a scroll queued behind
	// the keys included, in order.
	e = newEcho(echoConfig{flushAfter: echoFlushAfter})
	e.fieldAnswer(claudeField([]string{""}))
	typeKeys(e, "ab")
	e.input([]byte(wheel))
	if send := e.suspend(); string(send) != "ab"+wheel || len(e.pending) != 0 || e.unconfirmed() != 0 {
		t.Fatalf("suspend: sent %q, pending %q, unconfirmed %d", send, e.pending, e.unconfirmed())
	}

	// A click with nothing typed goes at once and pauses until the field
	// is seen again.
	e = newEcho(echoConfig{flushAfter: echoFlushAfter})
	e.fieldAnswer(claudeField([]string{""}))
	if send, paint := e.input([]byte(click)); string(send) != click || paint != nil || !e.paused || e.probeDue.IsZero() {
		t.Fatalf("click while idle: send %q, paint %q, paused %v", send, paint, e.paused)
	}

	// Past the cap every key goes raw, after the batch, without a pause.
	for _, in := range []string{wheel, "\x1bOP"} {
		e = newEcho(echoConfig{flushAfter: echoFlushAfter})
		e.fieldAnswer(claudeField([]string{""}))
		typeKeys(e, strings.Repeat("a", echoMaxStates))
		send, _ := e.input([]byte(in))
		send = append(send, e.flush()...)
		if !bytes.HasSuffix(send, []byte(in)) || e.paused || len(e.pending) != 0 {
			t.Errorf("%q at the cap: send ends %q, paused %v", in, send[max(len(send)-8, 0):], e.paused)
		}
	}
}

// Shift-Enter (kitty form) and Alt-Enter are newlines on every agent;
// Ctrl-Enter and Ctrl-J are not the same everywhere, so they go raw.
func TestEchoNewlineKeys(t *testing.T) {
	e := newEcho(echoConfig{flushAfter: echoFlushAfter})
	e.fieldAnswer(claudeField([]string{""}))
	typeKeys(e, "one")
	if send, paint := e.input([]byte("\x1b[13;2u")); send != nil || paint == nil || e.paused {
		t.Fatalf("shift-enter: send %q, paused %v", send, e.paused)
	}
	e.input([]byte("\x1b\r"))
	typeKeys(e, "three")
	if rows := e.states[len(e.states)-1].rows; !reflect.DeepEqual(rows, []string{"one", "", "three"}) {
		t.Fatalf("rows %q", rows)
	}
	for _, key := range []string{"\x1b[13;5u", "\x1b[106;5u", "\r"} {
		e.probe()
		e.fieldAnswer(claudeField([]string{""}))
		// Unpredicted: it goes at once, after whatever was still batched.
		if send, _ := e.input([]byte(key)); !bytes.HasSuffix(send, []byte(key)) || !e.paused {
			t.Fatalf("%q: send %q, paused %v", key, send, e.paused)
		}
	}
}

func TestEchoAdoptsWhatTheAgentDrew(t *testing.T) {
	e := newEcho(echoConfig{flushAfter: echoFlushAfter})
	e.fieldAnswer(claudeField([]string{""}))
	typeKeys(e, "hel")
	// The agent shows something none of the predictions match.
	e.fieldAnswer(claudeField([]string{"hex"}))
	if e.unconfirmed() != 0 || !e.dirty || cellsString(e.text.lines[0]) != "hex" {
		t.Fatalf("unconfirmed %d, dirty %v, text %q", e.unconfirmed(), e.dirty, cellsString(e.text.lines[0]))
	}
	// Keys owed are replayed over what the agent shows: here the agent
	// put the cursor elsewhere.
	typeKeys(e, "yz")
	e.flush()
	e.probe()
	e.fieldAnswer(claudeField([]string{"hexy"}))
	if e.unconfirmed() != 1 || e.states[0].rows[0] != "hexyz" {
		t.Fatalf("replay: unconfirmed %d, states %+v", e.unconfirmed(), e.states)
	}
}

// A confirmation that lands after a typed space must keep the space: the
// worker reports blanks before the cursor as text, and so does the echo.
func TestEchoKeepsTypedSpacesAcrossConfirmation(t *testing.T) {
	e := newEcho(echoConfig{flushAfter: echoFlushAfter})
	e.fieldAnswer(claudeField([]string{""}))
	typeKeys(e, "hello ")
	if rows := e.states[len(e.states)-1].rows; rows[0] != "hello " {
		t.Fatalf("predicted rows %q", rows)
	}
	e.fieldAnswer(claudeField([]string{"hello "}))
	if e.unconfirmed() != 0 || cellsString(e.text.lines[0]) != "hello " || e.text.col != 6 {
		t.Fatalf("after confirmation: unconfirmed %d, text %q, col %d", e.unconfirmed(), cellsString(e.text.lines[0]), e.text.col)
	}
	typeKeys(e, "world")
	if rows := e.states[len(e.states)-1].rows; rows[0] != "hello world" {
		t.Fatalf("after more typing: %q", rows)
	}
	// Spaces after the cursor are not kept (the cursor moved left).
	e.input([]byte("\x1b[D\x1b[D"))
	if rows := e.states[len(e.states)-1].rows; rows[0] != "hello world" || e.states[len(e.states)-1].cCol != 9 {
		t.Fatalf("after left: %q, col %d", rows, e.states[len(e.states)-1].cCol)
	}
}

func TestEchoResendsAfterReattach(t *testing.T) {
	e := newEcho(echoConfig{flushAfter: echoFlushAfter})
	e.fieldAnswer(claudeField([]string{""}))
	typeKeys(e, "abc")
	e.tick(e.now().Add(time.Second)) // flushed
	e.reattached()
	// The agent had taken "a" before the connection went; "bc" is sent again.
	send, _ := e.fieldAnswer(claudeField([]string{"a"}))
	if string(send) != "bc" || e.unconfirmed() != 2 {
		t.Fatalf("resend %q, unconfirmed %d", send, e.unconfirmed())
	}
	// A field that matches nothing after a reattach resends nothing.
	e.reattached()
	if send, _ := e.fieldAnswer(claudeField([]string{""})); send != nil || e.unconfirmed() != 0 {
		t.Fatalf("resend %q after a mismatch", send)
	}
}

func TestEchoPaintMovesTheRuleAsTheBoxGrows(t *testing.T) {
	e := newEcho(echoConfig{flushAfter: echoFlushAfter})
	e.fieldAnswer(claudeField([]string{""}))
	_, paint := typeKeys(e, "one")
	p := string(paint)
	// The rule stays where it was, the text sits after the glyph.
	if !strings.Contains(p, cup(35, 0)+ansiReset+strings.Repeat("─", 120)) || !strings.Contains(p, cup(36, 2)+"\x1b[0mone\x1b[113X") || !strings.HasSuffix(p, cup(36, 5)+syncEnd) {
		t.Fatalf("one row:\n%q", p)
	}
	_, paint = e.input([]byte("\x1b\rtwo"))
	p = string(paint)
	// Two rows: the box grew up by one, the rule above with it, and
	// the glyph row's chrome moved up too.
	if !strings.Contains(p, cup(34, 0)+ansiReset+strings.Repeat("─", 120)) || !strings.Contains(p, cup(35, 0)+ansiReset+"\x1b[2K❯ ") ||
		!strings.Contains(p, cup(36, 2)+"\x1b[0mtwo\x1b[113X") || !strings.HasSuffix(p, cup(36, 5)+syncEnd) {
		t.Fatalf("two rows:\n%q", p)
	}
}

func TestEchoPaintInverseCursorAndDownwardGrowth(t *testing.T) {
	f := &protocol.Field{Found: true, Cols: 120, Lines: 40, Row: 28, Col: 0, Top: 28, BoxTop: 27, BoxBottom: 29, TextCol: 0, Width: 119,
		InverseCursor: true, WrapKeepsSpace: true, Rows: []string{""}, Style: []byte("\x1b[0m"),
		Chrome: []protocol.ScreenRow{{Index: 27, VT: []byte("RULE")}, {Index: 28, VT: nil}, {Index: 29, VT: []byte("RULE")}, {Index: 30, VT: []byte("status")}}}
	e := newEcho(echoConfig{flushAfter: echoFlushAfter})
	e.fieldAnswer(f)
	_, paint := typeKeys(e, "ab")
	p := string(paint)
	if !strings.Contains(p, cup(28, 0)+"\x1b[0mab\x1b[7m \x1b[27m\x1b[116X") {
		t.Fatalf("inverse cursor:\n%q", p)
	}
	_, paint = e.input([]byte("\x1b\rc"))
	p = string(paint)
	// The rule below and the status row moved down by one.
	if !strings.Contains(p, cup(30, 0)+ansiReset+"RULE") || !strings.Contains(p, cup(31, 0)+ansiReset+"status") || !strings.Contains(p, cup(29, 0)+"\x1b[0m") {
		t.Fatalf("grown down:\n%q", p)
	}
}

func TestEchoPaintScrollsWhenTheBoxReachesTheBottom(t *testing.T) {
	f := &protocol.Field{Found: true, Cols: 80, Lines: 10, Row: 8, Col: 0, Top: 8, BoxTop: 7, BoxBottom: 9, TextCol: 0, Width: 79,
		InverseCursor: true, WrapKeepsSpace: true, Rows: []string{""}, Style: []byte("\x1b[0m"),
		Chrome: []protocol.ScreenRow{{Index: 7, VT: []byte("RULE")}, {Index: 8, VT: nil}, {Index: 9, VT: []byte("RULE")}}}
	e := newEcho(echoConfig{flushAfter: echoFlushAfter})
	e.fieldAnswer(f)
	var all []byte
	for _, key := range []string{"a", "\x1b\r", "b"} {
		_, paint := e.input([]byte(key))
		all = append(all, paint...)
	}
	p := string(all)
	// One row past the bottom: the screen scrolls by one, once, and
	// everything is drawn one row higher from then on.
	if strings.Count(p, cup(9, 0)+"\n") != 1 || e.offset != 1 || !strings.HasSuffix(p, cup(8, 1)+syncEnd) || !strings.Contains(p, cup(6, 0)+ansiReset+"RULE") {
		t.Fatalf("scrolled:\n%q (offset %d)", p, e.offset)
	}
}

// A new echo asks for the field once output has been quiet for a moment
// after the attach, even when nothing follows the snapshot: the prompt may
// already be in it.
func TestEchoAsksForTheFieldAfterAttach(t *testing.T) {
	e := newEcho(echoConfig{flushAfter: echoFlushAfter})
	if _, reqs := e.tick(time.Now()); reqs != nil {
		t.Fatalf("asked at once: %v", reqs)
	}
	if _, reqs := e.tick(time.Now().Add(echoProbeAfter)); len(reqs) != 1 || reqs[0].AttachField == nil {
		t.Fatalf("not asked after the quiet moment: %v", reqs)
	}
}

// fixedClockEcho is an echo with its clock stopped at the time it returns.
func fixedClockEcho(flushAfter time.Duration) (*echo, time.Time) {
	now := time.Unix(1_700_000_000, 0)
	e := newEcho(echoConfig{flushAfter: flushAfter})
	e.now = func() time.Time { return now }
	return e, now
}

// The echo forwards the keys it does not predict exactly as they came,
// whether or not it has a field: none is dropped or changed. A key release
// does not stop the echo.
func TestEchoForwardsWhatItDoesNotPredict(t *testing.T) {
	keys := []string{"\x1b[15~", "\x1bOP", "\x1b[<0;10;5M", "\x1b[12;5R", "\x1b[?62;22c", "\x1b[?1u", "\x1b[I", "\x1b[57399u"}
	for _, withField := range []bool{false, true} {
		for _, key := range keys {
			e, now := fixedClockEcho(echoFlushAfter)
			if withField {
				e.fieldAnswer(claudeField([]string{""}))
			}
			var sent []byte
			for _, chunk := range []string{"a", key[:2], key[2:], "b"} {
				send, _ := e.input([]byte(chunk))
				sent = append(sent, send...)
			}
			send, _ := e.tick(now.Add(time.Second))
			sent = append(sent, send...)
			if string(sent) != "a"+key+"b" {
				t.Errorf("field %v, %q: sent %q", withField, key, sent)
			}
		}
	}
	e, now := fixedClockEcho(echoFlushAfter)
	e.fieldAnswer(claudeField([]string{""}))
	_, paint := typeKeys(e, "a")
	if send, _ := e.input([]byte("\x1b[97;1:3u")); send != nil || e.paused || paint == nil {
		t.Fatalf("release: send %q, paused %v", send, e.paused)
	}
	if _, paint := typeKeys(e, "b"); paint == nil || e.unconfirmed() != 2 {
		t.Fatalf("after a release: paint %q, unconfirmed %d", paint, e.unconfirmed())
	}
	if send, _ := e.tick(now.Add(time.Second)); string(send) != "a\x1b[97;1:3ub" {
		t.Fatalf("batch %q", send)
	}
}

// A field with the predicted text but not the predicted cursor has not
// reached that state: Left and Right change only the cursor.
func TestEchoMatchesTheCursorToo(t *testing.T) {
	e, now := fixedClockEcho(echoFlushAfter)
	e.fieldAnswer(claudeField([]string{"ab"}))
	e.input([]byte("\x1b[D"))
	e.tick(now.Add(time.Second))
	e.probe()
	// The agent has not moved its cursor yet.
	e.fieldAnswer(claudeField([]string{"ab"}))
	if e.unconfirmed() != 1 || !e.holding() {
		t.Fatalf("left confirmed by the text alone: unconfirmed %d", e.unconfirmed())
	}
	moved := claudeField([]string{"ab"})
	moved.Col--
	e.probe()
	e.fieldAnswer(moved)
	if e.unconfirmed() != 0 {
		t.Fatalf("left not confirmed by the cursor: unconfirmed %d", e.unconfirmed())
	}

	// After a reattach, editing keys the agent never got are sent again,
	// although two of the states show the text it has.
	e, now = fixedClockEcho(echoFlushAfter)
	e.fieldAnswer(claudeField([]string{"abc"}))
	edits := "\x1b[D\x1b[DX\x7f" // abc| ab|c a|bc aX|bc a|bc
	e.input([]byte(edits))
	e.tick(now.Add(time.Second))
	e.reattached()
	e.probe()
	if send, _ := e.fieldAnswer(claudeField([]string{"abc"})); string(send) != edits || e.unconfirmed() != 4 {
		t.Fatalf("none arrived: resend %q, unconfirmed %d", send, e.unconfirmed())
	}
	// The first Left arrived, the rest did not.
	e, now = fixedClockEcho(echoFlushAfter)
	e.fieldAnswer(claudeField([]string{"abc"}))
	e.input([]byte(edits))
	e.tick(now.Add(time.Second))
	e.reattached()
	e.probe()
	oneLeft := claudeField([]string{"abc"})
	oneLeft.Col--
	if send, _ := e.fieldAnswer(oneLeft); string(send) != edits[3:] {
		t.Fatalf("one arrived: resend %q", send)
	}
}

// flush_after = "0" sends every key at once.
func TestEchoWithoutBatching(t *testing.T) {
	e, now := fixedClockEcho(0)
	e.fieldAnswer(claudeField([]string{""}))
	for _, k := range []string{"h", "i", "\x1b[D"} {
		if send, paint := e.input([]byte(k)); string(send) != k || paint == nil {
			t.Fatalf("%q: send %q, paint %q", k, send, paint)
		}
	}
	if send, _ := e.tick(now.Add(time.Second)); send != nil {
		t.Fatalf("left over: %q", send)
	}
}

type discardStream struct{ transport.Stream }

func (discardStream) Write(p []byte) (int, error) { return len(p), nil }

// A snapshot asked for on a lost attachment is never answered; the overlay
// must not wait for it on the new one.
func TestEchoReattachForgetsTheLostSnapshot(t *testing.T) {
	e, now := fixedClockEcho(echoFlushAfter)
	e.fieldAnswer(claudeField([]string{""}))
	typeKeys(e, "a")
	if _, reqs := e.tick(now.Add(time.Second)); len(reqs) != 2 || !e.snapshotDue {
		t.Fatalf("probe: %v", reqs)
	}
	e.reattached() // the connection went before the answers came
	e.suspend()    // the overlay opens
	stream := &attachStream{s: discardStream{}, frames: make(chan frame, 1)}
	stream.frames <- frame{resp: &protocol.Response{AttachSnapshot: &protocol.Bytes{Data: []byte("NEW")}}}
	close(stream.frames)
	if got, _, err := restoreSnapshot(stream, nil, e); err != nil || string(got) != "NEW" {
		t.Fatalf("restored %q, %v", got, err)
	}
}

// A field asked for before an unpredicted key (or a resize) may show a
// field that is gone: it does not resume the echo.
func TestEchoIgnoresAFieldAskedBeforeAnUnpredictedKey(t *testing.T) {
	e, now := fixedClockEcho(echoFlushAfter)
	e.fieldAnswer(claudeField([]string{""}))
	typeKeys(e, "abc")
	e.tick(now.Add(time.Second))
	e.probe()
	e.input([]byte("\r")) // submits; the answer below was read before it
	e.fieldAnswer(claudeField([]string{"abc"}))
	if e.active() {
		t.Fatal("a stale field resumed the echo")
	}
	if send, paint := typeKeys(e, "x"); string(send) != "x" || paint != nil {
		t.Fatalf("after a stale field: send %q, paint %q", send, paint)
	}
	if _, reqs := e.tick(now.Add(time.Second)); len(reqs) == 0 || reqs[0].AttachField == nil {
		t.Fatalf("not asked again: %v", reqs)
	}
	e.fieldAnswer(claudeField([]string{"x"}))
	if !e.active() {
		t.Fatal("a fresh field did not resume the echo")
	}

	e.probe()
	e.resized()
	e.fieldAnswer(claudeField([]string{"x"}))
	if e.field != nil {
		t.Fatal("a field from before a resize was adopted")
	}
}

// Across a reattach every key goes once, in order: keys still batched are
// not sent on top of the resend, and keys typed while it is owed follow it.
func TestEchoReattachSendsEachKeyOnce(t *testing.T) {
	e, now := fixedClockEcho(echoFlushAfter)
	e.fieldAnswer(claudeField([]string{""}))
	typeKeys(e, "abc") // batched, not sent, when the connection goes
	e.reattached()
	var sent []byte
	send, reqs := e.tick(now.Add(time.Second))
	sent = append(sent, send...)
	if len(reqs) == 0 || reqs[0].AttachField == nil {
		t.Fatalf("not asked: %v", reqs)
	}
	send, _ = e.fieldAnswer(claudeField([]string{""}))
	sent = append(sent, send...)
	if string(sent) != "abc" {
		t.Fatalf("sent %q", sent)
	}

	e, now = fixedClockEcho(echoFlushAfter)
	e.fieldAnswer(claudeField([]string{""}))
	typeKeys(e, "ab")
	e.tick(now.Add(time.Second)) // "ab" sent
	typeKeys(e, "c")             // batched
	e.reattached()
	if send, paint := e.input([]byte("d\r")); send != nil || paint != nil {
		t.Fatalf("typed while the resend is owed: send %q, paint %q", send, paint)
	}
	e.tick(now.Add(time.Second))
	// The agent drew "a": "b" was lost with the connection.
	if send, _ := e.fieldAnswer(claudeField([]string{"a"})); string(send) != "bcd\r" || !e.paused {
		t.Fatalf("resend %q, paused %v", send, e.paused)
	}
}

// Bytes the decoder holds for the rest of a sequence go as they are when
// nothing follows: Alt-[ in a legacy terminal is ESC [.
func TestEchoExpiresIncompleteSequences(t *testing.T) {
	for _, withField := range []bool{false, true} {
		e, now := fixedClockEcho(echoFlushAfter)
		if withField {
			e.fieldAnswer(claudeField([]string{""}))
		}
		if send, _ := e.input([]byte("\x1b[")); send != nil {
			t.Fatalf("sent at once: %q", send)
		}
		if send, _ := e.tick(now.Add(echoIncompleteAfter - time.Millisecond)); send != nil {
			t.Fatalf("sent early: %q", send)
		}
		if send, _ := e.tick(now.Add(echoIncompleteAfter)); string(send) != "\x1b[" {
			t.Fatalf("field %v: sent %q", withField, send)
		}
		if withField && !e.paused {
			t.Fatal("an unknown sequence did not pause the echo")
		}
	}
	// Leaving the attachment sends them too.
	e, _ := fixedClockEcho(echoFlushAfter)
	e.input([]byte("\x1bO"))
	if send := e.suspend(); string(send) != "\x1bO" {
		t.Fatalf("suspend sent %q", send)
	}
}

// A long paste goes as it arrives, unpredicted; a short one is predicted.
func TestEchoPastes(t *testing.T) {
	e, now := fixedClockEcho(echoFlushAfter)
	e.fieldAnswer(claudeField([]string{""}))
	if send, paint := e.input([]byte("\x1b[200~hi\x1b[201~")); send != nil || !bytes.Contains(paint, []byte("hi")) {
		t.Fatalf("short paste: send %q, paint %q", send, paint)
	}
	e.tick(now.Add(time.Second))

	e, _ = fixedClockEcho(echoFlushAfter)
	e.fieldAnswer(claudeField([]string{""}))
	in := string(pasteStart) + strings.Repeat("x", 2*pasteMax) + string(pasteEnd)
	var sent []byte
	for _, chunk := range chunked(in, 4096) {
		send, paint := e.input([]byte(chunk))
		if paint != nil {
			t.Fatal("a long paste was drawn")
		}
		sent = append(sent, send...)
	}
	if string(sent) != in || !e.paused {
		t.Fatalf("sent %d of %d bytes, paused %v", len(sent), len(in), e.paused)
	}
}

// Keys that arrive in one read are drawn once, after the last, and a box
// that grows past the bottom on the way scrolls the screen once.
func TestEchoPaintsOnceForARead(t *testing.T) {
	f := &protocol.Field{Found: true, Cols: 80, Lines: 10, Row: 8, Col: 0, Top: 8, BoxTop: 7, BoxBottom: 9, TextCol: 0, Width: 79,
		InverseCursor: true, WrapKeepsSpace: true, Rows: []string{""}, Style: []byte("\x1b[0m"),
		Chrome: []protocol.ScreenRow{{Index: 7, VT: []byte("RULE")}, {Index: 8, VT: nil}, {Index: 9, VT: []byte("RULE")}}}
	e := newEcho(echoConfig{flushAfter: echoFlushAfter})
	e.fieldAnswer(f)
	_, paint := e.input([]byte("a\x1b\rb"))
	p := string(paint)
	if strings.Count(p, syncBegin) != 1 || strings.Count(p, cup(9, 0)+"\n") != 1 || e.offset != 1 || !strings.HasSuffix(p, cup(8, 1)+syncEnd) {
		t.Fatalf("one read:\n%q (offset %d)", p, e.offset)
	}
}

// Value: protects=bytes the decoder holds go out after echoIncompleteAfter
// in a real attach, where tick runs only when the echo's timer fires;
// fails_when=schedule stops arming the timer for decodeDue;
// why_new=TestEchoExpiresIncompleteSequences calls tick by hand and never
// waits on the timer; seam=none; no_claim=the length of the wait.
// The attach loop runs tick when the timer fires, so the timer must fire
// for what the decoder holds, even while a field is asked for.
func TestEchoTimerFiresForHeldBytes(t *testing.T) {
	e, now := fixedClockEcho(echoFlushAfter)
	e.probe()
	e.input([]byte("\x1b["))
	select {
	case <-e.timerC():
	case <-time.After(5 * time.Second):
		t.Fatal("the timer never fired for the held bytes")
	}
	if send, _ := e.tick(now.Add(echoIncompleteAfter)); string(send) != "\x1b[" {
		t.Fatalf("sent %q", send)
	}
}

// Value: protects=keys still batched and keys typed while a resend is owed
// are sent, in order, when the field is not found or the overlay opens;
// fails_when=fieldAnswer's not-found branch or suspend leaves the resend
// owed, so flush keeps returning nothing;
// why_new=TestEchoReattachSendsEachKeyOnce answers only with a found field
// and never suspends; seam=none; no_claim=resending keys sent on the lost
// connection, which no field can prove missing here.
// Keys held for a resend are not stranded when no field settles it.
func TestEchoReattachReleasesHeldKeysWithoutAField(t *testing.T) {
	owed := func() *echo {
		e, now := fixedClockEcho(echoFlushAfter)
		e.fieldAnswer(claudeField([]string{""}))
		typeKeys(e, "ab")
		e.tick(now.Add(time.Second)) // "ab" sent
		typeKeys(e, "c")             // batched
		e.reattached()
		if send, _ := e.input([]byte("d")); send != nil {
			t.Fatalf("typed while the resend is owed: send %q", send)
		}
		return e
	}

	// The agent's field is gone (another screen took over).
	e := owed()
	if _, reqs := e.tick(e.now()); len(reqs) == 0 || reqs[0].AttachField == nil {
		t.Fatalf("not asked: %v", reqs)
	}
	if send, paint := e.fieldAnswer(&protocol.Field{}); string(send) != "cd" || paint != nil {
		t.Fatalf("no field: send %q, paint %q", send, paint)
	}
	if send, _ := e.input([]byte("e")); string(send) != "e" {
		t.Fatalf("after the answer: send %q", send)
	}

	// The overlay opens before the field answers.
	e = owed()
	if send := e.suspend(); string(send) != "cd" {
		t.Fatalf("suspend sent %q", send)
	}
	// Afterwards keys are batched and go as usual.
	e.input([]byte("e"))
	if send, _ := e.tick(e.now().Add(time.Second)); string(send) != "e" {
		t.Fatalf("after the overlay: send %q", send)
	}
}

// A field can only show keys that had been sent when it was asked for. A
// batched key that happens to recreate what the field shows (an A, B, A
// edit) must not count as drawn, or the lost keys before it are not sent
// again and the batched one doubles.
func TestEchoResendNeverMatchesUnsentState(t *testing.T) {
	e, now := fixedClockEcho(echoFlushAfter)
	e.fieldAnswer(claudeField([]string{""}))
	typeKeys(e, "ab")
	e.input([]byte{0x7f})
	e.tick(now.Add(time.Second)) // "ab", Backspace sent; the Backspace is lost
	typeKeys(e, "b")             // batched
	e.reattached()
	e.tick(now.Add(2 * time.Second))
	if send, _ := e.fieldAnswer(claudeField([]string{"ab"})); string(send) != "\x7fb" {
		t.Fatalf("resend %q, want %q", send, "\x7fb")
	}
}

// A second reattach while a resend is owed keeps it owed, so keys held for
// it are not stranded when a resize has dropped the predictions.
func TestEchoSecondReattachKeepsTheResend(t *testing.T) {
	e, now := fixedClockEcho(echoFlushAfter)
	e.fieldAnswer(claudeField([]string{""}))
	typeKeys(e, "ab")
	e.tick(now.Add(time.Second))
	e.reattached()
	e.resized()
	e.input([]byte("c\r"))
	e.reattached()
	var sent []byte
	s, _ := e.tick(now.Add(time.Second))
	sent = append(sent, s...)
	s, _ = e.fieldAnswer(claudeField([]string{""}))
	sent = append(sent, s...)
	// "ab" went on the lost connection and the resize took the evidence
	// of which landed: they are not sent again on a guess.
	if string(sent) != "c\r" {
		t.Fatalf("sent %q, pending %q", sent, e.pending)
	}

	// Typed keys held across two reattaches go after the missing ones.
	e, now = fixedClockEcho(echoFlushAfter)
	e.fieldAnswer(claudeField([]string{""}))
	typeKeys(e, "ab")
	e.tick(now.Add(time.Second))
	e.reattached()
	e.input([]byte("d"))
	e.reattached()
	e.probe()
	if send, _ := e.fieldAnswer(claudeField([]string{"a"})); string(send) != "bd" {
		t.Fatalf("sent %q", send)
	}
}

// Keys held for a resend are bounded: past echoHeldMax the resend is given
// up and everything goes, in order.
func TestEchoHeldKeysAreBounded(t *testing.T) {
	e, now := fixedClockEcho(echoFlushAfter)
	e.fieldAnswer(claudeField([]string{""}))
	typeKeys(e, "a")
	e.tick(now.Add(time.Second))
	e.reattached()
	in := string(pasteStart) + strings.Repeat("x", 4*echoHeldMax) + string(pasteEnd)
	var sent []byte
	for _, chunk := range chunked(in, 4096) {
		s, _ := e.input([]byte(chunk))
		sent = append(sent, s...)
		if len(e.pending) > echoHeldMax+4096 {
			t.Fatalf("holding %d bytes", len(e.pending))
		}
	}
	s, _ := e.tick(now.Add(2 * time.Second))
	sent = append(sent, s...)
	if string(sent) != in || e.resend {
		t.Fatalf("sent %d of %d bytes, resend owed %v", len(sent), len(in), e.resend)
	}
}

// The overlay opening in a paste (a hotkey byte in pasted text) takes the
// paste the decoder holds with it and leaves the decoder empty, so keys
// typed after it are keys.
func TestEchoSuspendInAPasteResetsTheDecoder(t *testing.T) {
	e, _ := fixedClockEcho(echoFlushAfter)
	e.fieldAnswer(claudeField([]string{""}))
	e.input([]byte("\x1b[200~abc"))
	if send := e.suspend(); string(send) != "\x1b[200~abc" || e.keys.held() {
		t.Fatalf("suspend sent %q, decoder holding %v", send, e.keys.held())
	}
	e.probe()
	e.fieldAnswer(claudeField([]string{""}))
	if send, _ := e.input([]byte("\r")); string(send) != "\r" {
		t.Fatalf("Enter after the overlay: sent %q", send)
	}
}

// A new session size from a size ownership note (another attachment took
// the size, or the overlay's Take over size) makes a field asked for before
// it stale, as SIGWINCH does; a note that keeps the size does not.
func TestEchoSizeOwnerResizes(t *testing.T) {
	note := func(cols, rows uint16) frame {
		return frame{resp: &protocol.Response{AttachSizeOwner: &protocol.SizeOwnership{Geometry: protocol.Geometry{Cols: cols, Rows: rows}}}}
	}
	e := newEcho(echoConfig{flushAfter: echoFlushAfter})
	size := &protocol.SizeOwnership{Geometry: protocol.Geometry{Cols: 120, Rows: 40}}
	e.probe()
	handleFrame(note(120, 40), false, size, e, nil)
	e.fieldAnswer(claudeField([]string{""}))
	if e.field == nil {
		t.Fatal("a note that kept the size made the field stale")
	}
	e.probe()
	handleFrame(note(100, 30), false, size, e, nil)
	e.fieldAnswer(claudeField([]string{""}))
	if e.field != nil || size.Geometry.Cols != 100 {
		t.Fatalf("a new size: field %v, size %+v", e.field != nil, size.Geometry)
	}
}

// The echo's other transitions: predictions batched or sent, meeting a
// field that matches none, the overlay, a resize; a resend meeting a stale
// field; a long paste cut short.
func TestEchoStateCells(t *testing.T) {
	typed := func(sent bool) (*echo, time.Time) {
		e, now := fixedClockEcho(echoFlushAfter)
		e.fieldAnswer(claudeField([]string{""}))
		typeKeys(e, "hel")
		if sent {
			e.tick(now.Add(time.Second)) // flushed, and the field asked for
		}
		return e, now
	}
	t.Run("batched, field matches none", func(t *testing.T) {
		e, now := typed(false)
		e.fieldAnswer(claudeField([]string{"hex"}))
		if send, _ := e.tick(now.Add(time.Second)); string(send) != "hel" {
			t.Fatalf("sent %q", send)
		}
	})
	t.Run("batched, overlay", func(t *testing.T) {
		e, _ := typed(false)
		if send := e.suspend(); string(send) != "hel" || e.holding() || e.painted {
			t.Fatalf("suspend sent %q", send)
		}
	})
	t.Run("batched, resize", func(t *testing.T) {
		e, now := typed(false)
		e.resized()
		send, reqs := e.tick(now.Add(time.Second))
		if string(send) != "hel" || len(reqs) == 0 || reqs[0].AttachField == nil {
			t.Fatalf("send %q reqs %v", send, reqs)
		}
	})
	t.Run("sent, field matches none", func(t *testing.T) {
		e, _ := typed(true)
		if send, _ := e.fieldAnswer(claudeField([]string{"hex"})); send != nil || !e.dirty || e.holding() {
			t.Fatalf("send %q dirty %v", send, e.dirty)
		}
	})
	t.Run("sent, overlay", func(t *testing.T) {
		e, _ := typed(true)
		if send := e.suspend(); send != nil || e.holding() {
			t.Fatalf("suspend sent %q", send)
		}
	})
	t.Run("sent, resize", func(t *testing.T) {
		e, now := typed(true)
		e.resized()
		e.fieldAnswer(claudeField([]string{"hel"})) // asked before the resize
		send, reqs := e.tick(now.Add(2 * time.Second))
		if send != nil || len(reqs) == 0 || reqs[0].AttachField == nil {
			t.Fatalf("send %q reqs %v", send, reqs)
		}
	})
	t.Run("resend owed, stale field", func(t *testing.T) {
		e, _ := typed(true)
		e.reattached()
		e.probe()
		e.input([]byte("d"))
		e.resized()
		if send, _ := e.fieldAnswer(claudeField([]string{""})); send != nil || !e.resend {
			t.Fatalf("stale answer sent %q, resend owed %v", send, e.resend)
		}
	})
	t.Run("long paste cut short by the overlay", func(t *testing.T) {
		e, _ := fixedClockEcho(echoFlushAfter)
		in := string(pasteStart) + strings.Repeat("x", pasteMax+10)
		var sent []byte
		for _, chunk := range chunked(in, 4096) {
			s, _ := e.input([]byte(chunk))
			sent = append(sent, s...)
		}
		sent = append(sent, e.suspend()...)
		if string(sent) != in || e.keys.held() {
			t.Fatalf("sent %d of %d, decoder holding %v", len(sent), len(in), e.keys.held())
		}
	})
}

// A long paste whose end never comes goes in full once typing stops: the
// echo's expiry is armed while the decoder is still in the paste.
func TestEchoReleasesAnUnendedPaste(t *testing.T) {
	e, now := fixedClockEcho(echoFlushAfter)
	in := string(pasteStart) + strings.Repeat("x", pasteMax+10)
	var sent []byte
	for _, chunk := range chunked(in, 4096) {
		s, _ := e.input([]byte(chunk))
		sent = append(sent, s...)
	}
	for i := 1; i <= 2; i++ {
		s, _ := e.tick(now.Add(time.Duration(i) * echoIncompleteAfter))
		sent = append(sent, s...)
	}
	if string(sent) != in {
		t.Fatalf("sent %d of %d bytes", len(sent), len(in))
	}
}

// Some terminals (Ghostty) report the focus whenever focus reporting is
// turned on, which a repaint of a focus-reporting agent's snapshot does
// (twice for a fullscreen one). A report repeating the last one just after
// a repaint stays here; a change goes at any time, in any terminal, and so
// does a repeat away from a repaint (a new program turning reporting on).
func TestEchoDropsTheFocusReportsARepaintCauses(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	e := newEcho(echoConfig{flushAfter: echoFlushAfter})
	e.now = func() time.Time { return now }
	e.fieldAnswer(claudeField([]string{""}))
	send := func(in string) string {
		out, _ := e.input([]byte(in))
		return string(out)
	}
	if got := send("\x1b[I"); got != "\x1b[I" {
		t.Fatalf("first report: %q", got)
	}
	e.repainted()
	if got := send("\x1b[I") + send("\x1b[I"); got != "" {
		t.Fatalf("the repaint's answers went on: %q", got)
	}
	if got := send("\x1b[O"); got != "\x1b[O" {
		t.Fatalf("switching away right after a repaint: %q", got)
	}
	now = now.Add(echoFocusWindow)
	if got := send("\x1b[O"); got != "\x1b[O" {
		t.Fatalf("a repeat away from a repaint: %q", got)
	}
	// A change while keys are batched goes behind them, without a pause.
	typeKeys(e, "ab")
	if out, _ := e.input([]byte("\x1b[I")); out != nil || e.paused || e.unconfirmed() != 2 {
		t.Fatalf("focus in while typing: send %q, paused %v", out, e.paused)
	}
	if out := e.flush(); string(out) != "ab\x1b[I" {
		t.Fatalf("flushed %q", out)
	}
	// After a reattach the last report may never have arrived: the next
	// one goes even inside the reattach repaint's window, behind the keys
	// the field proves missing.
	e.reattached()
	e.repainted()
	if got := send("\x1b[I"); got != "" {
		t.Fatalf("a report overtook the resend: %q", got)
	}
	e.probe()
	if out, _ := e.fieldAnswer(claudeField([]string{""})); string(out) != "ab\x1b[I" {
		t.Fatalf("resent %q", out)
	}
	// With no resend owed, it goes at once.
	e.probe()
	e.fieldAnswer(claudeField([]string{"ab"}))
	e.reattached()
	e.repainted()
	if got := send("\x1b[I"); got != "\x1b[I" {
		t.Fatalf("first report after a reattach: %q", got)
	}
	var none *echo
	none.repainted() // no echo: nothing to note
}
