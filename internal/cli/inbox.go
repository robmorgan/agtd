package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"agtd.sh/internal/config"
	"agtd.sh/internal/paths"
	"agtd.sh/internal/protocol"
	"agtd.sh/internal/session"
	"agtd.sh/internal/transport"
)

// The inbox (bare `agt`): every session on this machine and on every host
// in hosts.toml in one list, the ones that need you first, drawn below the
// prompt the way Claude Code draws its own: a header, the hosts, the rows,
// a bordered input and one line of keys. Enter attaches to a session on
// whichever host runs it; typing filters; Tab opens a row's actions.
//
// Ownership: the inbox's state (an *inbox) belongs to the goroutine running
// runInbox. Host probes reach it as immutable snapshots from a hostSet, and
// actions (remove, create) run on goroutines of their own and report on a
// channel, so a slow host never stalls the screen. See "The inbox" in
// ARCHITECTURE.md.

const (
	// The cursor is hidden, and pastes are bracketed so a pasted Tab or
	// line break arrives as text, never as keys that act on a row.
	inboxEnterSequence = "\x1b[?25l\x1b[?2004h"
	inboxExitSequence  = "\x1b[?2004l\x1b[?25h"

	// inboxNoticeTime is how long a notice stays up without a key
	// (failures stay until one; see fail).
	inboxNoticeTime = 10 * time.Second
	// inboxTick redraws for the elapsed times and expiring notices.
	inboxTick = time.Second
	// inboxActionTimeout bounds a remove or a create: longer than the
	// daemon may take to stop a session's agent (workerStopTimeout plus
	// workerKillTimeout, 10s) with a remote round trip on top.
	inboxActionTimeout = 30 * time.Second
	// inboxFailuresShown is how many unseen failures are drawn in full,
	// and inboxAcknowledgeAfter how long after the newest one a key clears
	// them.
	inboxFailuresShown    = 3
	inboxAcknowledgeAfter = 500 * time.Millisecond
	// inboxIncompleteAfter is how long an incomplete key (a lone ESC)
	// waits for the rest of a sequence before it is taken as it is, and
	// inboxPasteGiveUp how long a paste waits for its end marker before
	// it is given up (its rest is then dropped: keyDecoder.discarding),
	// and how long input must then be quiet before that dropping ends.
	inboxIncompleteAfter = 50 * time.Millisecond
	inboxPasteGiveUp     = 2 * time.Second
	// inboxDiscardMax bounds how long the rest of a given-up paste is
	// dropped, so input that never stops cannot lock the keys out.
	inboxDiscardMax = 10 * time.Second

	// Below inboxMinWidth cells the inbox prints its rows as `agt ls`
	// does and exits; below inboxFoldWidth the host folds into the name;
	// from inboxAgentWidth the agent has a column.
	inboxMinWidth   = 40
	inboxFoldWidth  = 60
	inboxAgentWidth = 90

	inboxHostCol    = 7
	inboxAgentCol   = 6
	inboxStateCol   = 12
	inboxNameMin    = 8
	inboxNameMax    = 24
	inboxMessageMin = 8

	inboxPlaceholder = "Filter, or name a new session"

	// codexDefaultModel is the model sessions started from the inbox or
	// the overlay ask codex for.
	codexDefaultModel = "gpt-5.4"
)

// rowKey identifies a row across refreshes and reorders: the host and the
// session's incarnation (its name, from a daemon without UIDs), or the
// create row.
type rowKey struct {
	host, uid, name string
	create          bool
}

var createRowKey = rowKey{create: true}

// inboxRow is one session on one host, from that host's latest probe.
type inboxRow struct {
	host *hostProbe
	rec  *session.Record
}

func (r inboxRow) key() rowKey {
	if r.rec.UID != "" {
		return rowKey{host: r.host.name, uid: r.rec.UID}
	}
	return rowKey{host: r.host.name, name: r.rec.SessionID}
}

// address is how `agt attach` names the session: its name on this machine,
// host/name elsewhere.
func (r inboxRow) address() string {
	if r.host.host == nil {
		return r.rec.SessionID
	}
	return r.host.name + "/" + r.rec.SessionID
}

func (r inboxRow) live() bool {
	return r.rec.Status == session.StatusRunning || r.rec.Status == session.StatusCreating
}

type inboxMode int

const (
	inboxBrowse inboxMode = iota
	inboxActions
	inboxConfirm
	inboxCreateAgent
)

type rowAction int

const (
	rowAttach rowAction = iota
	rowRemove
)

func (a rowAction) label() string {
	if a == rowAttach {
		return "attach"
	}
	return "remove"
}

// inboxNotice is a passing message: an action's success, or the answer to a key
// (a refusal in the error style).
type inboxNotice struct {
	isError bool
	message string
}

// inboxLeave is the user's request to end the inbox: attach to outcome,
// or quit when it is nil.
type inboxLeave struct{ outcome *inboxOutcome }

// inboxOutcome is the session the user chose to attach to.
type inboxOutcome struct {
	host    *transport.Host // nil for this machine
	id, uid string
}

// inboxEffect is work a key asked for, which the loop starts on a goroutine
// of its own.
type inboxEffect struct {
	remove      *inboxRow
	createName  string
	createAgent string
	create      bool
	reprobe     string // a host to probe again now
	reprobeAll  bool
}

// inboxResult is what an action reports back.
type inboxResult struct {
	key     rowKey
	address string
	host    string
	err     error
	// unknown means the answer was lost: the daemon may have acted.
	unknown bool
	// created is the session a create started.
	created *session.CreateResult
	create  bool
}

type inbox struct {
	theme *overlayTheme
	// locked is the one host of `agt --host NAME`, or "".
	locked string
	// hosts are the latest probe of each host, local first, then in
	// hosts.toml order.
	hosts []*hostProbe

	defaultAgent string
	createAgents []string

	query    string
	selected rowKey
	selIndex int // where selected last was, for when its row goes

	mode      inboxMode
	modeRow   rowKey
	modeIndex int
	showKeys  bool
	// modeActions are the actions the open menu was drawn with.
	// confirmShown is when a remove confirmation was first drawn (zero
	// while it is not on screen), and confirmCropped is set while a
	// terminal too short for it hides it: it takes Enter or y only once it
	// has been on screen for inboxAcknowledgeAfter.
	modeActions    []rowAction
	confirmShown   time.Time
	confirmCropped bool

	// What the inbox tells the user. A notice goes after inboxNoticeTime
	// or at the next key. failures are actions that failed and that the
	// user has not acknowledged with a key (see acknowledge): they stay on
	// screen until then, and while there are any the inbox never leaves or
	// attaches by itself.
	notice      *inboxNotice
	noticeUntil time.Time
	failures    []string
	// failuresShown is when the failures were first drawn (zero until
	// then, and again when one more comes).
	failuresShown time.Time

	// hidden is set while the inbox is not on screen: before its first
	// draw, and while the terminal is too narrow for it. Keys then do
	// nothing, and acknowledge nothing, but Esc and Ctrl-C quit.
	hidden bool

	// pending are rows with an action in progress, by the word shown in
	// place of their state; creating is set while a session is created.
	pending  map[rowKey]string
	creating bool
	// gone are rows removed since their host's last probe.
	gone map[rowKey]bool
	// leaving, when set, waits for the pending actions before the inbox
	// ends as the user asked. A failure cancels it.
	leaving *inboxLeave

	effects []inboxEffect
	now     func() time.Time
}

