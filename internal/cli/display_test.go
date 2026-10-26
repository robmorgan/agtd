package cli

import (
	"strings"
	"testing"
	"time"

	"agtd.sh/internal/session"
)

func TestFormatElapsedUsesLargestUnit(t *testing.T) {
	for seconds, want := range map[int64]string{0: "0s", 59: "59s", 60: "1m", 3599: "59m", 3600: "1h", 86399: "23h", 86400: "1d", 3 * 86400: "3d"} {
		if got := formatElapsed(seconds); got != want {
			t.Errorf("%d: got %q want %q", seconds, got, want)
		}
	}
}

func TestElapsedLabelUsesExitTimeAndClampsNegative(t *testing.T) {
	now := time.Now()
	s := demoWith("a", session.StatusExited)
	s.CreatedAt = now.Add(-5 * time.Hour)
	exited := s.CreatedAt.Add(90 * time.Minute)
	s.ExitedAt = &exited
	if got := elapsedLabel(&s, now); got != "1h" {
		t.Fatalf("exited: %q", got)
	}
	s.ExitedAt = nil
	s.CreatedAt = now.Add(time.Hour)
	if got := elapsedLabel(&s, now); got != "0s" {
		t.Fatalf("future start: %q", got)
	}
}

func TestDisplayRowMarksAttentionFromActionOrFailure(t *testing.T) {
	now := time.Now()
	s := demo("a")
	if buildDisplayRow(&s, now).needsAttention {
		t.Fatal("running session needs attention")
	}
	s.Attention = session.AttentionAction
	if !buildDisplayRow(&s, now).needsAttention {
		t.Fatal("action not marked")
	}
	f := demoWith("f", session.StatusFailed)
	if !buildDisplayRow(&f, now).needsAttention {
		t.Fatal("failure not marked")
	}
}

func TestRunStatesAndIcons(t *testing.T) {
	for status, want := range map[session.Status]struct {
		run  runState
		icon string
	}{
		session.StatusCreating:         {runStarting, "●"},
		session.StatusRunning:          {runRunning, "●"},
		session.StatusExited:           {runExited, "○"},
		session.StatusFailed:           {runFailed, "✖"},
		session.StatusUnknownRecovered: {runRecovered, "○"},
	} {
		s := demoWith("a", status)
		if got := sessionRunState(&s); got != want.run || runIcon(got) != want.icon {
			t.Errorf("%s: %v %q", status, got, runIcon(got))
		}
	}
	if !strings.HasPrefix(styleRun("x", runRunning), ansiRunning) || !strings.HasPrefix(styleRun("x", runFailed), ansiFailed) ||
		!strings.HasPrefix(styleRun("x", runExited), ansiInactive) {
		t.Fatal("run colours")
	}
}

func TestPathsWithControlCharactersAreEscaped(t *testing.T) {
	if got := escapeControls("/tmp/\x1b]0;evil\x07\tx\u0085"); got != `/tmp/\u{1b}]0;evil\u{7}\tx\u{85}` {
		t.Fatalf("got %q", got)
	}
	if got := escapeControls("/tmp/plain"); got != "/tmp/plain" {
		t.Fatalf("got %q", got)
	}
	// Value: protects=invisible format characters (unicode.Cf: bidi
	// overrides, zero-width) escaped like controls, so a path cannot
	// reorder or hide what the line appears to say; fails_when=
	// escapeControls narrows back to unicode.IsControl only; why_new=the
	// existing cases cover only control characters; seam=none
	if got := escapeControls("/tmp/a‮b​c"); got != `/tmp/a\u{202e}b\u{200b}c` {
		t.Fatalf("format characters: got %q", got)
	}
}

func TestDisplayCwdAbbreviatesHome(t *testing.T) {
	for _, tc := range []struct{ cwd, home, want string }{
		{"/home/t", "/home/t", "~"},
		{"/home/t/src", "/home/t/", "~/src"},
		{"/home/tom", "/home/t", "/home/tom"},
		{"/srv", "", "/srv"},
	} {
		if got := displayCwd(tc.cwd, tc.home); got != tc.want {
			t.Errorf("%q in %q: got %q want %q", tc.cwd, tc.home, got, tc.want)
		}
	}
}

func TestOrderedSessionsKeepLiveFirstThenEnded(t *testing.T) {
	now := time.Now()
	running := demo("running")
	running.UpdatedAt = now.Add(-2 * time.Minute)
	creating := demoWith("creating", session.StatusCreating)
	creating.UpdatedAt = now.Add(-time.Minute)
	failed := demoWith("failed", session.StatusFailed)
	lost := demoWith("lost", session.StatusUnknownRecovered)
	exited := demoWith("exited", session.StatusExited)
	needsAction := demoWith("needs-action", session.StatusExited)
	needsAction.Attention = session.AttentionAction
	needsAction.UpdatedAt = now.Add(-5 * time.Minute)
	var got []string
	for _, s := range orderedSessions([]session.Record{lost, exited, failed, running, needsAction, creating}) {
		got = append(got, s.SessionID)
	}
	// Starting sorts with running; ended ones go exited, failed, lost.
	if strings.Join(got, " ") != "creating running exited needs-action failed lost" {
		t.Fatalf("got %v", got)
	}
}

