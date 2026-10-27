package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"agtd.sh/internal/protocol"
	"agtd.sh/internal/session"
	"agtd.sh/internal/transport"
)

// hostWith is an online probe of a host holding recs.
func hostWith(name string, recs ...session.Record) *hostProbe {
	p := &hostProbe{name: name, status: hostOnline, sessions: recs, latency: 38 * time.Millisecond}
	if name != localHostName {
		p.host = &transport.Host{Name: name, Address: name + ":7433"}
	}
	return p
}

// withUID gives a record a UID, as every current daemon reports, and a
// fixed update time, so rows only reorder when a test means them to.
func withUID(r session.Record, uid string) session.Record {
	r.UID = uid
	r.UpdatedAt = time.Unix(1_700_000_000, 0)
	return r
}

// testInbox is an inbox over hosts, drawn without colour.
func testInbox(t *testing.T, hosts ...*hostProbe) *inbox {
	t.Helper()
	i := newInbox(testClient(t).paths, hosts, "")
	i.theme = inboxTheme(&overlayTheme{})
	return i
}

// keys presses evs one at a time, as separate reads: the inbox is drawn
// after each, as runInbox does (unless it is hidden), and the user reads
// it before the next key (a confirmation has been up long enough; see
// TestInboxConfirmsOnlyWhatWasDrawn for the timing itself).
func (i *inbox) keys(t *testing.T, evs ...keyEvent) (bool, *inboxOutcome) {
	t.Helper()
	for n, ev := range evs {
		if done, out := i.handleKey(ev); done {
			if n != len(evs)-1 {
				t.Fatalf("done after key %d of %d", n+1, len(evs))
			}
			return done, out
		}
		if !i.hidden {
			i.render(100, 30)
			if !i.confirmShown.IsZero() {
				i.confirmShown = i.confirmShown.Add(-inboxAcknowledgeAfter)
			}
		}
	}
	return false, nil
}

func (i *inbox) typed(t *testing.T, s string) {
	t.Helper()
	for _, r := range s {
		i.keys(t, char(r))
	}
}

func (i *inbox) screen(cols, rows int) string {
	return stripANSI(strings.Join(i.render(cols, rows), "\n"))
}

func rowNames(rows []inboxRow) string {
	var out []string
	for _, r := range rows {
		out = append(out, r.address())
	}
	return strings.Join(out, " ")
}

// Sessions with the same name on two hosts are two rows; host/ narrows to
// one; Enter attaches to the selected row's host and incarnation.
func TestInboxDuplicateNamesAcrossHosts(t *testing.T) {
	i := testInbox(t, hostWith(localHostName, withUID(demo("demo"), "l1")), hostWith("dev", withUID(demo("demo"), "d1")))
	if got := rowNames(i.rows()); got != "demo dev/demo" {
		t.Fatalf("rows = %q", got)
	}
	i.typed(t, "dev/")
	if got := rowNames(i.rows()); got != "dev/demo" {
		t.Fatalf("dev/ rows = %q", got)
	}
	done, out := i.keys(t, key(keyEnter))
	if !done || out == nil || out.host == nil || out.host.Name != "dev" || out.id != "demo" || out.uid != "d1" {
		t.Fatalf("enter = %v %+v", done, out)
	}
	// A query that is not a host's name is plain text: "/tmp" matches cwds.
	i = testInbox(t, hostWith(localHostName, withUID(demo("demo"), "l1")))
	i.typed(t, "/tmp/de")
	if got := rowNames(i.rows()); got != "demo" {
		t.Fatalf("/tmp/de rows = %q", got)
	}
}

// The selection follows its session when a refresh reorders the rows,
// stays on a session that exits, and moves to the next row when its
// session goes.
func TestInboxSelectionFollowsRow(t *testing.T) {
	a, b, c := withUID(demo("a"), "1"), withUID(demo("b"), "2"), withUID(demo("c"), "3")
	a.UpdatedAt, b.UpdatedAt, c.UpdatedAt = time.Now(), time.Now().Add(-time.Minute), time.Now().Add(-2*time.Minute)
	i := testInbox(t, hostWith(localHostName, a, b, c))
	i.keys(t, key(keyDown))
	if i.selected.uid != "2" {
		t.Fatalf("selected %+v", i.selected)
	}
	// c starts asking for permission and sorts first.
	c.Attention = session.AttentionAction
	i.update(hostWith(localHostName, a, b, c))
	if got := rowNames(i.rows()); got != "c a b" || i.selection(i.rows()) != 2 {
		t.Fatalf("after reorder: %q, selected index %d", got, i.selection(i.rows()))
	}
	// b exits: still selected.
	b.Status = session.StatusExited
	i.update(hostWith(localHostName, a, b, c))
	if i.selected.uid != "2" {
		t.Fatalf("after exit: %+v", i.selected)
	}
	// b is removed: the row that takes its place is selected.
	i.update(hostWith(localHostName, a, c))
	if n := i.selection(i.rows()); n != 2 || !i.selected.create {
		t.Fatalf("after removal: index %d, %+v", n, i.selected)
	}
}

// n, r and ? are commands only while the filter is empty; a leading space
// lets a filter start with one of them, and once the filter has text
// every key is text.
func TestInboxLettersAndFilter(t *testing.T) {
	i := testInbox(t, hostWith(localHostName, withUID(demo("alpha"), "1"), withUID(demo("nano"), "2")))
	i.keys(t, char('?'))
	if !i.showKeys || i.query != "" {
		t.Fatalf("? = %v %q", i.showKeys, i.query)
	}
	i.keys(t, char('r'))
	if len(i.effects) != 1 || !i.effects[0].reprobeAll || i.query != "" {
		t.Fatalf("r = %+v %q", i.effects, i.query)
	}
	i.keys(t, char('n'))
	if i.mode != inboxCreateAgent || i.query != "" {
		t.Fatalf("n = %v %q", i.mode, i.query)
	}
	i.keys(t, key(keyEsc))
	i.typed(t, " nan")
	if i.query != " nan" || rowNames(i.rows()) != "nano" {
		t.Fatalf("leading space: %q %q", i.query, rowNames(i.rows()))
	}
	i.query = ""
	i.typed(t, "a n?r2")
	if i.query != "a n?r2" || i.mode != inboxBrowse {
		t.Fatalf("text: %q %v", i.query, i.mode)
	}
	i.keys(t, key(keyBackspace))
	if i.query != "a n?r" {
		t.Fatalf("backspace: %q", i.query)
	}
	i.query = ""
	i.handlePaste("alp")
	if rowNames(i.rows()) != "alpha" || i.selection(i.rows()) != 0 {
		t.Fatalf("paste: %q", rowNames(i.rows()))
	}
}

// Esc works innermost-out: the confirmation, the menu, the filter, then
// quit, with every session left running. The filter stays while a menu is
// open, and printable keys go nowhere then, except y and n on a confirm.
func TestInboxEscOrder(t *testing.T) {
	i := testInbox(t, hostWith(localHostName, withUID(demo("alpha"), "1")))
	i.typed(t, "alp")
	i.keys(t, key(keyTab))
	if i.mode != inboxActions {
		t.Fatalf("tab: %v", i.mode)
	}
	i.keys(t, char('x'))
	i.keys(t, char('2'))
	if i.mode != inboxConfirm || i.query != "alp" {
		t.Fatalf("2: %v %q", i.mode, i.query)
	}
	i.keys(t, key(keyEsc))
	if i.mode != inboxActions || i.modeIndex != 1 {
		t.Fatalf("esc from confirm: %v %d", i.mode, i.modeIndex)
	}
	i.keys(t, char('2'), char('n'))
	if i.mode != inboxActions {
		t.Fatalf("n on confirm: %v", i.mode)
	}
	i.keys(t, key(keyEsc))
	if i.mode != inboxBrowse || i.query != "alp" {
		t.Fatalf("esc from menu: %v %q", i.mode, i.query)
	}
	if done, _ := i.keys(t, key(keyEsc)); done || i.query != "" {
		t.Fatalf("esc clears the filter: %v %q", done, i.query)
	}
	if done, out := i.keys(t, key(keyEsc)); !done || out != nil {
		t.Fatalf("esc quits: %v %+v", done, out)
	}
	if len(i.effects) != 0 {
		t.Fatalf("quitting acted: %+v", i.effects)
	}
}

// Enter attaches to a running row and opens the actions of an ended one,
// whose only action is remove.
func TestInboxEnterAndActions(t *testing.T) {
	i := testInbox(t, hostWith(localHostName, withUID(demo("alpha"), "1"), withUID(demoWith("old", session.StatusExited), "2")))
	i.keys(t, key(keyDown))
	if done, _ := i.keys(t, key(keyEnter)); done || i.mode != inboxActions {
		t.Fatalf("enter on exited: %v %v", done, i.mode)
	}
	r, _ := i.row(i.modeRow)
	if got := i.actions(r); len(got) != 1 || got[0] != rowRemove {
		t.Fatalf("exited actions = %v", got)
	}
	i.keys(t, key(keyEsc), key(keyUp), key(keyRight))
	r, _ = i.row(i.modeRow)
	if got := i.actions(r); len(got) != 2 || got[0] != rowAttach || i.mode != inboxActions {
		t.Fatalf("running actions = %v", got)
	}
	if done, out := i.keys(t, char('1')); !done || out.id != "alpha" || out.host != nil {
		t.Fatalf("1 attaches: %v %+v", done, out)
	}
	i.mode = inboxBrowse
	if done, out := i.keys(t, key(keyEnter)); !done || out.id != "alpha" {
		t.Fatalf("enter attaches: %v %+v", done, out)
	}
}