func newInbox(p *paths.AppPaths, hosts []*hostProbe, locked string) *inbox {
	i := &inbox{
		theme: inboxTheme(overlayThemeFor(os.Getenv)), locked: locked, hosts: hosts,
		pending: map[rowKey]string{}, gone: map[rowKey]bool{}, now: time.Now,
	}
	i.loadAgents(p)
	return i
}

// loadAgents reads the configured agents, the default first.
func (i *inbox) loadAgents(p *paths.AppPaths) {
	names := []string{"claude"}
	i.defaultAgent = ""
	if cfg, err := config.Load(p.Config); err == nil {
		if agents := cfg.AgentNames(); len(agents) > 0 {
			names = agents
		}
		i.defaultAgent = cfg.DefaultAgent
	}
	i.createAgents = orderedCreateAgents(names, i.defaultAgent)
}

func orderedCreateAgents(names []string, defaultAgent string) []string {
	for i, name := range names {
		if name == defaultAgent && i > 0 {
			out := append([]string{name}, names[:i]...)
			return append(out, names[i+1:]...)
		}
	}
	return names
}

// update takes a host's new probe.
func (i *inbox) update(p *hostProbe) {
	if p.startFailed {
		i.fail("could not start agtd on this machine: %v", p.err)
	}
	for n, h := range i.hosts {
		if h.name == p.name {
			i.hosts[n] = p
		}
	}
	if p.online() {
		present := map[rowKey]bool{}
		for k := range p.sessions {
			present[inboxRow{host: p, rec: &p.sessions[k]}.key()] = true
		}
		for k := range i.gone {
			if k.host == p.name && !present[k] {
				delete(i.gone, k)
			}
		}
	}
	i.clampMode()
}

func (i *inbox) host(name string) *hostProbe {
	for _, h := range i.hosts {
		if h.name == name {
			return h
		}
	}
	return nil
}

// allRows are every reachable session, in the inbox's order: by
// inboxSortBucket, newest first within one, hosts in strip order on ties.
func (i *inbox) allRows() []inboxRow {
	var rows []inboxRow
	for _, h := range i.hosts {
		if !h.online() {
			continue
		}
		for k := range h.sessions {
			r := inboxRow{host: h, rec: &h.sessions[k]}
			if !i.gone[r.key()] {
				rows = append(rows, r)
			}
		}
	}
	slices.SortStableFunc(rows, func(a, b inboxRow) int {
		if d := inboxSortBucket(a.rec) - inboxSortBucket(b.rec); d != 0 {
			return d
		}
		return sortTime(b.rec).Compare(sortTime(a.rec))
	})
	return rows
}

// rows are the rows the filter matches. A query that starts with a host's
// name and a slash ("dev/", "dev/auth") narrows to that host, the way `agt
// attach dev/auth` addresses a session.
func (i *inbox) rows() []inboxRow {
	query := strings.TrimSpace(i.query)
	host := ""
	if prefix, rest, ok := strings.Cut(query, "/"); ok && i.host(prefix) != nil {
		host, query = prefix, rest
	}
	var out []inboxRow
	for _, r := range i.allRows() {
		if (host == "" || r.host.name == host) && matchesQuery(i.searchText(r), query) {
			out = append(out, r)
		}
	}
	return out
}

func (i *inbox) searchText(r inboxRow) string {
	word, _, _ := i.state(r)
	return r.host.name + " " + sessionSearchText(r.rec) + " " + word + " " + statusMessage(r.rec)
}

// selection is the selected row's index in rows (len(rows) for the create
// row). Until the user moves it the first row is selected, whichever that
// is as hosts answer; a row that went away leaves the selection where it
// was, on the row that took its place.
func (i *inbox) selection(rows []inboxRow) int {
	switch {
	case i.selected == rowKey{}:
		return 0
	case i.selected.create:
		return len(rows)
	}
	for n, r := range rows {
		if r.key() == i.selected {
			i.selIndex = n
			return n
		}
	}
	return i.selectIndex(rows, i.selIndex)
}

func (i *inbox) selectIndex(rows []inboxRow, n int) int {
	n = max(min(n, len(rows)), 0)
	i.selIndex = n
	if n == len(rows) {
		i.selected = createRowKey
	} else {
		i.selected = rows[n].key()
	}
	return n
}

func (i *inbox) row(k rowKey) (inboxRow, bool) {
	for _, r := range i.allRows() {
		if r.key() == k {
			return r, true
		}
	}
	return inboxRow{}, false
}

func (i *inbox) actions(r inboxRow) []rowAction {
	if r.live() {
		return []rowAction{rowAttach, rowRemove}
	}
	return []rowAction{rowRemove}
}

// clampMode leaves a menu whose row went away.
func (i *inbox) clampMode() {
	switch i.mode {
	case inboxActions, inboxConfirm:
		r, ok := i.row(i.modeRow)
		if !ok {
			i.mode = inboxBrowse
			return
		}
		if !slices.ContainsFunc(i.rows(), func(v inboxRow) bool { return v.key() == i.modeRow }) {
			// The filter no longer shows it (its status message changed,
			// say): a menu or confirmation is never left acting unseen.
			i.mode = inboxBrowse
			i.setNotice(&inboxNotice{message: r.address() + " is no longer shown; its menu closed"})
			return
		}
		if i.mode == inboxActions && !slices.Equal(i.actions(r), i.modeActions) {
			// The session changed (it exited, say): close the menu
			// rather than let the highlight slide onto another action.
			i.mode = inboxBrowse
			i.setNotice(&inboxNotice{message: r.address() + " changed; its menu closed"})
		}
	case inboxCreateAgent:
		if len(i.createAgents) == 0 {
			i.mode = inboxBrowse
		}
		i.modeIndex = min(i.modeIndex, max(len(i.createAgents)-1, 0))
	}
}

// setNotice shows a notice.
func (i *inbox) setNotice(t *inboxNotice) {
	i.notice = t
	i.noticeUntil = i.now().Add(inboxNoticeTime)
}

// errorNotice answers a key the inbox refuses.
func (i *inbox) errorNotice(format string, args ...any) {
	i.setNotice(&inboxNotice{isError: true, message: fmt.Sprintf(format, args...)})
}

// fail records an action that failed, for the user to see.
func (i *inbox) fail(format string, args ...any) {
	i.failures = append(i.failures, fmt.Sprintf(format, args...))
	i.failuresShown = time.Time{}
}

// acknowledge clears what the user has now seen: called for a key or a
// paste while the inbox is on screen. Failures go only with a key that
// comes inboxAcknowledgeAfter after they (the newest included) were first
// drawn: a key already on its way (typing, a held arrow) does not clear
// what the user has not had time to read.
func (i *inbox) acknowledge() {
	i.notice = nil
	if !i.failuresShown.IsZero() && i.now().Sub(i.failuresShown) >= inboxAcknowledgeAfter {
		i.failures, i.failuresShown = nil, time.Time{}
	}
}

// leave ends the inbox with outcome (nil quits) once no action is pending,
// waiting for them until then.
func (i *inbox) leave(outcome *inboxOutcome) (bool, *inboxOutcome) {
	if len(i.pending) == 0 && !i.creating {
		return true, outcome
	}
	i.leaving = &inboxLeave{outcome}
	return false, nil
}

