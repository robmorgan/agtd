package cli

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
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"agtd.sh/internal/paths"
	"agtd.sh/internal/protocol"
	"agtd.sh/internal/sshagent"
	"agtd.sh/internal/transport"
)

// These tests lend an agent (an in-process keyring standing in for ssh-agent
// or 1Password) through agt's relay. A fake daemon offers ssh-agent streams
// and hands each to the test, which plays the session's worker: it sends the
// requests programs in the session make, and reads what agt answers.

// workerEnd is the worker's end of an ssh-agent stream agt opened.
type workerEnd struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
	req  *protocol.ForwardSSHAgent
}

// forwardingClient is a client whose config.toml holds config, for a fake
// daemon that offers ssh-agent streams: it accepts each ForwardSSHAgent and
// passes the stream on.
func forwardingClient(t *testing.T, config string) (*client, <-chan *workerEnd) {
	t.Helper()
	return agentDaemon(t, config, nil)
}

// agentDaemon is forwardingClient with refuse, if set, answering each
// ForwardSSHAgent instead (an Error, say), or letting it through by
// returning nil.
func agentDaemon(t *testing.T, config string, refuse func(*protocol.ForwardSSHAgent) *protocol.Response) (*client, <-chan *workerEnd) {
	t.Helper()
	return agentDaemonWith(t, config, []string{protocol.CapControlStream, protocol.CapSSHAgent}, refuse)
}

// dropForward, returned by agentDaemon's refuse, closes the stream without
// an answer, as a daemon that fails for a moment does; oversizedAnswer
// answers with only the header of a frame declaring 32 MiB, and holds the
// stream until agt ends it.
var dropForward, oversizedAnswer = &protocol.Response{}, &protocol.Response{}

// fastRetries shortens the waits before agt opens a forwarder again for
// the rest of the test.
func fastRetries(t *testing.T, least, most time.Duration) {
	min, max := forwarderRetryMin, forwarderRetryMax
	t.Cleanup(func() { forwarderRetryMin, forwarderRetryMax = min, max })
	forwarderRetryMin, forwarderRetryMax = least, most
}

// agentDaemonWith is agentDaemon offering the capabilities caps.
func agentDaemonWith(t *testing.T, config string, caps []string, refuse func(*protocol.ForwardSSHAgent) *protocol.Response) (*client, <-chan *workerEnd) {
	t.Helper()
	// Socket paths are limited in length; /tmp keeps them short on macOS.
	dir, err := os.MkdirTemp("/tmp", "agcli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	p := paths.FromRoot(dir)
	if err := p.EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Config, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", p.Socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	ends := make(chan *workerEnd, 1)
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				r := bufio.NewReader(conn)
				req, err := protocol.ReadRequest(r)
				switch {
				case err != nil || req == nil:
				case req.Hello != nil:
					// A control stream, held open until agt closes it.
					protocol.WriteResponse(conn, &protocol.Response{Welcome: &protocol.Welcome{
						Version: 1, Capabilities: caps,
					}})
					io.Copy(io.Discard, r)
				case req.ForwardSSHAgent != nil:
					if refuse != nil {
						if resp := refuse(req.ForwardSSHAgent); resp != nil {
							switch resp {
							case dropForward:
							case oversizedAnswer:
								header := make([]byte, 16)
								binary.LittleEndian.PutUint32(header[0:], 0x4147_4450)
								binary.LittleEndian.PutUint16(header[4:], protocol.ProtocolVersion)
								binary.LittleEndian.PutUint16(header[6:], 180) // SSHAgentForwarding
								binary.LittleEndian.PutUint32(header[12:], 32<<20)
								conn.Write(header)
								io.Copy(io.Discard, r)
							default:
								protocol.WriteResponse(conn, resp)
							}
							break
						}
					}
					protocol.WriteResponse(conn, &protocol.Response{SSHAgentForwarding: &protocol.SSHAgentForwarding{}})
					ends <- &workerEnd{t: t, conn: conn, r: r, req: req.ForwardSSHAgent}
					return
				}
				conn.Close()
			}()
		}
	}()
	c := &client{paths: p}
	t.Cleanup(c.close)
	return c, ends
}

// forward starts c's forwarder for an attachment of session demo and
// returns the worker's end of the stream it opens.
func forward(t *testing.T, c *client, ends <-chan *workerEnd) *workerEnd {
	t.Helper()
	f := c.forwardAgent("demo", &protocol.Attached{AttachID: "attach-3", SessionUID: "0f0f"})
	if f == nil {
		t.Fatal("no forwarder with [ssh_agent] forward = true")
	}
	t.Cleanup(f.stop)
	select {
	case w := <-ends:
		t.Cleanup(func() { w.conn.Close() })
		return w
	case <-time.After(10 * time.Second):
		t.Fatal("agt opened no ssh-agent stream")
		return nil
	}
}

// send sends a frame as the worker does.
func (w *workerEnd) send(resp *protocol.Response) {
	w.t.Helper()
	w.conn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := protocol.WriteResponse(w.conn, resp); err != nil {
		w.t.Fatal(err)
	}
}

// ask sends a program's request on connection id and reads what agt sends
// back.
func (w *workerEnd) ask(id uint32, msg []byte) *protocol.Request {
	w.t.Helper()
	w.send(&protocol.Response{SSHAgentRequest: &protocol.SSHAgentMessage{Conn: id, Data: msg}})
	req, err := protocol.ReadRequest(w.r)
	if err != nil || req == nil {
		w.t.Fatalf("connection %d: no answer: %v", id, err)
	}
	return req
}

// reply is agt's reply to a request on connection id.
func (w *workerEnd) reply(id uint32, msg []byte) []byte {
	w.t.Helper()
	req := w.ask(id, msg)
	if req.SSHAgentReply == nil || req.SSHAgentReply.Conn != id {
		w.t.Fatalf("connection %d: got %#v, want a reply", id, req)
	}
	return req.SSHAgentReply.Data
}

// refused checks that agt closes connection id rather than answer a
// request on it.
func (w *workerEnd) refused(id uint32, msg []byte) {
	w.t.Helper()
	if req := w.ask(id, msg); req.SSHAgentClose == nil || req.SSHAgentClose.Conn != id {
		w.t.Fatalf("connection %d: got %#v, want it closed", id, req)
	}
}

// serveAgent serves ring on a Unix socket, as ssh-agent would. It returns
// the socket's path and a function that stops the agent, removing the
// socket.
func serveAgent(t *testing.T, ring agent.Agent) (string, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "agag-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	l, err := net.Listen("unix", filepath.Join(dir, "agent.sock"))
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
	return l.Addr().String(), func() { l.Close() }
}