// Remove asks on the line, then runs against the row's own host and
// incarnation; while it runs the row says so and refuses a second
// action; a failure stays until a key and a success is a notice, with the
// inbox still open.
func TestInboxRemove(t *testing.T) {
	i := testInbox(t, hostWith(localHostName, withUID(demo("demo"), "l1")), hostWith("dev", withUID(demo("demo"), "d1")))
	i.typed(t, "dev/")
	i.keys(t, key(keyTab), char('2'))
	if !strings.Contains(i.screen(100, 30), "Remove dev/demo? Its working directory is kept.") {
		t.Fatalf("confirm:\n%s", i.screen(100, 30))
	}
	i.keys(t, key(keyEnter))
	if len(i.effects) != 1 || i.effects[0].remove == nil || i.effects[0].remove.host.name != "dev" || i.effects[0].remove.rec.UID != "d1" {
		t.Fatalf("effects = %+v", i.effects)
	}
	e := i.effects[0]
	i.effects = nil
	if !strings.Contains(i.screen(100, 30), "removing…") {
		t.Fatalf("pending:\n%s", i.screen(100, 30))
	}
	i.keys(t, key(keyTab), char('2'))
	if i.mode == inboxConfirm || i.notice == nil || !strings.Contains(i.notice.message, "is removing") {
		t.Fatalf("second remove: %v %+v", i.mode, i.notice)
	}
	i.keys(t, key(keyEsc))

	// Refused: a failure shown until a key, the row unchanged.
	k := e.remove.key()
	i.pending[k] = "removing…"
	if done, _ := i.finish(inboxResult{key: k, address: "dev/demo", host: "dev", err: errors.New("session `demo` is not the session you chose")}); done {
		t.Fatal("a failed remove ended the inbox")
	}
	if len(i.failures) != 1 || !strings.Contains(i.failures[0], "could not remove dev/demo") || rowNames(i.rows()) != "dev/demo" {
		t.Fatalf("refused: %q %q", i.failures, rowNames(i.rows()))
	}
	// Lost answer: the host is asked again.
	i.pending[k] = "removing…"
	i.finish(inboxResult{key: k, address: "dev/demo", host: "dev", err: errors.New("connection lost"), unknown: true})
	if len(i.failures) != 2 || !strings.Contains(i.failures[1], "result unknown, refreshing") || len(i.effects) != 1 || i.effects[0].reprobe != "dev" {
		t.Fatalf("unknown: %q %+v", i.failures, i.effects)
	}
	i.effects = nil
	// Done: the row goes at once, and stays gone until dev's next probe.
	i.pending[k] = "removing…"
	i.finish(inboxResult{key: k, address: "dev/demo", host: "dev"})
	if i.notice == nil || i.notice.message != "removed session dev/demo" || rowNames(i.allRows()) != "demo" {
		t.Fatalf("removed: %+v %q", i.notice, rowNames(i.allRows()))
	}
	i.update(hostWith("dev"))
	if len(i.gone) != 0 {
		t.Fatalf("gone = %v", i.gone)
	}
}

// Leaving with an action in flight waits for it, saying so; a second Esc
// leaves at once.
func TestInboxLeaveWaitsForActions(t *testing.T) {
	i := testInbox(t, hostWith(localHostName, withUID(demo("a"), "1"), withUID(demo("b"), "2")))
	i.keys(t, key(keyTab), char('2'), key(keyEnter))
	k := i.effects[0].remove.key()
	if done, _ := i.keys(t, key(keyEsc)); done || !strings.Contains(i.screen(100, 30), "finishing removing") {
		t.Fatalf("esc with a remove pending: %v\n%s", done, i.screen(100, 30))
	}
	if done, out := i.finish(inboxResult{key: k, address: "a", host: localHostName}); !done || out != nil {
		t.Fatalf("after the remove: %v %+v", done, out)
	}

	i = testInbox(t, hostWith(localHostName, withUID(demo("a"), "1"), withUID(demo("b"), "2")))
	i.keys(t, key(keyTab), char('2'), key(keyEnter), key(keyDown))
	if done, _ := i.keys(t, key(keyEnter)); done || i.leaving == nil || i.leaving.outcome == nil {
		t.Fatalf("attach with a remove pending: %v", done)
	}
	// Value: protects=while a leave waits, typing, pastes, arrows, Tab and Enter change nothing, so the chosen session stays the one attached; fails_when=keys reach the list while leaving; why_new=only Esc and Ctrl-C were sent while waiting; seam=none; no_claim=drawing
	// While it waits, other keys and pastes change nothing.
	i.handlePaste("pasted")
	if done, _ := i.keys(t, char('x'), key(keyUp), key(keyTab), key(keyEnter), char('n')); done || i.query != "" || i.mode != inboxBrowse || len(i.effects) != 1 || i.leaving.outcome.id != "b" {
		t.Fatalf("keys while leaving: %v %q %v %+v %+v", done, i.query, i.mode, i.effects, i.leaving.outcome)
	}
	if done, out := i.keys(t, key(keyEsc)); !done || out == nil || out.id != "b" {
		t.Fatalf("second esc: %v %+v", done, out)
	}
}

// The create row starts a session in this directory with the chosen agent,
// named by the filter, and attaches to it.
func TestInboxCreate(t *testing.T) {
	i := testInbox(t, hostWith(localHostName, withUID(demo("alpha"), "1")))
	i.defaultAgent, i.createAgents = "codex", []string{"codex", "claude"}
	i.typed(t, "fresh")
	if !strings.Contains(i.screen(100, 30), "+ new session fresh") || i.selection(i.rows()) != 0 || len(i.rows()) != 0 {
		t.Fatalf("create row:\n%s", i.screen(100, 30))
	}
	i.keys(t, key(keyEnter))
	if i.mode != inboxCreateAgent || i.modeIndex != 0 {
		t.Fatalf("enter on +: %v %d", i.mode, i.modeIndex)
	}
	screen := i.screen(100, 30)
	for _, want := range []string{"Start fresh here with", "❯ 1. codex (default)", "2. claude"} {
		if !strings.Contains(screen, want) {
			t.Errorf("missing %q in\n%s", want, screen)
		}
	}
	i.keys(t, char('2'))
	if len(i.effects) != 1 || !i.effects[0].create || i.effects[0].createName != "fresh" || i.effects[0].createAgent != "claude" || !i.creating {
		t.Fatalf("effects = %+v", i.effects)
	}
	if done, out := i.finish(inboxResult{create: true, created: &session.CreateResult{SessionID: "fresh", UID: "9"}}); !done || out.id != "fresh" || out.uid != "9" || out.host != nil {
		t.Fatalf("created: %v %+v", done, out)
	}
	// An invalid name is refused before the chooser opens.
	i = testInbox(t, hostWith(localHostName))
	i.typed(t, "Bad Name")
	i.keys(t, key(keyEnter))
	if i.mode != inboxBrowse || i.notice == nil || !i.notice.isError {
		t.Fatalf("invalid name: %v %+v", i.mode, i.notice)
	}
	// A failed create is a failure, shown until a key.
	i.creating = true
	if done, _ := i.finish(inboxResult{create: true, err: errors.New("no such agent")}); done || i.creating || len(i.failures) != 1 || !strings.Contains(i.failures[0], "no such agent") {
		t.Fatalf("failed create: %v %q", done, i.failures)
	}
}

func TestInboxDefaultAgents(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no agent installed: claude is the default
	i := testInbox(t)
	if strings.Join(i.createAgents, ",") != "claude,codex,pi" || i.defaultAgent != "claude" {
		t.Fatalf("defaults: %v %q", i.createAgents, i.defaultAgent)
	}
	if got := orderedCreateAgents([]string{"codex", "claude", "zed"}, "claude"); strings.Join(got, ",") != "claude,codex,zed" {
		t.Fatalf("got %v", got)
	}
	if got := orderedCreateAgents([]string{"codex", "claude", "zed"}, ""); strings.Join(got, ",") != "codex,claude,zed" {
		t.Fatalf("got %v", got)
	}
}

// `agt --host dev` shows dev alone, says so in the input, and keeps the
// old picker's hint for starting a session there.
func TestInboxLockedToHost(t *testing.T) {
	i := testInbox(t, hostWith("dev", withUID(demo("demo"), "d1")))
	i.locked = "dev"
	screen := i.screen(100, 30)
	if !strings.Contains(screen, "❯ dev/") || strings.Contains(screen, "local") {
		t.Fatalf("locked:\n%s", screen)
	}
	i.keys(t, char('n'))
	if i.mode != inboxBrowse || i.notice == nil || !strings.Contains(i.notice.message, "agt new -C DIR dev/NAME") {
		t.Fatalf("n: %v %+v", i.mode, i.notice)
	}
}

