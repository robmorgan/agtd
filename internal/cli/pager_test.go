package cli

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"agtd.sh/internal/protocol"
)

// Value: protects=the pager never writes a session's bytes raw, lines up tabs as a terminal would, and colours a diff as `agt diff` does; fails_when=controls reach the screen, tabs are escaped, or diff lines lose their colours; why_new=phase 2 added the pager; seam=none; no_claim=drawing on a real terminal
// A page escapes controls, expands tabs and colours diff lines.
func TestNewPage(t *testing.T) {
	p := newPage("history of a", []byte("a\tb\r\nwide世\tx\n\x1b[2Jevil\n"), false)
	want := []string{"a       b", "wide世  x", `\u{1b}[2Jevil`}
	if !slices.Equal(p.lines, want) {
		t.Fatalf("lines = %q", p.lines)
	}
	d := newPage("diff of a", []byte(sampleDiff), true)
	if d.styles[0] != ansiEmphasis || d.styles[4] != "\x1b[36m" || d.styles[5] != "" || d.styles[6] != "\x1b[31m" || d.styles[7] != "\x1b[32m" {
		t.Fatalf("styles = %q", d.styles)
	}
}

// Value: protects=what the pager holds is bounded; fails_when=pageBuffer keeps writing past its limit or does not stop the stream; why_new=phase 2 added the pager; seam=none; no_claim=the stream itself (TestInboxLoadPage)
func TestPageBuffer(t *testing.T) {
	b := &pageBuffer{limit: 5}
	if n, err := b.Write([]byte("abc")); n != 3 || err != nil {
		t.Fatalf("first write: %d %v", n, err)
	}
	if n, err := b.Write([]byte("defg")); n != 2 || !errors.Is(err, errPageFull) || string(b.buf) != "abcde" || !b.truncated {
		t.Fatalf("second write: %d %v %q", n, err, b.buf)
	}
}

// Value: protects=the pager fills the screen exactly, scrolls within its page, ends with its note, and drops colour under NO_COLOR; fails_when=it draws more rows than the terminal has, scrolls past the end, or keeps colour off; why_new=phase 2 added the pager; seam=none; no_claim=the alternate screen switch (runInbox)
// The pager draws a title, the lines that fit and its keys, and moves
// through the page with the usual keys.
func TestInboxPager(t *testing.T) {
	var text strings.Builder
	for n := range 50 {
		text.WriteString(strings.Repeat("x", n) + "\n")
	}
	i := testInbox(t, hostWith(localHostName, withUID(demo("a"), "1")))
	i.mode, i.page = inboxPager, newPage("history of a", []byte(text.String()), false)
	i.page.note = "only the first 4.0 MiB are shown"
	lines := i.renderPage(40, 12)
	if len(lines) != 12 {
		t.Fatalf("%d lines for 12 rows", len(lines))
	}
	plain := make([]string, len(lines))
	for n, l := range lines {
		plain[n] = stripANSI(l)
		if w := cellWidth(plain[n]); w != 39 {
			t.Fatalf("line %d is %d cells: %q", n, w, plain[n])
		}
	}
	if !strings.HasPrefix(plain[0], " history of a  lines 1–10 of 50") || plain[1] != strings.Repeat(" ", 39) || !strings.HasPrefix(plain[11], " ↑↓ scroll") {
		t.Fatalf("screen:\n%s", strings.Join(plain, "\n"))
	}
	for _, tc := range []struct {
		ev  keyEvent
		top int
	}{
		{key(keyDown), 1}, {char('j'), 2}, {key(keyUp), 1}, {char(' '), 10}, {char('b'), 1},
		{key(keyUp), 0}, {key(keyUp), 0}, {char('G'), 41}, {key(keyDown), 41}, {char('g'), 0},
	} {
		i.handleKey(tc.ev)
		i.renderPage(40, 12)
		if i.page.top != tc.top {
			t.Fatalf("after %+v: top %d, want %d", tc.ev, i.page.top, tc.top)
		}
	}
	i.handleKey(key(keyEnd))
	plain = strings.Split(stripANSI(strings.Join(i.renderPage(40, 12), "\n")), "\n")
	if !strings.HasPrefix(plain[10], "only the first 4.0 MiB") || !strings.Contains(plain[0], "lines 42–50 of 50") {
		t.Fatalf("the end:\n%s", strings.Join(plain, "\n"))
	}
	// A screen shorter than the frame still gets no more lines than it has.
	if n := len(i.renderPage(40, 1)); n != 1 {
		t.Fatalf("%d lines for 1 row", n)
	}
	// Without colour, a diff's colours go too.
	i.page = newPage("diff of a", []byte(sampleDiff), true)
	for _, l := range i.renderPage(80, 20) {
		if strings.Contains(l, "\x1b[3") {
			t.Fatalf("colour with colour off: %q", l)
		}
	}
	i.handleKey(key(keyEsc))
	if i.mode != inboxBrowse || i.page != nil {
		t.Fatalf("esc: %v", i.mode)
	}
}