// inFlight names the actions in progress, such as "removing dev/demo".
func (i *inbox) inFlight() []string {
	var what []string
	for k, word := range i.pending {
		if r, ok := i.row(k); ok {
			what = append(what, strings.TrimSuffix(word, "…")+" "+r.address())
		}
	}
	slices.Sort(what)
	if i.creating {
		what = append(what, "starting the new session")
	}
	if len(what) == 0 && len(i.pending) > 0 {
		what = append(what, "the last action")
	}
	return what
}

// waitingFor says what a leave waits for.
func (i *inbox) waitingFor() string {
	return "finishing " + strings.Join(i.inFlight(), ", ") + "… esc leaves now"
}

func (i *inbox) handlePaste(text string) {
	if i.mode != inboxBrowse || i.leaving != nil || i.hidden {
		return
	}
	i.acknowledge()
	// A paste is one line of plain text: its runs of whitespace (line
	// breaks included) become one space, its ends are trimmed and its
	// control characters are dropped, so it cannot break the drawing.
	i.query += strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, strings.Join(strings.Fields(text), " "))
	i.selected = rowKey{}
}

// handleKey takes one key. done ends the inbox: with outcome, attach to
// it; without, quit.
func (i *inbox) handleKey(ev keyEvent) (done bool, outcome *inboxOutcome) {
	// Ctrl-C quits from anywhere, as Esc does from an empty list; while
	// the inbox waits for an action, it leaves at once without attaching.
	ctrlC := ev.code == keyChar && ev.r == 'c' && ev.mods == modCtrl
	switch {
	case i.leaving != nil:
		// Waiting for actions: Esc leaves at once as asked, Ctrl-C quits.
		switch {
		case ctrlC:
			return true, nil
		case ev.code == keyEsc:
			return true, i.leaving.outcome
		}
		return false, nil
	case i.hidden:
		// Not on screen (not drawn yet, or narrowed): a key the user
		// cannot see the effect of does nothing, and Esc quits.
		if ctrlC || ev.code == keyEsc {
			return i.leave(nil)
		}
		return false, nil
	}
	i.acknowledge()
	if ctrlC {
		return i.leave(nil)
	}
	switch i.mode {
	case inboxActions:
		return i.handleActionKey(ev)
	case inboxConfirm:
		i.handleConfirmKey(ev)
		return false, nil
	case inboxCreateAgent:
		i.handleCreateAgentKey(ev)
		return false, nil
	}
	return i.handleBrowseKey(ev)
}

func (i *inbox) handleBrowseKey(ev keyEvent) (bool, *inboxOutcome) {
	rows := i.rows()
	sel := i.selection(rows)
	switch {
	case ev.code == keyEsc:
		if i.query != "" {
			i.query = ""
			i.selected = rowKey{}
			return false, nil
		}
		return i.leave(nil)
	case ev.code == keyUp:
		i.selectIndex(rows, sel-1)
	case ev.code == keyDown:
		i.selectIndex(rows, sel+1)
	case ev.code == keyBackspace:
		i.query = popRune(i.query)
		i.selected = rowKey{}
	case ev.code == keyEnter:
		if sel == len(rows) {
			i.startCreate(len(rows) == 0)
			return false, nil
		}
		r := rows[sel]
		if r.live() {
			return i.attach(r)
		}
		i.openActions(r)
	case ev.code == keyTab || ev.code == keyRight:
		if sel < len(rows) {
			i.openActions(rows[sel])
		}
	case ev.printable() && i.query == "" && ev.r == 'n':
		i.selectIndex(rows, len(rows))
		i.startCreate(true)
	case ev.printable() && i.query == "" && ev.r == 'r':
		i.effects = append(i.effects, inboxEffect{reprobeAll: true})
		i.setNotice(&inboxNotice{message: "asking every host again"})
	case ev.printable() && i.query == "" && ev.r == '?':
		i.showKeys = !i.showKeys
	case ev.printable():
		i.query += string(ev.r)
		i.selected = rowKey{}
	}
	return false, nil
}

func (i *inbox) attach(r inboxRow) (bool, *inboxOutcome) {
	if word, ok := i.pending[r.key()]; ok {
		i.errorNotice("%s is %s", r.address(), strings.TrimSuffix(word, "…"))
		return false, nil
	}
	return i.leave(&inboxOutcome{host: r.host.host, id: r.rec.SessionID, uid: r.rec.UID})
}

func (i *inbox) openActions(r inboxRow) {
	i.mode, i.modeRow, i.modeIndex, i.modeActions = inboxActions, r.key(), 0, i.actions(r)
	// Pin the selection to it, so a reorder never moves the highlight
	// away from the row the menu acts on.
	i.selected = r.key()
}

// requestedName is the query as a session name, or "" when it is empty or
// not a valid name.
func (i *inbox) requestedName() string {
	name := strings.TrimSpace(i.query)
	if !session.ValidName(name) {
		return ""
	}
	return name
}

// startCreate opens the agent chooser for a new session in the current
// directory, named by the query (withName) or by the daemon.
func (i *inbox) startCreate(withName bool) {
	switch {
	case i.creating:
		i.errorNotice("a session is already starting")
		return
	case i.locked != "" && i.locked != localHostName:
		i.errorNotice("%s", remoteCreateHint(i.locked))
		return
	case withName && strings.TrimSpace(i.query) != "" && i.requestedName() == "":
		i.errorNotice("invalid session name: %s", session.NameRules)
		return
	}
	// createAgents has the default first (orderedCreateAgents).
	i.mode, i.modeIndex = inboxCreateAgent, 0
	i.clampMode()
}

func (i *inbox) handleActionKey(ev keyEvent) (bool, *inboxOutcome) {
	r, ok := i.row(i.modeRow)
	if !ok {
		i.mode = inboxBrowse
		return false, nil
	}
	actions := i.actions(r)
	run := func(a rowAction) (bool, *inboxOutcome) {
		if a == rowAttach {
			i.mode = inboxBrowse
			return i.attach(r)
		}
		if word, ok := i.pending[r.key()]; ok {
			i.errorNotice("%s is %s", r.address(), strings.TrimSuffix(word, "…"))
			return false, nil
		}
		i.mode, i.confirmShown, i.confirmCropped = inboxConfirm, time.Time{}, false
		return false, nil
	}
	switch {
	case ev.code == keyEsc || ev.code == keyLeft:
		i.mode = inboxBrowse
	case ev.code == keyUp:
		i.modeIndex = max(i.modeIndex-1, 0)
	case ev.code == keyDown:
		i.modeIndex = min(i.modeIndex+1, len(actions)-1)
	case ev.code == keyEnter:
		return run(actions[i.modeIndex])
	default:
		if n := digitIndex(ev); n >= 0 && n < len(actions) {
			return run(actions[n])
		}
	}
	return false, nil
}

func (i *inbox) handleConfirmKey(ev keyEvent) {
	back := func() {
		i.mode = inboxActions
		if r, ok := i.row(i.modeRow); ok {
			i.modeActions = i.actions(r)
			i.modeIndex = slices.Index(i.modeActions, rowRemove)
		}
	}
	switch {
	case ev.code == keyEnter, ev.code == keyChar && ev.mods == 0 && ev.r == 'y':
		switch {
		case i.confirmCropped:
			i.errorNotice("make the terminal taller to see what this removes")
			return
		case i.confirmShown.IsZero() || i.now().Sub(i.confirmShown) < inboxAcknowledgeAfter:
			// Not drawn yet, or only just (keys that came with the one
			// that asked, key repeat, a burst queued over ssh): the
			// user has not had time to see what this would remove.
			return
		}
		r, ok := i.row(i.modeRow)
		i.mode = inboxBrowse
		if ok {
			i.pending[r.key()] = "removing…"
			i.effects = append(i.effects, inboxEffect{remove: &r})
		}
	case ev.code == keyEsc, ev.code == keyChar && ev.mods == 0 && ev.r == 'n':
		back()
	}
}