// One case per row of the states table.
func TestInboxStates(t *testing.T) {
	now := time.Now()
	ago := func(d time.Duration) *time.Time { at := now.Add(-d); return &at }
	exit1 := int32(1)
	progress := 62
	for _, c := range []struct {
		name  string
		edit  func(*session.Record)
		word  string
		glyph string
	}{
		{"blocked", func(s *session.Record) {
			s.Attention, s.Activity, s.AttentionAt = session.AttentionAction, session.ActivityBlocked, ago(4*time.Minute)
		}, "blocked 4m", "⚠"},
		{"waiting", func(s *session.Record) {
			s.Attention, s.Activity, s.AttentionAt = session.AttentionAction, session.ActivityWaiting, ago(12*time.Minute)
		}, "waiting 12m", "⚠"},
		{"working", func(s *session.Record) { s.Activity, s.StatusProgress = session.ActivityWorking, &progress }, "working 62%", "●"},
		{"running", func(s *session.Record) {}, "running", "●"},
		{"starting", func(s *session.Record) { s.Status = session.StatusCreating }, "starting", "●"},
		{"idle", func(s *session.Record) { s.Activity, s.LastOutputAt = session.ActivityIdle, ago(38*time.Minute) }, "idle 38m", "●"},
		{"exited", func(s *session.Record) { s.Status = session.StatusExited }, "exited", "○"},
		{"exited (1)", func(s *session.Record) { s.Status, s.ExitCode = session.StatusExited, &exit1 }, "exited (1)", "○"},
		{"failed", func(s *session.Record) { s.Status = session.StatusFailed }, "failed", "✖"},
		{"lost", func(s *session.Record) { s.Status = session.StatusUnknownRecovered }, "lost", "○"},
	} {
		rec := withUID(demo("x"), "1")
		c.edit(&rec)
		i := testInbox(t, hostWith(localHostName, rec))
		i.now = func() time.Time { return now }
		word, glyph, _ := i.state(i.rows()[0])
		if word != c.word || glyph != c.glyph {
			t.Errorf("%s: %q %q, want %q %q", c.name, word, glyph, c.word, c.glyph)
		}
	}
}

// The header counts, the host strip's states, and the first-run and
// nothing-reachable notes.
func TestInboxHeaderStripAndEmptyStates(t *testing.T) {
	ask := withUID(demo("ask"), "1")
	ask.Attention = session.AttentionAction
	idle := withUID(demo("idle"), "2")
	idle.Activity = session.ActivityIdle
	gone := withUID(demoWith("gone", session.StatusExited), "3")
	i := testInbox(t, hostWith(localHostName, ask, idle, gone), &hostProbe{name: "sfo2", status: hostUnauthorized, host: &transport.Host{Name: "sfo2"}},
		&hostProbe{name: "box", status: hostOffline, host: &transport.Host{Name: "box"}}, &hostProbe{name: "new", status: hostAsking, host: &transport.Host{Name: "new"}})
	lines := strings.Split(i.screen(140, 30), "\n")
	if !strings.HasPrefix(lines[0], " agtd") || !strings.HasSuffix(strings.TrimRight(lines[0], " "), "1 need you · 1 idle · 1 exited") {
		t.Fatalf("header %q", lines[0])
	}
	for _, want := range []string{"local ● 3", "sfo2 ⚠ unauthorized · agt host info sfo2", "box ○ offline", "new ◌"} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("strip %q lacks %q", lines[1], want)
		}
	}
	// The strip wraps rather than overflow.
	if narrow := strings.Split(i.screen(50, 30), "\n"); !strings.Contains(narrow[2], "box") && !strings.Contains(narrow[3], "box") {
		t.Fatalf("strip did not wrap:\n%s", strings.Join(narrow, "\n"))
	}

	first := testInbox(t, hostWith(localHostName))
	if s := first.screen(100, 30); !strings.Contains(s, "No sessions yet.") || !strings.Contains(s, "agtd remote pair") || first.selection(first.rows()) != 0 {
		t.Fatalf("first run:\n%s", s)
	}
	none := testInbox(t, &hostProbe{name: localHostName, status: hostNotRunning}, &hostProbe{name: "dev", status: hostOffline, host: &transport.Host{Name: "dev"}})
	if s := none.screen(100, 30); !strings.Contains(s, "Nothing reachable.") {
		t.Fatalf("offline:\n%s", s)
	}
	starting := testInbox(t, &hostProbe{name: localHostName, status: hostStarting})
	if s := starting.screen(100, 30); !strings.Contains(s, "local ◌ starting") || strings.Contains(s, "Nothing reachable") {
		t.Fatalf("starting:\n%s", s)
	}
}

// Every line is exactly one cell short of the terminal's width, whatever
// the rows hold, and columns drop as the terminal narrows: the agent
// below 90 cells, the host into the name below 60.
func TestInboxLayouts(t *testing.T) {
	msg := "レビュー待ち: allow Bash(go test ./...) 👍 and then some more words to truncate"
	s := withUID(demo("auth-refactor"), "1")
	s.StatusMsg = &msg
	s.Attention = session.AttentionAction
	i := testInbox(t, hostWith(localHostName, withUID(demo("docs"), "2")), hostWith("dev", s))
	for _, cols := range []int{120, 96, 80, 60, 59, 45} {
		lines := i.render(cols, 30)
		for n, l := range lines {
			if w := cellWidth(stripANSI(l)); w != cols-1 {
				t.Errorf("%d cols, line %d is %d cells: %q", cols, n, w, stripANSI(l))
			}
		}
	}
	at := func(cols int) string { return i.screen(cols, 30) }
	if s := at(96); !strings.Contains(s, "codex") || !strings.Contains(s, " dev ") || !strings.Contains(s, "レビュー") {
		t.Fatalf("96:\n%s", s)
	}
	if s := at(80); strings.Contains(s, "codex") || !strings.Contains(s, " dev ") {
		t.Fatalf("80:\n%s", s)
	}
	if s := at(59); !strings.Contains(s, "dev/auth-refactor") || !strings.Contains(s, "local/docs") {
		t.Fatalf("59:\n%s", s)
	}
}

// The whole block fits the terminal's height less one line, scrolling the
// rows to keep the selection in view.
func TestInboxHeightCap(t *testing.T) {
	var recs []session.Record
	for n := range 30 {
		r := withUID(demo(fmt.Sprintf("s%02d", n)), fmt.Sprint(n))
		r.UpdatedAt = time.Now().Add(-time.Duration(n) * time.Minute)
		recs = append(recs, r)
	}
	i := testInbox(t, hostWith(localHostName, recs...))
	for range 20 {
		i.keys(t, key(keyDown))
	}
	lines := i.render(100, 16)
	if len(lines) > 15 {
		t.Fatalf("%d lines in a 16-row terminal", len(lines))
	}
	if s := stripANSI(strings.Join(lines, "\n")); !strings.Contains(s, "▌ ● s20") {
		t.Fatalf("selection scrolled away:\n%s", s)
	}
	i.keys(t, key(keyTab))
	if s := stripANSI(strings.Join(i.render(100, 16), "\n")); !strings.Contains(s, "2. remove") {
		t.Fatalf("menu scrolled away:\n%s", s)
	}
	// A tiny terminal still gets no more lines than it has.
	if n := len(i.render(100, 5)); n > 4 {
		t.Fatalf("%d lines in a 5-row terminal", n)
	}
}

// Every key the hint line names does what it says.
func TestInboxHints(t *testing.T) {
	i := testInbox(t, hostWith(localHostName, withUID(demo("a"), "1"), withUID(demoWith("b", session.StatusExited), "2")))
	hint := func() string {
		lines := strings.Split(i.screen(120, 30), "\n")
		return lines[len(lines)-1]
	}
	if h := hint(); !strings.Contains(h, "↑↓ choose · enter attach · tab actions · n new · esc quit · ? keys") {
		t.Fatalf("browse %q", h)
	}
	i.keys(t, key(keyDown))
	if h := hint(); !strings.Contains(h, "enter actions") {
		t.Fatalf("ended row %q", h)
	}
	i.keys(t, key(keyDown))
	if h := hint(); !strings.Contains(h, "enter new session") {
		t.Fatalf("create row %q", h)
	}
	i.typed(t, "a")
	if h := hint(); !strings.Contains(h, "esc clear") || strings.Contains(h, "n new") {
		t.Fatalf("filtered %q", h)
	}
	i.keys(t, key(keyTab))
	if h := hint(); !strings.Contains(h, "↑↓ choose · enter select · esc back") {
		t.Fatalf("menu %q", h)
	}
	i.keys(t, char('2'))
	if h := hint(); !strings.Contains(h, "enter confirm · esc back") {
		t.Fatalf("confirm %q", h)
	}
	i.keys(t, key(keyEsc), key(keyEsc), key(keyEsc), char('?'))
	if s := i.screen(120, 30); !strings.Contains(s, "r ask every host again") || !strings.Contains(s, "space first") {
		t.Fatalf("keys:\n%s", s)
	}
}

