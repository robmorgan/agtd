package worker

import (
	"bufio"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.mitchellh.com/libghostty"

	"agtd.sh/internal/protocol"
	"agtd.sh/internal/session"
)

// Screens drawn the way the agents draw theirs (see the measurements in
// docs/designs/smooth-remote-attach.md), on a 120x40 terminal. Rows in the
// helpers are 1-based, as CUP counts them.

const rule = "────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────"

func at(row, col int, text string) string { return fmt.Sprintf("\x1b[%d;%dH%s", row, col, text) }

// claudeScreen is Claude Code at idle: the alternate screen, a rule, the
// prompt row with a faint placeholder, a rule, two status rows, and the
// cursor after the glyph.
func claudeScreen(placeholder bool) string {
	s := "\x1b[?1049h" + at(36, 1, rule) + at(37, 1, "❯ ")
	if placeholder {
		s += "\x1b[2mTry \"fix typecheck errors\"\x1b[0m"
	}
	return s + at(38, 1, rule) + at(39, 1, "  🤖 model | 📁 dir") + at(40, 1, "  ⏵⏵ auto mode on") + at(37, 3, "")
}

func detect(t *testing.T, ts *terminalState, vt string, prof fieldProfile) *protocol.Field {
	t.Helper()
	ts.feed([]byte(vt))
	f, err := detectField(ts.term, prof, false)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestFieldClaudeIdle(t *testing.T) {
	ts := newTestTerminal(t, 120, 40)
	f := detect(t, ts, claudeScreen(true), fieldProfileFor("claude-code", "", ""))
	want := &protocol.Field{Found: true, Cols: 120, Lines: 40, Alternate: true, Row: 36, Col: 2, Top: 36, BoxTop: 35, BoxBottom: 37, TextCol: 2, Width: 116,
		GrowsUp: true, Rows: []string{""}, Style: []byte("\x1b[0m")}
	f.Chrome = nil
	if !reflect.DeepEqual(f, want) {
		t.Fatalf("field\n got %+v\nwant %+v", f, want)
	}
}

func TestFieldChromeCoversTheBoxAndTheRowsItGrowsInto(t *testing.T) {
	ts := newTestTerminal(t, 120, 40)
	f := detect(t, ts, claudeScreen(true), fieldProfileFor("claude-code", "", ""))
	// The rule above is row 35 (0-based); the box grows up, so the
	// chrome runs 16 rows above it, down to the rule below.
	if len(f.Chrome) == 0 || f.Chrome[0].Index != 35-fieldChromeRows || f.Chrome[len(f.Chrome)-1].Index != 37 {
		t.Fatalf("chrome rows %d..%d", f.Chrome[0].Index, f.Chrome[len(f.Chrome)-1].Index)
	}
	for _, r := range f.Chrome {
		switch r.Index {
		case 35, 37:
			if !strings.Contains(string(r.VT), "───") {
				t.Fatalf("row %d is not the rule: %q", r.Index, r.VT)
			}
		case 36:
			if !strings.HasPrefix(string(r.VT), "❯ ") || !strings.Contains(string(r.VT), "\x1b[2m") {
				t.Fatalf("row 36 lost the glyph or the faint placeholder: %q", r.VT)
			}
		}
		if strings.Contains(string(r.VT), "\r\n") || strings.Contains(string(r.VT), "\x1b[?") {
			t.Fatalf("row %d carries more than its cells: %q", r.Index, r.VT)
		}
	}
}

func TestFieldClaudeTyped(t *testing.T) {
	ts := newTestTerminal(t, 120, 40)
	// Two rows of text after a wrap: the box took the row above, so the
	// rule moved up; the cursor is at the end of the second row.
	vt := "\x1b[?1049h" + at(35, 1, rule) + at(36, 1, "❯ the quick brown fox") + at(37, 1, "  jumps over") +
		at(38, 1, rule) + at(39, 1, "  status") + at(37, 13, "")
	f := detect(t, ts, vt, fieldProfileFor("claude-code", "", ""))
	if !f.Found || f.Row != 36 || f.Col != 12 || f.Top != 35 || f.TextCol != 2 || !reflect.DeepEqual(f.Rows, []string{"the quick brown fox", "jumps over"}) {
		t.Fatalf("field %+v", f)
	}
}

func TestFieldKeepsTextAfterTheCursorButNotASuggestion(t *testing.T) {
	ts := newTestTerminal(t, 120, 40)
	// The cursor in the middle of typed text: all of it is text.
	f := detect(t, ts, "\x1b[?1049h"+at(36, 1, "› hello world")+at(36, 8, ""), fieldProfileFor("", "codex", ""))
	if !f.Found || f.Col != 7 || !reflect.DeepEqual(f.Rows, []string{"hello world"}) || f.Width != 117 {
		t.Fatalf("field %+v", f)
	}
	// Faint text after the cursor is the program's (a shell's
	// autosuggestion, a placeholder), not the user's.
	ts = newTestTerminal(t, 120, 40)
	f = detect(t, ts, at(1, 1, "$ git \x1b[2mstatus\x1b[0m")+at(1, 7, ""), defaultFieldProfile)
	if !f.Found || f.TextCol != 2 || !reflect.DeepEqual(f.Rows, []string{"git "}) {
		t.Fatalf("field %+v", f)
	}
}

// Spaces typed before the cursor are text, though they look like blanks:
// the cursor's row keeps them. Blanks after the cursor are not.
func TestFieldKeepsSpacesBeforeTheCursor(t *testing.T) {
	ts := newTestTerminal(t, 120, 40)
	f := detect(t, ts, "\x1b[?1049h"+at(36, 1, "❯ hello  ")+at(36, 10, ""), fieldProfileFor("claude", "", ""))
	if !f.Found || f.Col != 9 || !reflect.DeepEqual(f.Rows, []string{"hello  "}) {
		t.Fatalf("field %+v", f)
	}
	// On a continuation row too, and an empty field pads nothing.
	ts = newTestTerminal(t, 120, 40)
	f = detect(t, ts, "\x1b[?1049h"+at(35, 1, "❯ one")+at(36, 1, "  two ")+at(36, 7, ""), fieldProfileFor("claude", "", ""))
	if !f.Found || !reflect.DeepEqual(f.Rows, []string{"one", "two "}) {
		t.Fatalf("continuation: %+v", f)
	}
	ts = newTestTerminal(t, 120, 40)
	if f = detect(t, ts, claudeScreen(true), fieldProfileFor("claude", "", "")); !reflect.DeepEqual(f.Rows, []string{""}) {
		t.Fatalf("empty: %+v", f)
	}
}

func TestFieldPiIdleAndTyped(t *testing.T) {
	ts := newTestTerminal(t, 120, 40)
	// pi draws on the primary screen between two coloured rules, hides
	// the cursor and paints its own as one inverse cell.
	vt := "\x1b[?25l" + at(28, 1, "\x1b[38;2;178;148;187m"+rule+"\x1b[0m") + at(29, 1, "\x1b[7m \x1b[27m") +
		at(30, 1, "\x1b[38;2;178;148;187m"+rule+"\x1b[0m") + at(31, 1, "\x1b[38;2;102;102;102m/tmp/x (main)\x1b[0m") + at(32, 1, "\x1b[38;2;102;102;102m$0.000\x1b[0m")
	f := detect(t, ts, vt, fieldProfileFor("", "pi", "/usr/bin/pi"))
	want := &protocol.Field{Found: true, Cols: 120, Lines: 40, Row: 28, Col: 0, Top: 28, BoxTop: 27, BoxBottom: 29, TextCol: 0, Width: 119,
		InverseCursor: true, WrapKeepsSpace: true, Rows: []string{""}, Style: []byte("\x1b[0m")}
	chrome := f.Chrome
	f.Chrome = nil
	if !reflect.DeepEqual(f, want) {
		t.Fatalf("field\n got %+v\nwant %+v", f, want)
	}
	// The box grows down, so the chrome runs from the rule above to 16
	// rows under the rule below: the status rows move with it.
	if chrome[0].Index != 27 || chrome[len(chrome)-1].Index != min(29+fieldChromeRows, 39) {
		t.Fatalf("chrome rows %d..%d", chrome[0].Index, chrome[len(chrome)-1].Index)
	}

	// Typed text that wrapped: the box grew down and the cursor sits
	// after the text on the second row.
	ts = newTestTerminal(t, 120, 40)
	vt = "\x1b[?25l" + at(28, 1, rule) + at(29, 1, "the quick brown fox") + at(30, 1, "jumps\x1b[7m \x1b[27m") + at(31, 1, rule) + at(32, 1, "status")
	f = detect(t, ts, vt, fieldProfileFor("", "pi", ""))
	if !f.Found || f.Row != 29 || f.Col != 5 || f.TextCol != 0 || !reflect.DeepEqual(f.Rows, []string{"the quick brown fox", "jumps"}) {
		t.Fatalf("field %+v", f)
	}
}

// pi draws inline from the top of the primary screen, so on a tall
// terminal its box sits in the top rows at first; its painted cursor is
// found there because rules border the row.
func TestFieldPiBoxNearTheTop(t *testing.T) {
	ts := newTestTerminal(t, 169, 68)
	wide := strings.Repeat("─", 169)
	vt := "\x1b[?25l" + at(1, 1, " pi v0.73.1") + at(9, 1, wide) + at(10, 1, " Update Available") + at(13, 1, wide) +
		at(15, 1, wide) + at(16, 1, "\x1b[7m \x1b[27m") + at(17, 1, wide) + at(18, 1, "~") + at(19, 1, "0.0%/1.0M (auto)")
	f := detect(t, ts, vt, fieldProfileFor("", "pi", ""))
	if !f.Found || f.Row != 15 || f.Col != 0 || f.BoxTop != 14 || f.BoxBottom != 16 || !f.InverseCursor || f.Width != 168 {
		t.Fatalf("field %+v", f)
	}
	// An inverse cell in the top half with no rule beside it is not a
	// cursor (a highlighted character in a transcript).
	ts = newTestTerminal(t, 169, 68)
	if f = detect(t, ts, "\x1b[?25l"+at(5, 1, "see \x1b[7mthis\x1b[27m word")+at(6, 1, "x\x1b[7m \x1b[27m"), defaultFieldProfile); f.Found {
		t.Fatalf("found a field in a highlight: %+v", f)
	}
}

func TestFieldEmptyLineInsideTheBox(t *testing.T) {
	ts := newTestTerminal(t, 120, 40)
	vt := "\x1b[?1049h" + at(34, 1, rule) + at(35, 1, "❯ one") + at(37, 1, "  three") + at(38, 1, rule) + at(37, 8, "")
	f := detect(t, ts, vt, fieldProfileFor("claude", "", ""))
	if !f.Found || !reflect.DeepEqual(f.Rows, []string{"one", "", "three"}) || f.Row != 36 || f.Top != 34 || f.Col != 7 {
		t.Fatalf("field %+v", f)
	}
}

func TestFieldShellPrompt(t *testing.T) {
	ts := newTestTerminal(t, 120, 40)
	f := detect(t, ts, at(5, 1, "$ ")+at(5, 3, ""), defaultFieldProfile)
	if !f.Found || f.TextCol != 2 || f.Width != 117 || f.GrowsUp || !reflect.DeepEqual(f.Rows, []string{""}) {
		t.Fatalf("field %+v", f)
	}
	// A prompt that is not a glyph is part of the row's text, and the
	// shell wraps at column 0.
	ts = newTestTerminal(t, 120, 40)
	f = detect(t, ts, at(5, 1, "demo:/w$ ls")+at(5, 12, ""), defaultFieldProfile)
	if !f.Found || f.TextCol != 0 || f.Width != 119 || !reflect.DeepEqual(f.Rows, []string{"demo:/w$ ls"}) {
		t.Fatalf("field %+v", f)
	}
}

func TestFieldNotFound(t *testing.T) {
	for name, vt := range map[string]string{
		// A menu: the cursor hidden and a whole row highlighted.
		"menu": "\x1b[?25l" + at(9, 1, "\x1b[7m› 1. Trust and continue"+strings.Repeat(" ", 95)+"\x1b[27m") + at(10, 1, "  2. Quit"),
		// The cursor hidden and nothing drawn as one.
		"hidden cursor": "\x1b[?25l" + at(14, 1, "   ❯ Keep xhigh") + at(15, 1, "     Switch"),
		// The cursor on a rule.
		"cursor on a rule": at(36, 1, rule) + at(36, 5, ""),
		// Text before the cursor's column on what would be a
		// continuation row: not a field of ours.
		"cursor left of the text": at(36, 1, "❯ hello") + at(36, 2, ""),
	} {
		t.Run(name, func(t *testing.T) {
			ts := newTestTerminal(t, 120, 40)
			if f := detect(t, ts, vt, defaultFieldProfile); f.Found {
				t.Fatalf("found a field: %+v", f)
			}
		})
	}
	// Blocked (a dialog the program reported, or an editor in the
	// foreground): no field, whatever the screen.
	ts := newTestTerminal(t, 120, 40)
	ts.feed([]byte(claudeScreen(true)))
	f, err := detectField(ts.term, defaultFieldProfile, true)
	if err != nil || f.Found || f.Cols != 120 || !f.Alternate {
		t.Fatalf("blocked: %+v, %v", f, err)
	}
}

func TestFieldStyleTakesTheRowsBackground(t *testing.T) {
	ts := newTestTerminal(t, 120, 40)
	f := detect(t, ts, at(5, 1, "\x1b[48;2;10;20;30m› \x1b[0m")+at(5, 1, "\x1b[48;2;10;20;30m›                 \x1b[0m")+at(5, 3, ""), defaultFieldProfile)
	if !f.Found || string(f.Style) != "\x1b[0m\x1b[48;2;10;20;30m" {
		t.Fatalf("style %q (found %v)", f.Style, f.Found)
	}
}

func TestFieldWideCharacters(t *testing.T) {
	ts := newTestTerminal(t, 120, 40)
	f := detect(t, ts, at(5, 1, "❯ wide 日本語 👍 end")+at(5, 21, ""), defaultFieldProfile)
	if !f.Found || f.Col != 20 || !reflect.DeepEqual(f.Rows, []string{"wide 日本語 👍 end"}) {
		t.Fatalf("field %+v", f)
	}
}

func TestFieldProfiles(t *testing.T) {
	for _, c := range []struct {
		app, agent, command string
		want                fieldProfile
	}{
		{"claude-code", "x", "/x", fieldProfile{spare: 2, records: true}},
		{"", "codex", "", fieldProfile{spare: 1, records: true}},
		{"", "mine", "/home/me/.local/bin/pi", fieldProfile{spare: 1, wrapKeepsSpace: true, records: true}},
		{"", "", "/bin/sh", defaultFieldProfile},
	} {
		if got := fieldProfileFor(c.app, c.agent, c.command); got != c.want {
			t.Errorf("profile(%q, %q, %q) = %+v, want %+v", c.app, c.agent, c.command, got, c.want)
		}
	}
}

// A formatted row drawn into another terminal at the same row reproduces
// the cells, styles included.
func TestFormatRowRoundTrips(t *testing.T) {
	ts := newTestTerminal(t, 40, 5)
	ts.feed([]byte(at(3, 1, "ab \x1b[1;31mred\x1b[0m  \x1b[4mu\x1b[0m 日本")))
	vt, err := formatRow(ts.term, 40, 2)
	if err != nil {
		t.Fatal(err)
	}
	other := newTestTerminal(t, 40, 5)
	other.feed([]byte(at(3, 1, "") + string(vt)))
	want, got := formatContent(ts.term, libghostty.FormatterFormatPlain, false), formatContent(other.term, libghostty.FormatterFormatPlain, false)
	if want != got {
		t.Fatalf("row text\n got %q\nwant %q\nvt %q", got, want, vt)
	}
	for _, sgr := range []string{"\x1b[1m", "\x1b[4m"} {
		if !strings.Contains(string(vt), sgr) {
			t.Fatalf("row lost %q: %q", sgr, vt)
		}
	}
}

func BenchmarkDetectField(b *testing.B) {
	ts := newTestTerminal(b, 200, 50)
	var screen strings.Builder
	screen.WriteString("\x1b[?1049h")
	for y := 1; y <= 44; y++ {
		fmt.Fprintf(&screen, "\x1b[%d;1H\x1b[3%dmrow %d of a transcript with some text in it\x1b[0m", y, y%8, y)
	}
	screen.WriteString(at(46, 1, strings.Repeat("─", 200)) + at(47, 1, "❯ some typed text") + at(48, 1, strings.Repeat("─", 200)) + at(47, 19, ""))
	ts.feed([]byte(screen.String()))
	b.ResetTimer()
	for b.Loop() {
		f, err := detectField(ts.term, defaultFieldProfile, false)
		if err != nil || !f.Found {
			b.Fatalf("field %+v, %v", f, err)
		}
	}
}

// A client that asked for attach-field can ask where the input field is;
// the answer describes the screen as the output before it left it.
func TestAttachFieldRequest(t *testing.T) {
	h := startWorkerWith(t, `printf '$ '; while read line; do printf 'got:%s\n$ ' "$line"; done`)
	c := &client{t: t, conn: h.dial()}
	c.r = bufio.NewReader(c.conn)
	t.Cleanup(func() { c.conn.Close() })
	c.send(&protocol.Request{AttachSession: &protocol.AttachSession{
		SessionID: h.sessionID, Kind: session.AttachmentAttach, Geometry: protocol.Geometry{Cols: 80, Rows: 24},
		Features: []string{protocol.CapAttachField},
	}})
	resp := c.read()
	if resp.Attached == nil || !slices.Contains(resp.Attached.Features, protocol.CapAttachField) {
		t.Fatalf("attach: %#v", resp)
	}
	// The prompt was drawn before the attach: it is in the snapshot.
	c.output.Write(resp.Attached.Snapshot)
	c.expectOutput("$ ")

	field := func() *protocol.Field {
		t.Helper()
		c.send(&protocol.Request{AttachField: protocol.Empty})
		for {
			resp := c.read()
			switch {
			case resp.PtyOutput != nil:
				c.output.Write(resp.PtyOutput.Data)
			case resp.Field != nil:
				return resp.Field
			default:
				t.Fatalf("waiting for the field: %#v", resp)
			}
		}
	}
	f := field()
	if !f.Found || f.TextCol != 2 || f.Col != 2 || f.Width != 77 || len(f.Rows) != 1 || f.Rows[0] != "" || f.GrowsUp || f.Cols != 80 {
		t.Fatalf("idle field: %+v", f)
	}
	// Typed text, echoed by the PTY, is in the field: the answer waits
	// for the echo and comes after it on the stream, so asking right
	// after typing sees the keys drawn.
	c.input("abc")
	if f = field(); !f.Found || f.Col != 5 || len(f.Rows) != 1 || f.Rows[0] != "abc" {
		t.Fatalf("typed field: %+v", f)
	}
	if !strings.Contains(c.output.String(), "abc") {
		t.Fatalf("the echo did not precede the field: output %q", c.output.String())
	}
	// After Enter the line has gone to the program and the field is
	// empty again, at the next prompt.
	c.input("\r")
	c.expectOutput("got:abc")
	c.expectOutput("got:abc\r\n$ ")
	if f = field(); !f.Found || f.Col != 2 || f.Rows[0] != "" {
		t.Fatalf("field after enter: %+v", f)
	}
}

// Without the feature, the request is unexpected and ends the attachment.
func TestAttachFieldNeedsTheFeature(t *testing.T) {
	h := startWorker(t)
	c := h.attach(protocol.Geometry{Cols: 80, Rows: 24})
	c.send(&protocol.Request{AttachField: protocol.Empty})
	if resp := c.expectEnd(); resp.Error == nil {
		t.Fatalf("expected an error, got %#v", resp)
	}
}
