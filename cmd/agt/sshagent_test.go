package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"agtd.sh/internal/transport/transporttest"
)

// SSH agent forwarding, end to end: the real agt lends an agent (an
// in-process keyring standing in for ssh-agent or 1Password) to sessions,
// and their shells run the real ssh-add and ssh against it.

func requireOpenSSH(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"ssh", "ssh-add"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
}

// testAgent is an agent holding one key, served on a Unix socket.
type testAgent struct {
	path string
	key  ssh.PublicKey
	fp   string
	// hops counts the forwarding bindings it took: only agt's binding of
	// the agtd hop sets is_forwarding, ssh's own bindings do not.
	hops atomic.Int32
}

// bindingAgent accepts the session bindings ssh sends, as OpenSSH's agent
// does (x/crypto's keyring refuses the extension), so the events can name
// the host a login went to.
type bindingAgent struct {
	agent.ExtendedAgent
	hops *atomic.Int32
}

func (b bindingAgent) Extension(name string, contents []byte) ([]byte, error) {
	if name == "session-bind@openssh.com" {
		// is_forwarding is the binding's last byte.
		if len(contents) > 0 && contents[len(contents)-1] == 1 {
			b.hops.Add(1)
		}
		return nil, nil
	}
	return b.ExtendedAgent.Extension(name, contents)
}

func serveTestAgent(t *testing.T) *testAgent {
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
	if err := ring.Add(agent.AddedKey{PrivateKey: priv, Comment: "e2e"}); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("/tmp", "agag-")
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
	a := &testAgent{path: path, key: key, fp: ssh.FingerprintSHA256(key)}
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = agent.ServeAgent(bindingAgent{ring.(agent.ExtendedAgent), &a.hops}, conn)
			}()
		}
	}()
	return a
}

// startSSHServer runs an SSH server that lets in only the holder of
// allowed, and answers any command by greeting the user. It returns the
// command that logs in to it.
func startSSHServer(t *testing.T, allowed ssh.PublicKey) string {
	t.Helper()
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostKey, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if bytes.Equal(key.Marshal(), allowed.Marshal()) {
				return nil, nil
			}
			return nil, fmt.Errorf("unknown key")
		},
	}
	config.AddHostKey(hostKey)
	l, err := net.Listen("tcp", "127.0.0.1:0")
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
			go serveSSH(conn, config)
		}
	}()
	host, port, _ := net.SplitHostPort(l.Addr().String())
	// No config, known hosts or key files of the user running the tests:
	// the only key on offer is the agent's.
	return "ssh -F /dev/null -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o GlobalKnownHostsFile=/dev/null" +
		" -o IdentityFile=/dev/null -o LogLevel=ERROR -p " + port + " test@" + host + " true"
}

func serveSSH(conn net.Conn, config *ssh.ServerConfig) {
	defer conn.Close()
	sc, chans, reqs, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	defer sc.Close()
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			nc.Reject(ssh.UnknownChannelType, "sessions only")
			continue
		}
		ch, chReqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go func() {
			defer ch.Close()
			for req := range chReqs {
				if req.Type != "exec" {
					req.Reply(false, nil)
					continue
				}
				req.Reply(true, nil)
				fmt.Fprintf(ch, "the server greets %s\n", sc.User())
				ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
				return
			}
		}()
	}
}

// forwardingConfig turns forwarding on, lending agent.
func forwardingConfig(agentPath string) string {
	return "\n[ssh_agent]\nforward = true\nagent = \"" + agentPath + "\"\n"
}

// listKeysAs lists the session's agent keys from its shell, each line
// prefixed with tag, once the forwarder has had the moment it takes to
// open after an attach.
func listKeysAs(tag string) string {
	return "until ssh-add -l >/dev/null 2>&1; do sleep 0.1; done; ssh-add -l | sed 's/^/" + tag + ":/'\r"
}