// NO_COLOR and TERM=dumb draw the inbox without colour; its glyphs stay.
func TestInboxNoColor(t *testing.T) {
	ask := withUID(demo("ask"), "1")
	ask.Attention = session.AttentionAction
	i := testInbox(t, hostWith(localHostName, ask))
	i.theme = inboxTheme(overlayThemeFor(func(k string) string {
		if k == "TERM" {
			return "dumb"
		}
		return ""
	}))
	out := strings.Join(i.render(100, 30), "\n")
	if strings.Contains(strings.NewReplacer("\x1b[7m", "", ansiReset, "").Replace(out), "\x1b") || !strings.Contains(out, "⚠ ask") {
		t.Fatalf("no colour:\n%q", out)
	}

	// Value: protects=a host's session name, agent and status message never reach the terminal as raw control bytes; fails_when=a field skips escapeControls; why_new=no inbox test fed controls; seam=none; no_claim=notices and failures (TestInboxPasteAndNoticesAreClean)
	// A daemon's records are drawn escaped, so a status message cannot
	// clear the screen, set the title or move the cursor.
	msg := "\x1b[2J\x1b]0;title\x07wiped"
	evil := withUID(demo("ev\x1b[1Ail"), "2")
	evil.Agent = "s\x1b[Hh"
	evil.StatusMsg = &msg
	i.update(hostWith(localHostName, ask, evil))
	out = strings.Join(i.render(120, 30), "\n")
	if strings.Contains(strings.NewReplacer("\x1b[7m", "", ansiReset, "").Replace(out), "\x1b") || strings.Contains(out, "\x07") || !strings.Contains(out, `wiped`) {
		t.Fatalf("controls reached the terminal:\n%q", out)
	}
}

// Value: protects=a menu or confirmation on a row that a probe drops closes, so keys reach the list again; fails_when=update stops calling clampMode; why_new=no inbox test drops a row under an open menu; seam=none; no_claim=redraw timing
// A row's menu or confirmation closes when a refresh no longer has the row,
// and the keys go back to the list.
func TestInboxMenuClosesWhenItsRowGoes(t *testing.T) {
	a, b := withUID(demo("a"), "1"), withUID(demo("b"), "2")
	for _, keys := range [][]keyEvent{{key(keyTab)}, {key(keyTab), char('2')}} {
		i := testInbox(t, hostWith(localHostName, a, b))
		i.keys(t, key(keyDown))
		i.keys(t, keys...)
		if i.mode == inboxBrowse {
			t.Fatalf("%v: no menu opened", keys)
		}
		i.update(hostWith(localHostName, a))
		if i.mode != inboxBrowse {
			t.Fatalf("%v: mode %v after the row went", keys, i.mode)
		}
		if s := i.screen(100, 30); strings.Contains(s, "1. attach") || strings.Contains(s, "Remove b?") {
			t.Fatalf("%v: menu still drawn:\n%s", keys, s)
		}
		i.typed(t, "a")
		if i.query != "a" {
			t.Fatalf("%v: typing after the menu closed: %q", keys, i.query)
		}
	}
}

// Value: protects=one create at a time, and Esc during a create quits without attaching to it while another choice still wins; fails_when=a create result attaches though the user left; why_new=finish/leave with creating is untested; seam=none; no_claim=the daemon's create
// While a session is being started a second create is refused; leaving
// waits for it, saying so, and then quits (Esc) or attaches to the row the
// user chose, never to the new session.
func TestInboxLeaveWhileCreating(t *testing.T) {
	created := inboxResult{create: true, created: &session.CreateResult{SessionID: "fresh", UID: "9"}}
	i := testInbox(t, hostWith(localHostName, withUID(demo("a"), "1")))
	i.createAgents, i.defaultAgent = []string{"claude"}, "claude"
	i.keys(t, char('n'), key(keyEnter))
	if !i.creating || len(i.effects) != 1 || !i.effects[0].create {
		t.Fatalf("create: %v %+v", i.creating, i.effects)
	}
	if s := i.screen(100, 30); !strings.Contains(s, "starting a new session…") {
		t.Fatalf("creating:\n%s", s)
	}
	i.keys(t, char('n'))
	if i.mode != inboxBrowse || len(i.effects) != 1 || i.notice == nil || !strings.Contains(i.notice.message, "already starting") {
		t.Fatalf("second create: %v %+v %+v", i.mode, i.effects, i.notice)
	}
	if done, _ := i.keys(t, key(keyEsc)); done || !strings.Contains(i.screen(100, 30), "finishing starting the new session") {
		t.Fatalf("esc while creating: %v\n%s", done, i.screen(100, 30))
	}
	if done, out := i.finish(created); !done || out != nil {
		t.Fatalf("create done after esc: %v %+v", done, out)
	}

	i = testInbox(t, hostWith(localHostName, withUID(demo("a"), "1")))
	i.createAgents, i.defaultAgent = []string{"claude"}, "claude"
	i.keys(t, char('n'), key(keyEnter), key(keyUp))
	if done, _ := i.keys(t, key(keyEnter)); done || i.leaving == nil {
		t.Fatalf("attach while creating: %v", done)
	}
	if done, out := i.finish(created); !done || out == nil || out.id != "a" || out.uid != "1" {
		t.Fatalf("create done after attach: %v %+v", done, out)
	}
}

// Value: protects=a terminal under 40 cells gets the inbox's rows as `agt ls --all` prints them, host column and all, without removed rows; fails_when=printNarrow drops hosts or gone; why_new=printNarrow is untested; seam=none; no_claim=the 40-cell check in runInbox
// Too narrow to draw, the inbox prints its own rows, with their hosts and
// without the ones just removed, and says why.
func TestInboxPrintNarrow(t *testing.T) {
	i := testInbox(t, hostWith(localHostName, withUID(demo("demo"), "l1"), withUID(demo("old"), "l2")), hostWith("dev", withUID(demo("demo"), "d1")))
	i.gone[rowKey{host: localHostName, uid: "l2"}] = true
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	t.Cleanup(func() { os.Stdout = stdout })
	os.Stdout = w
	i.printNarrow(38)
	os.Stdout = stdout
	w.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	out := stripANSI(string(b))
	if !strings.Contains(out, "HOST") || !strings.Contains(out, "dev") || !strings.Contains(out, "local") || strings.Count(out, " demo ") != 2 || strings.Contains(out, "old") ||
		!strings.Contains(out, "the inbox needs 40 columns") {
		t.Fatalf("narrow:\n%s", out)
	}
}

// Redrawing after the terminal narrowed clears the rows the old, wider
// lines now wrap onto.
func TestInlineScreenClearsRewrappedLines(t *testing.T) {
	s := inlineScreen{lines: []string{"a", "b", "c"}, cols: 100}
	if got := s.clearSequence(100); got != "\r\x1b[2A\x1b[J" {
		t.Fatalf("same width %q", got)
	}
	if got := s.clearSequence(50); got != "\r\x1b[5A\x1b[J" {
		t.Fatalf("half width %q", got)
	}
}

// Value: protects=a remove on a daemon without kill-expect-uid sends no kill when the session's UID changed; fails_when=checkIncarnation is skipped or compares the wrong UID; why_new=the daemon test covers only daemons with the capability; seam=none; no_claim=a replacement between the check and the kill on such a daemon
// A daemon without kill-expect-uid would remove by name, so the inbox asks
// it for the session first and removes it only if it is still the
// incarnation the row showed. A daemon with it is asked nothing extra.
func TestInboxRemoveChecksTheUIDOnOlderDaemons(t *testing.T) {
	older := slices.DeleteFunc(protocol.Capabilities(), func(c string) bool { return c == protocol.CapKillExpectUID })
	remove := func(t *testing.T, caps []string) (*fakeRemote, <-chan inboxResult) {
		f, c := newFakeRemote(t, caps)
		a := &app{paths: c.paths}
		p := hostWith("dev")
		p.host = c.host
		rec := withUID(demo("demo"), "old")
		done := make(chan inboxResult, 1)
		go func() {
			done <- a.runInboxEffect(context.Background(), inboxEffect{remove: &inboxRow{host: p, rec: &rec}})
		}()
		return f, done
	}
	result := func(t *testing.T, done <-chan inboxResult) inboxResult {
		t.Helper()
		select {
		case r := <-done:
			return r
		case <-time.After(10 * time.Second):
			t.Fatal("remove did not finish")
			return inboxResult{}
		}
	}
	session := func(uid string) *protocol.Response {
		rec := withUID(demo("demo"), uid)
		return &protocol.Response{Session: &rec}
	}

	t.Run("replaced", func(t *testing.T) {
		f, done := remove(t, older)
		r := f.next()
		if r.req.GetSession == nil || r.req.GetSession.SessionID != "demo" {
			t.Fatalf("first request = %s, want GetSession", r.req.Role())
		}
		r.answer(session("new"))
		if res := result(t, done); res.err == nil || !strings.Contains(res.err.Error(), "not the session you chose") || res.unknown {
			t.Fatalf("result = %+v", res)
		}
		select {
		case r := <-f.requests:
			t.Fatalf("sent %#v after the UID changed", r.req)
		default:
		}
	})
	t.Run("gone", func(t *testing.T) {
		// Nothing was sent when the check itself fails, so it is certain.
		f, done := remove(t, older)
		f.next().answer(&protocol.Response{Error: &protocol.ErrorResponse{Message: "session `demo` not found"}})
		if res := result(t, done); res.err == nil || res.unknown {
			t.Fatalf("result = %+v", res)
		}
		select {
		case r := <-f.requests:
			t.Fatalf("sent %#v after the check failed", r.req)
		default:
		}
	})
	t.Run("same", func(t *testing.T) {
		f, done := remove(t, older)
		f.next().answer(session("old"))
		r := f.next()
		if r.req.KillSession == nil || !r.req.KillSession.Remove {
			t.Fatalf("second request = %#v, want KillSession", r.req)
		}
		r.answer(&protocol.Response{KillSession: &protocol.KillSessionResult{Removed: true}})
		if res := result(t, done); res.err != nil {
			t.Fatalf("result = %+v", res)
		}
	})
	// Value: protects=a daemon's refusal is a certain failure and a lost answer an unknown one; fails_when=runInboxEffect's error arms are swapped or unknown is set on a refusal; why_new=finish is tested with hand-built results only; seam=none; no_claim=retries with request tokens (left out here)
	// A refusal is certain; a connection lost after the kill was sent
	// leaves the result unknown. (Without request tokens, so the lost
	// kill is not sent again.)
	noTokens := slices.DeleteFunc(protocol.Capabilities(), func(c string) bool { return c == protocol.CapRequestTokens })
	t.Run("refused", func(t *testing.T) {
		f, done := remove(t, noTokens)
		f.next().answer(&protocol.Response{Error: &protocol.ErrorResponse{Message: "session `demo` is not the session you chose"}})
		if res := result(t, done); res.err == nil || !strings.Contains(res.err.Error(), "not the session you chose") || res.unknown {
			t.Fatalf("result = %+v", res)
		}
	})
	t.Run("lost", func(t *testing.T) {
		f, done := remove(t, noTokens)
		if r := f.next(); r.req.KillSession == nil {
			t.Fatalf("first request = %#v, want KillSession", r.req)
		}
		f.restart()
		if res := result(t, done); res.err == nil || !res.unknown {
			t.Fatalf("result = %+v", res)
		}
	})
	t.Run("current daemon", func(t *testing.T) {
		f, done := remove(t, protocol.Capabilities())
		r := f.next()
		if r.req.KillSession == nil || r.req.KillSession.ExpectUID != "old" {
			t.Fatalf("first request = %#v, want KillSession with ExpectUID", r.req)
		}
		r.answer(&protocol.Response{KillSession: &protocol.KillSessionResult{Removed: true}})
		if res := result(t, done); res.err != nil {
			t.Fatalf("result = %+v", res)
		}
	})
}

