package daemon

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"agtd.sh/internal/protocol"
	"agtd.sh/internal/session"
	"agtd.sh/internal/sshagent"
	"agtd.sh/internal/transport"
)

// These tests forward an SSH agent through the daemon to real session
// workers: the daemon checks the stream and stamps who the client is, and
// the agent socket survives a live handoff.

func newKeyring(t *testing.T) (agent.Agent, ssh.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	ring := agent.NewKeyring()
	if err := ring.Add(agent.AddedKey{PrivateKey: priv}); err != nil {
		t.Fatal(err)
	}
	return ring, key
}

// forwarder answers a session's agent requests from a keyring, as agt does
// from the user's agent.
type forwarder struct {
	done chan struct{}
}

// forwardOn opens a forwarder over stream with req. It returns the
// forwarder, nil if the daemon refused it, and the daemon's answer.
func forwardOn(t *testing.T, stream transport.Stream, req protocol.ForwardSSHAgent, ring agent.Agent) (*forwarder, *protocol.Response) {
	t.Helper()
	t.Cleanup(func() { stream.Close() })
	if err := protocol.WriteRequest(stream, &protocol.Request{ForwardSSHAgent: &req}); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(stream)
	stream.SetReadDeadline(time.Now().Add(testTimeout))
	resp, err := protocol.ReadResponse(r)
	if err != nil || resp == nil {
		t.Fatalf("forward: %v", err)
	}
	stream.SetReadDeadline(time.Time{})
	if resp.SSHAgentForwarding == nil {
		return nil, resp
	}
	f := &forwarder{done: make(chan struct{})}
	go func() {
		defer close(f.done)
		var mu sync.Mutex
		pipes := map[uint32]net.Conn{}
		defer func() {
			for _, p := range pipes {
				p.Close()
			}
		}()
		for {
			resp, err := protocol.ReadResponse(r)
			if err != nil || resp == nil {
				return
			}
			switch {
			case resp.SSHAgentRequest != nil:
				m := resp.SSHAgentRequest
				mu.Lock()
				p := pipes[m.Conn]
				if p == nil {
					var served net.Conn
					p, served = net.Pipe()
					go func() { _ = agent.ServeAgent(ring, served) }()
					pipes[m.Conn] = p
				}
				mu.Unlock()
				reply := sshagent.Failure()
				if sshagent.WriteMessage(p, m.Data) == nil {
					if got, err := sshagent.ReadMessage(p); err == nil {
						reply = got
					}
				}
				if protocol.WriteRequest(stream, &protocol.Request{SSHAgentReply: &protocol.SSHAgentMessage{Conn: m.Conn, Data: reply}}) != nil {
					return
				}
			case resp.SSHAgentClose != nil:
				mu.Lock()
				if p := pipes[resp.SSHAgentClose.Conn]; p != nil {
					p.Close()
					delete(pipes, resp.SSHAgentClose.Conn)
				}
				mu.Unlock()
			}
		}
	}()
	return f, resp
}

// agentClient is a client of a session's agent socket, as a program in the
// session would be.
func agentClient(t *testing.T, path string) agent.ExtendedAgent {
	t.Helper()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(testTimeout))
	t.Cleanup(func() { conn.Close() })
	return agent.NewClient(conn)
}

func keyFingerprints(t *testing.T, client agent.ExtendedAgent) []string {
	t.Helper()
	keys, err := client.List()
	if err != nil {
		t.Fatalf("listing keys: %v", err)
	}
	var out []string
	for _, k := range keys {
		out = append(out, sshagent.Fingerprint(k.Blob))
	}
	return out
}

