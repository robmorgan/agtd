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
// Goroutines: an agentForwarder owns one goroutine that opens the stream and
// then reads the worker's frames until the stream ends, and one per program
// connection the worker numbers (at most maxLocalAgentConns), each holding
// its own connection to the local agent, since an agent keeps state per
// connection. stop closes the stream, which ends the reader, and waits for
// all of them. Writes to the stream are serialised by writeMu.

import (
	"bufio"
	"context"
	"net"
	"sync"
	"time"

	"agtd.sh/internal/config"
	"agtd.sh/internal/protocol"
	"agtd.sh/internal/sshagent"
	"agtd.sh/internal/transport"
)

const (
	// maxLocalAgentConns bounds the local agent connections a forwarder
	// holds; a program connection over it is refused (SSHAgentClose).
	maxLocalAgentConns = 16
	// localAgentDialTimeout bounds connecting to the local agent.
	localAgentDialTimeout = 2 * time.Second
	// localAgentReplyTimeout bounds the wait for the local agent, which may
	// be asking the user to confirm a signature; the worker gives up a
	// little earlier.
	localAgentReplyTimeout = 90 * time.Second
	// forwarderWriteTimeout bounds a write to the worker; a stream that
	// takes longer is closed.
	forwarderWriteTimeout = 10 * time.Second
)

// agentForwarder lends this machine's agent to one attachment.
type agentForwarder struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// forwardAgent starts lending this machine's agent to an attachment, when
// [ssh_agent] forward is set, there is an agent to lend, and the daemon
// forwards. It returns at once, nil when there is nothing to do: the stream
// is opened in the background, so attaching never waits for it, and a
// session that cannot forward simply goes without.
func (c *client) forwardAgent(id string, attached *protocol.Attached) *agentForwarder {
	cfg, err := config.Load(c.paths.Config)
	if err != nil || !cfg.SSHAgent.Forward {
		return nil
	}
	agent := cfg.SSHAgent.AgentSocket()
	if agent == "" {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	f := &agentForwarder{cancel: cancel, done: make(chan struct{})}
	r := &agentRelay{agent: agent, keys: sshagent.NewKeys(cfg.SSHAgent.Keys), conns: map[uint32]*localAgentConn{}}
	req := &protocol.ForwardSSHAgent{SessionID: id, ExpectUID: attached.SessionUID, Attachment: attached.AttachID}
	go func() {
		defer close(f.done)
		c.runForwarder(ctx, req, r)
	}()
	return f
}

// stop ends the forwarder and waits for its goroutines. Safe on nil.
func (f *agentForwarder) stop() {
	if f == nil {
		return
	}
	f.cancel()
	<-f.done
}

// runForwarder opens the ssh-agent stream and relays on it until it ends or
// ctx is cancelled.
func (c *client) runForwarder(ctx context.Context, req *protocol.ForwardSSHAgent, r *agentRelay) {
	open, cancel := context.WithTimeout(ctx, c.requestTimeout())
	defer cancel()
	cs, err := c.controlStream(open)
	if err != nil || !cs.features.Has(protocol.CapSSHAgent) {
		return
	}
	s, err := c.open(open)
	if err != nil {
		return
	}
	defer s.Close()
	stopWatch := context.AfterFunc(ctx, func() { s.Close() })
	defer stopWatch()
	if deadline, ok := open.Deadline(); ok {
		s.SetDeadline(deadline)
	}
	reader := bufio.NewReader(s)
	if err := protocol.WriteRequest(s, &protocol.Request{ForwardSSHAgent: req}); err != nil {
		return
	}
	resp, err := protocol.ReadResponse(reader)
	if err != nil || resp == nil || resp.SSHAgentForwarding == nil {
		// The session cannot forward (started without an agent socket, or
		// by an older agtd), or the attachment is already gone.
		return
	}
	s.SetDeadline(time.Time{})
	r.s = s
	r.serve(reader)
	r.closeAll()
}

// agentRelay answers the worker's agent requests on one ssh-agent stream.
type agentRelay struct {
	s     transport.Stream
	agent string
	keys  sshagent.Keys

	writeMu sync.Mutex

	mu    sync.Mutex
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
}

// serve reads the worker's frames until the stream ends.
func (r *agentRelay) serve(reader *bufio.Reader) {
	for {
		resp, err := protocol.ReadResponse(reader)
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
// request; nil when maxLocalAgentConns are open.
func (r *agentRelay) conn(id uint32) *localAgentConn {
	r.mu.Lock()
	defer r.mu.Unlock()
	if lc := r.conns[id]; lc != nil {
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

// drop ends the local side of program connection id.
func (r *agentRelay) drop(id uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if lc := r.conns[id]; lc != nil {
		delete(r.conns, id)
		r.endLocked(lc)
	}
}

// closeAll ends every connection and waits for their goroutines.
func (r *agentRelay) closeAll() {
	r.mu.Lock()
	for id, lc := range r.conns {
		delete(r.conns, id)
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
	agent.SetDeadline(time.Now().Add(localAgentReplyTimeout))
	if err := sshagent.WriteMessage(agent, msg); err != nil {
		return nil, err
	}
	reply, err := sshagent.ReadMessage(agent)
	if err != nil {
		return nil, err
	}
	if sshagent.Type(msg) == sshagent.MsgRequestIdentities {
		reply = r.keys.FilterIdentities(reply)
	}
	return reply, nil
}

// dial connects lc to the local agent, once.
func (r *agentRelay) dial(lc *localAgentConn) (net.Conn, error) {
	r.mu.Lock()
	agent := lc.agent
	r.mu.Unlock()
	if agent != nil {
		return agent, nil
	}
	agent, err := net.DialTimeout("unix", r.agent, localAgentDialTimeout)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if lc.closed {
		agent.Close()
		return nil, net.ErrClosed
	}
	lc.agent = agent
	return agent, nil
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