// Value: protects=Ctrl-C quits from any mode, and at once while an action is awaited, never attaching; fails_when=Ctrl-C is ignored or attaches; why_new=no inbox test sends Ctrl-C; seam=none; no_claim=the terminal's ISIG handling
// Ctrl-C quits the inbox from anywhere; while it waits for an action, a
// second Ctrl-C leaves at once without attaching.
func TestInboxCtrlCQuits(t *testing.T) {
	ctrlC := keyEvent{code: keyChar, r: 'c', mods: modCtrl}
	i := testInbox(t, hostWith(localHostName, withUID(demo("a"), "1"), withUID(demo("b"), "2")))
	if done, out := i.keys(t, ctrlC); !done || out != nil {
		t.Fatalf("ctrl-c in the list: %v %+v", done, out)
	}
	i = testInbox(t, hostWith(localHostName, withUID(demo("a"), "1")))
	if done, out := i.keys(t, key(keyTab), ctrlC); !done || out != nil {
		t.Fatalf("ctrl-c in a menu: %v %+v", done, out)
	}
	i = testInbox(t, hostWith(localHostName, withUID(demo("a"), "1"), withUID(demo("b"), "2")))
	i.keys(t, key(keyTab), char('2'), key(keyEnter), key(keyDown), key(keyEnter))
	if i.leaving == nil || i.leaving.outcome == nil {
		t.Fatal("attach with a remove pending did not wait")
	}
	if done, out := i.keys(t, ctrlC); !done || out != nil {
		t.Fatalf("ctrl-c while waiting: %v %+v", done, out)
	}
}

// Value: protects=pasted line breaks and control bytes, and control bytes in a notice, never reach the terminal raw; fails_when=handlePaste or the notice line skips its cleaning; why_new=no test pastes controls or puts them in a notice; seam=none; no_claim=pastes arriving through runInbox (handlePaste is called directly)
// A paste is one line of plain text, and a notice (which may carry a
// daemon's error) is drawn escaped.
func TestInboxPasteAndNoticesAreClean(t *testing.T) {
	i := testInbox(t, hostWith(localHostName, withUID(demo("a"), "1")))
	i.handlePaste("de\nmo\x1b[2J\u202e")
	if i.query != "de mo[2J" {
		t.Fatalf("query = %q", i.query)
	}
	i.errorNotice("could not remove a: %s", "\x1b]0;title\x07\x1b[2Jboom")
	out := strings.Join(i.render(100, 30), "\n")
	if strings.Contains(strings.ReplaceAll(out, "\x1b[7m", ""), "\x1b]") || strings.Contains(out, "\x07") || strings.Contains(out, "\x1b[2J") || !strings.Contains(out, "boom") {
		t.Fatalf("notice reached the terminal raw:\n%q", out)
	}
}

// Value: protects=an open inbox narrowed under 40 columns draws one line that fits; fails_when=the notice is wider than the terminal; why_new=narrowNotice is new; seam=none; no_claim=runInbox's choice between notice and list
// An inbox narrowed below its width says so on one line that fits.
func TestInboxNarrowNotice(t *testing.T) {
	for _, cols := range []int{10, 30, 39} {
		lines := narrowNotice(cols, "")
		if len(lines) != 1 || cellWidth(lines[0]) > lineWidth(cols) || !strings.HasPrefix(lines[0], "agt: wid") {
			t.Fatalf("%d columns: %q", cols, lines)
		}
	}
	// Value: protects=a narrowed inbox still says that an action failed or what its leave waits for, within the width; fails_when=narrowNotice drops status or narrowStatus misses the leave; why_new=only the empty status was tested; seam=none; no_claim=runInbox's redraw
	// Narrowed while leaving, it says what it waits for, the last action
	// when that row has gone, and a failure before either.
	i := testInbox(t, hostWith(localHostName, withUID(demo("a"), "1")))
	i.pending[rowKey{host: localHostName, uid: "1"}] = "removing…"
	i.pending[rowKey{host: localHostName, uid: "gone"}] = "removing…"
	i.leaving = &inboxLeave{}
	if got := narrowNotice(200, i.narrowStatus())[0]; got != "agt: finishing removing a… esc leaves now; widen the terminal to 40 columns" {
		t.Fatalf("leaving: %q", got)
	}
	delete(i.pending, rowKey{host: localHostName, uid: "1"})
	if got := i.narrowStatus(); got != "finishing the last action… esc leaves now" {
		t.Fatalf("leaving for a row that went: %q", got)
	}
	i.fail("could not remove a")
	if lines := narrowNotice(30, i.narrowStatus()); cellWidth(lines[0]) > lineWidth(30) || !strings.HasPrefix(lines[0], "agt: an action failed") {
		t.Fatalf("failed: %q", lines)
	}
}

// Value: protects=an inbox narrowed after drawing keeps running (notice), and only one too narrow from the start prints and leaves; fails_when=the drawn case is dropped or reordered; why_new=runInbox's choice was untested; seam=none; no_claim=terminalGeometry
// What a terminal shows: the inbox when wide enough; a notice once drawn
// and then narrowed; nothing and then the list when narrow from the start.
func TestChooseInboxView(t *testing.T) {
	for _, tc := range []struct {
		cols           int
		drawn, settled bool
		want           inboxView
	}{
		{100, false, false, inboxDrawn},
		{40, true, true, inboxDrawn},
		{30, true, false, inboxNarrowed},
		{30, true, true, inboxNarrowed},
		{30, false, false, inboxWaiting},
		{30, false, true, inboxListed},
	} {
		if got := chooseInboxView(tc.cols, tc.drawn, tc.settled); got != tc.want {
			t.Errorf("chooseInboxView(%d, %v, %v) = %d, want %d", tc.cols, tc.drawn, tc.settled, got, tc.want)
		}
	}
}

// Value: protects=keys do nothing while the inbox is not on screen, except Esc and Ctrl-C; fails_when=the hidden check is removed; why_new=type-ahead before the first draw and keys while narrowed were untested; seam=none; no_claim=runInbox setting hidden
// While the inbox is not on screen, typing, Enter and Tab do nothing;
// Esc and Ctrl-C still leave.
func TestInboxIgnoresKeysWhileHidden(t *testing.T) {
	i := testInbox(t, hostWith(localHostName, withUID(demo("a"), "1")))
	i.hidden = true
	i.handlePaste("pasted")
	if done, out := i.keys(t, char('x'), key(keyEnter), key(keyTab), char('n')); done || out != nil || i.query != "" || i.mode != inboxBrowse || len(i.effects) != 0 {
		t.Fatalf("keys while hidden: %v %+v %q %v %+v", done, out, i.query, i.mode, i.effects)
	}
	if done, out := i.keys(t, key(keyEsc)); !done || out != nil {
		t.Fatalf("esc while hidden: %v %+v", done, out)
	}
	// Esc quits at once even from a menu the user cannot see.
	i = testInbox(t, hostWith(localHostName, withUID(demo("a"), "1")))
	i.keys(t, char('a'), key(keyTab))
	i.hidden = true
	if done, out := i.keys(t, key(keyEsc)); !done || out != nil {
		t.Fatalf("esc in a hidden menu: %v %+v", done, out)
	}
	i.hidden = false
	if done, out := i.keys(t, key(keyEnter)); !done || out == nil {
		t.Fatalf("enter once shown: %v %+v", done, out)
	}
}