func (i *inbox) handleCreateAgentKey(ev keyEvent) {
	create := func(agent string) {
		i.mode = inboxBrowse
		i.creating = true
		i.effects = append(i.effects, inboxEffect{create: true, createName: i.requestedName(), createAgent: agent})
	}
	switch {
	case ev.code == keyEsc:
		i.mode = inboxBrowse
	case ev.code == keyUp:
		i.modeIndex = max(i.modeIndex-1, 0)
	case ev.code == keyDown:
		i.modeIndex = min(i.modeIndex+1, len(i.createAgents)-1)
	case ev.code == keyEnter:
		if i.modeIndex < len(i.createAgents) {
			create(i.createAgents[i.modeIndex])
		}
	default:
		if n := digitIndex(ev); n >= 0 && n < len(i.createAgents) {
			create(i.createAgents[n])
		}
	}
}

// digitIndex is the 0-based index a digit key 1-9 picks, or -1.
func digitIndex(ev keyEvent) int {
	if ev.code == keyChar && ev.mods == 0 && ev.r >= '1' && ev.r <= '9' {
		return int(ev.r - '1')
	}
	return -1
}

// finish takes an action's result. done ends the inbox, as for handleKey.
func (i *inbox) finish(res inboxResult) (done bool, outcome *inboxOutcome) {
	if res.create {
		i.creating = false
		switch {
		case res.unknown:
			i.fail("starting a session: %v; result unknown, refreshing", res.err)
			i.effects = append(i.effects, inboxEffect{reprobe: localHostName})
		case res.err != nil:
			i.fail("could not start a session: %v", res.err)
		case i.leaving == nil && len(i.failures) == 0:
			// Attach to it, as creating it asked.
			return i.leave(&inboxOutcome{id: res.created.SessionID, uid: res.created.UID})
		default:
			// A failure the user has not seen comes first, or the user
			// is leaving (and may yet stay, if another action fails).
			i.setNotice(&inboxNotice{message: "started session " + res.created.SessionID})
			i.effects = append(i.effects, inboxEffect{reprobe: localHostName})
		}
	} else {
		delete(i.pending, res.key)
		switch {
		case res.unknown:
			i.fail("removing %s: %v; result unknown, refreshing", res.address, res.err)
			i.effects = append(i.effects, inboxEffect{reprobe: res.host})
		case res.err != nil:
			i.fail("could not remove %s: %v", res.address, res.err)
		default:
			i.gone[res.key] = true
			i.setNotice(&inboxNotice{message: "removed session " + res.address})
			i.effects = append(i.effects, inboxEffect{reprobe: res.host})
		}
	}
	if i.leaving != nil && res.err != nil {
		// The user left (or chose a row) expecting this to work: stay
		// to show that it did not, and let them choose again.
		if i.leaving.outcome != nil {
			i.failures[len(i.failures)-1] += "; not attaching"
		}
		i.leaving = nil
	}
	if i.leaving != nil && len(i.pending) == 0 && !i.creating {
		return true, i.leaving.outcome
	}
	i.clampMode()
	return false, nil
}

// answered reports whether every host has been heard from.
func (i *inbox) answered() bool {
	for _, h := range i.hosts {
		if h.status == hostAsking || h.status == hostStarting {
			return false
		}
	}
	return true
}

// Rendering

// state is a row's word, glyph and the glyph's style, from the states
// table of docs/designs/agt-inbox.md.
func (i *inbox) state(r inboxRow) (word, glyph, style string) {
	t := i.theme
	now := i.now()
	s := r.rec
	row := buildDisplayRow(s, now)
	switch sessionRunState(s) {
	case runFailed:
		word, glyph, style = "failed", "✖", ""
		if t.mutedFG != "" {
			style = ansiFailed
		}
	case runRecovered:
		word, glyph, style = "lost", "○", t.quietFG
	case runExited:
		word, glyph, style = "exited", "○", t.quietFG
		if s.ExitCode != nil && *s.ExitCode != 0 {
			word = fmt.Sprintf("exited (%d)", *s.ExitCode)
		}
	default:
		word, glyph, style = row.activity, "●", t.okFG
		if s.Activity == session.ActivityIdle && s.Status == session.StatusRunning {
			style = t.quietFG
		}
		if s.Attention == session.AttentionAction {
			glyph, style = "⚠", t.signalFG
			if s.Activity != session.ActivityWaiting && s.Activity != session.ActivityBlocked {
				word = "needs you"
			}
		}
	}
	if p, ok := i.pending[r.key()]; ok {
		word = p
	}
	return word, glyph, style
}

// statusMessage is the program's own word on what it is doing (OSC 7501),
// or what needs attention when it reports nothing.
func statusMessage(s *session.Record) string {
	text := ""
	switch {
	case s.StatusMsg != nil && *s.StatusMsg != "":
		text = *s.StatusMsg
	case s.Attention.Rank() > 0 && s.AttentionSummary != nil:
		text = *s.AttentionSummary
	}
	return escapeControls(strings.Join(strings.Fields(text), " "))
}

// inboxLayout is the row columns for a width.
type inboxLayout struct {
	name, host, agent, state int
	fold                     bool // host/name in the name column
}

func (i *inbox) layout(width int, rows []inboxRow) inboxLayout {
	l := inboxLayout{host: inboxHostCol, agent: inboxAgentCol, state: inboxStateCol, fold: width < inboxFoldWidth}
	if width < inboxAgentWidth {
		l.agent = 0
	}
	if l.fold {
		l.host = 0
	}
	l.name = inboxNameMin
	for _, r := range rows {
		name := escapeControls(r.rec.SessionID)
		if l.fold {
			name = r.host.name + "/" + name
		}
		l.name = max(l.name, cellWidth(name))
	}
	l.name = min(l.name, inboxNameMax)
	return l
}

func (i *inbox) sessionLine(r inboxRow, width int, l inboxLayout, selected bool) line {
	t := i.theme
	word, glyph, glyphStyle := i.state(r)
	name := escapeControls(r.rec.SessionID)
	if l.fold {
		name = r.host.name + "/" + name
	}
	nameStyle := ""
	switch {
	case selected:
		nameStyle = t.bold
	case !r.live():
		nameStyle = t.mutedFG
	}
	spans := []span{{glyphStyle, glyph}, {"", " "}, {nameStyle, padCells(name, l.name)}, {"", "  "}}
	if l.host > 0 {
		spans = append(spans, span{t.mutedFG, padCells(r.host.name, l.host)}, span{"", "  "})
	}
	if l.agent > 0 {
		spans = append(spans, span{t.mutedFG, padCells(escapeControls(r.rec.Agent), l.agent)}, span{"", "  "})
	}
	spans = append(spans, span{"", padCells(word, l.state)})
	elapsed := elapsedLabel(r.rec, i.now())
	used := 2 + line{spans: spans}.width()
	if room := width - used - cellWidth(elapsed) - 3; room >= inboxMessageMin {
		if msg := statusMessage(r.rec); msg != "" {
			style := ""
			if r.rec.Attention == session.AttentionAction {
				style = t.signalFG
			}
			spans = append(spans, span{"", "  "}, span{style, truncateCells(msg, room)})
		}
	}
	return t.listRow(width, selected, spans, span{t.quietFG, elapsed})
}