// addKey adds a new key to ring and returns its public half.
func addKey(t *testing.T, ring agent.Agent, comment string) ssh.PublicKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := ring.Add(agent.AddedKey{PrivateKey: priv, Comment: comment}); err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// requestFrom is the message x/crypto's agent client sends for call, as a
// program in a session (ssh, ssh-add) sends it to the session's agent
// socket. The client is answered SSH_AGENT_FAILURE.
func requestFrom(t *testing.T, call func(agent.ExtendedAgent) error) []byte {
	t.Helper()
	var sent bytes.Buffer
	_ = call(agent.NewClient(struct {
		io.Reader
		io.Writer
	}{bytes.NewReader([]byte{0, 0, 0, 1, sshagent.MsgFailure}), &sent}))
	msg, err := sshagent.ReadMessage(&sent)
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

// agt's policy holds whatever the worker sends it: only listing keys and
// signing reach this machine's agent, only the keys [ssh_agent] keys lists
// are offered, and only they sign. Requests that would change the agent are
// refused without reaching it.
//
// Value: protects=agt passes only listing and signing to this machine's
// agent, offers only [ssh_agent] keys and signs only with them, whatever the
// worker sends; fails_when=the relay loses its Forwardable check or its keys
// filter on identities answers or sign requests (the worker's own filter
// hides this from every other test); why_new=internal/cli/sshagent.go had no
// tests, the e2e tests never set keys, and the worker refuses other requests
// before agt sees them; seam=none; no_claim=worker routing or the daemon
// proxy (a fake daemon plays the worker, x/crypto's keyring the agent).
func TestSSHAgentRelayPolicy(t *testing.T) {
	ring := agent.NewKeyring()
	unlisted, listed := addKey(t, ring, "unlisted"), addKey(t, ring, "listed")
	path, _ := serveAgent(t, ring)
	c, ends := forwardingClient(t, "[ssh_agent]\nforward = true\nagent = \""+path+"\"\nkeys = [\""+ssh.FingerprintSHA256(listed)+"\"]\n")
	w := forward(t, c, ends)
	if r := w.req; r.SessionID != "demo" || r.ExpectUID != "0f0f" || r.Attachment != "attach-3" {
		t.Fatalf("forwarder opened for %+v", r)
	}

	ids, err := sshagent.ParseIdentities(w.reply(1, []byte{sshagent.MsgRequestIdentities}))
	if err != nil || len(ids) != 1 || sshagent.Fingerprint(ids[0].Blob) != ssh.FingerprintSHA256(listed) {
		t.Fatalf("keys offered: %+v, %v", ids, err)
	}
	sign := func(key ssh.PublicKey) []byte {
		return requestFrom(t, func(a agent.ExtendedAgent) error { _, err := a.Sign(key, []byte("data")); return err })
	}
	if got := w.reply(1, sign(listed)); sshagent.Type(got) != sshagent.MsgSignResponse {
		t.Fatalf("signing with a listed key: % x", got)
	}
	// The agent holds the unlisted key, and would sign with it.
	if got := w.reply(1, sign(unlisted)); sshagent.Type(got) != sshagent.MsgFailure {
		t.Fatalf("signed with a key [ssh_agent] keys leaves out: % x", got)
	}

	_, other, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func(agent.ExtendedAgent) error{
		"add a key":        func(a agent.ExtendedAgent) error { return a.Add(agent.AddedKey{PrivateKey: other}) },
		"remove every key": func(a agent.ExtendedAgent) error { return a.RemoveAll() },
		"lock the agent":   func(a agent.ExtendedAgent) error { return a.Lock([]byte("pw")) },
	} {
		if got := w.reply(1, requestFrom(t, call)); sshagent.Type(got) != sshagent.MsgFailure {
			t.Fatalf("%s: answered % x", name, got)
		}
	}
	// Had any of them reached the agent, it would list three keys, or none.
	if keys, err := ring.List(); err != nil || len(keys) != 2 {
		t.Fatalf("the agent lists %d keys (%v) after requests agt refuses", len(keys), err)
	}
}

// agt holds at most maxLocalAgentConns connections to this machine's agent,
// one for each program connection the worker numbers: one more is closed at
// once (its program carries on without an agent), and one the worker closes
// frees its place once its goroutine has ended. Once the agent has gone, a
// new connection is closed rather than left waiting, and those already open
// carry on.
//
// Value: protects=agt holds at most maxLocalAgentConns local agent
// connections per forwarder, closes one more at once, frees one the worker
// closes, and closes a connection whose agent has gone; fails_when=the cap is
// lost or off by one, drop stops freeing a place (forwarding fails after
// maxLocalAgentConns program connections), or a dead local agent leaves a
// request unanswered;
// why_new=no test opens more than a few program connections per forwarder,
// and none takes the client's agent away; seam=none; no_claim=the worker's
// side of SSHAgentClose, covered in internal/worker (a fake daemon plays the
// worker, x/crypto's keyring the agent).
func TestSSHAgentRelayBoundsLocalConnections(t *testing.T) {
	ring := agent.NewKeyring()
	addKey(t, ring, "laptop")
	path, stopAgent := serveAgent(t, ring)
	c, ends := forwardingClient(t, "[ssh_agent]\nforward = true\nagent = \""+path+"\"\n")
	w := forward(t, c, ends)
	list := []byte{sshagent.MsgRequestIdentities}

	for id := uint32(1); id <= maxLocalAgentConns; id++ {
		w.reply(id, list)
	}
	w.refused(maxLocalAgentConns+1, list)
	// A program closed connection 1: its place is free once its goroutine
	// has ended, a moment later.
	w.send(&protocol.Response{SSHAgentClose: &protocol.SSHAgentConn{Conn: 1}})
	next := w.replyOnceFree(maxLocalAgentConns+2, list)

	// With a place free again, the agent goes away.
	w.send(&protocol.Response{SSHAgentClose: &protocol.SSHAgentConn{Conn: 2}})
	stopAgent()
	w.refused(next+1, list)
	w.reply(3, list)
}

// replyOnceFree asks on new connections, from id up, until agt answers one
// rather than close it, and returns the one it answered.
func (w *workerEnd) replyOnceFree(id uint32, msg []byte) uint32 {
	w.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for ; ; id++ {
		req := w.ask(id, msg)
		if req.SSHAgentReply != nil && req.SSHAgentReply.Conn == id {
			return id
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("connection %d: got %#v, and no place came free", id, req)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// blockingAgent signs only once released, as an agent waiting for a touch
// or a confirmation does.
type blockingAgent struct {
	agent.Agent
	release chan struct{}
}

func (b *blockingAgent) Sign(key ssh.PublicKey, data []byte) (*ssh.Signature, error) {
	<-b.release
	return b.Agent.Sign(key, data)
}

// One program's request waiting on this machine's agent holds up only its
// own connection: another program's request is answered meanwhile.
func TestSSHAgentRelayAnswersConnectionsIndependently(t *testing.T) {
	ring := agent.NewKeyring()
	key := addKey(t, ring, "laptop")
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	path, _ := serveAgent(t, &blockingAgent{Agent: ring, release: release})
	c, ends := forwardingClient(t, "[ssh_agent]\nforward = true\nagent = \""+path+"\"\n")
	w := forward(t, c, ends)

	sign := requestFrom(t, func(a agent.ExtendedAgent) error { _, err := a.Sign(key, []byte("data")); return err })
	w.send(&protocol.Response{SSHAgentRequest: &protocol.SSHAgentMessage{Conn: 1, Data: sign}})
	if got := w.reply(2, []byte{sshagent.MsgRequestIdentities}); sshagent.Type(got) != sshagent.MsgIdentitiesAnswer {
		t.Fatalf("connection 2 answered % x", got)
	}
	once.Do(func() { close(release) })
	req, err := protocol.ReadRequest(w.r)
	if err != nil || req == nil || req.SSHAgentReply == nil || req.SSHAgentReply.Conn != 1 || sshagent.Type(req.SSHAgentReply.Data) != sshagent.MsgSignResponse {
		t.Fatalf("connection 1: %#v, %v", req, err)
	}
}

// openSSHAgent is OpenSSH's own agent, which applies destination limits,
// holding a key limited with `ssh-add -h` to dest.example and a free one.
type openSSHAgent struct {
	sock          string
	limited, free ssh.PublicKey
	destination   ssh.Signer // dest.example's host key
}

// startOpenSSHAgent starts one, skipping the test when OpenSSH's tools are
// missing or too old to limit a key to a host.
func startOpenSSHAgent(t *testing.T) *openSSHAgent {
	t.Helper()
	for _, tool := range []string{"ssh-agent", "ssh-add", "ssh-keygen"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	dir, err := os.MkdirTemp("/tmp", "aghop-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "agent.sock")
	agentCmd := exec.Command("ssh-agent", "-D", "-a", sock)
	if err := agentCmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		agentCmd.Process.Kill()
		agentCmd.Wait()
	})
	for deadline := time.Now().Add(10 * time.Second); ; {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ssh-agent never listened")
		}
		time.Sleep(20 * time.Millisecond)
	}
	keygen := func(name string) (path string, key ssh.PublicKey) {
		path = filepath.Join(dir, name)
		if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", name, "-f", path).CombinedOutput(); err != nil {
			t.Fatalf("ssh-keygen: %v\n%s", err, out)
		}
		pub, err := os.ReadFile(path + ".pub")
		if err != nil {
			t.Fatal(err)
		}
		if key, _, _, _, err = ssh.ParseAuthorizedKey(pub); err != nil {
			t.Fatal(err)
		}
		return path, key
	}
	limitedPath, limited := keygen("limited")
	freePath, free := keygen("free")
	hostPath, hostKey := keygen("host")
	hostPEM, err := os.ReadFile(hostPath)
	if err != nil {
		t.Fatal(err)
	}
	destination, err := ssh.ParsePrivateKey(hostPEM)
	if err != nil {
		t.Fatal(err)
	}
	known := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(known, []byte("dest.example "+string(ssh.MarshalAuthorizedKey(hostKey))), 0o600); err != nil {
		t.Fatal(err)
	}
	add := func(args ...string) error {
		cmd := exec.Command("ssh-add", args...)
		cmd.Env = append(os.Environ(), "SSH_AUTH_SOCK="+sock)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("ssh-add %v: %v\n%s", args, err, out)
		}
		return nil
	}
	if err := add("-H", known, "-h", "dest.example", limitedPath); err != nil {
		t.Skipf("this ssh-add cannot limit a key to a host: %v", err)
	}
	if err := add(freePath); err != nil {
		t.Fatal(err)
	}
	return &openSSHAgent{sock: sock, limited: limited, free: free, destination: destination}
}

// For a session on another host, each connection to this machine's agent is
// bound to the agtd hop first, as ssh -A binds what it forwards, so a key
// limited with `ssh-add -h` to other hosts is hidden and refused there, as
// it would be through ssh -A; a session on this machine is local use, as
// before. This needs OpenSSH's own agent, which applies the limits.
func TestSSHAgentRelayBindsTheHop(t *testing.T) {
	a := startOpenSSHAgent(t)
	sock, limited, free := a.sock, a.limited, a.free

	ask := func(hop ssh.Signer, msg []byte) []byte {
		t.Helper()
		r := newAgentRelay(t.Context(), sock, sshagent.NewKeys(nil), hop)
		defer r.cancel()
		lc := &localAgentConn{id: 1, reqs: make(chan []byte, 1), done: make(chan struct{})}
		defer func() {
			if lc.agent != nil {
				lc.agent.Close()
			}
		}()
		reply, err := r.answer(lc, msg)
		if err != nil {
			t.Fatal(err)
		}
		return reply
	}
	offered := func(hop ssh.Signer) []string {
		t.Helper()
		ids, err := sshagent.ParseIdentities(ask(hop, []byte{sshagent.MsgRequestIdentities}))
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, id := range ids {
			names = append(names, id.Comment)
		}
		slices.Sort(names)
		return names
	}
	sign := func(hop ssh.Signer, key ssh.PublicKey) byte {
		t.Helper()
		return sshagent.Type(ask(hop, requestFrom(t, func(a agent.ExtendedAgent) error { _, err := a.Sign(key, []byte("data")); return err })))
	}

	// On this machine: local use, as ssh-agent sees a connection with no
	// binding.
	if got := offered(nil); !slices.Equal(got, []string{"free", "limited"}) {
		t.Fatalf("keys offered locally: %v", got)
	}
	hop, err := newHopSigner()
	if err != nil {
		t.Fatal(err)
	}
	if got := offered(hop); !slices.Equal(got, []string{"free"}) {
		t.Fatalf("keys offered through the hop: %v", got)
	}
	if got := sign(hop, limited); got != sshagent.MsgFailure {
		t.Fatalf("signing with the limited key through the hop: type %d", got)
	}
	if got := sign(hop, free); got != sshagent.MsgSignResponse {
		t.Fatalf("signing with the free key through the hop: type %d", got)
	}
}

// lyingSigner signs something other than what it is given, so OpenSSH's
// agent rejects the bindings it makes.
type lyingSigner struct{ ssh.Signer }

func (s lyingSigner) Sign(rand io.Reader, data []byte) (*ssh.Signature, error) {
	return s.Signer.Sign(rand, append([]byte("not "), data...))
}

// Until OpenSSH's agent has taken the agtd hop's binding on a connection,
// agt refuses the program's own bindings on it: one would become the first
// hop, as if this machine logged in to dest.example itself, and let the
// devbox use the key limited to it. This holds when the agent rejects the
// hop's binding and when it was locked as the connection opened; once
// unlocked, the hop is bound before the program's binding passes.
func TestSSHAgentRelayKeepsProgramBindingsBehindTheHop(t *testing.T) {
	a := startOpenSSHAgent(t)
	hop, err := newHopSigner()
	if err != nil {
		t.Fatal(err)
	}
	// What ssh on the devbox sends to log in to dest.example with the
	// limited key: its binding of the session, then the signature.
	sessionID := make([]byte, 32)
	if _, err := rand.Read(sessionID); err != nil {
		t.Fatal(err)
	}
	sig, err := a.destination.Sign(rand.Reader, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	programBind := (&sshagent.SessionBind{HostKey: a.destination.PublicKey().Marshal(), SessionID: sessionID, Signature: ssh.Marshal(sig)}).Request()
	str := func(b []byte) []byte { return append(binary.BigEndian.AppendUint32(nil, uint32(len(b))), b...) }
	login := slices.Concat(str(sessionID), []byte{50}, str([]byte("git")), str([]byte("ssh-connection")),
		str([]byte("publickey")), []byte{1}, str([]byte(a.limited.Type())), str(a.limited.Marshal()))
	signLogin := requestFrom(t, func(ag agent.ExtendedAgent) error { _, err := ag.Sign(a.limited, login); return err })

	conn := func(hop ssh.Signer) (*agentRelay, *localAgentConn) {
		r := newAgentRelay(t.Context(), a.sock, sshagent.NewKeys(nil), hop)
		lc := &localAgentConn{id: 1, reqs: make(chan []byte, 1), done: make(chan struct{})}
		t.Cleanup(func() {
			r.cancel()
			if lc.agent != nil {
				lc.agent.Close()
			}
		})
		return r, lc
	}
	answer := func(r *agentRelay, lc *localAgentConn, msg []byte) byte {
		t.Helper()
		reply, err := r.answer(lc, msg)
		if err != nil {
			t.Fatal(err)
		}
		return sshagent.Type(reply)
	}

	// The agent rejects the hop's binding.
	r, lc := conn(lyingSigner{hop})
	if got := answer(r, lc, programBind); got != sshagent.MsgFailure {
		t.Fatalf("the program's binding passed after the agent rejected the hop's: type %d", got)
	}
	if got := answer(r, lc, signLogin); got != sshagent.MsgFailure {
		t.Fatalf("the limited key signed a login to dest.example after a rejected hop binding: type %d", got)
	}

	// The agent is locked as the connection opens, and unlocked after.
	control, err := net.Dial("unix", a.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	owner := agent.NewClient(control)
	if err := owner.Lock([]byte("passphrase")); err != nil {
		t.Fatal(err)
	}
	r, lc = conn(hop)
	if got := answer(r, lc, []byte{sshagent.MsgRequestIdentities}); got != sshagent.MsgIdentitiesAnswer {
		t.Fatalf("a locked agent's identities: type %d", got)
	}
	if err := owner.Unlock([]byte("passphrase")); err != nil {
		t.Fatal(err)
	}
	if got := answer(r, lc, programBind); got != sshagent.MsgSuccess {
		t.Fatalf("the program's binding, after the hop's once unlocked: type %d", got)
	}
	if got := answer(r, lc, signLogin); got != sshagent.MsgFailure {
		t.Fatalf("the limited key signed a login to dest.example through the hop: type %d", got)
	}
}

// unlockingAgent relays connections to OpenSSH's agent at sock, and unlocks
// it with passphrase once it has refused a session binding, before passing
// that answer on: the unlock lands after the hop's binding was tried and
// before the request that follows it.
func unlockingAgent(t *testing.T, sock string, passphrase []byte) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "agunl-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	l, err := net.Listen("unix", filepath.Join(dir, "agent.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	var unlock sync.Once
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				up, err := net.Dial("unix", sock)
				if err != nil {
					return
				}
				defer up.Close()
				for {
					msg, err := sshagent.ReadMessage(conn)
					if err != nil || sshagent.WriteMessage(up, msg) != nil {
						return
					}
					reply, err := sshagent.ReadMessage(up)
					if err != nil {
						return
					}
					if name, ok := sshagent.ExtensionName(msg); ok && name == sshagent.ExtSessionBind && sshagent.Type(reply) != sshagent.MsgSuccess {
						unlock.Do(func() {
							if c, err := net.Dial("unix", sock); err == nil {
								agent.NewClient(c).Unlock(passphrase)
								c.Close()
							}
						})
					}
					if sshagent.WriteMessage(conn, reply) != nil {
						return
					}
				}
			}()
		}
	}()
	return l.Addr().String()
}