// Value: protects=the rules for actions finishing, failing and attaching while the user leaves; fails_when=a failure is lost, or the inbox leaves or attaches past a failure the user has not seen; why_new=replaces piecemeal tests of the old flags with one table; seam=none; no_claim=drawing, and the terminal
// How actions that finish meet the user's request to leave. A failure
// stays until a key the user can see; while one is unseen the inbox never
// leaves or attaches by itself; a failure cancels a leave; a leave waits
// for every action.
func TestInboxActionsAndLeaving(t *testing.T) {
	a, b := rowKey{host: localHostName, uid: "1"}, rowKey{host: localHostName, uid: "2"}
	removed := func(k rowKey) inboxResult { return inboxResult{key: k, address: k.uid, host: localHostName} }
	refused := func(k rowKey) inboxResult {
		r := removed(k)
		r.err = errors.New("refused")
		return r
	}
	created := inboxResult{create: true, created: &session.CreateResult{SessionID: "fresh", UID: "9"}}
	attachB := &inboxOutcome{id: "b", uid: "2"}
	type step struct {
		res      *inboxResult
		key      *keyEvent // pressed while the inbox is shown
		soon     bool      // ... at once, rather than after inboxAcknowledgeAfter
		undrawn  bool      // ... with nothing drawn since the last step
		hiddenEv *keyEvent // pressed while it is hidden
	}
	res := func(r inboxResult) step { return step{res: &r} }
	press := func(ev keyEvent) step { return step{key: &ev} }
	pressSoon := func(ev keyEvent) step { return step{key: &ev, soon: true} }
	pressUndrawn := func(ev keyEvent) step { return step{key: &ev, undrawn: true} }
	pressHidden := func(ev keyEvent) step { return step{hiddenEv: &ev} }
	for _, tc := range []struct {
		name       string
		pending    []rowKey
		creating   bool
		leave      *inboxLeave // asked before the steps
		steps      []step
		done       bool
		outcome    *inboxOutcome
		failures   int
		noteSuffix string // of the last failure
		notice     string // the notice, if one is wanted
	}{
		{name: "quit waits, then quits", pending: []rowKey{a}, leave: &inboxLeave{}, steps: []step{res(removed(a))}, done: true},
		{name: "attach waits for every action", pending: []rowKey{a}, creating: true, leave: &inboxLeave{attachB},
			steps: []step{res(removed(a)), res(created)}, done: true, outcome: attachB},
		{name: "a failure cancels a quit", pending: []rowKey{a}, leave: &inboxLeave{}, steps: []step{res(refused(a))}, failures: 1},
		{name: "a failure cancels an attach, saying so", pending: []rowKey{a}, leave: &inboxLeave{attachB}, steps: []step{res(refused(a))},
			failures: 1, noteSuffix: "; not attaching"},
		{name: "a create after a cancelled quit does not attach", pending: []rowKey{a}, creating: true, leave: &inboxLeave{},
			steps: []step{res(refused(a)), res(created)}, failures: 1},
		{name: "a create with an unseen failure does not attach, but says it started", pending: []rowKey{a}, creating: true,
			steps: []step{res(refused(a)), res(created)}, failures: 1, notice: "started session fresh"},
		{name: "a key right after a failure does not acknowledge it", pending: []rowKey{a}, creating: true,
			steps: []step{res(refused(a)), pressSoon(key(keyDown)), res(created)}, failures: 1, notice: "started session fresh"},
		{name: "a failure not yet drawn is not acknowledged", pending: []rowKey{a}, creating: true,
			steps: []step{res(refused(a)), pressUndrawn(key(keyDown)), res(created)}, failures: 1, notice: "started session fresh"},
		{name: "a newer failure waits to be drawn", pending: []rowKey{a, b},
			steps: []step{res(refused(a)), pressSoon(key(keyDown)), res(refused(b)), pressUndrawn(key(keyDown))}, failures: 2},
		{name: "a create attaches once the failure is seen", pending: []rowKey{a}, creating: true,
			steps: []step{res(refused(a)), press(key(keyDown)), res(created)}, done: true, outcome: &inboxOutcome{id: "fresh", uid: "9"}},
		{name: "a hidden key acknowledges nothing", pending: []rowKey{a}, creating: true,
			steps: []step{res(refused(a)), pressHidden(char('x')), res(created)}, failures: 1},
		{name: "a success does not clear a failure", pending: []rowKey{a, b},
			steps: []step{res(refused(a)), res(removed(b))}, failures: 1},
		{name: "a later failure keeps the earlier one", pending: []rowKey{a, b},
			steps: []step{res(refused(a)), res(refused(b))}, failures: 2},
		{name: "esc while hidden quits", pending: []rowKey{a}, steps: []step{res(refused(a)), pressHidden(key(keyEsc))}, done: true, failures: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := testInbox(t, hostWith(localHostName, withUID(demo("1"), "1"), withUID(demo("2"), "2")))
			now := time.Unix(1_700_000_000, 0)
			i.now = func() time.Time { return now }
			for _, k := range tc.pending {
				i.pending[k] = "removing…"
			}
			i.creating, i.leaving = tc.creating, tc.leave
			var done bool
			var out *inboxOutcome
			for _, s := range tc.steps {
				switch {
				case s.res != nil:
					done, out = i.finish(*s.res)
				case s.key != nil:
					if !s.undrawn {
						i.render(100, 30) // the user sees what is there
					}
					if !s.soon {
						now = now.Add(inboxAcknowledgeAfter)
					}
					done, out = i.handleKey(*s.key)
				default:
					i.hidden = true
					done, out = i.handleKey(*s.hiddenEv)
					i.hidden = false
				}
				if done {
					break
				}
			}
			if done != tc.done || (out == nil) != (tc.outcome == nil) || (out != nil && *out != *tc.outcome) || len(i.failures) != tc.failures {
				t.Fatalf("done %v outcome %+v failures %q, want %v %+v %d", done, out, i.failures, tc.done, tc.outcome, tc.failures)
			}
			if tc.noteSuffix != "" && !strings.HasSuffix(i.failures[len(i.failures)-1], tc.noteSuffix) {
				t.Fatalf("failures %q, want the last to end %q", i.failures, tc.noteSuffix)
			}
			if tc.notice != "" && (i.notice == nil || i.notice.message != tc.notice || !slices.Contains(i.effects, inboxEffect{reprobe: localHostName})) {
				t.Fatalf("notice %+v effects %+v, want %q and a reprobe of %s", i.notice, i.effects, tc.notice, localHostName)
			}
		})
	}
}

// Value: protects=unseen failures stay drawn however long the user is away, and the leave line is never hidden by them; fails_when=failures expire or replace the waiting line; why_new=failures and the leave line are separate lines and neither may hide the other; seam=none; no_claim=colours
// Failures stay on screen until a key, beside what a leave waits for;
// notices pass.
func TestInboxMessages(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	i := testInbox(t, hostWith(localHostName, withUID(demo("a"), "1"), withUID(demo("b"), "2")))
	i.now = func() time.Time { return now }
	for n := range 5 {
		i.fail("failure %d", n)
	}
	i.setNotice(&inboxNotice{message: "removed session b"})
	i.pending[rowKey{host: localHostName, uid: "1"}] = "removing…"
	i.leaving = &inboxLeave{}
	s := i.screen(100, 30)
	for _, want := range []string{"2 earlier actions failed too", "failure 2", "failure 4", "finishing removing a… esc leaves now", "removed session b"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "failure 1") {
		t.Fatalf("more than %d failures drawn:\n%s", inboxFailuresShown, s)
	}
	now = now.Add(time.Hour)
	if s := i.screen(100, 30); strings.Contains(s, "removed session b") || !strings.Contains(s, "failure 4") || !strings.Contains(s, "finishing") {
		t.Fatalf("an hour later:\n%s", s)
	}
	if got := i.narrowStatus(); got != "an action failed" {
		t.Fatalf("narrow status %q", got)
	}
	i.leaving = nil
	i.keys(t, key(keyDown))
	if s := i.screen(100, 30); strings.Contains(s, "failure") || len(i.failures) != 0 {
		t.Fatalf("after a key:\n%s", s)
	}
}

// Value: protects=failures the user never saw, and actions a second Esc abandoned, are printed when the inbox quits; fails_when=printUnseen skips either or prints raw controls; why_new=quitting while hidden or leaving at once could lose them; seam=none; no_claim=runInbox calling it
// Quitting prints, escaped, the failures the user has not seen and the
// actions left in flight.
func TestInboxPrintsWhatIsUnseen(t *testing.T) {
	i := testInbox(t, hostWith(localHostName, withUID(demo("a"), "1")))
	i.fail("could not remove b: \x1b[2Jrefused")
	i.pending[rowKey{host: localHostName, uid: "1"}] = "removing…"
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	t.Cleanup(func() { os.Stdout = stdout })
	os.Stdout = w
	printUnseen(i.unseen())
	os.Stdout = stdout
	w.Close()
	b, _ := io.ReadAll(r)
	want := "agt: could not remove b: " + escapeControls("\x1b[2J") + "refused\r\nagt: abandoned removing a; result unknown\r\n"
	if got := string(b); got != want {
		t.Fatalf("printed %q", got)
	}
}