func TestCellFormatting(t *testing.T) {
	for _, tc := range []struct {
		in    string
		width int
		want  string
	}{{"/a/b", 6, "/a/b  "}, {"/srv/repo/checkout", 10, "...heckout"}, {"/srv/repo", 3, "epo"}} {
		if got := formatPathCell(tc.in, tc.width); got != tc.want {
			t.Errorf("path %q/%d: %q", tc.in, tc.width, got)
		}
	}
	if got := formatCell("abcdefgh", 6); got != "abc..." {
		t.Fatalf("cell: %q", got)
	}
}

func TestHeader(t *testing.T) {
	// Tests do not run on a terminal, so the header is plain.
	if got := header(); got != headerText {
		t.Fatalf("got %q", got)
	}
	if got := stripANSI(colorHeader()); got != headerText {
		t.Fatalf("coloured header text: %q", got)
	}
	if !strings.Contains(colorHeader(), fgRGB(primaryBlue)+"agtd") {
		t.Fatal("agtd is not in the primary blue")
	}
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	for want, vars := range map[bool]map[string]string{
		true:  {"COLORTERM": "truecolor"},
		false: {"COLORTERM": "truecolor", "NO_COLOR": "1"},
	} {
		if got := supportsTrueColor(env(vars)); got != want {
			t.Errorf("%v: got %v", vars, got)
		}
	}
	if supportsTrueColor(env(map[string]string{"TERM": "xterm-256color"})) {
		t.Error("256 colours are not true colour")
	}
}

// needing returns a session with pending attention raised ago before now.
func needing(id string, status session.Status, level session.AttentionLevel, summary string, ago time.Duration) session.Record {
	s := demoWith(id, status)
	at := time.Now().Add(-ago)
	s.Attention, s.AttentionSummary, s.AttentionAt = level, &summary, &at
	return s
}

// The inbox's order: needs you, working, running, idle, then exited,
// failed and lost; newest first within each.
func TestOrderedSessionsPutLiveSessionsNeedingActionFirst(t *testing.T) {
	older := needing("older-ask", session.StatusRunning, session.AttentionAction, "bell", 5*time.Minute)
	newer := needing("newer-ask", session.StatusRunning, session.AttentionAction, "Approve?", time.Minute)
	notice := needing("went-idle", session.StatusRunning, session.AttentionNotice, "idle", time.Minute)
	notice.Activity = session.ActivityIdle
	working := demo("working")
	working.Activity = session.ActivityWorking
	plain := demo("plain")
	plain.UpdatedAt = time.Now().Add(-time.Hour)
	failed := needing("failed", session.StatusFailed, session.AttentionAction, "exit 1", time.Minute)
	finished := needing("finished", session.StatusExited, session.AttentionNotice, "finished", time.Minute)
	seen := demoWith("seen", session.StatusExited)
	var got []string
	for _, s := range orderedSessions([]session.Record{seen, finished, failed, plain, working, notice, older, newer}) {
		got = append(got, s.SessionID)
	}
	if strings.Join(got, " ") != "newer-ask older-ask working plain went-idle seen finished failed" {
		t.Fatalf("got %v", got)
	}
}

func TestActivityText(t *testing.T) {
	now := time.Now()
	s := demo("a")
	if got := activityText(&s, now); got != "running" {
		t.Fatalf("unknown activity: %q", got)
	}
	s.Activity = session.ActivityIdle
	last := now.Add(-3 * time.Minute)
	s.LastOutputAt = &last
	if got := activityText(&s, now); got != "idle 3m" {
		t.Fatalf("idle: %q", got)
	}
	s.Activity, s.AttentionAt = session.ActivityWaiting, &last
	if got := activityText(&s, now); got != "waiting 3m" {
		t.Fatalf("waiting: %q", got)
	}
	s.Activity = session.ActivityBlocked
	if got := activityText(&s, now); got != "blocked 3m" {
		t.Fatalf("blocked: %q", got)
	}
	s.Activity = session.ActivityWorking
	progress := 47
	s.StatusProgress = &progress
	if got := activityText(&s, now); got != "working 47%" {
		t.Fatalf("working with progress: %q", got)
	}
	// 100% would overflow the column and ends at once anyway.
	progress = 100
	if got := activityText(&s, now); got != "working" {
		t.Fatalf("working at 100%%: %q", got)
	}
	s.StatusProgress = nil
	s.Activity = "something-new"
	if got := activityText(&s, now); got != "something-new" {
		t.Fatalf("unknown: %q", got)
	}
	e := demoWith("e", session.StatusExited)
	e.Activity = session.ActivityWaiting
	if got := activityText(&e, now); got != "exited" {
		t.Fatalf("exited: %q", got)
	}
}

