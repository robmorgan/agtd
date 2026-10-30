package worker

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"

	"agtd.sh/internal/protocol"
	"agtd.sh/internal/session"
	"agtd.sh/internal/sshagent"
)

// These tests run the worker with an agent socket, attach to it as the
// daemon's clients do, open forwarders backed by in-memory keyrings, and use
// the agent socket as programs in the session do (x/crypto/ssh/agent's
// client speaks the real protocol).

// agentScript echoes lines like echoAgent; "auth" prints its SSH_AUTH_SOCK.
const agentScript = `stty -echo; echo ready
while IFS= read -r l; do
  case "$l" in
    auth) echo "auth:$SSH_AUTH_SOCK";;
    done) exit 0;;
    *) echo "got:$l";;
  esac
done`

// startAgentWorker starts a worker with an agent socket, whose own
// SSH_AUTH_SOCK (the agent it inherited) is inherited, "" for none.
func startAgentWorker(t *testing.T, inherited string) *harness {
	t.Helper()
	t.Setenv("SSH_AUTH_SOCK", inherited)
	return startWorkerEdit(t, agentScript, func(a *Args) { a.SSHAgentSocket = true })
}

// endSession ends the session through attachment c and waits for the
// worker, so that variables a test changed can be restored.
func (h *harness) endSession(c *client) {
	h.t.Helper()
	c.input("done\n")
	c.expectEnd()
	h.waitExit()
}

func newSigner(t *testing.T) (ssh.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return key, priv
}

// newKeyring is an agent holding one new key.
func newKeyring(t *testing.T, comment string) (agent.Agent, ssh.PublicKey) {
	t.Helper()
	key, priv := newSigner(t)
	ring := agent.NewKeyring()
	if err := ring.Add(agent.AddedKey{PrivateKey: priv, Comment: comment}); err != nil {
		t.Fatal(err)
	}
	return ring, key
}

// bindingAgent accepts session bindings, as OpenSSH's agent does once it has
// checked the host key's signature; one whose signature is "rejected" fails
// that check. x/crypto's keyring refuses the extension.
type bindingAgent struct{ agent.ExtendedAgent }

func (b bindingAgent) Extension(name string, contents []byte) ([]byte, error) {
	if name != sshagent.ExtSessionBind {
		return b.ExtendedAgent.Extension(name, contents)
	}
	if bytes.Contains(contents, []byte("rejected")) {
		return nil, errors.New("the binding's signature does not verify")
	}
	return nil, nil
}

// serveKeyring serves ring on a Unix socket, as ssh-agent would, and
// returns its path.
func serveKeyring(t *testing.T, ring agent.Agent) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "agdk-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "agent.sock")
	serveKeyringAt(t, ring, path)
	return path
}

// serveKeyringAt serves ring on a Unix socket at path.
func serveKeyringAt(t *testing.T, ring agent.Agent, path string) {
	t.Helper()
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = agent.ServeAgent(ring, conn)
			}()
		}
	}()
}

// testForwarder is a client's forwarder: it answers the worker's requests
// from a keyring, one agent connection per program connection.
type testForwarder struct {
	conn net.Conn
	ring agent.Agent
	// hold leaves requests unanswered.
	hold atomic.Bool

	mu     sync.Mutex
	pipes  map[uint32]net.Conn
	seen   int
	closes []uint32
	done   chan struct{}
}

func (h *harness) forward(attachID string, ring agent.Agent) *testForwarder {
	h.t.Helper()
	f, resp := h.tryForward(protocol.ForwardSSHAgent{Attachment: attachID, ExpectUID: h.uid}, ring)
	if f == nil {
		h.t.Fatalf("forward: unexpected response %#v (error %v)", resp, resp.Error)
	}
	return f
}

func (h *harness) tryForward(req protocol.ForwardSSHAgent, ring agent.Agent) (*testForwarder, *protocol.Response) {
	h.t.Helper()
	conn := h.dial()
	req.SessionID = h.sessionID
	req.Client = "laptop (SHA256:test)"
	if err := protocol.WriteRequest(conn, &protocol.Request{ForwardSSHAgent: &req}); err != nil {
		h.t.Fatal(err)
	}
	r := bufio.NewReader(conn)
	conn.SetReadDeadline(time.Now().Add(testTimeout))
	resp, err := protocol.ReadResponse(r)
	if err != nil || resp == nil {
		h.t.Fatalf("forward: %v", err)
	}
	conn.SetReadDeadline(time.Time{})
	if resp.SSHAgentForwarding == nil {
		conn.Close()
		return nil, resp
	}
	f := &testForwarder{conn: conn, ring: ring, pipes: map[uint32]net.Conn{}, done: make(chan struct{})}
	go f.run(r)
	h.t.Cleanup(func() {
		conn.Close()
		<-f.done
	})
	return f, resp
}

func (f *testForwarder) run(r *bufio.Reader) {
	defer close(f.done)
	defer func() {
		f.mu.Lock()
		for _, p := range f.pipes {
			p.Close()
		}
		f.mu.Unlock()
	}()
	for {
		resp, err := protocol.ReadResponse(r)
		if err != nil || resp == nil {
			return
		}
		switch {
		case resp.SSHAgentRequest != nil:
			m := resp.SSHAgentRequest
			f.mu.Lock()
			f.seen++
			f.mu.Unlock()
			if f.hold.Load() {
				continue
			}
			reply := f.answer(m.Conn, m.Data)
			if err := protocol.WriteRequest(f.conn, &protocol.Request{SSHAgentReply: &protocol.SSHAgentMessage{Conn: m.Conn, Data: reply}}); err != nil {
				return
			}
		case resp.SSHAgentClose != nil:
			f.mu.Lock()
			f.closes = append(f.closes, resp.SSHAgentClose.Conn)
			if p := f.pipes[resp.SSHAgentClose.Conn]; p != nil {
				p.Close()
				delete(f.pipes, resp.SSHAgentClose.Conn)
			}
			f.mu.Unlock()
		}
	}
}

func (f *testForwarder) answer(id uint32, msg []byte) []byte {
	f.mu.Lock()
	p := f.pipes[id]
	if p == nil {
		var served net.Conn
		p, served = net.Pipe()
		go func() { _ = agent.ServeAgent(f.ring, served) }()
		f.pipes[id] = p
	}
	f.mu.Unlock()
	if err := sshagent.WriteMessage(p, msg); err != nil {
		return sshagent.Failure()
	}
	reply, err := sshagent.ReadMessage(p)
	if err != nil {
		return sshagent.Failure()
	}
	return reply
}

func (f *testForwarder) counts() (seen, closes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen, len(f.closes)
}

// agentConn connects to the session's agent socket as a program in the
// session would.
func (h *harness) agentConn() net.Conn {
	h.t.Helper()
	conn, err := net.Dial("unix", h.paths.SSHAgentSocketPath(h.uid))
	if err != nil {
		h.t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(testTimeout))
	h.t.Cleanup(func() { conn.Close() })
	return conn
}

// agentClient is an agent client on a new connection to the session's agent
// socket. One client per connection: x/crypto's client reads replies on a
// goroutine of its own, so a second one on the same connection would race
// it for them.
func (h *harness) agentClient() agent.ExtendedAgent {
	h.t.Helper()
	return agent.NewClient(h.agentConn())
}

// listKeys lists the keys the agent socket offers to client, by fingerprint.
func (h *harness) listKeys(client agent.ExtendedAgent) []string {
	h.t.Helper()
	keys, err := listKeys(client)
	if err != nil {
		h.t.Fatalf("listing keys: %v", err)
	}
	return keys
}

func listKeys(client agent.ExtendedAgent) ([]string, error) {
	keys, err := client.List()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, k := range keys {
		out = append(out, sshagent.Fingerprint(k.Blob))
	}
	return out, nil
}

func fingerprints(keys ...ssh.PublicKey) []string {
	var out []string
	for _, k := range keys {
		out = append(out, ssh.FingerprintSHA256(k))
	}
	return out
}

// events are the summaries of the session's events of kind.
func (h *harness) events(kind session.EventKind) []string {
	h.t.Helper()
	evs, err := h.store.EventsAfter(0, h.sessionID, 500)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []string
	for _, e := range evs {
		if e.Kind == kind {
			out = append(out, e.Summary)
		}
	}
	return out
}

// eventLike waits for an event of kind whose summary contains want.
func (h *harness) eventLike(kind session.EventKind, want string) {
	h.t.Helper()
	h.eventually(string(kind)+" event "+want, func() bool {
		return slices.ContainsFunc(h.events(kind), func(s string) bool { return strings.Contains(s, want) })
	})
}

func sshString(b []byte) []byte {
	return append(binary.BigEndian.AppendUint32(nil, uint32(len(b))), b...)
}

// The agent's SSH_AUTH_SOCK names the session's agent socket, which the
// worker says it can forward through. With no forwarder and no agent to
// fall back on it offers no keys; nothing is recorded, since the session
// was never forwarded to. The socket goes with the session.
func TestSSHAgentSocketForTheAgent(t *testing.T) {
	h := startAgentWorker(t, "")
	c, _ := h.attachWith(protocol.AttachSession{Features: rawAttachCapabilities()})
	path := h.paths.SSHAgentSocketPath(h.uid)
	c.input("auth\n")
	c.expectOutput("auth:" + path)
	if resp := h.request(&protocol.Request{Hello: protocol.NewHello("test")}); resp.Welcome == nil || !slices.Contains(resp.Welcome.Capabilities, protocol.CapSSHAgent) {
		t.Fatalf("hello = %#v", resp)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("agent socket: %v, %v", info, err)
	}

	client := h.agentClient()
	if keys := h.listKeys(client); len(keys) != 0 {
		t.Fatalf("keys without an agent: %v", keys)
	}
	key, _ := newSigner(t)
	if _, err := client.Sign(key, []byte("data")); err == nil {
		t.Fatal("signed without an agent")
	}
	h.endSession(c)
	// Ending the session writes every pending event, so none is missed.
	if evs := h.events(session.EventSSHAgentUnavailable); len(evs) != 0 {
		t.Fatalf("a session never forwarded to recorded %v", evs)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("agent socket after the session: %v", err)
	}
}