// Value: protects=a lost create is unknown and asks again; fails_when=finish treats it as certain; why_new=create had no unknown result; seam=none; no_claim=which errors count as sent (requestSent)
// A create whose answer was lost is unknown: the inbox says so and asks
// this machine's daemon again.
func TestInboxLostCreateIsUnknown(t *testing.T) {
	i := testInbox(t, hostWith(localHostName))
	i.creating = true
	i.finish(inboxResult{create: true, err: errors.New("connection lost"), unknown: true})
	if i.creating || len(i.failures) != 1 || !strings.Contains(i.failures[0], "result unknown") || len(i.effects) != 1 || i.effects[0].reprobe != localHostName {
		t.Fatalf("lost create: %v %q %+v", i.creating, i.failures, i.effects)
	}
}

// Value: protects=the highlight stays on the row whose menu is open when a probe reorders rows; fails_when=openActions leaves selected unset; why_new=reorders under an open menu were untested; seam=none; no_claim=drawing
// Opening a row's actions pins the selection to it, so a reorder never
// moves the highlight off the row the menu acts on.
func TestInboxActionsPinTheRow(t *testing.T) {
	a, b := withUID(demo("a"), "1"), withUID(demo("b"), "2")
	i := testInbox(t, hostWith(localHostName, a, b))
	i.keys(t, key(keyTab))
	ask := b
	ask.Attention = session.AttentionAction
	i.update(hostWith(localHostName, a, ask))
	rows := i.rows()
	if r := rows[i.selection(rows)]; r.key() != i.modeRow || r.rec.SessionID != "a" {
		t.Fatalf("selection moved to %s while a's menu is open", r.rec.SessionID)
	}
}

// Value: protects=the inbox brackets pastes while it runs and restores the mode on exit; fails_when=the enter or exit sequence drops 2004; why_new=no test checked the modes; seam=none; no_claim=terminals that ignore 2004
// The inbox turns bracketed paste on while it runs, and off again.
func TestInboxBracketsPastes(t *testing.T) {
	if !strings.Contains(inboxEnterSequence, "\x1b[?2004h") || !strings.Contains(inboxExitSequence, "\x1b[?2004l") {
		t.Fatalf("enter %q exit %q", inboxEnterSequence, inboxExitSequence)
	}
}

// Value: protects=a session started from the inbox runs in this directory, under the name typed, with codex's default model, and a daemon's refusal is a message, not an error; fails_when=createHere drops the name, cwd or model, or reports a refusal as a lost request; why_new=createHere had no test; seam=none; no_claim=the create row's keys (TestInboxCreate)
// The inbox's create asks for a session in this directory, named as typed,
// with codex's default model for codex alone; a refusal is a message.
func TestInboxCreateHere(t *testing.T) {
	f, c := newFakeRemote(t, protocol.Capabilities())
	cwd, err := resolveCwd("")
	if err != nil {
		t.Fatal(err)
	}
	type created struct {
		res     *session.CreateResult
		message string
		err     error
	}
	create := func(name, agent string) <-chan created {
		ch := make(chan created, 1)
		go func() {
			res, message, err := c.createHere(context.Background(), name, agent)
			ch <- created{res, message, err}
		}()
		return ch
	}
	wait := func(ch <-chan created) created {
		t.Helper()
		select {
		case r := <-ch:
			return r
		case <-time.After(10 * time.Second):
			t.Fatal("create did not finish")
			return created{}
		}
	}

	ch := create("fresh", "codex")
	r := f.next()
	req := r.req.CreateSession
	if req == nil || req.Cwd != cwd || req.Agent != "codex" || req.Name == nil || *req.Name != "fresh" || req.Model == nil || *req.Model != codexDefaultModel {
		t.Fatalf("codex create = %#v", r.req.CreateSession)
	}
	r.answer(&protocol.Response{CreateSession: &session.CreateResult{SessionID: "fresh", UID: "9", Cwd: cwd, Status: session.StatusRunning, Mode: session.ModeExecute}})
	if got := wait(ch); got.err != nil || got.message != "" || got.res == nil || got.res.UID != "9" {
		t.Fatalf("codex result = %+v", got)
	}

	ch = create("", "claude")
	r = f.next()
	if req := r.req.CreateSession; req == nil || req.Name != nil || req.Model != nil || req.Agent != "claude" {
		t.Fatalf("claude create = %#v", r.req.CreateSession)
	}
	r.answer(&protocol.Response{Error: &protocol.ErrorResponse{Message: "unknown agent `claude`"}})
	if got := wait(ch); got.err != nil || got.res != nil || got.message != "unknown agent `claude`" {
		t.Fatalf("refused result = %+v", got)
	}
}

// Value: protects=why this machine's daemon could not start is shown once as a failure; fails_when=the start probe's error is dropped or repeated per probe; why_new=the start error was replaced by the next probe; seam=none; no_claim=ensureDaemon itself
// A daemon that could not be started is a failure, once.
func TestInboxShowsWhyTheDaemonDidNotStart(t *testing.T) {
	i := testInbox(t, hostWith(localHostName))
	start := &hostProbe{name: localHostName, status: hostError, err: errors.New("agtd not found"), startFailed: true}
	i.update(start)
	i.update(&hostProbe{name: localHostName, status: hostNotRunning})
	if len(i.failures) != 1 || !strings.Contains(i.failures[0], "could not start agtd on this machine: agtd not found") {
		t.Fatalf("failures %q", i.failures)
	}
}

// Value: protects=a row's open menu closes when its actions change, so the highlight never slides onto remove; fails_when=clampMode only clamps the index; why_new=a session exiting under its open menu was untested; seam=none; no_claim=drawing
// A session that exits while its menu is open closes the menu, saying so.
func TestInboxMenuClosesWhenItsActionsChange(t *testing.T) {
	a := withUID(demo("a"), "1")
	i := testInbox(t, hostWith(localHostName, a))
	i.keys(t, key(keyTab))
	if i.mode != inboxActions {
		t.Fatalf("mode %v", i.mode)
	}
	// A probe that changes nothing leaves the menu as it was.
	i.keys(t, key(keyDown))
	index := i.modeIndex
	i.update(hostWith(localHostName, a))
	if i.mode != inboxActions || i.modeIndex != index || i.notice != nil {
		t.Fatalf("unchanged probe: %v %d %+v", i.mode, i.modeIndex, i.notice)
	}
	a.Status = session.StatusExited
	i.update(hostWith(localHostName, a))
	if i.mode != inboxBrowse || i.notice == nil || !strings.Contains(i.notice.message, "a changed; its menu closed") {
		t.Fatalf("after exit: %v %+v", i.mode, i.notice)
	}
}

// Value: protects=a create's refusal is certain, and its error is unknown only once the request was sent; fails_when=createResult swaps or drops the cases; why_new=runInboxEffect's create mapping was untested; seam=none; no_claim=createHere talking to a daemon (TestInboxCreateHere)
// What a create reports, by how it ended.
func TestCreateResult(t *testing.T) {
	res := &session.CreateResult{SessionID: "fresh"}
	lost := errors.New("connection lost")
	for _, tc := range []struct {
		name    string
		got     inboxResult
		err     string
		unknown bool
	}{
		{"created", createResult(res, "", nil, false), "", false},
		{"refused", createResult(nil, "no such agent", nil, false), "no such agent", false},
		{"never sent", createResult(nil, "", lost, false), "connection lost", false},
		{"lost after sending", createResult(nil, "", lost, true), "connection lost", true},
	} {
		if (tc.got.err == nil) != (tc.err == "") || (tc.got.err != nil && tc.got.err.Error() != tc.err) || tc.got.unknown != tc.unknown || !tc.got.create || (tc.err == "" && tc.got.created != res) {
			t.Errorf("%s: %+v", tc.name, tc.got)
		}
	}
}

// Value: protects=bare `agt --host auto` says auto only chooses a host for new; fails_when=runInbox treats auto as a host name; why_new=the inbox bypassed connect's check; seam=none; no_claim=none beyond protects
// `--host auto` is not a host the inbox can show.
func TestInboxRefusesHostAuto(t *testing.T) {
	a := &app{paths: testClient(t).paths, host: autoHostName}
	if err := a.runInbox(); err == nil || !strings.Contains(err.Error(), "only chooses a host for `agt new`") {
		t.Fatalf("err = %v", err)
	}
}

