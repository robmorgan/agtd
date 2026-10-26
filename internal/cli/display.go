package cli

import (
	"fmt"
	"math"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"agtd.sh/internal/session"
)

const (
	ansiReset    = "\x1b[0m"
	ansiRunning  = "\x1b[32m"
	ansiFailed   = "\x1b[31m"
	ansiInactive = "\x1b[90m"
	ansiEmphasis = "\x1b[1m"
	ansiDimText  = "\x1b[2m\x1b[90m"
	ansiAction   = "\x1b[33m"
)

// `agt list`

const (
	// listHeaderStyle is the header row's: white on grey.
	listHeaderStyle = "\x1b[48;2;62;63;71m\x1b[38;2;255;255;255m"

	sessionListDefaultWidth    = 120
	sessionListSummaryWidth    = 160
	sessionListRunWidth        = 3
	sessionListAgeWidth        = 5
	sessionListActivityWidth   = 11
	sessionListSummaryMinWidth = 20
	sessionListNameMinWidth    = 10
	sessionListNameMaxWidth    = 26
	sessionListNameFloorWidth  = 8
	sessionListCwdMinWidth     = 16
	sessionListCwdMaxWidth     = 48
	sessionListCwdSummaryWidth = 32
	sessionListCwdFloorWidth   = 10
	sessionListStructuralWidth = 10
)

type sessionListLayout struct {
	visible, run, age, name, activity, cwd int
	// summary is the attention column's width; 0 hides it.
	summary int
}

// fitColumns grows cwd then name into spare width, or shrinks name then cwd
// down to their floors when there is too little.
func fitColumns(name, cwd *int, available, fixed int) {
	total := fixed + *name + *cwd
	if available >= total {
		remaining := available - total
		grow := func(w *int, maxWidth int) {
			add := min(remaining, max(maxWidth-*w, 0))
			*w += add
			remaining -= add
		}
		grow(cwd, sessionListCwdMaxWidth)
		grow(name, sessionListNameMaxWidth)
		return
	}
	deficit := total - available
	shrink := func(w *int, floor int) {
		remove := min(deficit, max(*w-floor, 0))
		*w -= remove
		deficit -= remove
	}
	shrink(name, sessionListNameFloorWidth)
	shrink(cwd, sessionListCwdFloorWidth)
}

// listLayout fits the columns of `agt list` into width. With summaries
// (some session has attention to explain) an ATTENTION column takes what
// the others leave, and the list may be wider.
func listLayout(width int, summaries bool) sessionListLayout {
	limit := sessionListDefaultWidth
	if summaries {
		limit = sessionListSummaryWidth
	}
	l := sessionListLayout{
		visible:  min(max(width, 1), limit),
		run:      sessionListRunWidth,
		age:      sessionListAgeWidth,
		name:     sessionListNameMinWidth,
		activity: sessionListActivityWidth,
		cwd:      sessionListCwdMinWidth,
	}
	fixed := sessionListStructuralWidth + l.run + l.age + l.activity
	if !summaries {
		fitColumns(&l.name, &l.cwd, l.visible, fixed)
		return l
	}
	// The attention column, with its separator, gets what is left once
	// name and cwd have their usual widths; without room for it, they keep
	// the room. `agt status` shows the summary in full.
	fitColumns(&l.name, &l.cwd, l.visible-sessionListSummaryMinWidth, fixed+2)
	if l.name < sessionListNameMinWidth || l.cwd < sessionListCwdMinWidth {
		l.name, l.cwd = sessionListNameMinWidth, sessionListCwdMinWidth
		fitColumns(&l.name, &l.cwd, l.visible, fixed)
		return l
	}
	// What needs attention matters more than the end of a long path.
	l.cwd = min(l.cwd, sessionListCwdSummaryWidth)
	l.summary = l.visible - fixed - 2 - l.name - l.cwd
	return l
}

func renderSessionListLines(sessions []session.Record, width int, now time.Time) []string {
	return renderSessionList(sessions, nil, width, now)
}