// Value: protects=the inbox reads a diff over the artifact stream, refuses a replaced session, cuts a large one with a note, and falls back to GetHistory on an older daemon; fails_when=loadPage skips the UID check, buffers past its limit, or needs CapArtifacts for history; why_new=phase 2 added diff and history; seam=fakeRemote; no_claim=git itself
// Loading a page talks to the row's host.
func TestInboxLoadPage(t *testing.T) {
	f, c := newFakeRemote(t, protocol.Capabilities())
	p := hostWith("dev")
	p.host = c.host
	rec := withUID(demo("demo"), "d1")
	row := inboxRow{host: p, rec: &rec}
	a := &app{paths: c.paths}
	run := func(what rowAction) <-chan inboxResult {
		done := make(chan inboxResult, 1)
		go func() { done <- a.runInboxEffect(context.Background(), inboxEffect{load: &row, loadWhat: what}) }()
		return done
	}
	answerSession := func(uid string) {
		r := f.next()
		if r.req.GetSession == nil || r.req.GetSession.SessionID != "demo" {
			t.Fatalf("request = %#v, want GetSession", r.req)
		}
		s := withUID(demo("demo"), uid)
		r.answer(&protocol.Response{Session: &s})
	}

	done := run(rowDiff)
	answerSession("d1")
	r := f.next()
	if r.req.GetArtifact == nil || r.req.GetArtifact.Name != "diff" {
		t.Fatalf("request = %#v, want GetArtifact diff", r.req)
	}
	r.answer(&protocol.Response{ArtifactChunk: &protocol.Bytes{Data: []byte(sampleDiff[:30])}})
	r.answer(&protocol.Response{ArtifactChunk: &protocol.Bytes{Data: []byte(sampleDiff[30:])}})
	r.answer(&protocol.Response{EndOfStream: &struct{}{}})
	res := <-done
	if res.err != nil || res.page == nil || res.page.title != "diff of dev/demo" || len(res.page.lines) != 11 || res.page.note != "" || *res.load != (inboxLoad{key: row.key(), what: rowDiff}) {
		t.Fatalf("diff: %+v", res)
	}

	// The session was replaced under its name.
	done = run(rowDiff)
	answerSession("other")
	if res := <-done; res.err == nil || !strings.Contains(res.err.Error(), "is not the session you chose") {
		t.Fatalf("replaced: %+v", res)
	}

	// More than the pager holds: cut, with a note.
	done = run(rowHistory)
	answerSession("d1")
	r = f.next()
	if r.req.GetArtifact == nil || r.req.GetArtifact.Name != "history" {
		t.Fatalf("request = %#v, want GetArtifact history", r.req)
	}
	chunk := []byte(strings.Repeat("line\n", 64<<10/5))
	for sent := 0; sent <= inboxPageMax; sent += len(chunk) {
		r.answer(&protocol.Response{ArtifactChunk: &protocol.Bytes{Data: chunk}})
	}
	res = <-done
	if res.err != nil || res.page == nil || !strings.Contains(res.page.note, "`agt history dev/demo` prints all of it") {
		t.Fatalf("cut: %+v", res.err)
	}

	// An older daemon: the history comes in one response.
	older := slices.DeleteFunc(protocol.Capabilities(), func(c string) bool { return c == protocol.CapArtifacts })
	f, c = newFakeRemote(t, older)
	p.host = c.host
	a = &app{paths: c.paths}
	done = run(rowHistory)
	answerSession("d1")
	r = f.next()
	if r.req.GetHistory == nil || r.req.GetHistory.VT {
		t.Fatalf("request = %#v, want GetHistory", r.req)
	}
	r.answer(&protocol.Response{History: &protocol.History{Data: "$ make\nok\n"}})
	if res := <-done; res.err != nil || res.page == nil || !slices.Equal(res.page.lines, []string{"$ make", "ok"}) {
		t.Fatalf("older daemon: %+v", res)
	}
	// Nothing to show is no page.
	done = run(rowHistory)
	answerSession("d1")
	f.next().answer(&protocol.Response{History: &protocol.History{Data: "\n"}})
	if res := <-done; res.err != nil || res.page != nil {
		t.Fatalf("empty: %+v", res)
	}
}

// Value: protects=an artifact stream ends when its context does, over QUIC too, so a load never outlives the inbox; fails_when=streamArtifactCtx ignores ctx while a read blocks; why_new=the inbox cancels loads it no longer needs; seam=fakeRemote; no_claim=the Unix socket path (the same deadline)
func TestStreamArtifactCancels(t *testing.T) {
	f, c := newFakeRemote(t, protocol.Capabilities())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.streamArtifactCtx(ctx, "demo", "diff", &pageBuffer{limit: 1 << 10}) }()
	f.next() // the stream is open, and nothing comes
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream did not end with its context")
	}
}