// menuLine is an item of a numbered list under a row: "❯ 1. attach".
func (i *inbox) menuLine(n int, label string, selected bool) line {
	t := i.theme
	if selected {
		return line{spans: []span{{"", "    "}, {t.signalFG, "❯ "}, {t.bold, fmt.Sprintf("%d. %s", n+1, label)}}}
	}
	return line{spans: []span{{"", "      "}, {t.mutedFG, fmt.Sprintf("%d. %s", n+1, label)}}}
}

func (i *inbox) createLine(width int, selected bool, rows int) line {
	t := i.theme
	label := "new session"
	if q := strings.TrimSpace(i.query); q != "" && rows == 0 {
		label += " " + q
	}
	if i.creating {
		label = "starting a new session…"
	}
	style := t.mutedFG
	if selected {
		style = t.bold
	}
	return t.listRow(width, selected, []span{{t.signalFG, "+"}, {"", " "}, {style, label}}, span{})
}

func (i *inbox) header(width int) line {
	t := i.theme
	var counts [4]int // need you, working, idle, exited
	for _, r := range i.allRows() {
		switch inboxSortBucket(r.rec) {
		case bucketNeedsYou:
			counts[0]++
		case bucketWorking, bucketRunning:
			counts[1]++
		case bucketIdle:
			counts[2]++
		default:
			counts[3]++
		}
	}
	var parts []string
	for n, label := range []string{"need you", "working", "idle", "exited"} {
		if counts[n] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[n], label))
		}
	}
	l := line{spans: []span{{"", " "}, {t.bold, "agtd"}}}
	if right := strings.Join(parts, " · "); right != "" && 6+cellWidth(right)+1 <= width {
		l.spans = append(l.spans, span{"", strings.Repeat(" ", width-5-cellWidth(right)-1)}, span{t.mutedFG, right})
	}
	return l
}

// hostEntry is one host in the strip: name, glyph, and what is known.
func (i *inbox) hostEntry(h *hostProbe) []span {
	t := i.theme
	spans := []span{{"", escapeControls(h.name)}, {"", " "}}
	switch h.status {
	case hostOnline:
		spans = append(spans, span{t.okFG, "●"}, span{t.mutedFG, fmt.Sprintf(" %d", len(h.sessions))})
		if h.host != nil {
			spans = append(spans, span{t.quietFG, " " + formatLatency(h.latency)})
		}
	case hostAsking, hostStarting:
		word := ""
		if h.status == hostStarting {
			word = " starting"
		}
		spans = append(spans, span{t.quietFG, "◌" + word})
	case hostOffline, hostNotRunning:
		spans = append(spans, span{t.quietFG, "○ " + h.status})
	default:
		spans = append(spans, span{t.signalFG, "⚠"}, span{t.mutedFG, " " + h.status + " · agt host info " + h.name})
	}
	return spans
}

// strip is the host strip, wrapped onto as many lines as it needs.
func (i *inbox) strip(width int) []line {
	var lines []line
	cur := line{spans: []span{{"", " "}}}
	for _, h := range i.hosts {
		entry := i.hostEntry(h)
		w := line{spans: entry}.width()
		if cur.width() > 1 && cur.width()+3+w > width {
			lines = append(lines, cur)
			cur = line{spans: []span{{"", " "}}}
		}
		if cur.width() > 1 {
			cur.spans = append(cur.spans, span{"", "   "})
		}
		cur.spans = append(cur.spans, entry...)
	}
	return append(lines, cur)
}

// inputBox is the bordered input: three lines.
func (i *inbox) inputBox(width int) []line {
	t := i.theme
	edge := func(s string) span { return span{t.lineFG, s} }
	value := i.query
	active := i.mode == inboxBrowse && i.leaving == nil
	inner := t.input("❯", value, inboxPlaceholder, active)
	if i.locked != "" {
		inner.spans = append(inner.spans[:2:2], append([]span{{t.mutedFG, i.locked + "/"}}, inner.spans[2:]...)...)
	}
	inner.spans = slices.Insert(inner.spans, 0, edge("│"))
	inner.spans = append(inner.spans, span{"", strings.Repeat(" ", max(width-1-inner.width(), 0))}, edge("│"))
	inner.bg = ""
	bar := strings.Repeat("─", max(width-2, 0))
	return []line{{spans: []span{edge("╭" + bar + "╮")}}, inner, {spans: []span{edge("╰" + bar + "╯")}}}
}

// hints are the keys the inbox takes now.
func (i *inbox) hints(rows []inboxRow, sel int) [][2]string {
	switch {
	case i.leaving != nil:
		return [][2]string{{"esc", "leave now"}}
	case i.mode == inboxConfirm:
		return [][2]string{{"enter", "confirm"}, {"esc", "back"}}
	case i.mode != inboxBrowse:
		return [][2]string{{"↑↓", "choose"}, {"enter", "select"}, {"esc", "back"}}
	}
	enter := "attach"
	switch {
	case sel == len(rows):
		enter = "new session"
	case !rows[sel].live():
		enter = "actions"
	}
	h := [][2]string{{"↑↓", "choose"}, {"enter", enter}, {"tab", "actions"}}
	if i.query != "" {
		return append(h, [2]string{"esc", "clear"})
	}
	return append(h, [2]string{"n", "new"}, [2]string{"esc", "quit"}, [2]string{"?", "keys"})
}

// inboxKeyLines are the full key list, shown for ?.
var inboxKeyLines = [][][2]string{
	{{"↑↓", "choose"}, {"enter", "attach (actions for an ended session)"}, {"tab/→", "actions"}},
	{{"type", "filter; host/ narrows to a host"}, {"n", "new session"}, {"r", "ask every host again"}},
	{{"space", "first, to filter by n, r or ?"}, {"esc", "back, clear, then quit (sessions keep running)"}, {"ctrl-c", "quit"}},
}

func (i *inbox) hintLine(h [][2]string) line {
	t := i.theme
	l := line{spans: []span{{"", " "}}}
	for n, k := range h {
		if n > 0 {
			l.spans = append(l.spans, span{t.quietFG, " · "})
		}
		l.spans = append(l.spans, span{"", k[0]}, span{t.mutedFG, " " + k[1]})
	}
	return l
}