// Forwarding through the daemon over the local socket: the program in the
// session gets the client's keys and signatures, and the event names the
// client as local whatever it claimed. A forwarder for another incarnation
// is refused, and the daemon's other streams refuse forwarding messages.
func TestSSHAgentForwardingThroughTheDaemon(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	h := newHarness(t)
	id := h.mustCreate("fwd")
	a, attached := mustAttachOn(t, h.dial(), id, protocol.AttachSession{Features: allAttachFeatures})
	ring, key := newKeyring(t)
	req := protocol.ForwardSSHAgent{SessionID: id, ExpectUID: attached.SessionUID, Attachment: a.attachID, Client: "someone else"}
	if f, resp := forwardOn(t, h.dial(), req, ring); f == nil {
		t.Fatalf("forward: %#v (error %v)", resp, resp.Error)
	}

	client := agentClient(t, h.paths.SSHAgentSocketPath(attached.SessionUID))
	if got := keyFingerprints(t, client); !slices.Equal(got, []string{ssh.FingerprintSHA256(key)}) {
		t.Fatalf("keys = %v", got)
	}
	sig, err := client.Sign(key, []byte("data"))
	if err != nil || key.Verify([]byte("data"), sig) != nil {
		t.Fatalf("signature: %v", err)
	}
	if ev := h.waitEvent(id, session.EventSSHAgentSigned); ev.Summary != "a local client signed data using key "+ssh.FingerprintSHA256(key) || ev.Attention != session.AttentionInfo {
		t.Fatalf("signed event = %+v", ev)
	}

	stale := req
	stale.ExpectUID = strings.Repeat("0", 32)
	if f, resp := forwardOn(t, h.dial(), stale, ring); f != nil || resp.Error == nil || !strings.Contains(resp.Error.Message, "not the session you were attached to") {
		t.Fatalf("a forwarder for another incarnation: %#v", resp)
	}
	wantError(t, h.request(&protocol.Request{SSHAgentReply: &protocol.SSHAgentMessage{Conn: 1, Data: []byte{5}}}), "only valid on an ssh-agent stream")
	if resp := h.control().call(&protocol.Request{ForwardSSHAgent: &req}); resp.Error == nil || !strings.Contains(resp.Error.Message, "needs a stream of its own") {
		t.Fatalf("a forwarder on the control stream: %#v", resp)
	}
}

// Over QUIC the daemon names the client by its authorized key.
func TestSSHAgentForwardingOverQUICNamesTheClient(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	h, client := remoteHarness(t, 0)
	id := h.mustCreate("remote")
	qc := h.dialQUIC(client, h.srv.RemoteAddr())
	a, attached := mustAttachOn(t, openStream(t, qc), id, protocol.AttachSession{Features: allAttachFeatures})
	ring, key := newKeyring(t)
	req := protocol.ForwardSSHAgent{SessionID: id, ExpectUID: attached.SessionUID, Attachment: a.attachID}
	if f, resp := forwardOn(t, openStream(t, qc), req, ring); f == nil {
		t.Fatalf("forward: %#v (error %v)", resp, resp.Error)
	}
	if _, err := agentClient(t, h.paths.SSHAgentSocketPath(attached.SessionUID)).Sign(key, []byte("data")); err != nil {
		t.Fatal(err)
	}
	want := "test (" + client.Fingerprint + ") signed data using key " + ssh.FingerprintSHA256(key)
	if ev := h.waitEvent(id, session.EventSSHAgentSigned); ev.Summary != want {
		t.Fatalf("signed event = %q, want %q", ev.Summary, want)
	}
}

// A daemon configured without session sockets starts sessions as before:
// no agent socket, and forwarders are refused.
func TestSSHAgentNeedsASessionSocket(t *testing.T) {
	h := newHarnessWithConfig(t, "\n[ssh_agent]\nsession_socket = false\n")
	id := h.mustCreate("plain")
	a, attached := mustAttachOn(t, h.dial(), id, protocol.AttachSession{Features: allAttachFeatures})
	ring, _ := newKeyring(t)
	req := protocol.ForwardSSHAgent{SessionID: id, ExpectUID: attached.SessionUID, Attachment: a.attachID}
	if f, resp := forwardOn(t, h.dial(), req, ring); f != nil || resp.Error == nil || !strings.Contains(resp.Error.Message, "cannot forward an SSH agent") {
		t.Fatalf("forwarder to a session without a socket: %#v", resp)
	}
	if _, err := os.Stat(h.paths.SSHAgentSocketPath(attached.SessionUID)); !os.IsNotExist(err) {
		t.Fatalf("agent socket of a session started without one: %v", err)
	}
}

