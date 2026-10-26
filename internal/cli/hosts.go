package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"agtd.sh/internal/protocol"
	"agtd.sh/internal/session"
	"agtd.sh/internal/transport"
)

// localHostName names this machine's own daemon wherever hosts are listed
// or chosen. It cannot be used for a remote host, and neither can
// autoHostName, which asks `agt new` to choose a host.
const (
	localHostName = "local"
	autoHostName  = "auto"
)

// probeTimeout bounds one host probe: connecting, the handshake, and
// listing its sessions. Hosts are probed in parallel, so a whole listing
// takes about this long when a host is down.
const probeTimeout = 4 * time.Second

// Host states, as listed by `agt host ls`.
const (
	hostOnline       = "online"
	hostOffline      = "offline"
	hostNotRunning   = "not running"
	hostUnauthorized = "unauthorized"
	hostKeyChanged   = "key changed"
	hostError        = "error"
	// The inbox's states for a host it has not heard from yet: still
	// being asked, or this machine's daemon being started.
	hostAsking   = "asking"
	hostStarting = "starting"
)

// hostProbe is what probing one daemon found.
type hostProbe struct {
	name    string // localHostName for this machine's daemon
	address string
	host    *transport.Host // nil for the local daemon
	status  string
	err     error
	// latency is the round trip of one request on an established
	// connection, so it measures the network and the daemon, not the
	// handshake.
	latency    time.Duration
	welcome    *protocol.Welcome
	sessions   []session.Record
	workspaces []string // only when asked for
	// startFailed marks the one probe that says this machine's daemon
	// could not be started (err says why).
	startFailed bool
}

func (h *hostProbe) online() bool { return h.status == hostOnline }

// running counts the host's running sessions.
func (h *hostProbe) running() int {
	n := 0
	for _, s := range h.sessions {
		if s.Status == session.StatusRunning {
			n++
		}
	}
	return n
}

// hasAgent reports whether the host can run agent ("" means its default).
func (h *hostProbe) hasAgent(agent string) bool {
	return h.welcome != nil && (agent == "" || slices.Contains(h.welcome.Host.Agents, agent))
}

// probeHosts probes the local daemon (only if it is running: listing hosts
// never starts one) and every host in hosts.toml, in parallel, and returns
// them local first, then in file order. withWorkspaces also lists each
// host's workspaces.
func (a *app) probeHosts(withWorkspaces bool) ([]*hostProbe, error) {
	hosts, err := transport.ReadHosts(a.paths.HostsPath())
	if err != nil {
		return nil, err
	}
	probes := []*hostProbe{{name: localHostName, address: a.paths.Socket}}
	for i := range hosts {
		probes = append(probes, &hostProbe{name: hosts[i].Name, address: hosts[i].Address, host: &hosts[i]})
	}
	var wg sync.WaitGroup
	for _, p := range probes {
		wg.Go(func() { a.probe(p, withWorkspaces) })
	}
	wg.Wait()
	return probes, nil
}

func (a *app) probe(p *hostProbe, withWorkspaces bool) {
	c := &client{paths: a.paths, host: p.host}
	defer c.close()
	probeOn(context.Background(), c, p, withWorkspaces)
}