// renderSessionList renders `agt list`. hosts, when not nil, names the
// host of each session (hosts[i] for sessions[i]) and adds a HOST column,
// for `agt list --all`.
func renderSessionList(sessions []session.Record, hosts []string, width int, now time.Time) []string {
	index := make(map[*session.Record]int, len(sessions))
	for i := range sessions {
		index[&sessions[i]] = i
	}
	rows := make([]displayRow, 0, len(sessions))
	rowHosts := make([]string, 0, len(sessions))
	summaries := false
	hostWidth := 0
	if hosts != nil {
		hostWidth = len("HOST")
		for _, h := range hosts {
			hostWidth = max(hostWidth, runeLen(h))
		}
	}
	for _, s := range orderedSessions(sessions) {
		row := buildDisplayRow(s, now)
		summaries = summaries || row.summary != ""
		rows = append(rows, row)
		if hosts != nil {
			rowHosts = append(rowHosts, hosts[index[s]])
		}
	}
	l := listLayout(width-hostWidth-min(hostWidth, 2), summaries)
	hostCell := func(text string) string {
		if hostWidth == 0 {
			return ""
		}
		return formatCell(text, hostWidth) + "  "
	}
	headerRow := fmt.Sprintf("  %s  %s  %s%s  %s  %s", formatCell("RUN", l.run), formatCell("AGE", l.age), hostCell("HOST"), formatCell("NAME", l.name),
		formatCell("ACTIVITY", l.activity), formatCell("CWD", l.cwd))
	if l.summary > 0 {
		headerRow += "  " + formatCell("ATTENTION", l.summary)
	}
	visible := l.visible + hostWidth + min(hostWidth, 2)
	lines := []string{listHeaderStyle + takeRunes(headerRow, visible) + ansiReset}
	if len(sessions) == 0 {
		return append(lines, truncateCell("  No sessions.", visible))
	}
	for i, row := range rows {
		icon, iconStyle := rowIcon(row)
		host := ""
		if hosts != nil {
			host = styleAge(hostCell(rowHosts[i]))
		}
		line := fmt.Sprintf("  %s  %s  %s%s  %s  %s",
			iconStyle+formatCell(icon, l.run)+ansiReset,
			styleAge(formatCell(row.age, l.age)),
			host,
			styleName(formatCell(row.name, l.name)),
			formatCell(row.activity, l.activity),
			styleCwd(formatPathCell(row.cwd, l.cwd)))
		if l.summary > 0 && row.summary != "" {
			line += "  " + styleSummary(truncateCell(row.summary, l.summary), row.attention)
		}
		lines = append(lines, line)
	}
	return lines
}

// runState is how a session is shown: its status, with creating shown as
// starting and unknown_recovered as recovered.
type runState int

const (
	runStarting runState = iota
	runRunning
	runExited
	runFailed
	runRecovered
)

func sessionRunState(s *session.Record) runState {
	switch s.Status {
	case session.StatusCreating:
		return runStarting
	case session.StatusRunning:
		return runRunning
	case session.StatusFailed:
		return runFailed
	case session.StatusUnknownRecovered:
		return runRecovered
	}
	return runExited
}

type displayRow struct {
	run            runState
	age, name, cwd string
	needsAttention bool
	// attention is the session's pending attention, and summary its
	// text when it is above info (else "").
	attention session.AttentionLevel
	summary   string
	activity  string
}

func buildDisplayRow(s *session.Record, now time.Time) displayRow {
	run := sessionRunState(s)
	row := displayRow{
		run:            run,
		age:            elapsedLabel(s, now),
		name:           escapeControls(s.SessionID),
		cwd:            displayCwd(s.Cwd, os.Getenv("HOME")),
		needsAttention: run == runFailed || s.Attention == session.AttentionAction,
		attention:      s.Attention,
		activity:       activityText(s, now),
	}
	if s.Attention.Rank() > 0 && s.AttentionSummary != nil {
		row.summary = escapeControls(strings.Join(strings.Fields(*s.AttentionSummary), " "))
	}
	return row
}

// activityText is what a session is doing, for lists: "working" (with the
// progress the program reported, if any), "idle 5m" (since its last
// output), "waiting" or "blocked" (since they asked), or for one that is
// not running its state.
func activityText(s *session.Record, now time.Time) string {
	switch sessionRunState(s) {
	case runStarting:
		return "starting"
	case runExited, runFailed, runRecovered:
		return "exited"
	}
	switch s.Activity {
	case session.ActivityUnknown:
		return "running"
	case session.ActivityWorking:
		if p := s.StatusProgress; p != nil && *p > 0 && *p < 100 {
			return fmt.Sprintf("working %d%%", *p)
		}
	case session.ActivityIdle:
		if s.LastOutputAt != nil {
			return "idle " + formatElapsed(max(int64(now.Sub(*s.LastOutputAt)/time.Second), 0))
		}
	case session.ActivityWaiting, session.ActivityBlocked:
		if s.AttentionAt != nil {
			return string(s.Activity) + " " + formatElapsed(max(int64(now.Sub(*s.AttentionAt)/time.Second), 0))
		}
	}
	return escapeControls(string(s.Activity))
}