// An agent unlocked between the hop's binding and the request after it
// would list, as for local use, a key limited with `ssh-add -h` to another
// host: agt binds again and asks again, so the devbox never sees that key,
// and the check before a forwarder opens does not count it.
func TestSSHAgentUnlockAfterTheHopsBinding(t *testing.T) {
	passphrase := []byte("passphrase")
	// locked is a locked OpenSSH agent behind unlockingAgent, holding only
	// the key limited to dest.example when onlyLimited is set.
	locked := func(t *testing.T, onlyLimited bool) string {
		a := startOpenSSHAgent(t)
		control, err := net.Dial("unix", a.sock)
		if err != nil {
			t.Fatal(err)
		}
		defer control.Close()
		owner := agent.NewClient(control)
		if onlyLimited {
			if err := owner.Remove(a.free); err != nil {
				t.Fatal(err)
			}
		}
		if err := owner.Lock(passphrase); err != nil {
			t.Fatal(err)
		}
		return unlockingAgent(t, a.sock, passphrase)
	}
	hop, err := newHopSigner()
	if err != nil {
		t.Fatal(err)
	}
	t.Run("a program's listing", func(t *testing.T) {
		r := newAgentRelay(t.Context(), locked(t, false), sshagent.NewKeys(nil), hop)
		lc := &localAgentConn{id: 1, reqs: make(chan []byte, 1), done: make(chan struct{})}
		t.Cleanup(func() {
			r.cancel()
			if lc.agent != nil {
				lc.agent.Close()
			}
		})
		reply, err := r.answer(lc, []byte{sshagent.MsgRequestIdentities})
		if err != nil {
			t.Fatal(err)
		}
		ids, err := sshagent.ParseIdentities(reply)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, id := range ids {
			names = append(names, id.Comment)
		}
		if !slices.Equal(names, []string{"free"}) {
			t.Fatalf("keys listed through the hop as the agent was unlocked: %v, want only the free one", names)
		}
	})
	t.Run("the check before a forwarder opens", func(t *testing.T) {
		r := newAgentRelay(t.Context(), locked(t, true), sshagent.NewKeys(nil), hop)
		defer r.cancel()
		if r.lends() {
			t.Fatal("lent an agent whose only key is limited to another host, as it was unlocked")
		}
	})
}