// probeOn probes p's daemon through c, whose connection and control stream
// it reuses when they are open, within probeTimeout of ctx.
func probeOn(ctx context.Context, c *client, p *hostProbe, withWorkspaces bool) {
	if p.host == nil && !c.hasControl() && !c.localDaemonAnswers() {
		p.status = hostNotRunning
		return
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	p.err = func() error {
		cs, err := c.controlStream(ctx)
		if err != nil {
			return err
		}
		p.welcome = cs.welcome
		start := time.Now()
		resp, err := cs.roundTrip(ctx, &protocol.Request{ListSessions: protocol.Empty})
		p.latency = time.Since(start)
		if err != nil {
			return err
		}
		if resp.Sessions == nil {
			return fmt.Errorf("unexpected response: %s", describeResponse(resp))
		}
		p.sessions = *resp.Sessions
		if !withWorkspaces {
			return nil
		}
		resp, err = cs.roundTrip(ctx, &protocol.Request{ListWorkspaces: protocol.Empty})
		if err != nil {
			return err
		}
		if resp.Workspaces != nil {
			for _, w := range *resp.Workspaces {
				p.workspaces = append(p.workspaces, w.Name)
			}
		}
		return nil
	}()
	p.status = classifyProbe(p.err)
}

// hostSet keeps a probe of every host current while the inbox runs.
//
// Ownership: one goroutine per host owns that host's client (its connection
// and control stream, kept open between probes) and probes it in a loop,
// one probe at a time, sending each result on updates as a new hostProbe
// it never touches again. The inbox's goroutine owns everything it reads
// from updates. updates holds one probe per host, so a busy inbox only
// delays probing. stop cancels every goroutine, closing every connection,
// and waits for them.
type hostSet struct {
	updates chan *hostProbe
	watches []*hostWatch
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// hostWatch is one host of a hostSet.
type hostWatch struct {
	name string
	host *transport.Host // nil for this machine's daemon
	// reprobe asks for a probe now, resetting an offline host's backoff.
	reprobe chan struct{}
}

// How often a hostSet probes. The local daemon answers at once, so it is
// asked often enough for the inbox to feel live; a remote round trip is
// paid at most every hostRemoteInterval. A host that cannot be reached is
// asked again after hostRetryFirst, doubling up to hostRetryMax.
const (
	hostLocalInterval  = 200 * time.Millisecond
	hostRemoteInterval = 2 * time.Second
	hostRetryFirst     = 2 * time.Second
	hostRetryMax       = 30 * time.Second
)

// hostSetOptions are what a hostSet probes, and how; the zero value of a
// duration means its default.
type hostSetOptions struct {
	// only probes that host alone (`agt --host NAME`); "" probes this
	// machine and every host in hosts.toml.
	only string
	// startLocal starts this machine's daemon if it is not running.
	startLocal bool

	retryFirst, retryMax time.Duration
	// probe replaces probeOn, for tests.
	probe func(ctx context.Context, c *client, p *hostProbe)
}

// startHostSet starts probing. The probes it returns are each host's
// placeholder (hostAsking), local first then in hosts.toml order, until
// its first probe arrives.
func (a *app) startHostSet(o hostSetOptions) (*hostSet, []*hostProbe, error) {
	hosts, err := transport.ReadHosts(a.paths.HostsPath())
	if err != nil {
		return nil, nil, err
	}
	var watches []*hostWatch
	if o.only == "" || o.only == localHostName {
		watches = append(watches, &hostWatch{name: localHostName})
	}
	for i := range hosts {
		if o.only == "" || o.only == hosts[i].Name {
			watches = append(watches, &hostWatch{name: hosts[i].Name, host: &hosts[i]})
		}
	}
	if len(watches) == 0 {
		return nil, nil, fmt.Errorf("unknown host `%s`; see `agt host ls`", o.only)
	}
	o.retryFirst = cmp.Or(o.retryFirst, hostRetryFirst)
	o.retryMax = cmp.Or(o.retryMax, hostRetryMax)
	if o.probe == nil {
		o.probe = func(ctx context.Context, c *client, p *hostProbe) { probeOn(ctx, c, p, false) }
	}
	ctx, cancel := context.WithCancel(context.Background())
	set := &hostSet{updates: make(chan *hostProbe, len(watches)), watches: watches, cancel: cancel}
	placeholders := make([]*hostProbe, len(watches))
	for i, w := range watches {
		w.reprobe = make(chan struct{}, 1)
		placeholders[i] = w.placeholder(a.paths.Socket, hostAsking)
		set.wg.Go(func() { set.watch(ctx, a, w, o) })
	}
	return set, placeholders, nil
}

func (w *hostWatch) placeholder(socket, status string) *hostProbe {
	p := &hostProbe{name: w.name, address: socket, host: w.host, status: status}
	if w.host != nil {
		p.address = w.host.Address
	}
	return p
}

// watch is one host's goroutine: probe, send, wait, until ctx ends.
func (set *hostSet) watch(ctx context.Context, a *app, w *hostWatch, o hostSetOptions) {
	c := &client{paths: a.paths, host: w.host}
	defer c.close()
	send := func(p *hostProbe) bool {
		select {
		case set.updates <- p:
			return true
		case <-ctx.Done():
			return false
		}
	}
	if w.host == nil && o.startLocal && !c.localDaemonAnswers() {
		if !send(w.placeholder(a.paths.Socket, hostStarting)) {
			return
		}
		if err := c.ensureDaemon(); err != nil {
			p := w.placeholder(a.paths.Socket, hostError)
			p.err, p.startFailed = err, true
			if !send(p) {
				return
			}
		}
	}
	retry := time.Duration(0)
	for {
		p := w.placeholder(a.paths.Socket, "")
		o.probe(ctx, c, p)
		if ctx.Err() != nil {
			return
		}
		wait := hostRemoteInterval
		if w.host == nil {
			wait = hostLocalInterval
		}
		if p.online() {
			retry = 0
		} else {
			// Start again from a new connection, after a backoff.
			c.close()
			retry = min(max(2*retry, o.retryFirst), o.retryMax)
			wait = retry
		}
		if !send(p) {
			return
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-w.reprobe:
			timer.Stop()
			retry = 0
		case <-ctx.Done():
			timer.Stop()
			return
		}
	}
}

// reprobeAll asks every host for a probe now.
func (set *hostSet) reprobeAll() {
	for _, w := range set.watches {
		set.reprobeHost(w.name)
	}
}

// reprobeHost asks one host for a probe now. A request while one is
// already pending is the same request.
func (set *hostSet) reprobeHost(name string) {
	for _, w := range set.watches {
		if w.name == name {
			select {
			case w.reprobe <- struct{}{}:
			default:
			}
		}
	}
}

// stop ends every host's goroutine and connection and waits for them.
func (set *hostSet) stop() {
	set.cancel()
	set.wg.Wait()
}

// classifyProbe turns a probe's error into a host state.
func classifyProbe(err error) string {
	var changed *transport.KeyChangedError
	switch {
	case err == nil:
		return hostOnline
	case errors.As(err, &changed):
		return hostKeyChanged
	case transport.IsKeyRefused(err):
		return hostUnauthorized
	case transport.IsConnectionLost(err), errors.Is(err, context.DeadlineExceeded):
		return hostOffline
	}
	return hostError
}

// renderHosts writes the host table of `agt host ls`.
func renderHosts(w io.Writer, probes []*hostProbe) {
	rows := [][]string{{"NAME", "ADDRESS", "STATUS", "LATENCY", "SESSIONS", "CPUS", "AGENTS", "VERSION"}}
	for _, p := range probes {
		row := []string{p.name, p.address, p.status, "-", "-", "-", "-", "-"}
		if p.name == localHostName {
			row[1] = "(this machine)"
		}
		if p.online() {
			h := p.welcome.Host
			row[3] = formatLatency(p.latency)
			row[4] = fmt.Sprintf("%d/%d", p.running(), len(p.sessions))
			row[5] = fmt.Sprint(h.CPUs)
			row[6] = strings.Join(h.Agents, ",")
			row[7] = p.welcome.DaemonVersion
		}
		rows = append(rows, row)
	}
	widths := make([]int, len(rows[0]))
	for _, row := range rows {
		for i, cell := range row {
			widths[i] = max(widths[i], len(cell))
		}
	}
	for _, row := range rows {
		var line strings.Builder
		for i, cell := range row {
			if i > 0 {
				line.WriteString("  ")
			}
			fmt.Fprintf(&line, "%-*s", widths[i], escapeControls(cell))
		}
		fmt.Fprintln(w, strings.TrimRight(line.String(), " "))
	}
	for _, p := range probes {
		if p.err != nil && p.status != hostOffline {
			fmt.Fprintf(w, "%s: %v\n", p.name, p.err)
		}
	}
}

func formatLatency(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return fmt.Sprintf("%dµs", d.Microseconds())
	case d < 10*time.Millisecond:
		return fmt.Sprintf("%.1fms", float64(d.Microseconds())/1000)
	}
	return fmt.Sprintf("%dms", d.Milliseconds())
}

// renderHostInfo writes `agt host info`: the machine and daemon behind a
// host.
func renderHostInfo(w io.Writer, p *hostProbe) {
	fmt.Fprintf(w, "name: %s\n", p.name)
	if p.host != nil {
		fmt.Fprintf(w, "address: %s\n", p.host.Address)
		fmt.Fprintf(w, "fingerprint: %s\n", p.host.Fingerprint)
	} else {
		fmt.Fprintf(w, "socket: %s\n", p.address)
	}
	fmt.Fprintf(w, "status: %s\n", p.status)
	if p.err != nil {
		fmt.Fprintf(w, "error: %v\n", p.err)
	}
	if !p.online() {
		return
	}
	h := p.welcome.Host
	fmt.Fprintf(w, "latency: %s\n", formatLatency(p.latency))
	fmt.Fprintf(w, "hostname: %s\n", escapeControls(h.Name))
	fmt.Fprintf(w, "platform: %s/%s\n", escapeControls(h.OS), escapeControls(h.Arch))
	fmt.Fprintf(w, "cpus: %d\n", h.CPUs)
	if h.MemoryBytes > 0 {
		fmt.Fprintf(w, "memory: %s\n", formatBytes(h.MemoryBytes))
	}
	fmt.Fprintf(w, "agents: %s\n", escapeControls(strings.Join(h.Agents, ", ")))
	fmt.Fprintf(w, "default_agent: %s\n", escapeControls(h.DefaultAgent))
	fmt.Fprintf(w, "sessions: %d running, %d total\n", p.running(), len(p.sessions))
	fmt.Fprintf(w, "daemon_version: %s\n", escapeControls(p.welcome.DaemonVersion))
	fmt.Fprintf(w, "protocol_version: %d\n", p.welcome.Version)
	fmt.Fprintf(w, "capabilities: %s\n", escapeControls(strings.Join(p.welcome.Capabilities, ", ")))
}

// placement is where `agt new --host auto` runs a session, and why.
type placement struct {
	probe  *hostProbe
	reason string
}

// choosePlacement picks a host for a new session: one that is online, has
// the agent, and has the workspace when one is named, with the fewest
// running sessions per CPU; ties go to this machine, then to the host
// listed first. It is a hint, not a scheduler: it knows nothing about what
// the sessions are doing.
func choosePlacement(probes []*hostProbe, agent, workspace string) (*placement, error) {
	var candidates []*hostProbe
	var skipped []string
	for _, p := range probes {
		switch {
		case !p.online():
			skipped = append(skipped, fmt.Sprintf("%s is %s", p.name, p.status))
		case !p.hasAgent(agent):
			skipped = append(skipped, fmt.Sprintf("%s has no agent `%s`", p.name, agent))
		case workspace != "" && !slices.Contains(p.workspaces, workspace):
			skipped = append(skipped, fmt.Sprintf("%s has no workspace `%s`", p.name, workspace))
		default:
			candidates = append(candidates, p)
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no host can run this session: %s", strings.Join(skipped, "; "))
	}
	load := func(p *hostProbe) float64 { return float64(p.running()) / float64(max(p.welcome.Host.CPUs, 1)) }
	best := slices.MinFunc(candidates, func(x, y *hostProbe) int { return cmp.Compare(load(x), load(y)) })
	return &placement{
		probe: best,
		reason: fmt.Sprintf("%d running on %d CPUs, the lightest of %d %s", best.running(), best.welcome.Host.CPUs,
			len(candidates), plural(len(candidates), "candidate", "candidates")),
	}, nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
