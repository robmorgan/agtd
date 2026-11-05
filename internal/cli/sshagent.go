package cli

// SSH agent forwarding: with [ssh_agent] forward = true, every attachment agt
// opens lends this machine's agent to its session (see
// internal/protocol/sshagent.go). The session's worker passes the agent
// requests of programs in the session to the attachment that owns the
// session's size, and agt answers them from its own agent once its policy
// allows them: only listing keys, signing, and the session-bind and query
// extensions pass, and only for the keys [ssh_agent] keys lists, when it lists
// any. The private key never leaves this machine.
//
// When the session runs on another machine, every connection to this
// machine's agent is first bound to the agtd hop as a forwarding session
// (bindHop), as ssh -A binds the connections it forwards, so OpenSSH's
// destination constraints (ssh-add -h) apply to what the devbox asks. Until
// the agent has taken that binding, the program's own bindings are refused.
//
// Goroutines: an agentForwarder owns one goroutine that keeps a forwarder
// open while its attachment lives (runForwarders): it asks the local agent
// whether it has a key to lend, opens the stream, reads the worker's frames
// until the stream ends, and opens it again after a wait. Each open stream
// has one goroutine per program connection the worker numbers (at most
// maxLocalAgentConns, counted until they end), each holding its own
// connection to the local agent, since an agent keeps state per connection.
// stop cancels the forwarder, which ends a question to the local agent in
// progress, closes the stream and ends the reader, and waits for all of
// them. Writes to the stream are serialised by writeMu.

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"agtd.sh/internal/config"
	"agtd.sh/internal/paths"
	"agtd.sh/internal/protocol"
	"agtd.sh/internal/sshagent"
	"agtd.sh/internal/transport"
)

const (
	// maxLocalAgentConns bounds the local agent connections a forwarder
	// holds, each counted until its goroutine has ended; a program
	// connection over it is refused (SSHAgentClose). It matches the
	// worker's bound on a session's agent socket.
	maxLocalAgentConns = 64
	// localAgentDialTimeout bounds connecting to the local agent.
	localAgentDialTimeout = 2 * time.Second
	// localAgentReplyTimeout bounds the wait for the local agent's answer:
	// to a program's request, to the agtd hop's binding, or to the question
	// of which keys it holds. It may be asking the user to confirm a
	// signature, and OpenSSH's agent answers nothing else meanwhile. It
	// matches the worker's agentReplyTimeout, whose clock starts first
	// (before the request crosses the network), so the worker gives up
	// first.
	localAgentReplyTimeout = 90 * time.Second
	// forwarderWriteTimeout bounds a write to the worker; a stream that
	// takes longer is closed.
	forwarderWriteTimeout = 10 * time.Second
	// maxForwarderRefusals is how many refusals in a row end forwarding
	// for an attachment.
	maxForwarderRefusals = 3
)

var (
	// forwarderRetryMin and forwarderRetryMax bound the wait before a
	// forwarder is opened again while its attachment lives. The longest
	// stays under the 10 s the worker keeps programs waiting for the one
	// agt opens in place of one that ended, so they get it rather than the
	// devbox's agent. Variables for tests.
	forwarderRetryMin = time.Second
	forwarderRetryMax = 8 * time.Second
)