// An agent that does not know session-bind@openssh.com (x/crypto's keyring,
// an older agent) answers the hop's binding with a failure, and is lent as
// before: it has no destination limits to apply.
func TestSSHAgentHopWithAnAgentThatDoesNotBind(t *testing.T) {
	ring := agent.NewKeyring()
	addKey(t, ring, "laptop")
	path, _ := serveAgent(t, ring)
	hop, err := newHopSigner()
	if err != nil {
		t.Fatal(err)
	}
	r := newAgentRelay(t.Context(), path, sshagent.NewKeys(nil), hop)
	defer r.cancel()
	if !r.lends() {
		t.Fatal("an agent without session-bind is not lent through the hop")
	}
}

// slowBindAgent takes its time over its first session binding, as OpenSSH's
// agent does while it asks the user to confirm another request: it answers
// nothing else meanwhile, and at once after.
type slowBindAgent struct {
	agent.ExtendedAgent
	delay time.Duration
	busy  sync.Once
}

func (s *slowBindAgent) Extension(name string, contents []byte) ([]byte, error) {
	if name == sshagent.ExtSessionBind {
		s.busy.Do(func() { time.Sleep(s.delay) })
	}
	return s.ExtendedAgent.Extension(name, contents)
}

// The hop's binding waits for a busy agent as a request would, rather than
// failing the program's connection after the dial's 2 s.
func TestSSHAgentHopWaitsForABusyAgent(t *testing.T) {
	ring := agent.NewKeyring()
	addKey(t, ring, "laptop")
	path, _ := serveAgent(t, &slowBindAgent{ExtendedAgent: ring.(agent.ExtendedAgent), delay: localAgentDialTimeout + 500*time.Millisecond})
	hop, err := newHopSigner()
	if err != nil {
		t.Fatal(err)
	}
	r := newAgentRelay(t.Context(), path, sshagent.NewKeys(nil), hop)
	defer r.cancel()
	if !r.lends() {
		t.Fatal("a busy agent is not lent: the hop's binding gave up on it")
	}
}