// Over QUIC: the session's ssh-add lists the laptop's key and ssh logs in
// with it; forwarding comes back with the reattach after the connection is
// lost; once the client has gone, programs get no keys, and the events say
// who signed what and that the agent went missing.
func TestSSHAgentForwardingEndToEnd(t *testing.T) {
	requireOpenSSH(t)
	a := serveTestAgent(t)
	login := startSSHServer(t, a.key)
	probe, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.LocalAddr().String()
	probe.Close()
	e := newEnv(t, "\n[remote]\nlisten = \""+addr+"\"\n"+forwardingConfig(a.path))
	// The daemon, and so its sessions, inherit no agent: what the session
	// sees is what agt forwards.
	e.extraEnv = []string{"SSH_AUTH_SOCK="}
	e.startAndDetach("demo")
	relay := transporttest.NewRelay(t, addr)
	e.mustRun("agt", "host", "add", "dev", relay.Addr(), "--fingerprint", strings.TrimSpace(e.mustRun("agtd", "remote", "id")))
	clientKey := strings.TrimSpace(e.mustRun("agt", "remote", "id"))
	e.mustRun("agtd", "remote", "authorize", clientKey, "laptop")

	tm := e.start("attach", "dev/demo")
	m := tm.expect(0, "attached to demo")
	tm.write(listKeysAs("first"))
	m = tm.expect(m, "first:256 "+a.fp)
	tm.write(login + "\r")
	m = tm.expect(m, "the server greets test")
	// The session is on another host, so what reached this agent came
	// through the agtd hop, which ssh-add -h limits apply to.
	if a.hops.Load() == 0 {
		t.Fatal("agt lent its agent to a session on another host without binding the agtd hop")
	}

	// The daemon restarts: the attachment reconnects, and its forwarder with
	// it.
	e.mustRun("agt", "daemon", "restart")
	m = tm.expect(m, "connection to dev lost, reconnecting")
	m = tm.expect(m, "the server greets test")
	tm.write("ssh-add -l | sed 's/^/again:/'\r")
	m = tm.expect(m, "again:256 "+a.fp)
	tm.write("\x1c")
	tm.wait()

	// Nobody forwards now: programs get no keys, at once. The detach told
	// the worker it was leaving before agt closed its connection; had that
	// been lost, the worker would take it for a cut and keep programs
	// waiting 10 seconds for the owner to come back.
	out := filepath.Join(e.work, "keys.out")
	e.mustRun("agt", "send-input", "demo", "ssh-add -l > "+out+" 2>&1; echo status-$? >> "+out+"\r")
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, _ := os.ReadFile(out)
		if strings.Contains(string(data), "status-") {
			if !strings.Contains(string(data), "no identities") || !strings.Contains(string(data), "status-1") {
				t.Fatalf("ssh-add without a client:\n%s", data)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ssh-add never finished:\n%s", data)
		}
		time.Sleep(50 * time.Millisecond)
	}

	events := e.mustRun("agt", "events", "demo")
	for _, want := range []string{
		"laptop (" + clientKey + ") signed a login as test to an unknown host (key SHA256:",
		"using key " + a.fp,
		"ssh_agent_unavailable",
	} {
		if !strings.Contains(events, want) {
			t.Fatalf("no %q in events:\n%s", want, events)
		}
	}
	e.mustRun("agt", "send-input", "demo", "exit\r")
}