// agentForwarder lends this machine's agent to one attachment.
type agentForwarder struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// forwardAgent starts lending this machine's agent to an attachment, when
// [ssh_agent] forward is set and there is an agent to lend. It returns at
// once, nil when there is nothing to do: the stream is opened in the
// background, so attaching never waits for it, and a session that cannot
// forward simply goes without.
func (c *client) forwardAgent(id string, attached *protocol.Attached) *agentForwarder {
	cfg, err := config.Load(c.paths.Config)
	if err != nil || !cfg.SSHAgent.Forward || cfg.SSHAgent.Problem != "" {
		// A mistake in [ssh_agent] could offer every key the agent holds:
		// nothing is forwarded, and attach and the inbox say why
		// (sshAgentWarning).
		return nil
	}
	agent := cfg.SSHAgent.AgentSocket()
	if agent == "" {
		return nil
	}
	var hop ssh.Signer
	if c.host != nil {
		// The session runs on another machine: its requests reach this
		// machine's agent through the agtd hop.
		if hop, err = newHopSigner(); err != nil {
			return nil
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	f := &agentForwarder{cancel: cancel, done: make(chan struct{})}
	req := &protocol.ForwardSSHAgent{SessionID: id, ExpectUID: attached.SessionUID, Attachment: attached.AttachID}
	keys := sshagent.NewKeys(cfg.SSHAgent.Keys)
	go func() {
		defer close(f.done)
		c.runForwarders(ctx, req, agent, keys, hop)
	}()
	return f
}

// checkForwarding says why an attachment to c's daemon would not lend this
// machine's SSH agent, or returns nil when it would: [ssh_agent] forward is
// set and sound, and the agent answers with a key the policy offers, through
// the agtd hop for a remote daemon, as forwardAgent and runForwarders would
// find it. A clone, which a session runs with no agent but a forwarded one,
// checks this before it starts.
func (c *client) checkForwarding() error {
	cfg, err := config.Load(c.paths.Config)
	switch {
	case err != nil:
		return err
	case cfg.SSHAgent.Problem != "":
		return fmt.Errorf("SSH agent forwarding is off: %s in %s", cfg.SSHAgent.Problem, c.paths.Config)
	case !cfg.SSHAgent.Forward:
		return fmt.Errorf("SSH agent forwarding is off: set `forward = true` under [ssh_agent] in %s", c.paths.Config)
	}
	agent := cfg.SSHAgent.AgentSocket()
	if agent == "" {
		return errors.New("there is no SSH agent to forward: SSH_AUTH_SOCK is not set, nor [ssh_agent] agent")
	}
	var hop ssh.Signer
	if c.host != nil {
		if hop, err = newHopSigner(); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.requestTimeout())
	defer cancel()
	r := newAgentRelay(ctx, agent, sshagent.NewKeys(cfg.SSHAgent.Keys), hop)
	defer r.cancel()
	if !r.lends() {
		if len(cfg.SSHAgent.Keys) > 0 {
			return fmt.Errorf("your SSH agent (%s) does not answer, or holds none of the keys [ssh_agent] keys allows", agent)
		}
		return fmt.Errorf("your SSH agent (%s) does not answer, or holds no keys (add one with ssh-add)", agent)
	}
	return nil
}

// sshAgentWarning is why agt forwards no SSH agent when [ssh_agent] has a
// mistake (config.SSHAgentConfig.Problem), or "" when it has none or the
// file turns forwarding off. attach says it before it takes the screen and
// again once it gives it back, since its first paint clears the screen; the
// inbox shows it until a key acknowledges it.
func sshAgentWarning(p *paths.AppPaths) string {
	cfg, err := config.Load(p.Config)
	if err != nil || cfg.SSHAgent.Problem == "" || cfg.SSHAgent.ForwardOff() {
		return ""
	}
	return fmt.Sprintf("not forwarding your SSH agent: %s in %s", cfg.SSHAgent.Problem, p.Config)
}

// stop ends the forwarder and waits for its goroutines. Safe on nil.
func (f *agentForwarder) stop() {
	if f == nil {
		return
	}
	f.cancel()
	<-f.done
}

// runForwarders keeps a forwarder open for the attachment until ctx is
// cancelled. While this machine's agent has no key to lend (it is not
// running, or holds none that [ssh_agent] keys allows), none is opened: it
// would take every agent request in the session and answer none, hiding the
// agent the devbox has. One that ends while the attachment lives (a write
// that timed out, a stream that failed to open) is opened again, after a
// wait that grows to forwarderRetryMax; it starts again from
// forwarderRetryMin only after a stream has served that long, so one that
// keeps ending at once is not reopened every second. Refusals from the
// daemon or the worker (the session cannot forward, the attachment is gone)
// are final once maxForwarderRefusals come in a row: one alone may only
// mean the worker has yet to see the last stream end, or a daemon that
// failed for a moment.
func (c *client) runForwarders(ctx context.Context, req *protocol.ForwardSSHAgent, agent string, keys sshagent.Keys, hop ssh.Signer) {
	wait := forwarderRetryMin
	refusals := 0
	for {
		r := newAgentRelay(ctx, agent, keys, hop)
		if r.lends() {
			start := time.Now()
			served, refused := c.runForwarder(ctx, req, r)
			switch {
			case refused:
				if refusals++; refusals >= maxForwarderRefusals {
					return
				}
			case served:
				refusals = 0
				if time.Since(start) >= forwarderRetryMax {
					wait = forwarderRetryMin
				}
			}
		}
		r.cancel()
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(2*wait, forwarderRetryMax)
	}
}

// runForwarder opens the ssh-agent stream and relays on it until it ends or
// ctx is cancelled. It reports whether the stream opened, and whether the
// daemon or the worker refused it.
func (c *client) runForwarder(ctx context.Context, req *protocol.ForwardSSHAgent, r *agentRelay) (opened, refused bool) {
	open, cancel := context.WithTimeout(ctx, c.requestTimeout())
	defer cancel()
	cs, err := c.controlStream(open)
	if err != nil {
		return false, false
	}
	if !cs.features.Has(protocol.CapSSHAgent) {
		// An agtd from before forwarding.
		return false, true
	}
	s, err := c.open(open)
	if err != nil {
		return false, false
	}
	defer s.Close()
	stopWatch := context.AfterFunc(ctx, func() { s.Close() })
	defer stopWatch()
	if deadline, ok := open.Deadline(); ok {
		s.SetDeadline(deadline)
	}
	reader := bufio.NewReader(s)
	if err := protocol.WriteRequest(s, &protocol.Request{ForwardSSHAgent: req}); err != nil {
		return false, false
	}
	resp, err := protocol.ReadSSHAgentResponse(reader)
	switch {
	case err != nil || resp == nil:
		return false, false
	case resp.SSHAgentForwarding == nil:
		// The session cannot forward (started without an agent socket, or
		// by an older agtd), or the attachment is already gone.
		return false, true
	}
	s.SetDeadline(time.Time{})
	r.s = s
	r.serve(reader)
	r.closeAll()
	return true, false
}

// agentRelay answers the worker's agent requests on one ssh-agent stream.
type agentRelay struct {
	// ctx ends with the relay (closeAll) or the forwarder: it ends a
	// connection to the local agent being made, and the probe, which a
	// busy agent can hold up for as long as a request.
	ctx    context.Context
	cancel context.CancelFunc

	s     transport.Stream
	agent string
	keys  sshagent.Keys
	// hop is the agtd hop's host key, which every local agent connection is
	// bound to first, for a session on another machine; nil for a local
	// one, whose programs use the agent from this machine.
	hop ssh.Signer

	writeMu sync.Mutex

	mu sync.Mutex
	// conns holds each program connection's local side until its goroutine
	// has ended, closed or not, so they all count against
	// maxLocalAgentConns.
	conns map[uint32]*localAgentConn
	wg    sync.WaitGroup
}

// localAgentConn is the local side of one program connection on the
// session's agent socket.
type localAgentConn struct {
	id uint32
	// reqs holds the program's next request: one slot, since the worker
	// sends the next only after the reply.
	reqs chan []byte
	// done is closed when the worker closes the connection or the relay
	// ends.
	done chan struct{}
	// agent is the local agent connection, dialled on the first request;
	// guarded by agentRelay.mu, so ending the relay can close it.
	agent  net.Conn
	closed bool
	// bound is set once the agent has taken the agtd hop's binding on
	// agent; only the connection's goroutine uses it.
	bound bool
}

// newAgentRelay is a relay to the agent at the socket agent, ending with ctx.
func newAgentRelay(ctx context.Context, agent string, keys sshagent.Keys, hop ssh.Signer) *agentRelay {
	ctx, cancel := context.WithCancel(ctx)
	return &agentRelay{ctx: ctx, cancel: cancel, agent: agent, keys: keys, hop: hop, conns: map[uint32]*localAgentConn{}}
}

// serve reads the worker's frames until the stream ends.
func (r *agentRelay) serve(reader *bufio.Reader) {
	for {
		resp, err := protocol.ReadSSHAgentResponse(reader)
		if err != nil || resp == nil {
			return
		}
		switch {
		case resp.SSHAgentRequest != nil:
			m := resp.SSHAgentRequest
			if len(m.Data) == 0 || len(m.Data) > sshagent.MaxMessageLen {
				return
			}
			lc := r.conn(m.Conn)
			if lc == nil {
				_ = r.write(&protocol.Request{SSHAgentClose: &protocol.SSHAgentConn{Conn: m.Conn}})
				continue
			}
			select {
			case lc.reqs <- m.Data:
			default:
				// A second request before the reply: the worker never
				// sends one.
				return
			}
		case resp.SSHAgentClose != nil:
			r.drop(resp.SSHAgentClose.Conn)
		default:
			return
		}
	}
}

// conn is the local side of program connection id, started on its first
// request; nil when id was closed already, or when maxLocalAgentConns are
// open, counting those still winding down.
func (r *agentRelay) conn(id uint32) *localAgentConn {
	r.mu.Lock()
	defer r.mu.Unlock()
	if lc := r.conns[id]; lc != nil {
		if lc.closed {
			return nil
		}
		return lc
	}
	if len(r.conns) >= maxLocalAgentConns {
		return nil
	}
	lc := &localAgentConn{id: id, reqs: make(chan []byte, 1), done: make(chan struct{})}
	r.conns[id] = lc
	r.wg.Add(1)
	go r.serveConn(lc)
	return lc
}

// drop ends the local side of program connection id. It keeps its place
// until its goroutine has ended.
func (r *agentRelay) drop(id uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if lc := r.conns[id]; lc != nil {
		r.endLocked(lc)
	}
}

// closeAll ends every connection, and any being made, and waits for their
// goroutines.
func (r *agentRelay) closeAll() {
	r.cancel()
	r.mu.Lock()
	for _, lc := range r.conns {
		r.endLocked(lc)
	}
	r.mu.Unlock()
	r.wg.Wait()
}

func (r *agentRelay) endLocked(lc *localAgentConn) {
	if lc.closed {
		return
	}
	lc.closed = true
	close(lc.done)
	if lc.agent != nil {
		lc.agent.Close()
	}
}

// serveConn answers one program connection's requests in order.
func (r *agentRelay) serveConn(lc *localAgentConn) {
	defer r.wg.Done()
	defer func() {
		r.mu.Lock()
		if r.conns[lc.id] == lc {
			delete(r.conns, lc.id)
		}
		r.endLocked(lc)
		r.mu.Unlock()
	}()
	for {
		select {
		case <-lc.done:
			return
		case msg := <-lc.reqs:
			reply, err := r.answer(lc, msg)
			if err != nil {
				// This machine's agent is gone or did not answer: the
				// worker closes the program's connection.
				_ = r.write(&protocol.Request{SSHAgentClose: &protocol.SSHAgentConn{Conn: lc.id}})
				return
			}
			if err := r.write(&protocol.Request{SSHAgentReply: &protocol.SSHAgentMessage{Conn: lc.id, Data: reply}}); err != nil {
				return
			}
		}
	}
}

// answer applies the policy to one request and, if it passes, asks the
// local agent. What the policy refuses gets SSH_AGENT_FAILURE without
// reaching the agent; an identities answer lists only the keys allowed.
func (r *agentRelay) answer(lc *localAgentConn, msg []byte) ([]byte, error) {
	if !sshagent.Forwardable(msg) {
		return sshagent.Failure(), nil
	}
	if sshagent.Type(msg) == sshagent.MsgSignRequest {
		req, err := sshagent.ParseSignRequest(msg)
		if err != nil || !r.keys.Allows(req.Key) {
			return sshagent.Failure(), nil
		}
	}
	agent, err := r.dial(lc)
	if err != nil {
		return nil, err
	}
	if r.hop != nil && !lc.bound {
		// The connection is bound to the hop before its first request.
		// Until the agent has taken the binding (it was locked: it answers
		// nothing until unlocked; it rejected it; or it does not know the
		// extension), it is tried again before each request and after each
		// listing (relist), and the program's own bindings are refused: one
		// would become the first hop, and let the devbox use a key limited
		// to another host.
		// OpenSSH's agent keeps such keys to itself once a binding has been
		// tried, and an agent without the extension has no limits to keep.
		if lc.bound, err = r.bind(agent); err != nil {
			return nil, err
		}
		if name, ok := sshagent.ExtensionName(msg); !lc.bound && ok && name == sshagent.ExtSessionBind {
			return sshagent.Failure(), nil
		}
	}
	reply, err := askAgent(agent, msg)
	if err != nil {
		return nil, err
	}
	if sshagent.Type(msg) == sshagent.MsgRequestIdentities {
		if reply, err = r.relist(agent, &lc.bound, reply); err != nil {
			return nil, err
		}
		reply = r.keys.FilterIdentities(reply)
	}
	return reply, nil
}

// askAgent sends msg to the local agent and reads its reply, waiting as
// long as a request may.
func askAgent(agent net.Conn, msg []byte) ([]byte, error) {
	agent.SetDeadline(time.Now().Add(localAgentReplyTimeout))
	if err := sshagent.WriteMessage(agent, msg); err != nil {
		return nil, err
	}
	return sshagent.ReadMessage(agent)
}

// relist checks an identities answer the agent gave on a connection the
// hop's binding has yet to hold on (bound false). The agent may have been
// unlocked after the binding was tried, and then listed keys as for local
// use, those limited to other hosts too. So the binding is tried again, and
// if it holds now, the agent is asked again, as the hop sees it.
func (r *agentRelay) relist(agent net.Conn, bound *bool, reply []byte) ([]byte, error) {
	if r.hop == nil || *bound {
		return reply, nil
	}
	var err error
	if *bound, err = r.bind(agent); err != nil || !*bound {
		return reply, err
	}
	return askAgent(agent, []byte{sshagent.MsgRequestIdentities})
}

// dial connects lc to the local agent, once.
func (r *agentRelay) dial(lc *localAgentConn) (net.Conn, error) {
	r.mu.Lock()
	agent := lc.agent
	r.mu.Unlock()
	if agent != nil {
		return agent, nil
	}
	agent, err := r.connect()
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if lc.closed {
		agent.Close()
		return nil, net.ErrClosed
	}
	// lc's from now on, before answer binds it to the hop: ending lc (drop,
	// closeAll) closes it, which ends a binding still waiting on the agent.
	lc.agent = agent
	return agent, nil
}

// connect opens a connection to the local agent.
func (r *agentRelay) connect() (net.Conn, error) {
	d := net.Dialer{Timeout: localAgentDialTimeout}
	return d.DialContext(r.ctx, "unix", r.agent)
}

// bind binds agent to the agtd hop (bindHop). The binding waits its turn as
// a request does, since the agent may be asking the user to confirm another
// request; ending the relay ends the wait.
func (r *agentRelay) bind(agent net.Conn) (bool, error) {
	stop := context.AfterFunc(r.ctx, func() { agent.Close() })
	bound, err := bindHop(agent, r.hop)
	if !stop() && err == nil {
		err = net.ErrClosed
	}
	return bound, err
}

// lends reports whether the local agent answers, and holds a key the policy
// offers, as the session would be offered it: through the hop, so keys
// limited to other hosts do not count. A busy agent gets a request's time to
// answer; ending the relay ends the wait.
func (r *agentRelay) lends() bool {
	agent, err := r.connect()
	if err != nil {
		return false
	}
	defer agent.Close()
	stop := context.AfterFunc(r.ctx, func() { agent.Close() })
	defer stop()
	bound := false
	if r.hop != nil {
		if bound, err = bindHop(agent, r.hop); err != nil {
			return false
		}
	}
	reply, err := askAgent(agent, []byte{sshagent.MsgRequestIdentities})
	if err != nil {
		return false
	}
	if reply, err = r.relist(agent, &bound, reply); err != nil {
		return false
	}
	ids, err := sshagent.ParseIdentities(r.keys.FilterIdentities(reply))
	return err == nil && len(ids) > 0
}

// newHopSigner makes the host key a forwarder binds its local agent
// connections to. It is new for each forwarder, so no destination
// constraint can name it.
func newHopSigner() (ssh.Signer, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return ssh.NewSignerFromKey(key)
}

// bindHop binds a local agent connection to the agtd hop as a forwarding
// session (session-bind@openssh.com with forwarding set), as ssh -A binds
// the connections it forwards, and reports whether the agent took it.
// OpenSSH's agent then applies destination constraints (ssh-add -h) to the
// requests that follow: a key limited to other hosts is left out of the
// identities answer and refused for signing, since no constraint names the
// hop. An agent answers with a failure when it does not know the extension,
// rejects the binding, or is locked; the caller decides what that means.
func bindHop(agent net.Conn, hop ssh.Signer) (bool, error) {
	sessionID := make([]byte, 32)
	if _, err := rand.Read(sessionID); err != nil {
		return false, err
	}
	sig, err := hop.Sign(rand.Reader, sessionID)
	if err != nil {
		return false, err
	}
	bind := sshagent.SessionBind{HostKey: hop.PublicKey().Marshal(), SessionID: sessionID, Signature: ssh.Marshal(sig), Forwarding: true}
	agent.SetDeadline(time.Now().Add(localAgentReplyTimeout))
	defer agent.SetDeadline(time.Time{})
	if err := sshagent.WriteMessage(agent, bind.Request()); err != nil {
		return false, err
	}
	reply, err := sshagent.ReadMessage(agent)
	if err != nil {
		return false, err
	}
	return sshagent.Type(reply) == sshagent.MsgSuccess, nil
}

// write sends one frame to the worker. A stream that does not take it in
// time is closed, which ends the relay.
func (r *agentRelay) write(req *protocol.Request) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	r.s.SetWriteDeadline(time.Now().Add(forwarderWriteTimeout))
	if err := protocol.WriteRequest(r.s, req); err != nil {
		r.s.Close()
		return err
	}
	return nil
}