// rowIcon is the icon a list shows for a session: its run state, or a
// warning sign for a live session that needs action.
func rowIcon(row displayRow) (icon, style string) {
	if row.attention == session.AttentionAction && (row.run == runRunning || row.run == runStarting) {
		return "⚠", ansiAction
	}
	return runIcon(row.run), runStyle(row.run)
}

// styleSummary styles an attention summary: action stands out.
func styleSummary(text string, a session.AttentionLevel) string {
	if a == session.AttentionAction {
		return ansiAction + text + ansiReset
	}
	return text
}

// displayCwd shortens a session cwd for list views by replacing the home
// directory with ~. The full path is still shown by `agt status`.
func displayCwd(cwd, home string) string {
	cwd = escapeControls(cwd)
	home = strings.TrimRight(home, "/")
	if home == "" {
		return cwd
	}
	rest, ok := strings.CutPrefix(cwd, home)
	switch {
	case !ok:
		return cwd
	case rest == "":
		return "~"
	case strings.HasPrefix(rest, "/"):
		return "~" + rest
	}
	return cwd
}

// escapeControls makes control characters in a path visible (\t, \u{1b})
// rather than letting a terminal act on them. Anything shown from a path
// goes through it first.
func escapeControls(text string) string {
	// Format characters (unicode.Cf: bidirectional overrides, zero-width
	// characters) are escaped like controls: invisible, they can reorder
	// or hide what the line appears to say.
	suspect := func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }
	if !strings.ContainsFunc(text, suspect) {
		return text
	}
	var b strings.Builder
	for _, r := range text {
		switch {
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case suspect(r):
			fmt.Fprintf(&b, `\u{%x}`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func runIcon(r runState) string {
	switch r {
	case runStarting, runRunning:
		return "●"
	case runFailed:
		return "✖"
	}
	return "○"
}

func styleRun(text string, r runState) string {
	switch r {
	case runStarting, runRunning:
		return ansiRunning + text + ansiReset
	case runFailed:
		return ansiFailed + text + ansiReset
	}
	return ansiInactive + text + ansiReset
}

func styleAge(text string) string  { return ansiDimText + text + ansiReset }
func styleName(text string) string { return ansiEmphasis + text + ansiReset }
func styleCwd(text string) string  { return ansiDimText + text + ansiReset }

// elapsedLabel is how long a session ran (or has been running): 42s, 5m,
// 3h, 2d.
func elapsedLabel(s *session.Record, now time.Time) string {
	end := now
	if s.ExitedAt != nil {
		end = *s.ExitedAt
	}
	return formatElapsed(max(int64(end.Sub(s.CreatedAt)/time.Second), 0))
}

func formatElapsed(seconds int64) string {
	switch {
	case seconds < 60:
		return fmt.Sprintf("%ds", seconds)
	case seconds < 60*60:
		return fmt.Sprintf("%dm", seconds/60)
	case seconds < 60*60*24:
		return fmt.Sprintf("%dh", seconds/(60*60))
	}
	return fmt.Sprintf("%dd", seconds/(60*60*24))
}

// inboxSortBucket orders sessions for every list (the inbox, `agt ls`,
// `agt ls --all`, the overlay's switcher): live sessions that need you
// first, then working ones, running ones that report nothing (and those
// still starting), idle ones, and then the ended ones: exited, failed and
// lost. Ended sessions stay below live ones even when they need action:
// only removing them clears it, and they would otherwise crowd out the
// sessions the user can still act in.
func inboxSortBucket(s *session.Record) int {
	switch s.Status {
	case session.StatusCreating, session.StatusRunning:
	case session.StatusFailed:
		return bucketFailed
	case session.StatusUnknownRecovered:
		return bucketLost
	default:
		return bucketExited
	}
	switch {
	case s.Attention == session.AttentionAction:
		return bucketNeedsYou
	case s.Status == session.StatusCreating:
		return bucketRunning
	case s.Activity == session.ActivityWorking:
		return bucketWorking
	case s.Activity == session.ActivityIdle:
		return bucketIdle
	}
	return bucketRunning
}

// inboxSortBucket's buckets, in list order.
const (
	bucketNeedsYou = iota + 1
	bucketWorking
	bucketRunning
	bucketIdle
	bucketExited
	bucketFailed
	bucketLost
)

// sortTime orders sessions within a bucket, newest first: when their
// attention was raised, or else when they last changed.
func sortTime(s *session.Record) time.Time {
	if s.Attention.Rank() > 0 && s.AttentionAt != nil {
		return *s.AttentionAt
	}
	return s.UpdatedAt
}

// orderedSessions sorts by bucket, most recent first within one.
func orderedSessions(sessions []session.Record) []*session.Record {
	out := make([]*session.Record, len(sessions))
	for i := range sessions {
		out[i] = &sessions[i]
	}
	slices.SortStableFunc(out, func(a, b *session.Record) int {
		if d := inboxSortBucket(a) - inboxSortBucket(b); d != 0 {
			return d
		}
		return sortTime(b).Compare(sortTime(a))
	})
	return out
}

func sessionSearchText(s *session.Record) string {
	return fmt.Sprintf("%s %s %s %s %s %s", s.SessionID, s.Agent, s.Cwd, s.Status, s.Attention, s.Activity)
}

func matchesQuery(haystack, query string) bool {
	query = strings.TrimSpace(query)
	if query == "" {
		return true
	}
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(query))
}

// Widths below count runes, as a terminal counts cells for the ASCII and
// most of the text shown here.

func runeLen(s string) int { return utf8.RuneCountInString(s) }

func takeRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	for i := range s {
		if n == 0 {
			return s[:i]
		}
		n--
	}
	return s
}

func lastRunes(s string, n int) string {
	r := []rune(s)
	if n >= len(r) {
		return s
	}
	return string(r[len(r)-n:])
}

func padRight(s string, width int) string {
	if n := runeLen(s); n < width {
		return s + strings.Repeat(" ", width-n)
	}
	return s
}

// truncateCell cuts s to width runes, ending in "..." when there is room.
func truncateCell(s string, width int) string {
	if runeLen(s) <= width {
		return s
	}
	if width <= 3 {
		return takeRunes(s, width)
	}
	return takeRunes(s, width-3) + "..."
}

func formatCell(s string, width int) string { return padRight(truncateCell(s, width), width) }

// formatPathCell is formatCell, but truncates from the left so the end of a
// path (usually the most specific part) stays visible.
func formatPathCell(s string, width int) string {
	n := runeLen(s)
	switch {
	case n <= width:
	case width <= 3:
		s = lastRunes(s, width)
	default:
		s = "..." + lastRunes(s, width-3)
	}
	return padRight(s, width)
}

const (
	headerText        = "agtd - agent multiplexer"
	headerSuffix      = " - agent multiplexer"
	headerSuffixGamma = 1.35
)

type rgb struct{ r, g, b uint8 }

var (
	primaryBlue   = rgb{95, 251, 255}
	defaultViolet = rgb{185, 131, 255}
)

func fgRGB(c rgb) string { return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", c.r, c.g, c.b) }

// header is the "agtd - agent multiplexer" banner: a fixed truecolor
// gradient on a truecolor terminal, plain text anywhere else.
func header() string {
	if !stdoutIsTerminal() || !supportsTrueColor(os.Getenv) {
		return headerText
	}
	return colorHeader()
}

func colorHeader() string {
	var b strings.Builder
	b.WriteString("\x1b[1m" + fgRGB(primaryBlue) + "agtd\x1b[39m")
	suffix := []rune(headerSuffix)
	denom := math.Max(float64(len(suffix)-1), 1)
	for i, r := range suffix {
		t := math.Pow(float64(i)/denom, headerSuffixGamma)
		lerp := func(a, b uint8) uint8 { return uint8(math.Round(float64(a) + (float64(b)-float64(a))*t)) }
		b.WriteString(fgRGB(rgb{lerp(primaryBlue.r, defaultViolet.r), lerp(primaryBlue.g, defaultViolet.g), lerp(primaryBlue.b, defaultViolet.b)}))
		b.WriteRune(r)
	}
	b.WriteString("\x1b[39m\x1b[22m")
	return b.String()
}

// supportsTrueColor follows the usual conventions: NO_COLOR and TERM=dumb
// turn colour off, and COLORTERM (or a TERM naming it) says the terminal
// takes 24-bit colour.
func supportsTrueColor(getenv func(string) string) bool {
	if getenv("NO_COLOR") != "" || getenv("TERM") == "dumb" {
		return false
	}
	switch strings.ToLower(getenv("COLORTERM")) {
	case "truecolor", "24bit":
		return true
	}
	term := getenv("TERM")
	return strings.Contains(term, "truecolor") || strings.Contains(term, "24bit") || strings.HasSuffix(term, "-direct")
}