// cliUnixPair is a connected pair of Unix sockets, the worker's end first.
func cliUnixPair(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "agst-")
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

// A program connection's hop binding waiting on a busy agent ends when the
// worker closes the connection, freeing its place at once, and when the
// stream ends, so closeAll, and the reopen after it, never wait on it.
func TestSSHAgentRelayEndsAWaitingBinding(t *testing.T) {
	for _, how := range []string{"the worker closes the connection", "the stream ends"} {
		t.Run(how, func(t *testing.T) {
			ring := agent.NewKeyring()
			addKey(t, ring, "laptop")
			path, _ := serveAgent(t, &slowBindAgent{ExtendedAgent: ring.(agent.ExtendedAgent), delay: 20 * time.Second})
			hop, err := newHopSigner()
			if err != nil {
				t.Fatal(err)
			}
			r := newAgentRelay(t.Context(), path, sshagent.NewKeys(nil), hop)
			defer r.cancel()
			worker, relayEnd := cliUnixPair(t)
			r.s = relayEnd
			served := make(chan struct{})
			go func() {
				defer close(served)
				r.serve(bufio.NewReader(relayEnd))
			}()
			if err := protocol.WriteResponse(worker, &protocol.Response{SSHAgentRequest: &protocol.SSHAgentMessage{Conn: 1, Data: []byte{sshagent.MsgRequestIdentities}}}); err != nil {
				t.Fatal(err)
			}
			time.Sleep(300 * time.Millisecond) // the binding waits on the agent
			if how == "the stream ends" {
				worker.Close()
				<-served
				done := make(chan struct{})
				go func() {
					r.closeAll()
					close(done)
				}()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Fatal("closeAll waited on a binding the agent had yet to answer")
				}
				return
			}
			if err := protocol.WriteResponse(worker, &protocol.Response{SSHAgentClose: &protocol.SSHAgentConn{Conn: 1}}); err != nil {
				t.Fatal(err)
			}
			for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(20 * time.Millisecond) {
				r.mu.Lock()
				n := len(r.conns)
				r.mu.Unlock()
				if n == 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("a connection closed during its binding kept its place")
				}
			}
			worker.Close()
			<-served
			r.closeAll()
		})
	}
}

// bindCounter counts the session bindings an agent is asked for.
type bindCounter struct {
	agent.ExtendedAgent
	binds atomic.Int32
}

func (b *bindCounter) Extension(name string, contents []byte) ([]byte, error) {
	if name == sshagent.ExtSessionBind {
		b.binds.Add(1)
	}
	return b.ExtendedAgent.Extension(name, contents)
}

// A connection's first request binds it to the hop once, and signing and
// listing work through an agent that never takes the binding (gpg-agent, an
// OpenSSH before 8.9): only the program's own bindings wait for the hop's.
func TestSSHAgentHopWithAnAgentThatNeverBinds(t *testing.T) {
	ring := agent.NewKeyring()
	key := addKey(t, ring, "laptop")
	b := &bindCounter{ExtendedAgent: ring.(agent.ExtendedAgent)}
	path, _ := serveAgent(t, b)
	hop, err := newHopSigner()
	if err != nil {
		t.Fatal(err)
	}
	r := newAgentRelay(t.Context(), path, sshagent.NewKeys(nil), hop)
	defer r.cancel()
	lc := &localAgentConn{id: 1, reqs: make(chan []byte, 1), done: make(chan struct{})}
	defer func() {
		if lc.agent != nil {
			lc.agent.Close()
		}
	}()
	sign := requestFrom(t, func(a agent.ExtendedAgent) error { _, err := a.Sign(key, []byte("data")); return err })
	reply, err := r.answer(lc, sign)
	if err != nil || sshagent.Type(reply) != sshagent.MsgSignResponse {
		t.Fatalf("signing through the hop with an agent that does not bind: % x, %v", reply, err)
	}
	if n := b.binds.Load(); n != 1 {
		t.Fatalf("the first request tried the hop's binding %d times, want 1", n)
	}
	if reply, err := r.answer(lc, sign); err != nil || sshagent.Type(reply) != sshagent.MsgSignResponse {
		t.Fatalf("signing again: % x, %v", reply, err)
	}
	if reply, err := r.answer(lc, []byte{sshagent.MsgRequestIdentities}); err != nil || sshagent.Type(reply) != sshagent.MsgIdentitiesAnswer {
		t.Fatalf("listing through the hop: % x, %v", reply, err)
	}
	if lc.bound {
		t.Fatal("an agent without session-bind took the hop's binding")
	}
}

// Stopping a forwarder ends a question to the local agent in progress: a
// detach, a switch or a reattach never waits on an agent that does not
// answer.
func TestSSHAgentForwarderStopsWhileTheAgentIsSilent(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "aghang-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	l, err := net.Listen("unix", filepath.Join(dir, "agent.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	conns := make(chan net.Conn, 8)
	go func() {
		for {
			conn, err := l.Accept() // an agent that accepts and never answers
			if err != nil {
				return
			}
			conns <- conn
		}
	}()
	t.Cleanup(func() {
		for {
			select {
			case conn := <-conns:
				conn.Close()
			default:
				return
			}
		}
	})
	for _, remote := range []bool{false, true} {
		c, _ := forwardingClient(t, "[ssh_agent]\nforward = true\nagent = \""+l.Addr().String()+"\"\n")
		if remote {
			// A session on another host: the hop's binding waits on the agent
			// first.
			c.host = &transport.Host{Name: "devbox"}
		}
		f := c.forwardAgent("demo", &protocol.Attached{AttachID: "attach-3", SessionUID: "0f0f"})
		if f == nil {
			t.Fatal("no forwarder with [ssh_agent] forward = true")
		}
		select {
		case conn := <-conns: // the probe now waits on the agent
			t.Cleanup(func() { conn.Close() })
		case <-time.After(10 * time.Second):
			t.Fatal("agt did not ask its agent")
		}
		start := time.Now()
		f.stop()
		if took := time.Since(start); took > time.Second {
			t.Fatalf("remote %v: stop waited %v for the agent", remote, took)
		}
	}
}

