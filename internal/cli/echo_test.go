package cli

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/robmorgan/agtd/internal/protocol"
)

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
	if paint := e.snapshotAnswer([]byte("SNAP")); string(paint) != syncBegin+attachClearSequence+"SNAP"+syncEnd {
		t.Fatalf("repaint %q", paint)
	}
	if e.dropped != 0 || e.painted {
		t.Fatalf("dropped %d, painted %v after the repaint", e.dropped, e.painted)
	}
}

func TestEchoUnpredictedKeysGoAtOnceAndPause(t *testing.T) {
	e := newEcho(echoConfig{})
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
	// The next field resumes the echo.
	e.fieldAnswer(claudeField([]string{""}))
	if send, paint := typeKeys(e, "d"); send != nil || paint == nil {
		t.Fatalf("resumed: send %q, paint %q", send, paint)
	}
}

// Shift-Enter (kitty form) and Alt-Enter are newlines on every agent;
// Ctrl-Enter and Ctrl-J are not the same everywhere, so they go raw.
func TestEchoNewlineKeys(t *testing.T) {
	e := newEcho(echoConfig{})
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
		e.fieldAnswer(claudeField([]string{""}))
		// Unpredicted: it goes at once, after whatever was still batched.
		if send, _ := e.input([]byte(key)); !bytes.HasSuffix(send, []byte(key)) || !e.paused {
			t.Fatalf("%q: send %q, paused %v", key, send, e.paused)
		}
	}
}

func TestEchoAdoptsWhatTheAgentDrew(t *testing.T) {
	e := newEcho(echoConfig{})
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
	e.fieldAnswer(claudeField([]string{"hexy"}))
	if e.unconfirmed() != 1 || e.states[0].rows[0] != "hexyz" {
		t.Fatalf("replay: unconfirmed %d, states %+v", e.unconfirmed(), e.states)
	}
}

// A confirmation that lands after a typed space must keep the space: the
// worker reports blanks before the cursor as text, and so does the echo.
func TestEchoKeepsTypedSpacesAcrossConfirmation(t *testing.T) {
	e := newEcho(echoConfig{})
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
	e := newEcho(echoConfig{})
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
	e := newEcho(echoConfig{})
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
	e := newEcho(echoConfig{})
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
	e := newEcho(echoConfig{})
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
