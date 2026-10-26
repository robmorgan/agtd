package worker

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	return path
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
	time.Sleep(50 * time.Millisecond)
	if evs := h.events(session.EventSSHAgentUnavailable); len(evs) != 0 {
		t.Fatalf("a session never forwarded to recorded %v", evs)
	}
	h.endSession(c)
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
	time.Sleep(50 * time.Millisecond)
	if evs := h.events(session.EventSSHAgentUnavailable); len(evs) != 1 {
		t.Fatalf("one gap recorded as %v", evs)
	}

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
// recorded as refused.
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
	h.forward(a.attachID, ring)
	fp := ssh.FingerprintSHA256(key)

	client := h.agentClient()
	sig, err := client.Sign(key, []byte("data"))
	if err != nil || key.Verify([]byte("data"), sig) != nil {
		t.Fatalf("signature: %v, %v", sig, err)
	}
	h.eventLike(session.EventSSHAgentSigned, "laptop (SHA256:test) signed data using key "+fp)

	// ssh binds the connection to the server, then signs its login.
	bind := slices.Concat(sshString(hostKey.Marshal()), sshString([]byte("session-id")), sshString([]byte("signature")), []byte{0})
	_, _ = client.Extension(sshagent.ExtSessionBind, bind) // the keyring does not support it
	login := slices.Concat(sshString([]byte("session-id")), []byte{50}, sshString([]byte("git")), sshString([]byte("ssh-connection")),
		sshString([]byte("publickey")), []byte{1}, sshString([]byte(key.Type())), sshString(key.Marshal()))
	if _, err := client.Sign(key, login); err != nil {
		t.Fatalf("login signature: %v", err)
	}
	h.eventLike(session.EventSSHAgentSigned, "signed a login as git to github.com using key "+fp)

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
// connections routed to it; new connections go where ownership stands.
func TestSSHAgentForwarderLostMidRequest(t *testing.T) {
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
	// The owner is still attached, without a forwarder now.
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
