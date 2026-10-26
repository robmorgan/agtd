package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"agtd.sh/internal/protocol"
	"agtd.sh/internal/session"
	"agtd.sh/internal/transport"
)

func onlineProbe(name string, cpus uint32, running int, agents []string, workspaces ...string) *hostProbe {
	p := &hostProbe{name: name, status: hostOnline, workspaces: workspaces, latency: 1500 * time.Microsecond,
		welcome: &protocol.Welcome{Version: 1, DaemonVersion: "0.1.0",
			Host: protocol.HostInfo{CPUs: cpus, Agents: agents, DefaultAgent: agents[0]}}}
	if name != localHostName {
		p.host = &transport.Host{Name: name, Address: name + ":7433"}
	}
	for i := 0; i < running; i++ {
		p.sessions = append(p.sessions, session.Record{SessionID: fmt.Sprint(i), Status: session.StatusRunning})
	}
	p.sessions = append(p.sessions, session.Record{SessionID: "old", Status: session.StatusExited})
	return p
}

func TestChoosePlacement(t *testing.T) {
	local := onlineProbe(localHostName, 8, 4, []string{"claude", "codex"}, "mono")
	devbox := onlineProbe("devbox", 32, 4, []string{"claude"}, "mono")
	gpu := onlineProbe("gpu", 64, 0, []string{"codex"})
	down := &hostProbe{name: "down", status: hostOffline}

	p, err := choosePlacement([]*hostProbe{local, devbox, gpu, down}, "claude", "mono")
	if err != nil || p.probe != devbox || !strings.Contains(p.reason, "4 running on 32 CPUs") {
		t.Fatalf("claude in mono: %+v, %v", p, err)
	}
	// The lightest host wins when it has what the session needs.
	if p, _ := choosePlacement([]*hostProbe{local, devbox, gpu, down}, "codex", ""); p.probe != gpu {
		t.Fatalf("codex: %s", p.probe.name)
	}
	// "" is each host's default agent; ties go to the host listed first.
	if p, _ := choosePlacement([]*hostProbe{onlineProbe(localHostName, 4, 1, []string{"sh"}), onlineProbe("b", 4, 1, []string{"sh"})}, "", ""); p.probe.name != localHostName {
		t.Fatalf("tie: %s", p.probe.name)
	}
	_, err = choosePlacement([]*hostProbe{gpu, down}, "claude", "")
	if err == nil || !strings.Contains(err.Error(), "gpu has no agent `claude`") || !strings.Contains(err.Error(), "down is offline") {
		t.Fatalf("no candidate: %v", err)
	}
	if _, err := choosePlacement([]*hostProbe{gpu}, "codex", "mono"); err == nil || !strings.Contains(err.Error(), "no workspace `mono`") {
		t.Fatalf("missing workspace: %v", err)
	}
}

func TestClassifyProbe(t *testing.T) {
	changed := &hostKeyChangedError{changed: &transport.KeyChangedError{Pinned: "a", Presented: "b"}, msg: "changed"}
	for err, want := range map[error]string{
		nil:                          hostOnline,
		changed:                      hostKeyChanged,
		fmt.Errorf("x: %w", changed): hostKeyChanged,
		context.DeadlineExceeded:     hostOffline,
		fmt.Errorf("dial: %w", context.DeadlineExceeded): hostOffline,
		errors.New("unexpected response"):                hostError,
	} {
		if got := classifyProbe(err); got != want {
			t.Errorf("classifyProbe(%v) = %s, want %s", err, got, want)
		}
	}
}