// render is the inbox as lines of cols-1 cells (so the terminal never
// wraps one), at most rows-1 of them: drawing moves the cursor up over the
// previous drawing, which cannot go past the top of the screen. It also
// notes what made it onto the screen: when the failures were first drawn
// (failuresShown, which acknowledge relies on) and when a remove
// confirmation was (confirmShown, confirmCropped).
func (i *inbox) render(cols, rows int) []string {
	t := i.theme
	width := lineWidth(cols)
	list := i.rows()
	sel := i.selection(list)
	l := i.layout(width, list)

	var top, bottom []line
	top = append(top, i.header(width))
	top = append(top, i.strip(width)...)
	top = append(top, line{})

	// The list: rows, the menu open under one, the create row.
	var body []line
	selFirst, selLast := 0, 0
	confirmAt := -1 // the confirmation's line in body
	for n, r := range list {
		if n == sel {
			selFirst = len(body)
		}
		body = append(body, i.sessionLine(r, width, l, n == sel))
		if r.key() == i.modeRow && (i.mode == inboxActions || i.mode == inboxConfirm) {
			if i.mode == inboxConfirm {
				confirmAt = len(body)
			}
			body = append(body, i.menuLines(r)...)
		}
		if n == sel {
			selLast = len(body) - 1
		}
	}
	if sel == len(list) {
		selFirst = len(body)
	}
	body = append(body, i.createLine(width, sel == len(list), len(list)))
	if i.mode == inboxCreateAgent {
		body = append(body, i.createMenuLines()...)
	}
	if sel == len(list) {
		selLast = len(body) - 1
	}
	body = append(body, i.emptyLines()...)

	bottom = append(bottom, line{})
	failuresAt := len(bottom) // messageLines puts the failures first
	failureLines := min(len(i.failures), inboxFailuresShown)
	if len(i.failures) > failureLines {
		failureLines++ // the count of the rest
	}
	bottom = append(bottom, i.messageLines()...)
	bottom = append(bottom, i.inputBox(width)...)
	if i.showKeys && i.mode == inboxBrowse && i.leaving == nil {
		for _, k := range inboxKeyLines {
			bottom = append(bottom, i.hintLine(k))
		}
	} else {
		bottom = append(bottom, i.hintLine(i.hints(list, sel)))
	}

	// What does not fit scrolls, keeping the selection (and its menu) in
	// view.
	room := max(rows-1-len(top)-len(bottom), 1)
	first := 0
	if len(body) > room {
		first = max(min(selFirst, selLast-room+1), 0)
		first = min(first, len(body)-room)
		body = body[first : first+room]
	}
	all := append(append(top, body...), bottom...)
	extra := max(len(all)-max(rows-1, 1), 0)
	all = all[extra:] // a terminal too short even for the frame loses its top
	// What survived: a line at index n of the whole frame is on screen
	// when n >= extra.
	confirmIndex := -1
	if confirmAt >= first && confirmAt < first+len(body) {
		confirmIndex = len(top) + confirmAt - first
	}
	switch shown := i.mode == inboxConfirm && confirmIndex >= extra; {
	case !shown:
		i.confirmShown = time.Time{}
	case i.confirmShown.IsZero():
		i.confirmShown = i.now()
	}
	i.confirmCropped = i.mode == inboxConfirm && confirmIndex < extra
	// Failures count as drawn only when all of their lines are on screen:
	// cropping takes the top first, so the first one decides.
	if failureLines > 0 && i.failuresShown.IsZero() && len(top)+len(body)+failuresAt >= extra {
		i.failuresShown = i.now()
	}
	out := make([]string, len(all))
	for n, ln := range all {
		out[n] = ln.render(width, t)
	}
	return out
}

func (i *inbox) menuLines(r inboxRow) []line {
	t := i.theme
	if i.mode == inboxConfirm {
		return []line{{spans: []span{{"", "    "}, {t.signalFG, "Remove " + escapeControls(r.address()) + "?"}, {t.mutedFG, " Its working directory is kept."}}}}
	}
	var lines []line
	for n, a := range i.actions(r) {
		lines = append(lines, i.menuLine(n, a.label(), n == i.modeIndex))
	}
	return lines
}

func (i *inbox) createMenuLines() []line {
	t := i.theme
	heading := "Start a session here with"
	if name := i.requestedName(); name != "" {
		heading = "Start " + name + " here with"
	}
	lines := []line{{spans: []span{{"", "    "}, {t.mutedFG, heading}}}}
	for n, agent := range i.createAgents {
		label := agent
		if agent == i.defaultAgent {
			label += " (default)"
		}
		lines = append(lines, i.menuLine(n, label, n == i.modeIndex))
	}
	return lines
}

// emptyLines explain an empty list: nothing reachable yet, or nothing
// started yet.
func (i *inbox) emptyLines() []line {
	t := i.theme
	if len(i.allRows()) > 0 || i.query != "" {
		return nil
	}
	online, remote := false, false
	for _, h := range i.hosts {
		online = online || h.online()
		remote = remote || h.host != nil
	}
	note := func(text string) line { return line{spans: []span{{"", "  "}, {t.mutedFG, text}}} }
	switch {
	case !i.answered() && !online:
		return nil
	case !online:
		return []line{{}, note("Nothing reachable.")}
	case !remote && i.locked == "":
		return []line{{}, note("No sessions yet. Enter starts an agent in this directory."),
			note("To add a devbox, run `agtd remote pair` there, then `agt host add NAME ADDRESS --code CODE` here.")}
	}
	return []line{{}, note("No sessions yet.")}
}

// Drawing below the prompt.

// lineWidth is the width lines are cut to: one less than the terminal's,
// so the terminal never wraps them.
func lineWidth(width int) int { return max(width-1, 1) }

// inlineScreen redraws a block of lines in place below the prompt. Every
// line it draws is lineWidth(cols) cells wide; when the terminal narrows,
// the terminal rewraps them, so clearing counts the rows they now take.
type inlineScreen struct {
	lines []string
	cols  int
}

func (s *inlineScreen) draw(lines []string, cols int) {
	if cols == s.cols && slices.Equal(lines, s.lines) {
		return
	}
	var b strings.Builder
	if s.lines == nil {
		b.WriteString("\r\n")
	} else {
		b.WriteString(s.clearSequence(cols))
	}
	b.WriteString(strings.Join(lines, "\r\n"))
	writeOut([]byte(b.String()))
	s.lines, s.cols = lines, cols
}

// clear erases the block, leaving the cursor where it began.
func (s *inlineScreen) clear(cols int) {
	if s.lines != nil {
		writeOut([]byte(s.clearSequence(cols)))
	}
	s.lines = nil
}

func (s *inlineScreen) clearSequence(cols int) string {
	rows := len(s.lines)
	if drawn := lineWidth(s.cols); cols > 0 && drawn > cols {
		rows *= (drawn + cols - 1) / cols
	}
	seq := "\r"
	if rows > 1 {
		seq += fmt.Sprintf("\x1b[%dA", rows-1)
	}
	return seq + "\x1b[J"
}