// Over the local socket: agt new forwards, and so does the switch to
// another session; with forwarding off, an attachment lends nothing.
func TestSSHAgentForwardingLocally(t *testing.T) {
	requireOpenSSH(t)
	a := serveTestAgent(t)
	e := newEnv(t, forwardingConfig(a.path))
	e.extraEnv = []string{"SSH_AUTH_SOCK="}

	tm := e.start("new", "one")
	m := tm.expect(0, "attached to one")
	tm.write(listKeysAs("one"))
	m = tm.expect(m, "one:256 "+a.fp)

	e.startAndDetach("two")
	tm.write("\x1d")
	m = tm.expect(m, "\x1b]0;two - agtd\x07")
	tm.write(listKeysAs("two"))
	m = tm.expect(m, "two:256 "+a.fp)
	// Sessions on this machine are local use: no hop.
	if n := a.hops.Load(); n != 0 {
		t.Fatalf("a local session's agent connections were bound to a hop %d times", n)
	}
	tm.write("\x1c")
	tm.wait()

	config := filepath.Join(e.root, "config.toml")
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, bytes.Replace(data, []byte("forward = true"), []byte("forward = false"), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	tm = e.start("attach", "two")
	m = tm.expect(0, "attached to two")
	tm.write("ssh-add -l | sed 's/^/off:/'\r")
	m = tm.expect(m, "off:The agent has no identities")
	tm.write("\x1c")
	tm.wait()

	// A mistake in [ssh_agent] stops forwarding, and agt says so before the
	// attach takes the screen and again once it gives it back, since its
	// first paint clears the screen.
	if err := os.WriteFile(config, append(data, "key = []\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
	warning := "agt: not forwarding your SSH agent: [ssh_agent] has no setting `key`"
	tm = e.start("attach", "two")
	m = tm.expect(0, warning)
	m = tm.expect(m, "attached to two")
	// The shells exit rather than wait out a kill's grace at cleanup.
	tm.write("exit\r")
	m = tm.expect(m, warning)
	tm.expect(m, "session two finished")
	tm.wait()
	e.mustRun("agt", "send-input", "one", "exit\r")
}

// stubGit puts a git first on PATH (of agt, and so of the daemons the test
// starts) that clones by listing the agent's keys, failing with none, and
// making PATH/.git: `git clone -- URL PATH`. A URL with "missing" in it
// fails as a repository that is not there.
func stubGit(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	stub := "#!/bin/sh\ncase \"$3\" in *missing*) echo 'fatal: repository not found'; exit 128;; esac\nkeys=$(ssh-add -l) || { echo clone-keys:none; exit 128; }\necho \"$keys\" | sed 's/^/clone-keys:/'\nmkdir -p \"$4/.git\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// workspace add --clone clones with the forwarded agent alone, even with
// another agent in the daemon's environment, then registers the clone, on
// this machine and over QUIC through the agtd hop; agt new runs in it.
func TestWorkspaceClone(t *testing.T) {
	requireOpenSSH(t)
	stubGit(t)
	a := serveTestAgent(t)
	// The daemons inherit this agent; a clone must not use it.
	decoy := serveTestAgent(t)
	t.Setenv("SSH_AUTH_SOCK", decoy.path)
	e := newEnv(t, forwardingConfig(a.path))
	home := filepath.Join(e.root, "home")
	if err := os.Mkdir(home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	tm := e.start("workspace", "add", "api", "--clone", "git@example.com:org/api.git")
	m := tm.expect(0, "attached to clone-api")
	m = tm.expect(m, "clone-keys:256 "+a.fp)
	tm.expect(m, "added workspace api at "+filepath.Join(home, "src", "api"))
	tm.wait()
	if out := tm.output(); strings.Contains(out, decoy.fp) {
		t.Fatalf("the clone used the daemon's own agent:\n%s", out)
	}
	if out := e.mustRun("agt", "workspace", "ls"); !strings.Contains(out, "api\t"+filepath.Join(home, "src", "api")) {
		t.Fatalf("workspaces:\n%s", out)
	}
	// A clone git fails registers nothing, and says where its output is.
	tm = e.start("workspace", "add", "gone", "--clone", "git@example.com:org/missing.git")
	m = tm.expect(0, "fatal: repository not found")
	tm.expect(m, "session clone-gone failed: agent exited with code 128; its output is in `agt history clone-gone`")
	if err := tm.waitErr(); err == nil {
		t.Fatal("a failed clone exited 0")
	}
	if out := e.mustRun("agt", "workspace", "ls"); strings.Contains(out, "gone") {
		t.Fatalf("a failed clone was registered:\n%s", out)
	}
	if out, err := e.run("agt", "workspace", "add", "api2", "--clone", "git@example.com:org/api.git", "~/src/api"); err == nil || !strings.Contains(out, "already exists") {
		t.Fatalf("clone onto an existing path: %v\n%s", err, out)
	}

	r := e.remoteEnv("dev")
	tm = e.start("--host", "dev", "workspace", "add", "web", "--clone", "ssh://git@example.com/org/web.git", "~/web")
	m = tm.expect(0, "attached to clone-web")
	m = tm.expect(m, "clone-keys:256 "+a.fp)
	tm.expect(m, "added workspace web at "+filepath.Join(home, "web"))
	tm.wait()
	if a.hops.Load() == 0 {
		t.Fatal("a remote clone's agent connections were not bound to the agtd hop")
	}
	if out := r.mustRun("agt", "workspace", "ls"); !strings.Contains(out, "web\t"+filepath.Join(home, "web")) {
		t.Fatalf("dev's workspaces:\n%s", out)
	}
	tm = e.start("--host", "dev", "new", "-w", "web", "in-web")
	m = tm.expect(0, "attached to in-web")
	tm.write("pwd\r")
	m = tm.expect(m, filepath.Join(home, "web"))
	tm.write("exit\r")
	tm.expect(m, "session in-web finished")
	tm.wait()
}

// Without forwarding, or an agent with a key to forward, a clone fails
// before any session starts.
func TestWorkspaceCloneNeedsForwarding(t *testing.T) {
	requireOpenSSH(t)
	stubGit(t)
	a := serveTestAgent(t)
	other := serveTestAgent(t)
	t.Setenv("HOME", t.TempDir())
	for _, tc := range []struct{ config, want string }{
		{"", "--clone uses only your forwarded SSH agent: SSH agent forwarding is off: set `forward = true` under [ssh_agent]"},
		{"\n[ssh_agent]\nforward = true\nkey = []\n", "SSH agent forwarding is off: [ssh_agent] has no setting `key`"},
		{forwardingConfig("/nonexistent/agent.sock"), "your SSH agent (/nonexistent/agent.sock) does not answer"},
		{forwardingConfig(a.path) + "keys = [\"" + other.fp + "\"]\n", "holds none of the keys [ssh_agent] keys allows"},
	} {
		e := newEnv(t, tc.config)
		out, err := e.run("agt", "workspace", "add", "api", "--clone", "git@example.com:org/api.git")
		if err == nil || !strings.Contains(out, tc.want) {
			t.Fatalf("config %q: %v\n%s", tc.config, err, out)
		}
		if out := e.mustRun("agt", "ls"); strings.Contains(out, "clone-api") {
			t.Fatalf("config %q: a refused clone started a session:\n%s", tc.config, out)
		}
	}

	// What the daemon refuses comes back as is, and PATH is needed
	// without --clone.
	e := newEnv(t, forwardingConfig(a.path))
	out, err := e.run("agt", "workspace", "add", "bad", "--clone", "-oProxyCommand=x:r")
	if err == nil || !strings.Contains(out, "not a URL") {
		t.Fatalf("an option for a URL: %v\n%s", err, out)
	}
	if out, err := e.run("agt", "workspace", "add", "api"); err == nil || !strings.Contains(out, "PATH is needed unless --clone") {
		t.Fatalf("add without PATH: %v\n%s", err, out)
	}
}