func TestPrintSessionShowsActivityAndAttention(t *testing.T) {
	s := needing("fix", session.StatusRunning, session.AttentionAction, "Claude needs your permission", 2*time.Minute)
	now := s.AttentionAt.Add(2 * time.Minute)
	s.CreatedAt = now.Add(-time.Hour)
	fg, title := "claude", "✳ Fix tests"
	s.Activity, s.Foreground, s.Title, s.LastOutputAt = session.ActivityWaiting, &fg, &title, s.AttentionAt
	var b strings.Builder
	printSession(&b, &s, now)
	for _, want := range []string{"elapsed: 1h\n", "activity: waiting 2m\n", "foreground: claude\n", "title: ✳ Fix tests\n",
		"attention: action\n", "attention_summary: Claude needs your permission\n", "attention_since: 2m ago\n"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("missing %q in\n%s", want, b.String())
		}
	}
	details := sessionDetails(&s, now)
	for _, want := range []string{"activity   waiting 2m", "foreground claude", "attention  action: Claude needs your permission"} {
		if !strings.Contains(details, want) {
			t.Errorf("details lack %q:\n%s", want, details)
		}
	}
}
func TestSessionList(t *testing.T) {
	now := time.Now()
	rendered := strings.Join(renderSessionListLines([]session.Record{demo("alpha")}, 120, now), "\n")
	for _, want := range []string{"RUN", "NAME", "CWD", "/tmp/alpha", "●"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(rendered, "agtd - ") || strings.Contains(rendered, "› ") {
		t.Fatal("list has a header or selector")
	}
	empty := strings.Join(renderSessionListLines(nil, 120, now), "\n")
	if !strings.Contains(empty, "No sessions.") || !strings.Contains(empty, "RUN") {
		t.Fatalf("empty %q", empty)
	}
	s := demo("alpha")
	s.SessionID = "a-very-long-session-name-that-should-be-truncated-in-the-fixed-width-column"
	s.Cwd = "/srv/checkouts/a-very-long-directory-name/that-should-be-truncated"
	summary := "needs manual review because a long follow-up summary is present"
	s.AttentionSummary = &summary
	wide := renderSessionListLines([]session.Record{s}, 120, now)
	narrow := strings.Join(renderSessionListLines([]session.Record{s}, 80, now), "\n")
	if len(wide) != 2 || strings.Contains(wide[1], summary) || !strings.Contains(wide[1], "a-very") || !strings.Contains(wide[1], "that-should-be-truncated") {
		t.Fatalf("wide %q", wide)
	}
	if !strings.Contains(narrow, "...") || !strings.Contains(narrow, "that-should-be-truncated") {
		t.Fatalf("narrow %q", narrow)
	}
	r := demoWith("beta", session.StatusUnknownRecovered)
	if got := strings.Join(renderSessionListLines([]session.Record{demo("alpha"), r}, 120, now), "\n"); strings.Contains(got, " • ") {
		t.Fatal("list has a legend")
	}
	for _, width := range []int{200, 80} {
		l := listLayout(width, false)
		header := stripANSI(renderSessionListLines(nil, width, now)[0])
		if got, want := runeLen(header), 10+l.run+l.age+l.name+l.activity+l.cwd; got != want {
			t.Errorf("width %d: header is %d wide, want %d", width, got, want)
		}
	}
}

func TestSessionListShowsAttention(t *testing.T) {
	ask := needing("asks", session.StatusRunning, session.AttentionAction, "Claude needs your permission to use Bash", time.Minute)
	ask.Activity = session.ActivityWaiting
	now := ask.AttentionAt.Add(time.Minute)
	quiet := demo("quiet")
	quiet.Activity = session.ActivityIdle
	quiet.LastOutputAt = &now
	lines := renderSessionListLines([]session.Record{quiet, ask}, 160, now)
	header := stripANSI(lines[0])
	if !strings.Contains(header, "ACTIVITY") || !strings.Contains(header, "ATTENTION") {
		t.Fatalf("header %q", header)
	}
	first := stripANSI(lines[1])
	if !strings.Contains(first, "⚠") || !strings.Contains(first, "asks") || !strings.Contains(first, "waiting 1m") ||
		!strings.Contains(first, "Claude needs your permission to use Bash") || !strings.Contains(lines[1], ansiAction) {
		t.Fatalf("first row %q", first)
	}
	if second := stripANSI(lines[2]); !strings.Contains(second, "quiet") || !strings.Contains(second, "idle 0s") || strings.Contains(second, "⚠") {
		t.Fatalf("second row %q", second)
	}
	l := listLayout(160, true)
	if got, want := runeLen(header), 12+l.run+l.age+l.name+l.activity+l.cwd+l.summary; got != want || l.summary < sessionListSummaryMinWidth {
		t.Fatalf("header is %d wide, want %d (summary %d)", got, want, l.summary)
	}
	// Narrow terminals drop the summary before anything else.
	if narrow := listLayout(60, true); narrow.summary != 0 {
		t.Fatalf("narrow summary %d", narrow.summary)
	}
}