// Without an agent socket the agent keeps the SSH_AUTH_SOCK the daemon
// was started with, and the worker refuses forwarders.
func TestSSHAgentNeedsTheSocket(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "/inherited/agent.sock")
	h := startWorkerWith(t, agentScript)
	c, _ := h.attachWith(protocol.AttachSession{Features: rawAttachCapabilities()})
	c.input("auth\n")
	c.expectOutput("auth:/inherited/agent.sock")
	if resp := h.request(&protocol.Request{Hello: protocol.NewHello("test")}); resp.Welcome == nil || slices.Contains(resp.Welcome.Capabilities, protocol.CapSSHAgent) {
		t.Fatalf("hello = %#v", resp)
	}
	ring, _ := newKeyring(t, "laptop")
	if f, resp := h.tryForward(protocol.ForwardSSHAgent{Attachment: c.attachID, ExpectUID: h.uid}, ring); f != nil || resp.Error == nil || !strings.Contains(resp.Error.Message, "no SSH agent socket") {
		t.Fatalf("forwarder without a socket: %#v", resp)
	}
	if _, err := os.Stat(h.paths.SSHAgentSocketPath(h.uid)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("agent socket without the option: %v", err)
	}
	h.endSession(c)
}

// Requests go to the forwarder of the attachment that owns the session's
// size: taking the size takes the agent along, a pending attachment's
// forwarder is not used, and when the owner leaves the next owner's keys
// are. A connection keeps the route it started on.
func TestSSHAgentFollowsTheSizeOwner(t *testing.T) {
	h := startAgentWorker(t, "")
	all := rawAttachCapabilities()
	a, _ := h.attachWith(protocol.AttachSession{Features: all, Geometry: protocol.Geometry{Cols: 100, Rows: 30}})
	b, _ := h.attachWith(protocol.AttachSession{Features: all, Geometry: protocol.Geometry{Cols: 90, Rows: 20}})
	ringA, keyA := newKeyring(t, "a")
	ringB, keyB := newKeyring(t, "b")
	h.forward(a.attachID, ringA)
	h.forward(b.attachID, ringB)

	early := h.agentClient()
	if got := h.listKeys(early); !slices.Equal(got, fingerprints(keyA)) {
		t.Fatalf("keys with a owning the size: %v, want a's", got)
	}
	b.send(&protocol.Request{AttachTakeSize: &protocol.Geometry{Cols: 90, Rows: 20}})
	b.expectSizeNote("b to own the size", func(o protocol.SizeOwnership) bool { return o.Owner })
	if got := h.listKeys(h.agentClient()); !slices.Equal(got, fingerprints(keyB)) {
		t.Fatalf("keys once b took the size: %v, want b's", got)
	}
	if got := h.listKeys(early); !slices.Equal(got, fingerprints(keyA)) {
		t.Fatalf("a connection that started with a's keys moved: %v", got)
	}

	b.send(&protocol.Request{AttachLeave: protocol.Empty})
	b.conn.Close()
	a.expectSizeNote("a to own the size again", func(o protocol.SizeOwnership) bool { return o.Owner })
	if got := h.listKeys(h.agentClient()); !slices.Equal(got, fingerprints(keyA)) {
		t.Fatalf("keys once b left: %v, want a's", got)
	}
	h.endSession(a)
}

// When the attachment that owns the size does not forward, a forwarding
// pending one is not used: programs get no keys, which is recorded once per
// gap in forwarding.
func TestSSHAgentPendingForwarderIsNotUsed(t *testing.T) {
	h := startAgentWorker(t, "")
	all := rawAttachCapabilities()
	a, _ := h.attachWith(protocol.AttachSession{Features: all, Geometry: protocol.Geometry{Cols: 100, Rows: 30}})
	b, _ := h.attachWith(protocol.AttachSession{Features: all, Geometry: protocol.Geometry{Cols: 90, Rows: 20}})
	ringB, keyB := newKeyring(t, "b")
	h.forward(b.attachID, ringB)

	for range 2 {
		if got := h.listKeys(h.agentClient()); len(got) != 0 {
			t.Fatalf("keys from a pending attachment: %v", got)
		}
	}
	h.eventually("the gap to be recorded", func() bool { return len(h.events(session.EventSSHAgentUnavailable)) == 1 })

	// Taking the size uses b's keys, which ends the gap; the next one is
	// recorded again.
	b.send(&protocol.Request{AttachTakeSize: &protocol.Geometry{Cols: 90, Rows: 20}})
	b.expectSizeNote("b to own the size", func(o protocol.SizeOwnership) bool { return o.Owner })
	if got := h.listKeys(h.agentClient()); !slices.Equal(got, fingerprints(keyB)) {
		t.Fatalf("keys once b took the size: %v", got)
	}
	a.send(&protocol.Request{AttachTakeSize: &protocol.Geometry{Cols: 100, Rows: 30}})
	a.expectSizeNote("a to own the size", func(o protocol.SizeOwnership) bool { return o.Owner })
	if got := h.listKeys(h.agentClient()); len(got) != 0 {
		t.Fatalf("keys with a owning the size again: %v", got)
	}
	h.eventually("the second gap to be recorded", func() bool { return len(h.events(session.EventSSHAgentUnavailable)) == 2 })
	h.endSession(a)
	// Ending the session writes every pending event: one per gap, no more.
	if evs := h.events(session.EventSSHAgentUnavailable); len(evs) != 2 {
		t.Fatalf("two gaps recorded as %v", evs)
	}
}