// runInbox runs the inbox until the user quits, attaching to the sessions
// they choose on the way: a detach, or the session ending, returns to it.
func (a *app) runInbox() error {
	locked := a.host
	if locked == autoHostName {
		return errors.New("`--host auto` only chooses a host for `agt new`")
	}
	raw, err := enterRaw(false)
	if err != nil {
		return fmt.Errorf("failed to enable raw terminal mode: %w", err)
	}
	defer raw.restore()
	// This machine's daemon is started as for any command, unless the
	// inbox is locked to a remote host.
	set, hosts, err := a.startHostSet(hostSetOptions{only: locked, startLocal: locked == "" || locked == localHostName})
	if err != nil {
		return err
	}
	defer set.stop()
	writeOut([]byte(inboxEnterSequence))
	defer writeOut([]byte(inboxExitSequence))

	i := newInbox(a.paths, hosts, locked)

	// Actions run on their own goroutines, each reporting once on
	// results; cancelling ctx (on return) abandons them, and the inbox
	// waits for them to end.
	var actions sync.WaitGroup
	defer actions.Wait()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := make(chan inboxResult, 4)
	start := func(e inboxEffect) {
		switch {
		case e.reprobeAll:
			set.reprobeAll()
		case e.reprobe != "":
			set.reprobeHost(e.reprobe)
		default:
			actions.Go(func() {
				res := a.runInboxEffect(ctx, e)
				select {
				case results <- res:
				case <-ctx.Done():
				}
			})
		}
	}

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	ticker := time.NewTicker(inboxTick)
	defer ticker.Stop()

	keys := keyDecoder{holdEsc: true}
	var heldUntil, discardEnd time.Time
	handle := func(evs []keyEvent) (bool, *inboxOutcome) {
		for _, ev := range evs {
			if ev.code == keyUnknown {
				continue // what expire gave up on: not a key
			}
			if ev.isPaste {
				i.handlePaste(ev.paste)
				continue
			}
			if done, outcome := i.handleKey(ev); done {
				return true, outcome
			}
		}
		return false, nil
	}
	input := stdinChunks()
	began := time.Now()
	var screen inlineScreen
	drawn := false
	for {
		// A row's menu closes once it is not shown, even when only time
		// changed what the filter matches.
		i.clampMode()
		cols, rows, _, _ := terminalGeometry()
		view := chooseInboxView(int(cols), drawn, i.answered() || time.Since(began) > probeTimeout)
		i.setHidden(view != inboxDrawn)
		switch view {
		case inboxDrawn:
			screen.draw(i.render(int(cols), int(rows)), int(cols))
			drawn = true
		case inboxNarrowed:
			screen.draw(narrowNotice(int(cols), i.narrowStatus()), int(cols))
		case inboxListed:
			screen.clear(int(cols))
			i.printNarrow(int(cols))
			printUnseen(i.unseen())
			return nil
		}
		// What the decoder holds (a lone ESC, a paste, or the rest of a
		// given-up one) is given up when nothing completes it in time.
		var expire <-chan time.Time
		if keys.discarding {
			// The rest of a given-up paste is dropped until input is
			// quiet for inboxPasteGiveUp, and for inboxDiscardMax at most.
			if discardEnd.IsZero() {
				discardEnd = time.Now().Add(inboxDiscardMax)
			}
		} else {
			discardEnd = time.Time{}
		}
		if keys.held() {
			if heldUntil.IsZero() {
				wait := inboxIncompleteAfter
				if keys.pasting() {
					wait = inboxPasteGiveUp
				}
				heldUntil = time.Now().Add(wait)
			}
			expire = time.After(time.Until(heldUntil))
		} else {
			heldUntil = time.Time{}
		}
		done, outcome := false, (*inboxOutcome)(nil)
		select {
		case p := <-set.updates:
			i.update(p)
		case res := <-results:
			done, outcome = i.finish(res)
		case <-winch:
		case <-ticker.C:
		case <-expire:
			heldUntil = time.Time{}
			done, outcome = handle(keys.expire())
		case chunk, ok := <-input:
			if !ok {
				screen.clear(int(cols))
				return errors.New("stdin closed")
			}
			if c, r, _, _ := terminalGeometry(); c != cols || r != rows {
				// Resized since the last drawing: what is on screen is
				// not known (the terminal may have rewrapped or scrolled
				// it), so these keys meet a hidden inbox, and the top of
				// the loop draws it again.
				i.setHidden(true)
			}
			wasPasting, pastes := keys.pasting(), keys.pastes
			done, outcome = handle(keys.push(chunk))
			switch {
			case keys.discarding:
				heldUntil = minTime(time.Now().Add(inboxPasteGiveUp), discardEnd)
			case !wasPasting || keys.pastes != pastes || !keys.pasting():
				// A new wait for what is held now. A paste's wait runs
				// from its start, however many chunks (or presses of
				// Esc) come meanwhile.
				heldUntil = time.Time{}
			}
		}
		for _, e := range i.effects {
			start(e)
		}
		i.effects = nil
		if done && outcome == nil {
			cols, _, _, _ := terminalGeometry()
			screen.clear(int(cols))
			// Nothing the user did not see is lost.
			printUnseen(i.unseen())
			return nil
		}
		if done {
			// Attach, keeping the inbox where it is: the attachment
			// draws on the alternate screen and leaves it on return,
			// so the block is still there to redraw over. Hosts go on
			// being probed and actions go on running meanwhile; what
			// they report waits for the inbox.
			ended, err := a.attachFromInbox(outcome)
			writeOut([]byte(inboxEnterSequence))
			i.attached(outcome, ended, err)
		}
	}
}

// runInboxEffect runs one action against the row's host, over a client of
// its own: an attach never shares the hostSet's connections either.
func (a *app) runInboxEffect(ctx context.Context, e inboxEffect) inboxResult {
	if e.create {
		c := &client{paths: a.paths}
		defer c.close()
		ctx, cancel := context.WithTimeout(ctx, inboxActionTimeout)
		defer cancel()
		res, message, err := c.createHere(ctx, e.createName, e.createAgent)
		return createResult(res, message, c.explain(err), requestSent(err))
	}
	r := e.remove
	out := inboxResult{key: r.key(), address: r.address(), host: r.host.name}
	c := &client{paths: a.paths, host: r.host.host}
	defer c.close()
	ctx, cancel := context.WithTimeout(ctx, inboxActionTimeout)
	defer cancel()
	// ExpectUID makes the daemon refuse if the session the user confirmed
	// was replaced by a new one under its name (protocol.CapKillExpectUID).
	// A daemon without it would remove by name, so the inbox asks it for the
	// session first and refuses if the UID changed. A replacement between
	// that answer and the kill still goes through there. Nothing was sent
	// if this fails, so its errors are certain.
	if err := checkIncarnation(ctx, c, r.rec.SessionID, r.rec.UID); err != nil {
		out.err = c.explain(err)
		return out
	}
	resp, err := c.requestCtx(ctx, &protocol.Request{KillSession: &protocol.KillSession{SessionID: r.rec.SessionID, Remove: true, ExpectUID: r.rec.UID}})
	switch {
	case err != nil:
		// Unknown once the kill was sent: the daemon may have acted.
		out.err, out.unknown = c.explain(err), requestSent(err)
	case resp.Error != nil:
		out.err = errors.New(resp.Error.Message)
	case resp.KillSession == nil:
		out.err = fmt.Errorf("unexpected response: %s", describeResponse(resp))
	}
	return out
}

// createResult is what a create reports: a daemon's refusal (message) is
// certain, and an error is unknown once the request was sent (the daemon
// may have started the session before the answer was lost).
func createResult(res *session.CreateResult, message string, err error, sent bool) inboxResult {
	switch {
	case err != nil:
		return inboxResult{create: true, err: err, unknown: sent}
	case message != "":
		return inboxResult{create: true, err: errors.New(message)}
	}
	return inboxResult{create: true, created: res}
}

// checkIncarnation refuses when the daemon c reaches lacks
// protocol.CapKillExpectUID and the session named id no longer has the
// UID (uid) that the row showed. A daemon with the capability checks this itself, and a
// record without a UID has nothing to compare.
func checkIncarnation(ctx context.Context, c *client, id, uid string) error {
	if uid == "" {
		return nil
	}
	cs, err := c.controlStream(ctx)
	if err != nil {
		return err
	}
	if cs.features.Has(protocol.CapKillExpectUID) {
		return nil
	}
	resp, err := c.callCtx(ctx, &protocol.Request{GetSession: &protocol.SessionRef{SessionID: id}}, func(r *protocol.Response) bool { return r.Session != nil })
	if err != nil {
		return err
	}
	if resp.Session.UID != uid {
		// The daemon's own refusal, word for word (killSession in
		// internal/daemon/sessions.go).
		return fmt.Errorf("session `%s` is not the session you chose: it was removed and a new session was started under the same name", id)
	}
	return nil
}