// A live handoff keeps the agent socket: the same file, listening
// throughout. The forwarder ends with its attachment, a connection made
// meanwhile waits for the owner's reattach, and the reattached owner's
// forwarder answers it.
func TestSSHAgentSocketSurvivesALiveHandoff(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	h := newHarness(t)
	id := h.mustCreate("handoff")
	a, attached := mustAttachOn(t, h.dial(), id, protocol.AttachSession{Features: allAttachFeatures})
	uid := attached.SessionUID
	ring, key := newKeyring(t)
	f, resp := forwardOn(t, h.dial(), protocol.ForwardSSHAgent{SessionID: id, ExpectUID: uid, Attachment: a.attachID}, ring)
	if f == nil {
		t.Fatalf("forward: %#v (error %v)", resp, resp.Error)
	}
	path := h.paths.SSHAgentSocketPath(uid)
	want := []string{ssh.FingerprintSHA256(key)}
	if got := keyFingerprints(t, agentClient(t, path)); !slices.Equal(got, want) {
		t.Fatalf("keys before the handoff = %v", got)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if results := h.handoff(nextBinary(t)); len(results) != 1 || results[0].HandedOff == nil {
		t.Fatalf("handoff results = %+v", results)
	}
	a.expectRestarting()
	select {
	case <-f.done:
	case <-time.After(testTimeout):
		t.Fatal("the forwarder outlived its attachment")
	}
	if after, err := os.Stat(path); err != nil || !os.SameFile(before, after) {
		t.Fatalf("agent socket after the handoff: %v", err)
	}

	got := make(chan []string, 1)
	waiting := agentClient(t, path)
	go func() {
		keys, err := waiting.List()
		if err != nil {
			got <- []string{"error: " + err.Error()}
			return
		}
		var fps []string
		for _, k := range keys {
			fps = append(fps, sshagent.Fingerprint(k.Blob))
		}
		got <- fps
	}()
	select {
	case keys := <-got:
		t.Fatalf("answered %v before the owner came back", keys)
	case <-time.After(300 * time.Millisecond):
	}
	a2, _ := mustAttachOn(t, h.dial(), id, protocol.AttachSession{Features: allAttachFeatures, ExpectUID: uid, Replaces: a.attachID})
	if f, resp := forwardOn(t, h.dial(), protocol.ForwardSSHAgent{SessionID: id, ExpectUID: uid, Attachment: a2.attachID}, ring); f == nil {
		t.Fatalf("forward after the handoff: %#v (error %v)", resp, resp.Error)
	}
	select {
	case keys := <-got:
		if !slices.Equal(keys, want) {
			t.Fatalf("keys after the handoff = %v", keys)
		}
	case <-time.After(testTimeout):
		t.Fatal("the waiting connection was not answered after the reattach")
	}
}

// A starting daemon removes the agent sockets of incarnations that are no
// longer running, and leaves running sessions' alone.
func TestDaemonStartSweepsStaleAgentSockets(t *testing.T) {
	h := newHarness(t)
	id := h.mustCreate("live")
	_, attached := mustAttachOn(t, h.dial(), id, protocol.AttachSession{Features: allAttachFeatures})
	live := h.paths.SSHAgentSocketPath(attached.SessionUID)
	h.stop()
	stale := filepath.Join(h.paths.SSHDir, strings.Repeat("ab", 16)+".sock")
	if err := os.WriteFile(stale, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	h.start()
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale agent socket after a start: %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("a running session's agent socket: %v", err)
	}
}