func TestRenderHosts(t *testing.T) {
	var out bytes.Buffer
	renderHosts(&out, []*hostProbe{
		onlineProbe(localHostName, 8, 2, []string{"claude", "codex"}),
		{name: "gpu", address: "10.0.0.9:7433", status: hostOffline, err: context.DeadlineExceeded},
		{name: "new", address: "10.0.0.7:7433", status: hostUnauthorized, err: errors.New("refused")},
	})
	got := out.String()
	for _, want := range []string{
		"NAME   ADDRESS         STATUS        LATENCY  SESSIONS  CPUS  AGENTS        VERSION",
		"local  (this machine)  online        1.5ms    2/3       8     claude,codex  0.1.0",
		"gpu    10.0.0.9:7433   offline       -",
		"new: refused",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	// An offline host's timeout is its status, not an error line.
	if strings.Contains(got, "gpu:") {
		t.Errorf("offline host has an error line:\n%s", got)
	}
}

func TestReservedHostNames(t *testing.T) {
	for _, name := range []string{localHostName, autoHostName} {
		if err := transport.ValidHostName(name); err == nil {
			t.Errorf("%s accepted as a host name", name)
		}
	}
	a := &app{paths: testClient(t).paths, host: autoHostName}
	if _, err := a.connect(nil, "", nil); err == nil || !strings.Contains(err.Error(), "only chooses a host for `agt new`") {
		t.Fatalf("--host auto: %v", err)
	}
}

// hostSetApp is an app whose hosts.toml lists names, for hostSet tests
// that replace the probe.
func hostSetApp(t *testing.T, names ...string) *app {
	t.Helper()
	c := testClient(t)
	if err := c.paths.EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	for i, name := range names {
		if err := transport.AddHost(c.paths.HostsPath(), transport.Host{Name: name, Address: fmt.Sprintf("127.0.0.1:%d", 7000+i), Fingerprint: "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}); err != nil {
			t.Fatal(err)
		}
	}
	return &app{paths: c.paths}
}

// nextProbe waits for the set's next snapshot.
func nextProbe(t *testing.T, set *hostSet) *hostProbe {
	t.Helper()
	select {
	case p := <-set.updates:
		return p
	case <-time.After(5 * time.Second):
		t.Fatal("no probe arrived")
		return nil
	}
}

// Each host's snapshot arrives as soon as that host answers: a host that
// hangs never holds up another, and stop ends every goroutine, the hung
// probe included.
func TestHostSetStreamsEachHost(t *testing.T) {
	a := hostSetApp(t, "dev", "dead")
	set, placeholders, err := a.startHostSet(hostSetOptions{probe: func(ctx context.Context, c *client, p *hostProbe) {
		if p.name == "dead" {
			<-ctx.Done()
			p.status = hostOffline
			return
		}
		p.status = hostOnline
		p.sessions = []session.Record{{SessionID: p.name + "-a"}}
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(placeholders) != 3 || placeholders[0].name != localHostName || placeholders[1].name != "dev" || placeholders[2].status != hostAsking {
		t.Fatalf("placeholders = %+v", placeholders)
	}
	seen := map[string]bool{}
	for len(seen) < 2 {
		p := nextProbe(t, set)
		if p.name == "dead" {
			t.Fatal("the hung host answered")
		}
		seen[p.name] = true
	}
	done := make(chan struct{})
	go func() { set.stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not end the probes")
	}
}

// A host is probed one probe at a time, however often it is asked; a
// failing one is asked again after a backoff that doubles up to its cap,
// and r (reprobeHost) asks at once and starts the backoff over.
func TestHostSetBackoffAndReprobe(t *testing.T) {
	a := hostSetApp(t, "down")
	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0
	var calls []time.Time
	set, _, err := a.startHostSet(hostSetOptions{only: "down", retryFirst: 40 * time.Millisecond, retryMax: 160 * time.Millisecond,
		probe: func(ctx context.Context, c *client, p *hostProbe) {
			mu.Lock()
			inFlight++
			maxInFlight = max(maxInFlight, inFlight)
			calls = append(calls, time.Now())
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
			mu.Lock()
			inFlight--
			mu.Unlock()
			p.status = hostOffline
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer set.stop()
	for range 6 {
		nextProbe(t, set)
		set.reprobeHost("down") // asked again and again while probing
		set.reprobeHost("down")
	}
	mu.Lock()
	if maxInFlight != 1 {
		t.Errorf("%d probes of one host at once", maxInFlight)
	}
	mu.Unlock()

	// Without reprobes the gaps grow 40, 80, 160, 160 ms.
	for range 2 {
		nextProbe(t, set) // drain what the reprobes above started
	}
	mu.Lock()
	start := len(calls)
	mu.Unlock()
	for range 4 {
		nextProbe(t, set)
	}
	mu.Lock()
	gaps := []time.Duration{}
	for i := start; i+1 < len(calls); i++ {
		gaps = append(gaps, calls[i+1].Sub(calls[i]))
	}
	mu.Unlock()
	if len(gaps) < 2 || gaps[len(gaps)-1] < 150*time.Millisecond || gaps[len(gaps)-1] > 400*time.Millisecond {
		t.Errorf("backoff gaps = %v, want growing to the 160ms cap", gaps)
	}

	// A reprobe answers at once.
	asked := time.Now()
	set.reprobeHost("down")
	nextProbe(t, set)
	if waited := time.Since(asked); waited > 100*time.Millisecond {
		t.Errorf("reprobe took %v", waited)
	}
}

// A view locked to one remote host never probes, or starts, this
// machine's daemon.
func TestHostSetLockedToOneHost(t *testing.T) {
	a := hostSetApp(t, "dev", "other")
	var mu sync.Mutex
	probed := map[string]bool{}
	set, placeholders, err := a.startHostSet(hostSetOptions{only: "dev", startLocal: true, probe: func(ctx context.Context, c *client, p *hostProbe) {
		mu.Lock()
		probed[p.name] = true
		mu.Unlock()
		p.status = hostOnline
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(placeholders) != 1 || placeholders[0].name != "dev" {
		t.Fatalf("placeholders = %+v", placeholders)
	}
	if p := nextProbe(t, set); p.name != "dev" {
		t.Fatalf("probe of %s", p.name)
	}
	set.stop()
	if probed[localHostName] || probed["other"] {
		t.Fatalf("probed %v", probed)
	}
	if _, _, err := a.startHostSet(hostSetOptions{only: "nope"}); err == nil {
		t.Fatal("an unknown host was accepted")
	}
}