// A forwarding owner whose stream was cut has its id reserved for its
// reattach (size.go), and new agent connections wait for it rather than
// fall through: the reattached owner's forwarder answers them.
func TestSSHAgentWaitsForTheOwnersReattach(t *testing.T) {
	h := startAgentWorker(t, "")
	all := rawAttachCapabilities()
	a, _ := h.attachWith(protocol.AttachSession{Features: all, Geometry: protocol.Geometry{Cols: 100, Rows: 30}})
	ring, key := newKeyring(t, "a")
	f := h.forward(a.attachID, ring)

	// The connection is lost: neither stream says it is leaving.
	a.conn.Close()
	f.conn.Close()
	h.eventually("the attachment to go", func() bool { return len(h.attachments()) == 0 })

	got := make(chan []string, 1)
	failed := make(chan error, 1)
	client := h.agentClient()
	go func() {
		keys, err := listKeys(client)
		if err != nil {
			failed <- err
			return
		}
		got <- keys
	}()
	select {
	case keys := <-got:
		t.Fatalf("answered %v before the owner came back", keys)
	case err := <-failed:
		t.Fatalf("failed before the owner came back: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	a2, _ := h.attachWith(protocol.AttachSession{Features: all, Geometry: protocol.Geometry{Cols: 100, Rows: 30}, ExpectUID: h.uid, Replaces: a.attachID})
	h.forward(a2.attachID, ring)
	select {
	case keys := <-got:
		if !slices.Equal(keys, fingerprints(key)) {
			t.Fatalf("keys after the reattach: %v", keys)
		}
	case err := <-failed:
		t.Fatalf("after the reattach: %v", err)
	case <-time.After(testTimeout):
		t.Fatal("the waiting connection was not answered after the reattach")
	}
	h.endSession(a2)
}

// The wait lasts as long as the reservation: an owner that does not come
// back leaves the connection to the fallback, here no agent.
func TestSSHAgentWaitEndsWithTheReservation(t *testing.T) {
	// Set before the worker starts, which reads it.
	defer func(g time.Duration) { sizeOwnerGrace = g }(sizeOwnerGrace)
	sizeOwnerGrace = 400 * time.Millisecond
	h := startAgentWorker(t, "")
	all := rawAttachCapabilities()
	a, _ := h.attachWith(protocol.AttachSession{Features: all, Geometry: protocol.Geometry{Cols: 100, Rows: 30}})
	ring, _ := newKeyring(t, "a")
	f := h.forward(a.attachID, ring)
	a.conn.Close()
	f.conn.Close()
	h.eventually("the attachment to go", func() bool { return len(h.attachments()) == 0 })

	start := time.Now()
	if got := h.listKeys(h.agentClient()); len(got) != 0 {
		t.Fatalf("keys after the owner's reservation: %v", got)
	}
	if waited := time.Since(start); waited < 200*time.Millisecond {
		t.Fatalf("answered after %v, without waiting for the owner", waited)
	}
	b, _ := h.attachWith(protocol.AttachSession{Features: all})
	h.endSession(b)
}

// A session that requires a forwarder (a clone) never uses the agent the
// worker inherited: a program that asks before anyone forwards waits for
// the first forwarder, which answers it.
func TestSSHAgentRequiredForwarderIsAwaited(t *testing.T) {
	inherited, _ := newKeyring(t, "inherited")
	t.Setenv("SSH_AUTH_SOCK", serveKeyring(t, inherited))
	h := startWorkerEdit(t, agentScript, func(a *Args) { a.SSHAgentSocket, a.RequireForwarder = true, true })

	got := make(chan []string, 1)
	failed := make(chan error, 1)
	client := h.agentClient()
	go func() {
		keys, err := listKeys(client)
		if err != nil {
			failed <- err
			return
		}
		got <- keys
	}()
	select {
	case keys := <-got:
		t.Fatalf("answered %v before anyone forwarded", keys)
	case err := <-failed:
		t.Fatalf("failed before anyone forwarded: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	a, _ := h.attachWith(protocol.AttachSession{Features: rawAttachCapabilities()})
	ring, key := newKeyring(t, "a")
	h.forward(a.attachID, ring)
	select {
	case keys := <-got:
		if !slices.Equal(keys, fingerprints(key)) {
			t.Fatalf("keys once a forwarded: %v", keys)
		}
	case err := <-failed:
		t.Fatalf("once a forwarded: %v", err)
	case <-time.After(testTimeout):
		t.Fatal("the waiting connection was not answered once a forwarded")
	}
	h.endSession(a)
}

// The wait for a required forwarder is bounded; after it programs get no
// keys, not the inherited agent's, and the gap is recorded.
func TestSSHAgentRequiredForwarderWaitEnds(t *testing.T) {
	// Set before the worker starts, which reads it.
	defer func(w time.Duration) { requireForwarderWait = w }(requireForwarderWait)
	requireForwarderWait = 400 * time.Millisecond
	inherited, _ := newKeyring(t, "inherited")
	t.Setenv("SSH_AUTH_SOCK", serveKeyring(t, inherited))
	h := startWorkerEdit(t, agentScript, func(a *Args) { a.SSHAgentSocket, a.RequireForwarder = true, true })

	if got := h.listKeys(h.agentClient()); len(got) != 0 {
		t.Fatalf("keys without a forwarder: %v", got)
	}
	h.eventually("the gap to be recorded", func() bool { return len(h.events(session.EventSSHAgentUnavailable)) == 1 })
	a, _ := h.attachWith(protocol.AttachSession{Features: rawAttachCapabilities()})
	h.endSession(a)
}

// With nobody forwarding, the agent the worker inherited answers, byte for
// byte and unfiltered, as sessions got before; an owner that forwards takes
// precedence.
func TestSSHAgentFallsBackToTheInheritedAgent(t *testing.T) {
	inherited, inheritedKey := newKeyring(t, "inherited")
	h := startAgentWorker(t, serveKeyring(t, inherited))
	client := h.agentClient()
	if got := h.listKeys(client); !slices.Equal(got, fingerprints(inheritedKey)) {
		t.Fatalf("keys from the inherited agent: %v", got)
	}
	_, priv := newSigner(t)
	if err := client.Add(agent.AddedKey{PrivateKey: priv}); err != nil {
		t.Fatalf("adding a key to the inherited agent: %v", err)
	}
	if keys, _ := inherited.List(); len(keys) != 2 {
		t.Fatalf("inherited agent holds %d keys", len(keys))
	}

	a, _ := h.attachWith(protocol.AttachSession{Features: rawAttachCapabilities()})
	ring, key := newKeyring(t, "a")
	h.forward(a.attachID, ring)
	if got := h.listKeys(h.agentClient()); !slices.Equal(got, fingerprints(key)) {
		t.Fatalf("keys with a forwarding owner: %v", got)
	}
	h.endSession(a)
}

// A forwarded agent signs, and each signature is on record with what was
// signed, the key and the client; a login bound to a host the devbox knows
// names it. What a forwarded agent never takes is refused before it
// reaches the client, and a signature the client does not give is
// recorded as refused. When the program closes its connection the client
// is told, so it closes its agent connection for it.
func TestSSHAgentForwardedRequests(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	hostKey, _ := newSigner(t)
	os.Mkdir(filepath.Join(home, ".ssh"), 0o700)
	if err := os.WriteFile(filepath.Join(home, ".ssh", "known_hosts"), []byte(knownhosts.Line([]string{"github.com"}, hostKey)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := startAgentWorker(t, "")
	a, _ := h.attachWith(protocol.AttachSession{Features: rawAttachCapabilities()})
	ring, key := newKeyring(t, "laptop")
	f := h.forward(a.attachID, bindingAgent{ring.(agent.ExtendedAgent)})
	fp := ssh.FingerprintSHA256(key)

	conn := h.agentConn()
	client := agent.NewClient(conn)
	sig, err := client.Sign(key, []byte("data"))
	if err != nil || key.Verify([]byte("data"), sig) != nil {
		t.Fatalf("signature: %v, %v", sig, err)
	}
	h.eventLike(session.EventSSHAgentSigned, "laptop (SHA256:test) signed data using key "+fp)

	// ssh binds the connection to the server, then signs its login.
	bind := func(sessionID, signature string) {
		t.Helper()
		_, _ = client.Extension(sshagent.ExtSessionBind, slices.Concat(sshString(hostKey.Marshal()), sshString([]byte(sessionID)), sshString([]byte(signature)), []byte{0}))
	}
	login := func(sessionID, user string) {
		t.Helper()
		data := slices.Concat(sshString([]byte(sessionID)), []byte{50}, sshString([]byte(user)), sshString([]byte("ssh-connection")),
			sshString([]byte("publickey")), []byte{1}, sshString([]byte(key.Type())), sshString(key.Marshal()))
		if _, err := client.Sign(key, data); err != nil {
			t.Fatalf("login signature: %v", err)
		}
	}
	bind("session-id", "signature")
	login("session-id", "git")
	h.eventLike(session.EventSSHAgentSigned, "signed a login as git to github.com using key "+fp)
	// Only a login in the bound session names its host, and only once the
	// client's agent has accepted the binding: a program can send any.
	login("another-session", "deploy")
	h.eventLike(session.EventSSHAgentSigned, "signed a login as deploy using key "+fp)
	bind("forged-session", "rejected")
	login("forged-session", "admin")
	h.eventLike(session.EventSSHAgentSigned, "signed a login as admin using key "+fp)

	_, other := newSigner(t)
	if err := client.Add(agent.AddedKey{PrivateKey: other}); err == nil {
		t.Fatal("a key was added through the forwarded agent")
	}
	if keys, _ := ring.List(); len(keys) != 1 {
		t.Fatalf("the client's agent holds %d keys", len(keys))
	}
	h.eventLike(session.EventSSHAgentRefused, "refused a request to add a key on the agent laptop (SHA256:test) forwards")

	otherKey, _ := newSigner(t)
	if _, err := client.Sign(otherKey, []byte("data")); err == nil {
		t.Fatal("signed with a key the agent does not hold")
	}
	h.eventLike(session.EventSSHAgentRefused, "laptop (SHA256:test) refused to sign data using key "+ssh.FingerprintSHA256(otherKey))

	// Value: protects=a program closing its agent socket connection makes
	// the worker send SSHAgentClose, so the client frees its agent
	// connection for it; fails_when=forwarder.remove stops telling the
	// client, which then holds an agent connection per program connection
	// and refuses all new ones at its cap (maxLocalAgentConns); why_new=only the reply
	// timeout's SSHAgentClose was asserted, never a program closing its
	// connection; seam=none; no_claim=what agt does on SSHAgentClose (the
	// test forwarder stands in for agt, internal/cli covers the relay).
	if _, closes := f.counts(); closes != 0 {
		t.Fatalf("the client was told of %d closed connections before the program closed one", closes)
	}
	conn.Close()
	h.eventually("the client to be told the program closed its connection", func() bool { _, closes := f.counts(); return closes == 1 })
	h.endSession(a)
}

// A client that does not answer in time fails the request, is told to close
// its side, and the program's connection is closed.
func TestSSHAgentClientThatDoesNotAnswer(t *testing.T) {
	defer func(d time.Duration) { agentReplyTimeout = d }(agentReplyTimeout)
	agentReplyTimeout = 300 * time.Millisecond
	h := startAgentWorker(t, "")
	a, _ := h.attachWith(protocol.AttachSession{Features: rawAttachCapabilities()})
	ring, _ := newKeyring(t, "a")
	f := h.forward(a.attachID, ring)
	f.hold.Store(true)

	client := h.agentClient()
	start := time.Now()
	if keys, err := listKeys(client); err == nil {
		t.Fatalf("answered %v by a client that never replies", keys)
	}
	if waited := time.Since(start); waited < 250*time.Millisecond {
		t.Fatalf("gave up after %v", waited)
	}
	h.eventually("the client to be told to close", func() bool { _, closes := f.counts(); return closes == 1 })
	if _, err := listKeys(client); err == nil {
		t.Fatal("the connection outlived its client's agent connection")
	}
	h.endSession(a)
}

// A forwarder that goes away mid-request fails the request and closes the
// connections routed to it; new connections wait for the owner's next one,
// and without one go where ownership stands.
func TestSSHAgentForwarderLostMidRequest(t *testing.T) {
	defer func(d time.Duration) { sizeOwnerGrace = d }(sizeOwnerGrace)
	sizeOwnerGrace = 300 * time.Millisecond
	h := startAgentWorker(t, "")
	a, _ := h.attachWith(protocol.AttachSession{Features: rawAttachCapabilities()})
	ring, _ := newKeyring(t, "a")
	f := h.forward(a.attachID, ring)
	f.hold.Store(true)

	client := h.agentClient()
	failed := make(chan error, 1)
	go func() {
		_, err := listKeys(client)
		failed <- err
	}()
	h.eventually("the request to reach the client", func() bool { seen, _ := f.counts(); return seen == 1 })
	f.conn.Close()
	select {
	case err := <-failed:
		if err == nil {
			t.Fatal("a request whose forwarder went away succeeded")
		}
	case <-time.After(testTimeout):
		t.Fatal("a request whose forwarder went away was not answered")
	}
	if _, err := listKeys(client); err == nil {
		t.Fatal("a connection outlived its forwarder")
	}
	// The owner is still attached, without a forwarder now: once the wait
	// for its next one is over, a program gets no keys.
	if got := h.listKeys(h.agentClient()); len(got) != 0 {
		t.Fatalf("keys after the forwarder went: %v", got)
	}
	h.endSession(a)
}

// At most maxAgentConns program connections are served at once, and an
// oversized message closes its connection.
func TestSSHAgentConnectionsAreBounded(t *testing.T) {
	h := startAgentWorker(t, "")
	var conns []net.Conn
	for range maxAgentConns {
		conn := h.agentConn()
		h.listKeys(agent.NewClient(conn))
		conns = append(conns, conn)
	}
	extra := h.agentConn()
	if n, err := extra.Read(make([]byte, 1)); err == nil {
		t.Fatalf("a connection over the bound read %d bytes", n)
	}
	conns[0].Close()
	var big net.Conn
	h.eventually("a connection to be served again", func() bool {
		conn, err := net.Dial("unix", h.paths.SSHAgentSocketPath(h.uid))
		if err != nil {
			return false
		}
		conn.SetDeadline(time.Now().Add(time.Second))
		if err := sshagent.WriteMessage(conn, []byte{sshagent.MsgRequestIdentities}); err != nil {
			conn.Close()
			return false
		}
		if _, err := sshagent.ReadMessage(conn); err != nil {
			conn.Close()
			return false
		}
		big = conn
		return true
	})
	defer big.Close()

	big.SetDeadline(time.Now().Add(testTimeout))
	prefix := binary.BigEndian.AppendUint32(nil, sshagent.MaxMessageLen+1)
	if _, err := big.Write(append(prefix, bytes.Repeat([]byte{sshagent.MsgRequestIdentities}, 16)...)); err != nil {
		t.Fatal(err)
	}
	if _, err := big.Read(make([]byte, 1)); err == nil {
		t.Fatal("an oversized message was answered")
	}
	c, _ := h.attachWith(protocol.AttachSession{Features: rawAttachCapabilities()})
	h.endSession(c)
}

// A forwarder must name a live attachment of this incarnation, only one per
// attachment, and it ends with its attachment.
func TestSSHAgentForwarderRefusals(t *testing.T) {
	h := startAgentWorker(t, "")
	all := rawAttachCapabilities()
	a, _ := h.attachWith(protocol.AttachSession{Features: all})
	ring, _ := newKeyring(t, "a")
	for _, tc := range []struct {
		req  protocol.ForwardSSHAgent
		want string
	}{
		{protocol.ForwardSSHAgent{Attachment: "attach-99", ExpectUID: h.uid}, "not found"},
		{protocol.ForwardSSHAgent{Attachment: a.attachID, ExpectUID: strings.Repeat("0", 32)}, "not the session you were attached to"},
	} {
		if f, resp := h.tryForward(tc.req, ring); f != nil || resp.Error == nil || !strings.Contains(resp.Error.Message, tc.want) {
			t.Fatalf("%+v: %#v", tc.req, resp)
		}
	}
	f := h.forward(a.attachID, ring)
	if again, resp := h.tryForward(protocol.ForwardSSHAgent{Attachment: a.attachID, ExpectUID: h.uid}, ring); again != nil || resp.Error == nil || !strings.Contains(resp.Error.Message, "already forwards") {
		t.Fatalf("a second forwarder: %#v", resp)
	}
	b, _ := h.attachWith(protocol.AttachSession{Features: all})
	a.send(&protocol.Request{AttachLeave: protocol.Empty})
	a.conn.Close()
	select {
	case <-f.done:
	case <-time.After(testTimeout):
		t.Fatal("the forwarder outlived its attachment")
	}
	h.endSession(b)
}

// When the client closes a program connection (its agent has gone, or it is
// at its connection bound) the program is told at once: SSH_AGENT_FAILURE
// for a request in flight, rather than a wait of agentReplyTimeout, and an
// idle connection is closed.
func TestSSHAgentClientClosesConnections(t *testing.T) {
	h := startAgentWorker(t, "")
	a, _ := h.attachWith(protocol.AttachSession{Features: rawAttachCapabilities()})
	ring, _ := newKeyring(t, "a")
	f := h.forward(a.attachID, ring)
	list := []byte{sshagent.MsgRequestIdentities}

	idle := h.agentConn() // connection 1: answered once, then idle
	if err := sshagent.WriteMessage(idle, list); err != nil {
		t.Fatal(err)
	}
	if _, err := sshagent.ReadMessage(idle); err != nil {
		t.Fatal(err)
	}
	f.hold.Store(true)
	busy := h.agentConn() // connection 2: its request is never answered
	if err := sshagent.WriteMessage(busy, list); err != nil {
		t.Fatal(err)
	}
	h.eventually("the request to reach the client", func() bool { seen, _ := f.counts(); return seen == 2 })

	for _, id := range []uint32{2, 1} {
		if err := protocol.WriteRequest(f.conn, &protocol.Request{SSHAgentClose: &protocol.SSHAgentConn{Conn: id}}); err != nil {
			t.Fatal(err)
		}
	}
	busy.SetDeadline(time.Now().Add(2 * time.Second)) // agentReplyTimeout is 90 s
	if reply, err := sshagent.ReadMessage(busy); err != nil || sshagent.Type(reply) != sshagent.MsgFailure {
		t.Fatalf("a request whose connection the client closed: % x, %v", reply, err)
	}
	if _, err := sshagent.ReadMessage(busy); err == nil {
		t.Fatal("the connection outlived the client's close")
	}
	idle.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := sshagent.ReadMessage(idle); err == nil {
		t.Fatal("an idle connection the client closed stayed open")
	}
	h.endSession(a)
}

// Every write to a forwarder gets agentWriteTimeout of its own: a forwarder
// that keeps reading serves long after it opened, and one that stops reading
// is dropped, so the request routed to it fails instead of waiting, and the
// attachment never waits on it.
func TestSSHAgentForwarderThatStopsReading(t *testing.T) {
	defer func(d time.Duration) { agentWriteTimeout = d }(agentWriteTimeout)
	agentWriteTimeout = 200 * time.Millisecond
	h := startAgentWorker(t, "")
	a, _ := h.attachWith(protocol.AttachSession{Features: rawAttachCapabilities()})
	conn := h.dial()
	defer conn.Close()
	if err := protocol.WriteRequest(conn, &protocol.Request{ForwardSSHAgent: &protocol.ForwardSSHAgent{
		SessionID: h.sessionID, ExpectUID: h.uid, Attachment: a.attachID}}); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(conn)
	conn.SetReadDeadline(time.Now().Add(testTimeout))
	if resp, err := protocol.ReadResponse(r); err != nil || resp == nil || resp.SSHAgentForwarding == nil {
		t.Fatalf("forward: %#v, %v", resp, err)
	}

	// Long after the acknowledgement's write deadline, a request still
	// reaches the forwarder and its reply the program.
	time.Sleep(3 * agentWriteTimeout)
	prog := h.agentConn()
	if err := sshagent.WriteMessage(prog, []byte{sshagent.MsgRequestIdentities}); err != nil {
		t.Fatal(err)
	}
	resp, err := protocol.ReadResponse(r)
	if err != nil || resp == nil || resp.SSHAgentRequest == nil {
		t.Fatalf("request: %#v, %v", resp, err)
	}
	if err := protocol.WriteRequest(conn, &protocol.Request{SSHAgentReply: &protocol.SSHAgentMessage{
		Conn: resp.SSHAgentRequest.Conn, Data: sshagent.NoIdentities()}}); err != nil {
		t.Fatal(err)
	}
	if reply, err := sshagent.ReadMessage(prog); err != nil || sshagent.Type(reply) != sshagent.MsgIdentitiesAnswer {
		t.Fatalf("reply: % x, %v", reply, err)
	}

	// From here on the forwarder reads nothing. A sign request larger than
	// any socket buffer blocks the worker's write to it.
	key, _ := newSigner(t)
	sign := slices.Concat([]byte{sshagent.MsgSignRequest}, sshString(key.Marshal()),
		sshString(make([]byte, sshagent.MaxMessageLen-1024)), []byte{0, 0, 0, 0})
	start := time.Now()
	if err := sshagent.WriteMessage(prog, sign); err != nil {
		t.Fatal(err)
	}
	reply, err := sshagent.ReadMessage(prog)
	if waited := time.Since(start); waited > 5*time.Second {
		t.Fatalf("the request waited %v on a forwarder that stopped reading (%v)", waited, err)
	}
	if err == nil && sshagent.Type(reply) != sshagent.MsgFailure {
		t.Fatalf("answered message type %d", sshagent.Type(reply))
	}
	a.input("still\n")
	a.expectOutput("got:still")
	h.endSession(a)
}

// The agent the worker inherited has gone (the ssh -A login that started the
// daemon ended): with nobody forwarding, programs get no keys at once, as
// from an agent that holds none, rather than a dropped connection.
func TestSSHAgentInheritedAgentGone(t *testing.T) {
	h := startAgentWorker(t, filepath.Join(t.TempDir(), "gone.sock"))
	a, _ := h.attachWith(protocol.AttachSession{Features: rawAttachCapabilities()})
	if got := h.listKeys(h.agentClient()); len(got) != 0 {
		t.Fatalf("keys from an inherited agent that has gone: %v", got)
	}
	h.endSession(a)
}

// An SSH_AUTH_SOCK the worker inherited that is a socket in the runtime's
// ssh/ directory is another session's (the daemon was started from inside
// that session): its keys are never offered, and a session without a socket
// of its own does not get it either.
func TestSSHAgentIgnoresAnInheritedSessionSocket(t *testing.T) {
	other, _ := newKeyring(t, "another session's")
	inherit := func(a *Args) {
		path := filepath.Join(os.Getenv("AGTD_DIR"), "ssh", "0ther.sock")
		serveKeyringAt(t, other, path)
		t.Setenv("SSH_AUTH_SOCK", path)
	}
	h := startWorkerEdit(t, agentScript, func(a *Args) { inherit(a); a.SSHAgentSocket = true })
	c, _ := h.attachWith(protocol.AttachSession{Features: rawAttachCapabilities()})
	if got := h.listKeys(h.agentClient()); len(got) != 0 {
		t.Fatalf("keys from another session's socket: %v", got)
	}
	h.endSession(c)

	h = startWorkerEdit(t, agentScript, inherit)
	c, _ = h.attachWith(protocol.AttachSession{Features: rawAttachCapabilities()})
	c.input("auth\n")
	c.expectOutput("auth:\r\n")
	h.endSession(c)
}

// A forwarding owner cut, reattached, and cut again before its forwarder
// opens keeps new agent connections waiting through the second reservation
// too: the reattach carries on the forwarding owner.
func TestSSHAgentWaitThroughAChainOfCuts(t *testing.T) {
	defer func(d time.Duration) { sizeOwnerGrace = d }(sizeOwnerGrace)
	sizeOwnerGrace = 2 * time.Second
	inherited, _ := newKeyring(t, "devbox")
	h := startAgentWorker(t, serveKeyring(t, inherited))
	all := rawAttachCapabilities()
	geom := protocol.Geometry{Cols: 100, Rows: 30}
	a, _ := h.attachWith(protocol.AttachSession{Features: all, Geometry: geom})
	ring, key := newKeyring(t, "laptop")
	f := h.forward(a.attachID, ring)
	a.conn.Close()
	f.conn.Close()
	h.eventually("the attachment to go", func() bool { return len(h.attachments()) == 0 })
	cut := time.Now()
	a2, _ := h.attachWith(protocol.AttachSession{Features: all, Geometry: geom, ExpectUID: h.uid, Replaces: a.attachID})
	time.Sleep(time.Second)
	a2.conn.Close() // cut again, before its forwarder opened
	h.eventually("the reattach to go", func() bool { return len(h.attachments()) == 0 })

	// Past the first reservation, inside the second.
	time.Sleep(time.Until(cut.Add(sizeOwnerGrace + 200*time.Millisecond)))
	conn := h.agentConn()
	got := make(chan []string, 1)
	failed := make(chan error, 1)
	go func() {
		keys, err := listKeys(agent.NewClient(conn))
		if err != nil {
			failed <- err
			return
		}
		got <- keys
	}()
	select {
	case keys := <-got:
		t.Fatalf("answered %v between the reservations: the devbox's agent", keys)
	case err := <-failed:
		t.Fatalf("failed between the reservations: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	a3, _ := h.attachWith(protocol.AttachSession{Features: all, Geometry: geom, ExpectUID: h.uid, Replaces: a2.attachID})
	h.forward(a3.attachID, ring)
	select {
	case keys := <-got:
		if !slices.Equal(keys, fingerprints(key)) {
			t.Fatalf("keys once the owner was back: %v, want the laptop's", keys)
		}
	case err := <-failed:
		t.Fatalf("once the owner was back: %v", err)
	case <-time.After(testTimeout):
		t.Fatal("the waiting program was never answered")
	}
	h.endSession(a3)
}

// When the size owner's forwarder ends while the owner lives on (a write to
// it timed out), agt opens another; meanwhile a program waits for it rather
// than get the devbox's own agent.
func TestSSHAgentWaitsForTheOwnersForwarderToReopen(t *testing.T) {
	inherited, _ := newKeyring(t, "devbox")
	h := startAgentWorker(t, serveKeyring(t, inherited))
	a, _ := h.attachWith(protocol.AttachSession{Features: rawAttachCapabilities()})
	ring, key := newKeyring(t, "laptop")
	f := h.forward(a.attachID, ring)
	if got := h.listKeys(h.agentClient()); !slices.Equal(got, fingerprints(key)) {
		t.Fatalf("keys through the forwarder: %v", got)
	}
	f.conn.Close()
	<-f.done
	time.Sleep(100 * time.Millisecond) // the worker sees the stream end

	conn := h.agentConn()
	got := make(chan []string, 1)
	failed := make(chan error, 1)
	go func() {
		keys, err := listKeys(agent.NewClient(conn))
		if err != nil {
			failed <- err
			return
		}
		got <- keys
	}()
	select {
	case keys := <-got:
		t.Fatalf("a program was answered %v while the forwarder reopened: the devbox's agent", keys)
	case err := <-failed:
		t.Fatalf("listing keys while the forwarder reopened: %v", err)
	case <-time.After(500 * time.Millisecond): // it waits
	}
	h.forward(a.attachID, ring)
	select {
	case keys := <-got:
		if !slices.Equal(keys, fingerprints(key)) {
			t.Fatalf("keys once the forwarder reopened: %v, want the laptop's", keys)
		}
	case err := <-failed:
		t.Fatalf("listing keys once the forwarder reopened: %v", err)
	case <-time.After(testTimeout):
		t.Fatal("the waiting program was never answered")
	}
	h.endSession(a)
}

// An attachment whose forwarder ended while another owned the size, and
// which takes the size before agt has opened its next, is waited for as an
// owner's would be: a program does not get the devbox's agent meanwhile.
func TestSSHAgentNewOwnerWhoseForwarderReopens(t *testing.T) {
	inherited, _ := newKeyring(t, "devbox")
	h := startAgentWorker(t, serveKeyring(t, inherited))
	all := rawAttachCapabilities()
	a, _ := h.attachWith(protocol.AttachSession{Features: all, Geometry: protocol.Geometry{Cols: 100, Rows: 30}})
	b, _ := h.attachWith(protocol.AttachSession{Features: all, Geometry: protocol.Geometry{Cols: 90, Rows: 20}})
	ringA, _ := newKeyring(t, "a")
	ringB, keyB := newKeyring(t, "b")
	h.forward(a.attachID, ringA)
	fb := h.forward(b.attachID, ringB)
	fb.conn.Close()
	<-fb.done
	time.Sleep(100 * time.Millisecond) // the worker sees the stream end
	b.send(&protocol.Request{AttachTakeSize: &protocol.Geometry{Cols: 90, Rows: 20}})
	b.expectSizeNote("b to own the size", func(o protocol.SizeOwnership) bool { return o.Owner })

	conn := h.agentConn()
	got := make(chan []string, 1)
	failed := make(chan error, 1)
	go func() {
		keys, err := listKeys(agent.NewClient(conn))
		if err != nil {
			failed <- err
			return
		}
		got <- keys
	}()
	select {
	case keys := <-got:
		t.Fatalf("a program was answered %v while the new owner's forwarder reopened", keys)
	case err := <-failed:
		t.Fatalf("listing keys while the new owner's forwarder reopened: %v", err)
	case <-time.After(500 * time.Millisecond): // it waits
	}
	h.forward(b.attachID, ringB)
	select {
	case keys := <-got:
		if !slices.Equal(keys, fingerprints(keyB)) {
			t.Fatalf("keys once the forwarder reopened: %v, want b's", keys)
		}
	case err := <-failed:
		t.Fatalf("listing keys once the forwarder reopened: %v", err)
	case <-time.After(testTimeout):
		t.Fatal("the waiting program was never answered")
	}
	h.endSession(a)
}

// linkTo makes a symlink to target in a directory of its own, as a shell's
// `ln -sf "$SSH_AUTH_SOCK" ~/.ssh/ssh_auth_sock` does, and returns its path.
func linkTo(t *testing.T, target string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "aglink-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	link := filepath.Join(dir, "ssh_auth_sock")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	return link
}

// An inherited SSH_AUTH_SOCK that is a link, or a chain of links, to another
// session's socket is never relayed to, whether or not that socket is there
// yet. A session without a socket of its own keeps the link, as a shell
// would outside agtd: only a session's socket named outright is dropped.
func TestSSHAgentIgnoresALinkToASessionSocket(t *testing.T) {
	other, _ := newKeyring(t, "another session's")
	for _, tc := range []struct {
		name             string
		listening, chain bool
	}{
		{"a link", true, false},
		{"a link to a socket not there yet", false, false},
		{"a chain of links", true, true},
	} {
		var link string
		inherit := func(a *Args) {
			path := filepath.Join(os.Getenv("AGTD_DIR"), "ssh", "0ther.sock")
			if tc.listening {
				serveKeyringAt(t, other, path)
			}
			link = linkTo(t, path)
			if tc.chain {
				link = linkTo(t, link)
			}
			t.Setenv("SSH_AUTH_SOCK", link)
		}
		h := startWorkerEdit(t, agentScript, func(a *Args) { inherit(a); a.SSHAgentSocket = true })
		c, _ := h.attachWith(protocol.AttachSession{Features: rawAttachCapabilities()})
		if got := h.listKeys(h.agentClient()); len(got) != 0 {
			t.Fatalf("%s: keys through it from another session's socket: %v", tc.name, got)
		}
		h.endSession(c)

		h = startWorkerEdit(t, agentScript, inherit)
		c, _ = h.attachWith(protocol.AttachSession{Features: rawAttachCapabilities()})
		c.input("auth\n")
		c.expectOutput("auth:" + link + "\r\n")
		h.endSession(c)
	}
}

// A link that leads to another session's socket when the worker starts, and
// to a real agent later (the next login's ln -sf), is relayed to once it
// does: it is judged at each dial, not once.
func TestSSHAgentInheritedLinkRepointedAfterStart(t *testing.T) {
	inherited, key := newKeyring(t, "devbox")
	other, _ := newKeyring(t, "another session's")
	var link string
	h := startWorkerEdit(t, agentScript, func(a *Args) {
		otherPath := filepath.Join(os.Getenv("AGTD_DIR"), "ssh", "0ther.sock")
		serveKeyringAt(t, other, otherPath)
		link = linkTo(t, otherPath)
		t.Setenv("SSH_AUTH_SOCK", link)
		a.SSHAgentSocket = true
	})
	c, _ := h.attachWith(protocol.AttachSession{Features: rawAttachCapabilities()})
	if got := h.listKeys(h.agentClient()); len(got) != 0 {
		t.Fatalf("keys through a link to another session's socket: %v", got)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(serveKeyring(t, inherited), link); err != nil {
		t.Fatal(err)
	}
	if got := h.listKeys(h.agentClient()); !slices.Equal(got, fingerprints(key)) {
		t.Fatalf("keys once the link leads to the devbox's agent again: %v", got)
	}
	h.endSession(c)
}

// The link is followed again for each connection relayed to the inherited
// agent: one moved to another session's socket after the worker started is
// not followed there.
func TestSSHAgentFollowsTheInheritedLinkEachTime(t *testing.T) {
	inherited, key := newKeyring(t, "devbox")
	other, _ := newKeyring(t, "another session's")
	var link, otherPath string
	h := startWorkerEdit(t, agentScript, func(a *Args) {
		otherPath = filepath.Join(os.Getenv("AGTD_DIR"), "ssh", "0ther.sock")
		serveKeyringAt(t, other, otherPath)
		link = linkTo(t, serveKeyring(t, inherited))
		t.Setenv("SSH_AUTH_SOCK", link)
		a.SSHAgentSocket = true
	})
	c, _ := h.attachWith(protocol.AttachSession{Features: rawAttachCapabilities()})
	if got := h.listKeys(h.agentClient()); !slices.Equal(got, fingerprints(key)) {
		t.Fatalf("keys through the inherited link: %v", got)
	}
	// A shell in another session points the link at that session's socket.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(otherPath, link); err != nil {
		t.Fatal(err)
	}
	if got := h.listKeys(h.agentClient()); len(got) != 0 {
		t.Fatalf("keys once the link leads to another session's socket: %v", got)
	}
	h.endSession(c)
}

// A reattach that replaces its attachment while the old stream is still
// open (a remote client back on a new connection before the daemon has
// noticed the old one die) ends the old forwarder at once, and connections
// wait for the replacement's.
func TestSSHAgentReattachReplacingALiveOwner(t *testing.T) {
	h := startAgentWorker(t, "")
	all := rawAttachCapabilities()
	a, _ := h.attachWith(protocol.AttachSession{Features: all})
	ring, key := newKeyring(t, "a")
	f := h.forward(a.attachID, ring)
	a2, _ := h.attachWith(protocol.AttachSession{Features: all, ExpectUID: h.uid, Replaces: a.attachID})
	select {
	case <-f.done:
	case <-time.After(testTimeout):
		t.Fatal("the replaced attachment's forwarder outlived it")
	}
	got := make(chan []string, 1)
	client := h.agentClient()
	go func() { keys, _ := listKeys(client); got <- keys }()
	select {
	case keys := <-got:
		t.Fatalf("answered %v before the replacement forwarded", keys)
	case <-time.After(300 * time.Millisecond):
	}
	h.forward(a2.attachID, ring)
	select {
	case keys := <-got:
		if !slices.Equal(keys, fingerprints(key)) {
			t.Fatalf("keys = %v, want the replacement's", keys)
		}
	case <-time.After(testTimeout):
		t.Fatal("not answered once the replacement forwarded")
	}
	h.endSession(a2)
}

// While a forwarding owner's id is reserved for its reattach, connections
// wait for it even though the worker inherited an agent: they never fall
// through to that agent during the reservation.
func TestSSHAgentWaitDoesNotFallThroughToTheInheritedAgent(t *testing.T) {
	inherited, _ := newKeyring(t, "inherited")
	h := startAgentWorker(t, serveKeyring(t, inherited))
	all := rawAttachCapabilities()
	a, _ := h.attachWith(protocol.AttachSession{Features: all})
	ring, key := newKeyring(t, "a")
	f := h.forward(a.attachID, ring)
	a.conn.Close() // cut, not left: a's id is reserved
	f.conn.Close()
	h.eventually("a to go", func() bool { return len(h.attachments()) == 0 })
	got := make(chan []string, 1)
	client := h.agentClient()
	go func() { keys, _ := listKeys(client); got <- keys }()
	select {
	case keys := <-got:
		t.Fatalf("answered %v during the reservation", keys)
	case <-time.After(300 * time.Millisecond):
	}
	a2, _ := h.attachWith(protocol.AttachSession{Features: all, ExpectUID: h.uid, Replaces: a.attachID})
	h.forward(a2.attachID, ring)
	select {
	case keys := <-got:
		if !slices.Equal(keys, fingerprints(key)) {
			t.Fatalf("keys = %v, want the reattached owner's", keys)
		}
	case <-time.After(testTimeout):
		t.Fatal("not answered after the reattach")
	}
	h.endSession(a2)
}

// A connection waiting for a cut owner's reattach is routed again at once
// when another attachment takes the size, not when the reservation ends.
func TestSSHAgentWaitEndsWhenAnotherTakesTheSize(t *testing.T) {
	h := startAgentWorker(t, "")
	all := rawAttachCapabilities()
	a, _ := h.attachWith(protocol.AttachSession{Features: all, Geometry: protocol.Geometry{Cols: 100, Rows: 30}})
	b, _ := h.attachWith(protocol.AttachSession{Features: all, Geometry: protocol.Geometry{Cols: 90, Rows: 20}})
	ringA, _ := newKeyring(t, "a")
	ringB, keyB := newKeyring(t, "b")
	fa := h.forward(a.attachID, ringA)
	h.forward(b.attachID, ringB)
	a.conn.Close() // cut: a's id is reserved
	fa.conn.Close()
	h.eventually("a to go", func() bool { return len(h.attachments()) == 1 })
	got := make(chan []string, 1)
	client := h.agentClient()
	go func() { keys, _ := listKeys(client); got <- keys }()
	time.Sleep(200 * time.Millisecond) // waiting for a's reattach
	b.send(&protocol.Request{AttachTakeSize: &protocol.Geometry{Cols: 90, Rows: 20}})
	select {
	case keys := <-got:
		if !slices.Equal(keys, fingerprints(keyB)) {
			t.Fatalf("keys = %v, want b's", keys)
		}
	case <-time.After(5 * time.Second): // well inside sizeOwnerGrace
		t.Fatal("the waiting connection was not routed again when b took the size")
	}
	h.endSession(b)
}

// A gap in forwarding ends when forwarding resumes, whether or not a program
// asks for keys meanwhile: the next one is recorded again.
func TestSSHAgentGapEndsWhenForwardingResumes(t *testing.T) {
	h := startAgentWorker(t, "")
	all := rawAttachCapabilities()
	ring, _ := newKeyring(t, "a")
	leave := func(c *client) {
		c.send(&protocol.Request{AttachLeave: protocol.Empty})
		c.conn.Close()
		h.eventually("the attachment to go", func() bool { return len(h.attachments()) == 0 })
	}
	a, _ := h.attachWith(protocol.AttachSession{Features: all})
	h.forward(a.attachID, ring)
	leave(a)
	h.listKeys(h.agentClient()) // the first gap
	h.eventually("the first gap", func() bool { return len(h.events(session.EventSSHAgentUnavailable)) == 1 })
	// Forwarding resumes, and no program asks for keys while it lasts.
	b, _ := h.attachWith(protocol.AttachSession{Features: all})
	h.forward(b.attachID, ring)
	leave(b)
	h.listKeys(h.agentClient()) // a second gap
	h.eventually("the second gap", func() bool { return len(h.events(session.EventSSHAgentUnavailable)) == 2 })
	c, _ := h.attachWith(protocol.AttachSession{Features: all})
	h.endSession(c)
}

// A program that gives up while its connection waits for an owner's
// reattach has its request dropped: the reattached owner's agent is never
// asked to sign for it.
func TestSSHAgentProgramThatGivesUpWhileWaiting(t *testing.T) {
	h := startAgentWorker(t, "")
	all := rawAttachCapabilities()
	a, _ := h.attachWith(protocol.AttachSession{Features: all})
	ring, key := newKeyring(t, "a")
	f := h.forward(a.attachID, ring)
	a.conn.Close() // cut: a's id is reserved, programs wait
	f.conn.Close()
	h.eventually("a to go", func() bool { return len(h.attachments()) == 0 })
	conn := h.agentConn()
	go func() { _, _ = agent.NewClient(conn).Sign(key, []byte("data")) }() // held for a's reattach
	time.Sleep(100 * time.Millisecond)
	conn.Close() // the program gives up
	a2, _ := h.attachWith(protocol.AttachSession{Features: all, ExpectUID: h.uid, Replaces: a.attachID})
	f2 := h.forward(a2.attachID, ring)
	time.Sleep(300 * time.Millisecond) // room for the request, were it forwarded
	h.endSession(a2)
	if seen, _ := f2.counts(); seen != 0 {
		t.Fatalf("forwarded %d requests of a program that had gone", seen)
	}
	if evs := h.events(session.EventSSHAgentSigned); len(evs) != 0 {
		t.Fatalf("signed for a program that had gone: %v", evs)
	}
}

// A program repeating a refused request is recorded once, not once a
// request, so it can neither push the session's history out nor crowd out
// its attention events.
func TestSSHAgentRefusalsAreHeldBack(t *testing.T) {
	h := startAgentWorker(t, "")
	a, _ := h.attachWith(protocol.AttachSession{Features: rawAttachCapabilities()})
	ring, _ := newKeyring(t, "laptop")
	h.forward(a.attachID, ring)
	client := h.agentClient()
	_, other := newSigner(t)
	for range 200 {
		if err := client.Add(agent.AddedKey{PrivateKey: other}); err == nil {
			t.Fatal("a key was added through the forwarded agent")
		}
	}
	h.endSession(a)
	// Ending the session writes the count of those held back.
	if evs := h.events(session.EventSSHAgentRefused); len(evs) != 2 || evs[1] != evs[0]+" (and 199 more like it)" {
		t.Fatalf("200 of the same refusal recorded as %q, want it once and then counted", evs)
	}
}

// When the quiet time is up, an event counts the refusals held back, though
// no more come.
func TestSSHAgentHeldRefusalsAreCounted(t *testing.T) {
	defer func(d time.Duration) { agentEventQuiet = d }(agentEventQuiet)
	agentEventQuiet = 200 * time.Millisecond
	h := startAgentWorker(t, "")
	a, _ := h.attachWith(protocol.AttachSession{Features: rawAttachCapabilities()})
	ring, _ := newKeyring(t, "laptop")
	h.forward(a.attachID, ring)
	client := h.agentClient()
	_, other := newSigner(t)
	for range 5 {
		if err := client.Add(agent.AddedKey{PrivateKey: other}); err == nil {
			t.Fatal("a key was added through the forwarded agent")
		}
	}
	h.eventually("the held refusals to be counted", func() bool { return len(h.events(session.EventSSHAgentRefused)) == 2 })
	if evs := h.events(session.EventSSHAgentRefused); evs[1] != evs[0]+" (and 4 more like it)" {
		t.Fatalf("refusals recorded as %q", evs)
	}
	h.endSession(a)
}

// Each count comes when its quiet time is up, not at the session's end: the
// timer that writes them sets itself again for refusals held back after it
// was set, and for those counted together for want of room.
func TestSSHAgentCountsComeInTime(t *testing.T) {
	defer func(d time.Duration) { agentEventQuiet = d }(agentEventQuiet)
	agentEventQuiet = 300 * time.Millisecond
	t.Run("refusals like others", func(t *testing.T) {
		h, a, refuse := refusingWorker(t)
		refuse("first@example.com", 3)
		time.Sleep(agentEventQuiet / 2)
		refuse("second@example.com", 3)
		h.eventually("both counts", func() bool { return len(h.events(session.EventSSHAgentRefused)) == 4 })
		if evs := h.events(session.EventSSHAgentRefused); evs[2] != evs[0]+" (and 2 more like it)" || evs[3] != evs[1]+" (and 2 more like it)" {
			t.Fatalf("refusals recorded as %q", evs)
		}
		h.endSession(a)
	})
	t.Run("refusals without room", func(t *testing.T) {
		h, a, refuse := refusingWorker(t)
		for i := range maxAgentEventKeys + 3 {
			refuse(fmt.Sprintf("x%d@example.com", i), 1)
		}
		h.eventually("the count of those without room", func() bool { return len(h.events(session.EventSSHAgentRefused)) == maxAgentEventKeys+1 })
		if evs := h.events(session.EventSSHAgentRefused); evs[len(evs)-1] != "refused 3 more requests, too many different ones to list each" {
			t.Fatalf("ending %q", evs[len(evs)-1])
		}
		time.Sleep(50 * time.Millisecond) // the first ones' quiet times are over too
		refuse("late1@example.com", 1)
		refuse("late2@example.com", 1)
		h.eventually("the overflow's next count", func() bool { return len(h.events(session.EventSSHAgentRefused)) == maxAgentEventKeys+2 })
		if evs := h.events(session.EventSSHAgentRefused); evs[len(evs)-1] != "refused 2 more requests, too many different ones to list each" {
			t.Fatalf("refusals after the overflow's count recorded as %q", evs[maxAgentEventKeys:])
		}
		h.endSession(a)
	})
}

// refusingWorker is a worker with a forwarder, and a program connection
// whose refuse sends n requests the worker refuses (an unknown extension,
// name).
func refusingWorker(t *testing.T) (*harness, *client, func(name string, n int)) {
	t.Helper()
	h := startAgentWorker(t, "")
	a, _ := h.attachWith(protocol.AttachSession{Features: rawAttachCapabilities()})
	ring, _ := newKeyring(t, "laptop")
	h.forward(a.attachID, ring)
	client := h.agentClient()
	return h, a, func(name string, n int) {
		t.Helper()
		for range n {
			if _, err := client.Extension(name, nil); err == nil {
				t.Fatal("an unknown extension passed through the forwarded agent")
			}
		}
	}
}

// A refusal that keeps coming is counted less and less often: its quiet
// time doubles each time, up to agentEventQuietMax.
func TestSSHAgentQuietTimeGrows(t *testing.T) {
	defer func(d, most time.Duration) { agentEventQuiet, agentEventQuietMax = d, most }(agentEventQuiet, agentEventQuietMax)
	agentEventQuiet, agentEventQuietMax = 50*time.Millisecond, 200*time.Millisecond
	h, a, refuse := refusingWorker(t)
	for start := time.Now(); time.Since(start) < 1500*time.Millisecond; time.Sleep(5 * time.Millisecond) {
		refuse("x@example.com", 1)
	}
	counts := len(h.events(session.EventSSHAgentRefused)) - 1
	h.endSession(a)
	// Quiet times of 50, 100, then 200 ms: counts at 50, 150, 350, 550 ...
	// 1350 ms, 8 in all; 30 without the doubling, 4 without its cap.
	if counts < 6 || counts > 12 {
		t.Fatalf("a refusal repeated for 1.5 s, quiet for %v doubling to %v, was counted %d times", agentEventQuiet, agentEventQuietMax, counts)
	}
}

// A refusal whose quiet time is over is recorded again, not counted with
// the one before: it is forgotten once its time is up.
func TestSSHAgentRefusalRecordedAgainLater(t *testing.T) {
	defer func(d time.Duration) { agentEventQuiet = d }(agentEventQuiet)
	agentEventQuiet = 200 * time.Millisecond
	h := startAgentWorker(t, "")
	a, _ := h.attachWith(protocol.AttachSession{Features: rawAttachCapabilities()})
	ring, _ := newKeyring(t, "laptop")
	h.forward(a.attachID, ring)
	client := h.agentClient()
	for range 2 {
		if _, err := client.Extension("x@example.com", nil); err == nil {
			t.Fatal("an unknown extension passed through the forwarded agent")
		}
		time.Sleep(2 * agentEventQuiet)
	}
	h.endSession(a)
	if evs := h.events(session.EventSSHAgentRefused); len(evs) != 2 || evs[1] != evs[0] {
		t.Fatalf("a refusal repeated after its quiet time recorded as %q", evs)
	}
}

// Refusals that differ each time are held back too, once maxAgentEventKeys
// different ones are held, and counted together: a program cannot flood the
// session's events by varying what it asks.
func TestSSHAgentRefusalsThatDiffer(t *testing.T) {
	h := startAgentWorker(t, "")
	a, _ := h.attachWith(protocol.AttachSession{Features: rawAttachCapabilities()})
	ring, _ := newKeyring(t, "laptop")
	h.forward(a.attachID, ring)
	client := h.agentClient()
	for i := range maxAgentEventKeys + 5 {
		if _, err := client.Extension(fmt.Sprintf("x%d@example.com", i), nil); err == nil {
			t.Fatal("an unknown extension passed through the forwarded agent")
		}
	}
	h.endSession(a)
	evs := h.events(session.EventSSHAgentRefused)
	if len(evs) != maxAgentEventKeys+1 || evs[len(evs)-1] != "refused 5 more requests, too many different ones to list each" {
		t.Fatalf("%d different refusals recorded as %d events, ending %q", maxAgentEventKeys+5, len(evs), evs[len(evs)-1])
	}
}

// The most refusal events written at once, a full flush's counts with the
// first refusals that can follow it, fit the recorder's queue, which drops
// what does not fit.
func TestSSHAgentRefusalBurstFitsTheRecorder(t *testing.T) {
	if burst := 2*maxAgentEventKeys + 1; burst > maxPendingEvents {
		t.Fatalf("a burst of %d refusal events overflows the recorder's queue of %d", burst, maxPendingEvents)
	}
}

// A signature given just before the agent exits is on record though
// recording it is slow: the session's end waits for it. Here known_hosts is
// a pipe written only once the session is ending, so naming the login's host
// waits for that.
func TestSSHAgentSignatureAtTheSessionsEnd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	hostKey, _ := newSigner(t)
	os.Mkdir(filepath.Join(home, ".ssh"), 0o700)
	knownHosts := filepath.Join(home, ".ssh", "known_hosts")
	if err := syscall.Mkfifo(knownHosts, 0o600); err != nil {
		t.Fatal(err)
	}
	h := startAgentWorker(t, "")
	t.Cleanup(func() {
		// A reader left waiting by a failure is let go, so the worker ends.
		if f, err := os.OpenFile(knownHosts, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			f.Close()
		}
	})
	a, _ := h.attachWith(protocol.AttachSession{Features: rawAttachCapabilities()})
	ring, key := newKeyring(t, "laptop")
	h.forward(a.attachID, bindingAgent{ring.(agent.ExtendedAgent)})
	client := h.agentClient()
	if _, err := client.Extension(sshagent.ExtSessionBind, slices.Concat(sshString(hostKey.Marshal()), sshString([]byte("session-id")), sshString([]byte("signature")), []byte{0})); err != nil {
		t.Fatalf("session binding: %v", err)
	}
	login := slices.Concat(sshString([]byte("session-id")), []byte{50}, sshString([]byte("git")), sshString([]byte("ssh-connection")),
		sshString([]byte("publickey")), []byte{1}, sshString([]byte(key.Type())), sshString(key.Marshal()))
	if _, err := client.Sign(key, login); err != nil {
		t.Fatalf("login signature: %v", err)
	}
	written := make(chan error, 1)
	go func() {
		time.Sleep(300 * time.Millisecond) // the agent exits meanwhile
		written <- writePipe(knownHosts, knownhosts.Line([]string{"github.com"}, hostKey)+"\n")
	}()
	h.endSession(a)
	if err := <-written; err != nil {
		t.Fatalf("writing known_hosts: %v", err)
	}
	if evs := h.events(session.EventSSHAgentSigned); len(evs) != 1 || !strings.Contains(evs[0], "signed a login as git to github.com") {
		t.Fatalf("a signature given before the session's end recorded as %q", evs)
	}
}

// writePipe writes s to the named pipe at path once a reader has it open.
func writePipe(path, s string) error {
	deadline := time.Now().Add(testTimeout)
	for {
		f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			_, err = f.WriteString(s)
			f.Close()
			return err
		}
		if !errors.Is(err, syscall.ENXIO) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Every signature the client's agent gives is on record, however alike: it
// is the trail of what the session signed.
func TestSSHAgentEverySignatureIsOnRecord(t *testing.T) {
	h := startAgentWorker(t, "")
	a, _ := h.attachWith(protocol.AttachSession{Features: rawAttachCapabilities()})
	ring, key := newKeyring(t, "laptop")
	h.forward(a.attachID, ring)
	client := h.agentClient()
	for range 3 {
		if _, err := client.Sign(key, []byte("data")); err != nil {
			t.Fatal(err)
		}
	}
	h.endSession(a)
	if evs := h.events(session.EventSSHAgentSigned); len(evs) != 3 {
		t.Fatalf("3 signatures recorded as %q", evs)
	}
}

// A connection that never sends a request is closed after
// agentIdleTimeout, so it cannot hold one of the socket's places.
func TestSSHAgentConnectionThatNeverSpeaks(t *testing.T) {
	defer func(d time.Duration) { agentIdleTimeout = d }(agentIdleTimeout)
	agentIdleTimeout = 200 * time.Millisecond
	h := startAgentWorker(t, "")
	c, _ := h.attachWith(protocol.AttachSession{Features: rawAttachCapabilities()})
	conn := h.agentConn()
	if _, err := conn.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("a connection that never sent a request: %v", err)
	}
	h.endSession(c)
}

// A program that sends requests and never reads the answers loses its
// connection once an answer has waited agentWriteTimeout, rather than hold
// one of the socket's places for good, with no agent behind the socket and
// forwarded alike.
func TestSSHAgentProgramThatDoesNotRead(t *testing.T) {
	defer func(d time.Duration) { agentWriteTimeout = d }(agentWriteTimeout)
	agentWriteTimeout = 200 * time.Millisecond
	for _, forwarded := range []bool{false, true} {
		name := "no agent"
		if forwarded {
			name = "forwarded"
		}
		t.Run(name, func(t *testing.T) {
			h := startAgentWorker(t, "")
			c, _ := h.attachWith(protocol.AttachSession{Features: rawAttachCapabilities()})
			if forwarded {
				ring, _ := newKeyring(t, "laptop")
				h.forward(c.attachID, ring)
			}
			conn := h.agentConn()
			written := make(chan struct{})
			go func() {
				defer close(written)
				req := []byte{0, 0, 0, 1, sshagent.MsgRequestIdentities}
				for {
					conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
					if _, err := conn.Write(req); err != nil {
						return // the worker stopped taking them
					}
				}
			}()
			<-written
			// The worker has closed the connection: past the answers it
			// wrote, it ends (Linux resets it, since requests were left
			// unread).
			conn.SetReadDeadline(time.Now().Add(testTimeout))
			if _, err := io.Copy(io.Discard, conn); errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatal("a program that never read its answers kept its connection")
			}
			h.endSession(c)
		})
	}
}

// A reply the program is slow to take, with the client's close of its
// connection or the forwarder's end arriving while the worker writes it: the
// program gets the whole reply, then its connection closes, though nothing
// else would close it (it was busy when the close or end came).
func TestSSHAgentConnectionEndsAfterASlowReply(t *testing.T) {
	for _, end := range []string{"the client closes the connection", "the forwarder ends"} {
		t.Run(end, func(t *testing.T) {
			h := startAgentWorker(t, "")
			a, _ := h.attachWith(protocol.AttachSession{Features: rawAttachCapabilities()})
			fconn, r := h.rawForwarder(a.attachID)
			prog := h.agentConn()
			if err := sshagent.WriteMessage(prog, []byte{sshagent.MsgRequestIdentities}); err != nil {
				t.Fatal(err)
			}
			resp, err := protocol.ReadResponse(r)
			if err != nil || resp == nil || resp.SSHAgentRequest == nil {
				t.Fatalf("request: %#v, %v", resp, err)
			}
			id := resp.SSHAgentRequest.Conn
			// More than the socket's buffers hold, so the worker is still
			// writing it when the close or the end comes.
			big := make([]byte, sshagent.MaxMessageLen)
			big[0] = sshagent.MsgIdentitiesAnswer
			if err := protocol.WriteRequest(fconn, &protocol.Request{SSHAgentReply: &protocol.SSHAgentMessage{Conn: id, Data: big}}); err != nil {
				t.Fatal(err)
			}
			if end == "the forwarder ends" {
				fconn.Close()
			} else if err := protocol.WriteRequest(fconn, &protocol.Request{SSHAgentClose: &protocol.SSHAgentConn{Conn: id}}); err != nil {
				t.Fatal(err)
			}
			time.Sleep(200 * time.Millisecond) // the worker takes it meanwhile
			prog.SetReadDeadline(time.Now().Add(testTimeout))
			if got, err := sshagent.ReadMessage(prog); err != nil || len(got) != len(big) {
				t.Fatalf("the reply: %d bytes, %v", len(got), err)
			}
			prog.SetReadDeadline(time.Now().Add(2 * time.Second))
			if _, err := sshagent.ReadMessage(prog); err == nil {
				t.Fatal("a second answer to one request")
			} else if errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("the connection stayed open after %s", end)
			}
			h.endSession(a)
		})
	}
}

// While the size owner's forwarder has ended but is not yet removed, a new
// connection waits for the one agt opens in its place, rather than use the
// ended one (which would send it round again at once) or the fallback.
func TestSSHAgentRouteWaitsForAnEndedForwarder(t *testing.T) {
	conn, _ := unixPair(t)
	f := newForwarder(conn, "laptop")
	s := &ownerState{attachments: map[string]*ownerAttachment{"a": {forwarder: f, forwarded: true}}, sizeOwner: "a"}
	now := time.Now()
	if got, until, _ := s.agentRoute(now); got != f || !until.IsZero() {
		t.Fatalf("a live forwarder: routed to %p until %v, want %p", got, until, f)
	}
	f.close()
	if got, until, _ := s.agentRoute(now); got != nil || !until.Equal(now.Add(sizeOwnerGrace)) {
		t.Fatalf("an ended forwarder: routed to %p until %v, want a wait until %v", got, until, now.Add(sizeOwnerGrace))
	}
}

// A forwarder's frame over one agent message is refused at its header: the
// worker ends the stream rather than allocate the frame and wait for it.
func TestSSHAgentForwarderFrameBound(t *testing.T) {
	h := startAgentWorker(t, "")
	a, _ := h.attachWith(protocol.AttachSession{Features: rawAttachCapabilities()})
	conn := h.dial()
	defer conn.Close()
	if err := protocol.WriteRequest(conn, &protocol.Request{ForwardSSHAgent: &protocol.ForwardSSHAgent{
		SessionID: h.sessionID, ExpectUID: h.uid, Attachment: a.attachID}}); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(conn)
	conn.SetReadDeadline(time.Now().Add(testTimeout))
	if resp, err := protocol.ReadResponse(r); err != nil || resp == nil || resp.SSHAgentForwarding == nil {
		t.Fatalf("forward: %#v, %v", resp, err)
	}
	// Only the header of an SSHAgentReply (kind 81) declaring 32 MiB.
	header := make([]byte, 16)
	binary.LittleEndian.PutUint32(header[0:], 0x4147_4450)
	binary.LittleEndian.PutUint16(header[4:], protocol.ProtocolVersion)
	binary.LittleEndian.PutUint16(header[6:], 81)
	binary.LittleEndian.PutUint32(header[12:], 32<<20)
	if _, err := conn.Write(header); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := r.ReadByte(); errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("the worker waits for a 32 MiB frame instead of ending the stream at its header")
	}
	h.endSession(a)
}

// rawForwarder opens an ssh-agent stream for attachID, acknowledged, for a
// test that plays the client frame by frame.
func (h *harness) rawForwarder(attachID string) (net.Conn, *bufio.Reader) {
	h.t.Helper()
	conn := h.dial()
	h.t.Cleanup(func() { conn.Close() })
	if err := protocol.WriteRequest(conn, &protocol.Request{ForwardSSHAgent: &protocol.ForwardSSHAgent{
		SessionID: h.sessionID, ExpectUID: h.uid, Attachment: attachID}}); err != nil {
		h.t.Fatal(err)
	}
	r := bufio.NewReader(conn)
	conn.SetReadDeadline(time.Now().Add(testTimeout))
	if resp, err := protocol.ReadResponse(r); err != nil || resp == nil || resp.SSHAgentForwarding == nil {
		h.t.Fatalf("forward: %#v, %v", resp, err)
	}
	return conn, r
}

// A connection left unused after an answer is closed after agentIdleTimeout,
// with no agent behind the socket and forwarded alike, and the client is
// told it is gone.
func TestSSHAgentIdleConnectionsAreClosed(t *testing.T) {
	defer func(d time.Duration) { agentIdleTimeout = d }(agentIdleTimeout)
	agentIdleTimeout = 200 * time.Millisecond
	h := startAgentWorker(t, "")
	a, _ := h.attachWith(protocol.AttachSession{Features: rawAttachCapabilities()})
	idle := func(what string) {
		t.Helper()
		conn := h.agentConn()
		if err := sshagent.WriteMessage(conn, []byte{sshagent.MsgRequestIdentities}); err != nil {
			t.Fatal(err)
		}
		if _, err := sshagent.ReadMessage(conn); err != nil {
			t.Fatal(err)
		}
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := conn.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("%s: an idle connection stayed open: %v", what, err)
		}
	}
	idle("no agent")
	ring, _ := newKeyring(t, "laptop")
	f := h.forward(a.attachID, ring)
	idle("forwarded")
	h.eventually("the client to be told", func() bool { _, closes := f.counts(); return closes == 1 })
	h.endSession(a)
}

// A connection relayed to the inherited agent keeps no deadline once it is
// relayed: the relay is byte for byte, as the agent was reached before.
func TestSSHAgentRelayOutlivesTheIdleDeadline(t *testing.T) {
	defer func(d time.Duration) { agentIdleTimeout = d }(agentIdleTimeout)
	agentIdleTimeout = 200 * time.Millisecond
	inherited, key := newKeyring(t, "devbox")
	h := startAgentWorker(t, serveKeyring(t, inherited))
	a, _ := h.attachWith(protocol.AttachSession{Features: rawAttachCapabilities()})
	client := h.agentClient()
	h.listKeys(client)
	time.Sleep(3 * agentIdleTimeout)
	if got := h.listKeys(client); !slices.Equal(got, fingerprints(key)) {
		t.Fatalf("keys on a relay idle past the deadline: %v", got)
	}
	h.endSession(a)
}

// With an agent inherited as well, the agent's SSH_AUTH_SOCK is the
// session's own socket.
func TestSSHAgentSocketReplacesAnInheritedAgent(t *testing.T) {
	inherited, _ := newKeyring(t, "devbox")
	h := startAgentWorker(t, serveKeyring(t, inherited))
	c, _ := h.attachWith(protocol.AttachSession{Features: rawAttachCapabilities()})
	c.input("auth\n")
	c.expectOutput("auth:" + h.paths.SSHAgentSocketPath(h.uid) + "\r\n")
	h.endSession(c)
}

// A reply nobody asked for (a client answering twice) is dropped: the
// connection's next request gets its own answer, not that one.
func TestSSHAgentUnaskedReplyIsDropped(t *testing.T) {
	h := startAgentWorker(t, "")
	a, _ := h.attachWith(protocol.AttachSession{Features: rawAttachCapabilities()})
	fconn, r := h.rawForwarder(a.attachID)
	prog := h.agentConn()
	ask := func() uint32 {
		t.Helper()
		if err := sshagent.WriteMessage(prog, []byte{sshagent.MsgRequestIdentities}); err != nil {
			t.Fatal(err)
		}
		resp, err := protocol.ReadResponse(r)
		if err != nil || resp == nil || resp.SSHAgentRequest == nil {
			t.Fatalf("request: %#v, %v", resp, err)
		}
		return resp.SSHAgentRequest.Conn
	}
	reply := func(id uint32, data []byte) {
		t.Helper()
		if err := protocol.WriteRequest(fconn, &protocol.Request{SSHAgentReply: &protocol.SSHAgentMessage{Conn: id, Data: data}}); err != nil {
			t.Fatal(err)
		}
	}
	id := ask()
	reply(id, sshagent.NoIdentities())
	if _, err := sshagent.ReadMessage(prog); err != nil {
		t.Fatal(err)
	}
	reply(id, sshagent.Failure()) // nobody asked
	time.Sleep(100 * time.Millisecond)
	id = ask()
	reply(id, sshagent.NoIdentities())
	if got, err := sshagent.ReadMessage(prog); err != nil || sshagent.Type(got) != sshagent.MsgIdentitiesAnswer {
		t.Fatalf("the next request was answered % x (%v): the stale reply", got, err)
	}
	h.endSession(a)
}

// A reply and the client's close of the same connection, back to back: the
// program gets the client's reply, then its connection closes, whichever of
// the two the worker takes first.
func TestSSHAgentReplyRacingTheClientsClose(t *testing.T) {
	h := startAgentWorker(t, "")
	a, _ := h.attachWith(protocol.AttachSession{Features: rawAttachCapabilities()})
	fconn, r := h.rawForwarder(a.attachID)
	for i := range 20 {
		prog := h.agentConn()
		if err := sshagent.WriteMessage(prog, []byte{sshagent.MsgRequestIdentities}); err != nil {
			t.Fatal(err)
		}
		resp, err := protocol.ReadResponse(r)
		for err == nil && resp != nil && resp.SSHAgentRequest == nil {
			resp, err = protocol.ReadResponse(r)
		}
		if err != nil || resp == nil {
			t.Fatalf("round %d: %v", i, err)
		}
		id := resp.SSHAgentRequest.Conn
		protocol.WriteRequest(fconn, &protocol.Request{SSHAgentReply: &protocol.SSHAgentMessage{Conn: id, Data: sshagent.NoIdentities()}})
		protocol.WriteRequest(fconn, &protocol.Request{SSHAgentClose: &protocol.SSHAgentConn{Conn: id}})
		prog.SetReadDeadline(time.Now().Add(2 * time.Second))
		if got, err := sshagent.ReadMessage(prog); err != nil || sshagent.Type(got) != sshagent.MsgIdentitiesAnswer {
			t.Fatalf("round %d: answered % x (%v), want the client's reply", i, got, err)
		}
		if _, err := sshagent.ReadMessage(prog); err == nil {
			t.Fatalf("round %d: a second answer to one request", i)
		} else if errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("round %d: the connection stayed open after the client closed it", i)
		}
		prog.Close()
	}
	h.endSession(a)
}

// unixPair is a connected pair of Unix sockets.
func unixPair(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "agup-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	addr := &net.UnixAddr{Name: filepath.Join(dir, "s"), Net: "unix"}
	l, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	c, err := net.DialUnix("unix", nil, addr)
	if err != nil {
		t.Fatal(err)
	}
	s, err := l.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c.Close()
		s.Close()
	})
	return c, s
}

// An attachment's forwarder that has ended, but whose stream's handler has
// yet to remove it, gives way to the one agt opens in its place; the old
// one's removal, coming later, leaves the new one be. A live one does not
// give way. The window is too short to hit through the socket.
func TestSSHAgentEndedForwarderGivesWay(t *testing.T) {
	oldConn, _ := unixPair(t)
	old := newForwarder(oldConn, "")
	old.close()
	s := &ownerState{sessionID: "demo", sizeOwner: "attach-1",
		attachments: map[string]*ownerAttachment{"attach-1": {forwarder: old, forwarded: true}}}
	newConn, _ := unixPair(t)
	f := newForwarder(newConn, "")
	if err := s.addForwarder("attach-1", f); err != nil {
		t.Fatalf("an ended forwarder kept its place: %v", err)
	}
	s.removeForwarder("attach-1", old)
	if got := s.attachments["attach-1"].forwarder; got != f {
		t.Fatal("the old forwarder's removal took the new one")
	}
	thirdConn, _ := unixPair(t)
	if err := s.addForwarder("attach-1", newForwarder(thirdConn, "")); !errors.Is(err, errForwarderExists) {
		t.Fatalf("a forwarder took the place of a live one: %v", err)
	}
}