// Value: protects=a record without a UID is removed with no incarnation check, and an unreachable daemon is a certain failure; fails_when=checkIncarnation's early returns change; why_new=only the GetSession path was tested; seam=none; no_claim=QUIC dial failures (a local socket stands in)
// checkIncarnation's early returns: nothing to compare without a UID, and
// a daemon that cannot be reached sends nothing, so the failure is certain.
func TestInboxRemoveWithoutACheck(t *testing.T) {
	older := slices.DeleteFunc(protocol.Capabilities(), func(c string) bool { return c == protocol.CapKillExpectUID })
	f, c := newFakeRemote(t, older)
	p := hostWith("dev")
	p.host = c.host
	rec := demo("demo") // from a daemon without session UIDs
	done := make(chan inboxResult, 1)
	go func() {
		done <- (&app{paths: c.paths}).runInboxEffect(context.Background(), inboxEffect{remove: &inboxRow{host: p, rec: &rec}})
	}()
	r := f.next()
	if r.req.KillSession == nil {
		t.Fatalf("first request = %#v, want KillSession", r.req)
	}
	r.answer(&protocol.Response{KillSession: &protocol.KillSessionResult{Removed: true}})
	if res := <-done; res.err != nil {
		t.Fatalf("result = %+v", res)
	}

	local := hostWith(localHostName) // no daemon at the test root's socket
	withID := withUID(demo("demo"), "1")
	res := (&app{paths: testClient(t).paths}).runInboxEffect(context.Background(), inboxEffect{remove: &inboxRow{host: local, rec: &withID}})
	if res.err == nil || res.unknown {
		t.Fatalf("unreachable daemon: %+v", res)
	}
}

// Value: protects=the narrowed inbox's status never writes a daemon's session name raw; fails_when=narrowNotice skips escapeControls; why_new=the status names rows from a daemon; seam=none; no_claim=drawing
// The narrow notice escapes what it names.
func TestInboxNarrowNoticeIsEscaped(t *testing.T) {
	evil := withUID(demo("de\x1b[2Jmo\nx"), "1")
	i := testInbox(t, hostWith(localHostName, evil))
	i.pending[rowKey{host: localHostName, uid: "1"}] = "removing…"
	i.leaving = &inboxLeave{}
	got := narrowNotice(200, i.narrowStatus())[0]
	if strings.ContainsAny(got, "\x1b\n") || !strings.Contains(got, "finishing removing") {
		t.Fatalf("notice %q", got)
	}
}

// Value: protects=a remove is confirmed only by a key that comes inboxAcknowledgeAfter after its confirmation was drawn on screen; fails_when=handleConfirmKey takes Enter or y sooner, or while it is cropped; why_new=keys in one read, key repeat and queued ssh bursts confirmed unseen; seam=none; no_claim=runInbox's drawing between reads
// A confirmation takes Enter or y only once it has been on screen for
// inboxAcknowledgeAfter: not keys in the read that opened it, not keys
// right after its first drawing (key repeat, a queued burst), and not
// while a short terminal crops it, which says so.
func TestInboxConfirmsOnlyWhatWasDrawn(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	i := testInbox(t, hostWith(localHostName, withUID(demo("a"), "1")))
	i.now = func() time.Time { return now }
	i.keys(t, key(keyTab))
	for _, ev := range []keyEvent{char('2'), key(keyEnter), char('y')} { // one read
		i.handleKey(ev)
	}
	if i.mode != inboxConfirm || len(i.effects) != 0 {
		t.Fatalf("burst confirmed: %v %+v", i.mode, i.effects)
	}
	i.render(100, 30)
	now = now.Add(30 * time.Millisecond) // key repeat
	i.handleKey(key(keyEnter))
	i.render(100, 30)
	if len(i.effects) != 0 {
		t.Fatalf("confirmed 30 ms after drawing: %+v", i.effects)
	}
	now = now.Add(inboxAcknowledgeAfter)
	i.handleKey(key(keyEnter))
	if len(i.effects) != 1 || i.effects[0].remove == nil {
		t.Fatalf("seen confirmation: %+v", i.effects)
	}

	i = testInbox(t, hostWith(localHostName, withUID(demo("a"), "1")))
	i.now = func() time.Time { return now }
	i.keys(t, key(keyTab), char('2'))
	i.render(100, 5) // only the input box and hints fit
	now = now.Add(time.Second)
	i.handleKey(key(keyEnter))
	if len(i.effects) != 0 || i.notice == nil || !strings.Contains(i.notice.message, "taller") {
		t.Fatalf("cropped confirmation: %+v %+v", i.effects, i.notice)
	}
}

// Value: protects=a failure cut off a short terminal does not count as seen; fails_when=failuresShown is set before the frame is cropped; why_new=render marked failures drawn before cropping; seam=none; no_claim=terminal heights between those tried
// A failure that a short terminal crops away is not acknowledged by a key.
func TestInboxCroppedFailuresAreNotSeen(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	i := testInbox(t, hostWith(localHostName, withUID(demo("a"), "1")))
	i.now = func() time.Time { return now }
	i.fail("could not remove a: refused")
	i.render(100, 4)
	now = now.Add(time.Second)
	i.handleKey(key(keyDown))
	if len(i.failures) != 1 {
		t.Fatalf("a cropped failure was acknowledged: %q", i.failures)
	}
	i.render(100, 30)
	now = now.Add(time.Second)
	i.handleKey(key(keyDown))
	if len(i.failures) != 0 {
		t.Fatalf("a drawn failure was not acknowledged: %q", i.failures)
	}
}

// Value: protects=a row's menu or confirmation closes when the filter stops showing the row; fails_when=clampMode checks all rows, not the shown ones; why_new=a filtered-out row kept its remove confirmation; seam=none; no_claim=drawing
// A confirmation whose row the filter stops showing closes, saying so.
func TestInboxFilteredOutRowClosesItsMenu(t *testing.T) {
	msg := "xyzzy"
	a := withUID(demo("a"), "1")
	a.StatusMsg = &msg
	i := testInbox(t, hostWith(localHostName, a))
	i.typed(t, "xyzzy")
	i.keys(t, key(keyTab), char('2'))
	if i.mode != inboxConfirm {
		t.Fatalf("mode %v", i.mode)
	}
	other := "haystack"
	a.StatusMsg = &other
	i.update(hostWith(localHostName, a))
	if i.mode != inboxBrowse || i.notice == nil || !strings.Contains(i.notice.message, "no longer shown") {
		t.Fatalf("after the filter lost it: %v %+v", i.mode, i.notice)
	}
	i.handleKey(key(keyEnter))
	if slices.ContainsFunc(i.effects, func(e inboxEffect) bool { return e.remove != nil }) {
		t.Fatalf("enter removed an unseen row: %+v", i.effects)
	}
}

// Value: protects=a confirmation hidden by a narrowed terminal waits its full time again once shown; fails_when=hiding keeps confirmShown; why_new=a narrowed inbox never rendered, so the old time survived; seam=none; no_claim=runInbox calling setHidden
// Hiding the inbox restarts a confirmation's wait.
func TestInboxHidingRestartsAConfirmation(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	i := testInbox(t, hostWith(localHostName, withUID(demo("a"), "1")))
	i.now = func() time.Time { return now }
	i.keys(t, key(keyTab), char('2'))
	now = now.Add(time.Second)
	i.setHidden(true)
	i.setHidden(false)
	i.render(100, 30)
	i.handleKey(key(keyEnter))
	if len(i.effects) != 0 {
		t.Fatalf("confirmed at once after being hidden: %+v", i.effects)
	}
	now = now.Add(inboxAcknowledgeAfter)
	i.handleKey(key(keyEnter))
	if len(i.effects) != 1 {
		t.Fatalf("not confirmed after its wait: %+v", i.effects)
	}
}

// Value: protects=a return from an attachment says how the session ended or why it could not attach, asks its host again, and leaves nothing waiting to leave; fails_when=attached drops the end, the error or the reprobe; why_new=the inbox used to exit after an attachment; seam=none; no_claim=the attachment itself (TestInboxEndToEnd)
// Back from an attachment, the inbox says how the session ended, or why
// it could not attach, and asks the session's host again.
func TestInboxAfterAnAttachment(t *testing.T) {
	dev := &transport.Host{Name: "dev"}
	i := testInbox(t, hostWith(localHostName, withUID(demo("a"), "1")))
	i.leaving = &inboxLeave{&inboxOutcome{id: "a", uid: "1"}}
	exit := int32(0)
	i.attached(&inboxOutcome{id: "a", uid: "1"}, &protocol.SessionEnded{Status: session.StatusExited, ExitCode: &exit}, nil)
	if i.leaving != nil || i.notice == nil || !strings.HasPrefix(i.notice.message, "a ended: ") || len(i.effects) != 1 || i.effects[0].reprobe != localHostName {
		t.Fatalf("after the session ended: %+v %+v %+v", i.leaving, i.notice, i.effects)
	}
	i.effects = nil
	i.attached(&inboxOutcome{host: dev, id: "demo"}, nil, errors.New("connection refused"))
	if len(i.failures) != 1 || i.failures[0] != "could not attach to dev/demo: connection refused" || len(i.effects) != 1 || i.effects[0].reprobe != "dev" {
		t.Fatalf("after a failed attach: %q %+v", i.failures, i.effects)
	}
	i.effects, i.notice = nil, nil
	i.attached(&inboxOutcome{id: "a", uid: "1"}, nil, nil) // a detach
	if i.notice != nil || len(i.failures) != 1 || len(i.effects) != 1 {
		t.Fatalf("after a detach: %+v %q %+v", i.notice, i.failures, i.effects)
	}
}