// agt opens a forwarder only for an agent that answers and holds a key it
// may offer, since one that can answer nothing would hide the devbox's own
// agent; once the agent has a key, the forwarder opens.
func TestSSHAgentForwardsOnlyALendingAgent(t *testing.T) {
	ring := agent.NewKeyring() // no keys yet
	path, _ := serveAgent(t, ring)
	c, ends := forwardingClient(t, "[ssh_agent]\nforward = true\nagent = \""+path+"\"\n")
	f := c.forwardAgent("demo", &protocol.Attached{AttachID: "attach-3", SessionUID: "0f0f"})
	if f == nil {
		t.Fatal("no forwarder with [ssh_agent] forward = true")
	}
	t.Cleanup(f.stop)
	select {
	case w := <-ends:
		w.conn.Close()
		t.Fatal("forwarded an agent that holds no key")
	case <-time.After(1500 * time.Millisecond):
	}
	addKey(t, ring, "laptop")
	select {
	case w := <-ends:
		w.conn.Close()
	case <-time.After(10 * time.Second):
		t.Fatal("no forwarder once the agent held a key")
	}
}

// A forwarder that ends while its attachment lives (the worker dropped it
// after a write timed out, say) is opened again.
func TestSSHAgentForwarderReopensWhileAttached(t *testing.T) {
	ring := agent.NewKeyring()
	addKey(t, ring, "laptop")
	path, _ := serveAgent(t, ring)
	c, ends := forwardingClient(t, "[ssh_agent]\nforward = true\nagent = \""+path+"\"\n")
	w := forward(t, c, ends)
	w.conn.Close() // the stream ends; the attachment lives on
	select {
	case w2 := <-ends:
		t.Cleanup(func() { w2.conn.Close() })
		if got := w2.reply(1, []byte{sshagent.MsgRequestIdentities}); sshagent.Type(got) != sshagent.MsgIdentitiesAnswer {
			t.Fatalf("the reopened forwarder answered % x", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("agt did not open its forwarder again while the attachment lives")
	}
}

// A forwarder the daemon or the worker keeps refusing (the session has no
// agent socket, or an older agtd started it) ends quietly after
// maxForwarderRefusals tries, and is not asked for again: the refusal
// stands for the attachment's life.
func TestSSHAgentRefusedForwarderEndsQuietly(t *testing.T) {
	fastRetries(t, 10*time.Millisecond, 50*time.Millisecond)
	ring := agent.NewKeyring()
	addKey(t, ring, "laptop")
	path, _ := serveAgent(t, ring)
	var asked atomic.Int32
	c, _ := agentDaemon(t, "[ssh_agent]\nforward = true\nagent = \""+path+"\"\n", func(*protocol.ForwardSSHAgent) *protocol.Response {
		asked.Add(1)
		return protocol.ErrorResponsef("session `demo` cannot forward an SSH agent")
	})
	f := c.forwardAgent("demo", &protocol.Attached{AttachID: "attach-3", SessionUID: "0f0f"})
	if f == nil {
		t.Fatal("no forwarder with [ssh_agent] forward = true")
	}
	t.Cleanup(f.stop)
	select {
	case <-f.done:
	case <-time.After(10 * time.Second):
		t.Fatal("a refused forwarder did not end")
	}
	if n := asked.Load(); n != maxForwarderRefusals {
		t.Fatalf("asked %d times for a forwarder the daemon refused, want %d", n, maxForwarderRefusals)
	}
}

// Once this machine's agent has gone, a new program connection is closed at
// once rather than left waiting, far below the bound, and one already open
// carries on.
func TestSSHAgentRelayClosesConnectionsOnceTheAgentHasGone(t *testing.T) {
	ring := agent.NewKeyring()
	addKey(t, ring, "laptop")
	path, stopAgent := serveAgent(t, ring)
	c, ends := forwardingClient(t, "[ssh_agent]\nforward = true\nagent = \""+path+"\"\n")
	w := forward(t, c, ends)
	list := []byte{sshagent.MsgRequestIdentities}
	w.reply(1, list)
	stopAgent()
	w.refused(2, list)
	w.reply(1, list)
}

// agt opens a forwarder only for an agent holding a key [ssh_agent] keys
// allows: one holding only others would answer every request with no keys,
// and hide the devbox's own agent.
func TestSSHAgentForwardsOnlyAnAgentWithAnAllowedKey(t *testing.T) {
	ring := agent.NewKeyring()
	addKey(t, ring, "unlisted")
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	path, _ := serveAgent(t, ring)
	c, ends := forwardingClient(t, "[ssh_agent]\nforward = true\nagent = \""+path+"\"\nkeys = [\""+ssh.FingerprintSHA256(listed)+"\"]\n")
	f := c.forwardAgent("demo", &protocol.Attached{AttachID: "attach-3", SessionUID: "0f0f"})
	if f == nil {
		t.Fatal("no forwarder with [ssh_agent] forward = true")
	}
	t.Cleanup(f.stop)
	select {
	case w := <-ends:
		w.conn.Close()
		t.Fatal("forwarded an agent that holds no key [ssh_agent] keys allows")
	case <-time.After(1500 * time.Millisecond):
	}
	if err := ring.Add(agent.AddedKey{PrivateKey: priv, Comment: "listed"}); err != nil {
		t.Fatal(err)
	}
	select {
	case w := <-ends:
		w.conn.Close()
	case <-time.After(10 * time.Second):
		t.Fatal("no forwarder once the agent held a listed key")
	}
}

// A frame from the worker over one agent message ends the stream at its
// header: agt neither allocates it nor waits for it.
func TestSSHAgentRelayFrameBound(t *testing.T) {
	ring := agent.NewKeyring()
	addKey(t, ring, "laptop")
	path, _ := serveAgent(t, ring)
	c, ends := forwardingClient(t, "[ssh_agent]\nforward = true\nagent = \""+path+"\"\n")
	w := forward(t, c, ends)
	// Only the header of an SSHAgentRequest (kind 181) declaring 32 MiB.
	header := make([]byte, 16)
	binary.LittleEndian.PutUint32(header[0:], 0x4147_4450)
	binary.LittleEndian.PutUint16(header[4:], protocol.ProtocolVersion)
	binary.LittleEndian.PutUint16(header[6:], 181)
	binary.LittleEndian.PutUint32(header[12:], 32<<20)
	if _, err := w.conn.Write(header); err != nil {
		t.Fatal(err)
	}
	w.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := w.r.ReadByte(); errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("agt waits for a 32 MiB frame instead of ending the stream at its header")
	}
}

// An agtd from before forwarding (no CapSSHAgent) is never asked for a
// stream, and agt gives up on it.
func TestSSHAgentOlderDaemon(t *testing.T) {
	ring := agent.NewKeyring()
	addKey(t, ring, "laptop")
	path, _ := serveAgent(t, ring)
	var asked atomic.Int32
	c, _ := agentDaemonWith(t, "[ssh_agent]\nforward = true\nagent = \""+path+"\"\n", []string{protocol.CapControlStream},
		func(*protocol.ForwardSSHAgent) *protocol.Response { asked.Add(1); return nil })
	f := c.forwardAgent("demo", &protocol.Attached{AttachID: "attach-3", SessionUID: "0f0f"})
	if f == nil {
		t.Fatal("no forwarder with [ssh_agent] forward = true")
	}
	t.Cleanup(f.stop)
	select {
	case <-f.done:
	case <-time.After(10 * time.Second):
		t.Fatal("agt kept trying to forward to an agtd without CapSSHAgent")
	}
	if n := asked.Load(); n != 0 {
		t.Fatalf("opened %d ssh-agent streams to an agtd without CapSSHAgent", n)
	}
}

// A stream that fails to open (it closes before any answer) is opened
// again while the attachment lives: only a refusal counts toward giving up.
func TestSSHAgentFailedOpenIsRetried(t *testing.T) {
	fastRetries(t, 10*time.Millisecond, 50*time.Millisecond)
	ring := agent.NewKeyring()
	addKey(t, ring, "laptop")
	path, _ := serveAgent(t, ring)
	var asked atomic.Int32
	c, ends := agentDaemon(t, "[ssh_agent]\nforward = true\nagent = \""+path+"\"\n", func(*protocol.ForwardSSHAgent) *protocol.Response {
		if asked.Add(1) <= maxForwarderRefusals {
			return dropForward
		}
		return nil
	})
	f := c.forwardAgent("demo", &protocol.Attached{AttachID: "attach-3", SessionUID: "0f0f"})
	if f == nil {
		t.Fatal("no forwarder with [ssh_agent] forward = true")
	}
	t.Cleanup(f.stop)
	select {
	case w := <-ends:
		w.conn.Close()
	case <-time.After(20 * time.Second):
		t.Fatalf("agt gave up after %d streams that failed to open", asked.Load())
	}
}

// A misspelled [ssh_agent] setting, which could offer every key, stops
// forwarding, and nothing else: agt's other settings still hold (the local
// echo, on by default, here).
func TestSSHAgentConfigMistakeStopsOnlyForwarding(t *testing.T) {
	ring := agent.NewKeyring()
	addKey(t, ring, "laptop")
	path, _ := serveAgent(t, ring)
	c, ends := forwardingClient(t, "[ssh_agent]\nforward = true\nagent = \""+path+"\"\nkey = [\"SHA256:"+strings.Repeat("A", 43)+"\"]\n")
	if f := c.forwardAgent("demo", &protocol.Attached{AttachID: "attach-3", SessionUID: "0f0f"}); f != nil {
		f.stop()
		t.Fatal("forwarded with a misspelled [ssh_agent] setting")
	}
	select {
	case w := <-ends:
		w.conn.Close()
		t.Fatal("opened an ssh-agent stream with a misspelled [ssh_agent] setting")
	default:
	}
	if c.newEchoFor(&protocol.Attached{Features: []string{protocol.CapAttachField}}) == nil {
		t.Fatal("a misspelled [ssh_agent] setting turned the local echo off")
	}
}

// agt says why it forwards nothing when [ssh_agent] has a mistake, unless
// the file turns forwarding off; the inbox shows it until a key
// acknowledges it, rather than print it under its raw mode.
func TestSSHAgentWarning(t *testing.T) {
	c := testClient(t)
	if err := os.MkdirAll(filepath.Dir(c.paths.Config), 0o700); err != nil {
		t.Fatal(err)
	}
	mistake := "[ssh_agent]\nforward = true\nkey = []\n"
	for _, tc := range []struct{ text, want string }{
		{"", ""},
		{"[ssh_agent]\nforward = true\n", ""},
		{mistake, "not forwarding your SSH agent: [ssh_agent] has no setting `key`"},
		{"[ssh_agent]\nkey = []\n", "not forwarding your SSH agent: [ssh_agent] has no setting `key`"},
		{"[ssh_agent]\nforward = false\nkey = []\n", ""},
	} {
		if err := os.WriteFile(c.paths.Config, []byte(tc.text), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := sshAgentWarning(c.paths); !strings.HasPrefix(got, tc.want) || (got == "") != (tc.want == "") {
			t.Fatalf("%q: warning %q, want %q", tc.text, got, tc.want)
		}
	}
	if err := os.WriteFile(c.paths.Config, []byte(mistake), 0o600); err != nil {
		t.Fatal(err)
	}
	i := newInbox(c.paths, nil, "")
	if !slices.ContainsFunc(i.failures, func(f string) bool { return strings.HasPrefix(f, "not forwarding your SSH agent") }) {
		t.Fatalf("the inbox's failures %q do not say why nothing is forwarded", i.failures)
	}
}

// One refusal does not end forwarding: the worker may not yet have seen the
// last stream end when agt opens the next, and says it already forwards.
func TestSSHAgentForwarderOutlastsARefusal(t *testing.T) {
	fastRetries(t, 10*time.Millisecond, 50*time.Millisecond)
	ring := agent.NewKeyring()
	addKey(t, ring, "laptop")
	path, _ := serveAgent(t, ring)
	var asked atomic.Int32
	c, ends := agentDaemon(t, "[ssh_agent]\nforward = true\nagent = \""+path+"\"\n", func(*protocol.ForwardSSHAgent) *protocol.Response {
		if asked.Add(1) == 2 {
			return protocol.ErrorResponsef("this attachment already forwards an SSH agent")
		}
		return nil
	})
	w := forward(t, c, ends)
	w.conn.Close() // the stream ends; the attachment lives on
	select {
	case w2 := <-ends:
		t.Cleanup(func() { w2.conn.Close() })
	case <-time.After(15 * time.Second):
		t.Fatalf("agt stopped forwarding after one refusal (asked %d times)", asked.Load())
	}
	if n := asked.Load(); n != 3 {
		t.Fatalf("asked %d times, want the open, the refused reopen and the next", n)
	}
}

// A stream that served starts the count of refusals over: refusals between
// served streams, fewer than maxForwarderRefusals in a row each time, never
// end forwarding.
func TestSSHAgentServedStreamResetsRefusals(t *testing.T) {
	fastRetries(t, 10*time.Millisecond, 50*time.Millisecond)
	ring := agent.NewKeyring()
	addKey(t, ring, "laptop")
	path, _ := serveAgent(t, ring)
	var asked atomic.Int32
	c, ends := agentDaemon(t, "[ssh_agent]\nforward = true\nagent = \""+path+"\"\n", func(*protocol.ForwardSSHAgent) *protocol.Response {
		if asked.Add(1)%maxForwarderRefusals != 1 {
			return protocol.ErrorResponsef("this attachment already forwards an SSH agent")
		}
		return nil
	})
	w := forward(t, c, ends)
	for range 2 {
		w.conn.Close() // the stream ends; maxForwarderRefusals-1 refusals follow
		select {
		case w = <-ends:
		case <-time.After(10 * time.Second):
			t.Fatalf("agt stopped forwarding after %d opens", asked.Load())
		}
	}
	w.conn.Close()
}

// agt's longest wait before it opens a forwarder again is shorter than the
// worker keeps programs waiting for it (sizeOwnerGrace, 10 s, in
// internal/worker, which agt cannot import), or they would get the devbox's
// agent meanwhile.
func TestSSHAgentRetryWaitFitsTheWorkersWait(t *testing.T) {
	if forwarderRetryMax >= 10*time.Second {
		t.Fatalf("agt waits up to %v to open a forwarder again; the worker waits 10 s for it", forwarderRetryMax)
	}
}

// The wait before agt opens a forwarder again grows while streams keep
// ending at once, and starts over once a stream has served
// forwarderRetryMax.
func TestSSHAgentForwarderBackoff(t *testing.T) {
	fastRetries(t, 20*time.Millisecond, 320*time.Millisecond)
	ring := agent.NewKeyring()
	addKey(t, ring, "laptop")
	path, _ := serveAgent(t, ring)
	c, ends := forwardingClient(t, "[ssh_agent]\nforward = true\nagent = \""+path+"\"\n")
	w := forward(t, c, ends)
	// reopen ends the stream once it has served for held, and returns how
	// long the next took to open.
	reopen := func(held time.Duration) time.Duration {
		t.Helper()
		time.Sleep(held)
		w.conn.Close()
		closed := time.Now()
		select {
		case w = <-ends:
			return time.Since(closed)
		case <-time.After(10 * time.Second):
			t.Fatal("agt did not open its forwarder again")
			return 0
		}
	}
	var gaps []time.Duration
	for range 5 {
		gaps = append(gaps, reopen(0))
	}
	if gaps[4] < 250*time.Millisecond {
		t.Fatalf("waits before reopening streams that end at once: %v, want them grown to %v", gaps, forwarderRetryMax)
	}
	if gap := reopen(forwarderRetryMax + 100*time.Millisecond); gap > 160*time.Millisecond {
		t.Fatalf("waited %v to reopen after a stream that served %v, want about %v", gap, forwarderRetryMax, forwarderRetryMin)
	}
	w.conn.Close()
}

// The daemon's answer to opening a forwarder is read with the stream's
// frame bound: one declaring 32 MiB ends the stream at its header, and agt
// tries again at once rather than wait for it until the open times out.
func TestSSHAgentForwardingAnswerFrameBound(t *testing.T) {
	fastRetries(t, 10*time.Millisecond, 50*time.Millisecond)
	ring := agent.NewKeyring()
	addKey(t, ring, "laptop")
	path, _ := serveAgent(t, ring)
	var asked atomic.Int32
	c, ends := agentDaemon(t, "[ssh_agent]\nforward = true\nagent = \""+path+"\"\n", func(*protocol.ForwardSSHAgent) *protocol.Response {
		if asked.Add(1) == 1 {
			return oversizedAnswer
		}
		return nil
	})
	start := time.Now()
	forward(t, c, ends)
	if waited := time.Since(start); waited > 2*time.Second {
		t.Fatalf("took %v to forward after an answer declaring 32 MiB, rather than end that stream at its header", waited)
	}
}

// slowListAgent takes its time to list its keys, as OpenSSH's agent does
// while it asks the user to confirm another request.
type slowListAgent struct {
	agent.ExtendedAgent
	delay time.Duration
}

func (s slowListAgent) List() ([]*agent.Key, error) {
	time.Sleep(s.delay)
	return s.ExtendedAgent.List()
}

// Asking the agent which keys it holds, before a forwarder opens, waits for
// a busy agent as a request would: the agent is still lent.
func TestSSHAgentLendsABusyAgent(t *testing.T) {
	ring := agent.NewKeyring()
	addKey(t, ring, "laptop")
	path, _ := serveAgent(t, slowListAgent{ring.(agent.ExtendedAgent), localAgentDialTimeout + 500*time.Millisecond})
	r := newAgentRelay(t.Context(), path, sshagent.NewKeys(nil), nil)
	defer r.cancel()
	if !r.lends() {
		t.Fatal("a busy agent is not lent: asking for its keys gave up on it")
	}
}

// For a session on this machine there is no hop: a program's own binding
// reaches OpenSSH's agent, so a key limited with `ssh-add -h` signs the
// login it was limited to, as ssh run here would.
func TestSSHAgentLocalRelayPassesProgramBindings(t *testing.T) {
	a := startOpenSSHAgent(t)
	sessionID := make([]byte, 32)
	if _, err := rand.Read(sessionID); err != nil {
		t.Fatal(err)
	}
	sig, err := a.destination.Sign(rand.Reader, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	programBind := (&sshagent.SessionBind{HostKey: a.destination.PublicKey().Marshal(), SessionID: sessionID, Signature: ssh.Marshal(sig)}).Request()
	str := func(b []byte) []byte { return append(binary.BigEndian.AppendUint32(nil, uint32(len(b))), b...) }
	login := slices.Concat(str(sessionID), []byte{50}, str([]byte("git")), str([]byte("ssh-connection")),
		str([]byte("publickey")), []byte{1}, str([]byte(a.limited.Type())), str(a.limited.Marshal()))
	signLogin := requestFrom(t, func(ag agent.ExtendedAgent) error { _, err := ag.Sign(a.limited, login); return err })

	r := newAgentRelay(t.Context(), a.sock, sshagent.NewKeys(nil), nil)
	lc := &localAgentConn{id: 1, reqs: make(chan []byte, 1), done: make(chan struct{})}
	t.Cleanup(func() {
		r.cancel()
		if lc.agent != nil {
			lc.agent.Close()
		}
	})
	for _, step := range []struct {
		what string
		msg  []byte
		want byte
	}{
		{"the program's binding", programBind, sshagent.MsgSuccess},
		{"the limited key's login to dest.example", signLogin, sshagent.MsgSignResponse},
	} {
		reply, err := r.answer(lc, step.msg)
		if err != nil {
			t.Fatal(err)
		}
		if got := sshagent.Type(reply); got != step.want {
			t.Fatalf("%s on this machine: type %d, want %d", step.what, got, step.want)
		}
	}
}