// inboxView is what runInbox shows.
type inboxView int

const (
	// inboxDrawn: the inbox.
	inboxDrawn inboxView = iota
	// inboxNarrowed: narrowed while open (a split pane, say), a one-line
	// notice until it widens. Results still arrive; an error waits there.
	inboxNarrowed
	// inboxWaiting: too narrow from the start, nothing yet, until every
	// host has answered.
	inboxWaiting
	// inboxListed: too narrow from the start and every host answered, the
	// rows as `agt ls` prints them, then leave.
	inboxListed
)

// chooseInboxView picks what a terminal cols wide shows, given whether
// the inbox has been drawn yet and whether every host has answered (or
// the wait for them is over).
func chooseInboxView(cols int, drawn, settled bool) inboxView {
	switch {
	case cols >= inboxMinWidth:
		return inboxDrawn
	case drawn:
		return inboxNarrowed
	case settled:
		return inboxListed
	}
	return inboxWaiting
}

// setHidden records whether the inbox is on screen. Hiding it takes a
// remove confirmation off the screen too: once shown again, it waits its
// inboxAcknowledgeAfter afresh.
func (i *inbox) setHidden(hidden bool) {
	i.hidden = hidden
	if hidden {
		i.confirmShown, i.confirmCropped = time.Time{}, false
	}
}

// narrowNotice is what an open inbox draws in a terminal too narrow for
// it, with status (narrowStatus) when there is one.
func narrowNotice(cols int, status string) []string {
	text := fmt.Sprintf("agt: widen the terminal to %d columns for the inbox", inboxMinWidth)
	if status != "" {
		text = fmt.Sprintf("agt: %s; widen the terminal to %d columns", escapeControls(strings.Join(strings.Fields(status), " ")), inboxMinWidth)
	}
	return []string{truncateCells(text, lineWidth(cols))}
}

// narrowStatus is what a narrowed inbox has to say: that an action failed,
// or what it waits for to leave.
func (i *inbox) narrowStatus() string {
	switch {
	case len(i.failures) > 0:
		return "an action failed"
	case i.leaving != nil:
		return i.waitingFor()
	}
	return ""
}

// messageLines are the failures the user has not seen (the newest
// inboxFailuresShown of them, after a count of the rest), what a leave
// waits for, and the notice.
func (i *inbox) messageLines() []line {
	t := i.theme
	text := func(style, s string) line {
		return line{spans: []span{{"", " "}, {style, escapeControls(strings.Join(strings.Fields(s), " "))}}}
	}
	var lines []line
	shown := i.failures[max(len(i.failures)-inboxFailuresShown, 0):]
	if more := len(i.failures) - len(shown); more > 0 {
		lines = append(lines, text(t.signalFG, fmt.Sprintf("%d earlier actions failed too", more)))
	}
	for _, f := range shown {
		lines = append(lines, text(t.signalFG, f))
	}
	if i.leaving != nil {
		lines = append(lines, text(t.mutedFG, i.waitingFor()))
	}
	if i.notice != nil && i.now().Before(i.noticeUntil) {
		style := t.mutedFG
		if i.notice.isError {
			style = t.signalFG
		}
		lines = append(lines, text(style, i.notice.message))
	}
	return lines
}

// attachFromInbox attaches to the chosen session on its host, keeping the
// inbox's screen (client.fromInbox), and says how the session ended if it
// did.
func (a *app) attachFromInbox(o *inboxOutcome) (*protocol.SessionEnded, error) {
	c := &client{paths: a.paths, host: o.host, fromInbox: true}
	a.client = c // so a refused key's error explains itself
	defer c.close()
	return c.attachLoop(o.id, o.uid)
}

// attached takes what an attachment from the inbox came back with: the
// inbox is as it was, and says how the session ended or why it could not
// attach, and asks the session's host again.
func (i *inbox) attached(o *inboxOutcome, ended *protocol.SessionEnded, err error) {
	i.leaving = nil
	name, address := localHostName, o.id
	if o.host != nil {
		name, address = o.host.Name, o.host.Name+"/"+o.id
	}
	switch {
	case err != nil:
		i.fail("could not attach to %s: %v", address, err)
	case ended != nil:
		i.setNotice(&inboxNotice{message: address + " ended: " + formatSessionEnd(ended)})
	}
	i.effects = append(i.effects, inboxEffect{reprobe: name})
}

// unseen is what the user leaves the inbox without seeing: the failures
// not yet read, and the actions a second Esc (or Ctrl-C) abandoned in
// flight, whose result nobody will report.
func (i *inbox) unseen() []string {
	lines := slices.Clone(i.failures)
	for _, a := range i.inFlight() {
		lines = append(lines, "abandoned "+a+"; result unknown")
	}
	return lines
}

// printUnseen prints lines from unseen, escaped, one "agt:" line each.
func printUnseen(lines []string) {
	for _, l := range lines {
		writeOut([]byte("agt: " + escapeControls(strings.Join(strings.Fields(l), " ")) + "\r\n"))
	}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// printNarrow prints the rows the inbox holds the way `agt ls --all` does,
// for a terminal too narrow to draw it in. Only the inbox's own hosts are
// in it, so `--host NAME` still means that host alone.
func (i *inbox) printNarrow(cols int) {
	var sessions []session.Record
	var hosts []string
	for _, r := range i.allRows() {
		sessions = append(sessions, *r.rec)
		hosts = append(hosts, r.host.name)
	}
	lines := renderSessionList(sessions, hosts, max(cols, 1), i.now())
	lines = append(lines, fmt.Sprintf("agt: the inbox needs %d columns; this is its list", inboxMinWidth))
	writeOut([]byte(strings.Join(lines, "\r\n") + "\r\n"))
}

func remoteCreateHint(host string) string {
	return fmt.Sprintf("start sessions on `%s` with `agt new -C DIR %s/NAME`", host, host)
}

// createHere starts a session in the current directory for the inbox or
// the overlay. A daemon's refusal comes back as message (an inbox failure,
// an overlay toast).
func (c *client) createHere(ctx context.Context, name, agent string) (res *session.CreateResult, message string, err error) {
	cwd, err := resolveCwd("")
	if err != nil {
		return nil, "", err
	}
	req := &protocol.CreateSession{Cwd: cwd, Agent: agent, Colors: terminalColors()}
	if name != "" {
		req.Name = &name
	}
	if agent == "codex" {
		model := codexDefaultModel
		req.Model = &model
	}
	resp, err := c.requestCtx(ctx, &protocol.Request{CreateSession: req})
	switch {
	case err != nil:
		return nil, "", err
	case resp.Error != nil:
		return nil, resp.Error.Message, nil
	case resp.CreateSession == nil:
		return nil, "", fmt.Errorf("unexpected response: %s", describeResponse(resp))
	}
	return resp.CreateSession, "", nil
}

// createFromUI is createHere within the client's request timeout,
// returning the new session's name.
func (c *client) createFromUI(name, agent string) (id, message string, err error) {
	ctx, cancel := contextFor(c.requestTimeout())
	defer cancel()
	res, message, err := c.createHere(ctx, name, agent)
	if res != nil {
		id = res.SessionID
	}
	return id, message, err
}
