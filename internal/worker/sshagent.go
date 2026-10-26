package worker

// SSH agent forwarding (protocol.CapSSHAgent; docs/designs/ssh-agent-forwarding.md).
//
// A session started with an agent socket (Args.SSHAgentSocket) has one at
// paths.SSHAgentSocketPath(uid), bound before the agent starts, and the
// agent's SSH_AUTH_SOCK names it for its whole life, across handoffs. The
// socket belongs to the session; what answers on it does not. Each
// connection a program opens is routed, on its first message, to:
//
//  1. the forwarder of the attachment that owns the session's size (size.go):
//     an ssh-agent stream that client opened, which relays the request to
//     the client's own agent;
//  2. else, while the owner's id is reserved for its reattach and the owner
//     was forwarding, nowhere yet: the connection waits, at most
//     sizeOwnerGrace, for the reattached owner's forwarder;
//  3. else the agent the worker inherited (SSH_AUTH_SOCK in its own
//     environment), relayed byte for byte, as sessions got before;
//  4. else no agent: an empty identities answer, and SSH_AGENT_FAILURE to
//     anything else.
//
// A connection keeps its route until it closes, even when ownership moves,
// since the client's agent keeps state per connection (session bindings).
// The worker refuses what a forwarded agent never takes (sshagent.Forwardable)
// itself, to spare the round trip; the client enforces its own policy too.
//
// Bounds: maxAgentConns program connections at once (more are closed as
// they arrive); messages of sshagent.MaxMessageLen; one request in flight
// per connection, since the worker reads the next only after writing the
// reply; one forwarder per attachment; a reply waited for agentReplyTimeout;
// a write to a forwarder given agentWriteTimeout, after which the forwarder
// is dropped. A forwarder that stops reading stalls only the connections
// routed to it, never the PTY, the attachments or other forwarders.
//
// Goroutines: the accept loop, which ends when the listener closes (at the
// session's end or a handoff); one per program connection (at most
// maxAgentConns), ending with its connection, which stop closes; and for a
// connection relayed to the inherited agent one more, copying the other
// direction. A forwarder is served by a session-socket connection handler
// (serveForwarder), which reads the client's replies until the stream ends,
// as it does with its attachment.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"agtd.sh/internal/db"
	"agtd.sh/internal/protocol"
	"agtd.sh/internal/session"
	"agtd.sh/internal/sshagent"
	"agtd.sh/internal/transport"
)

const (
	// maxAgentConns bounds the program connections open on a session's
	// agent socket.
	maxAgentConns = 16
	// agentDialTimeout bounds connecting to the inherited agent.
	agentDialTimeout = 2 * time.Second
)

var (
	// agentReplyTimeout bounds the wait for a client's reply, leaving room
	// for its agent to ask the user to confirm (a key's touch, 1Password)
	// over a slow link. A variable for tests.
	agentReplyTimeout = 90 * time.Second
	// agentWriteTimeout bounds a write to a forwarder; one that takes
	// longer is dropped.
	agentWriteTimeout = 10 * time.Second
)

var (
	errAgentStopping   = errors.New("the agent socket is closing")
	errForwarderGone   = errors.New("the forwarder ended")
	errClientClosed    = errors.New("the client closed the connection")
	errClientTimeout   = errors.New("the client did not answer in time")
	errForwarderExists = errors.New("this attachment already forwards an SSH agent")
)

// agentServer serves a session's SSH agent socket.
type agentServer struct {
	rt   *runtime
	path string
	// ownSocket is the socket file this worker bound, so it is removed only
	// while the path is still its own.
	ownSocket os.FileInfo
	// inherited is the agent the worker started with, "" for none.
	inherited string

	// listener and acceptDone belong to the main goroutine, which stops
	// and restarts them around a handoff.
	listener   *transport.UnixListener
	acceptDone chan struct{}

	mu    sync.Mutex
	conns map[*agentConn]struct{}
	// quit is closed by stop, ending every wait for a route or a reply.
	quit chan struct{}
	// handlers counts the program connections' goroutines.
	handlers sync.WaitGroup

	nextID atomic.Uint32
}

// agentConn is one program connection to the agent socket.
type agentConn struct {
	id   uint32
	conn transport.Stream
	// up is the inherited agent's connection, for a connection relayed
	// there; guarded by agentServer.mu.
	up net.Conn
}

// listenAgentSocket binds a session's agent socket at path.
func listenAgentSocket(path string) (*transport.UnixListener, os.FileInfo, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("failed to remove %s: %w", path, err)
	}
	l, err := transport.ListenUnix(path, transport.UnixOptions{KeepSocketOnClose: true})
	if err != nil {
		return nil, nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		l.Close()
		return nil, nil, fmt.Errorf("failed to stat %s: %w", path, err)
	}
	return l, info, nil
}

func newAgentServer(rt *runtime, path string, ownSocket os.FileInfo, inherited string, l *transport.UnixListener) *agentServer {
	return &agentServer{rt: rt, path: path, ownSocket: ownSocket, inherited: inherited, listener: l, conns: map[*agentConn]struct{}{}}
}

// start serves the socket's listener.
func (a *agentServer) start() {
	if a.listener == nil {
		return
	}
	a.mu.Lock()
	a.quit = make(chan struct{})
	quit := a.quit
	a.mu.Unlock()
	done := make(chan struct{})
	a.acceptDone = done
	l := a.listener
	go func() {
		defer close(done)
		a.accept(l, quit)
	}()
}

// stop closes the listener and every program connection, and waits for
// their goroutines.
func (a *agentServer) stop() {
	if a.listener == nil {
		return
	}
	a.listener.Close()
	<-a.acceptDone
	a.mu.Lock()
	close(a.quit)
	for c := range a.conns {
		c.conn.Close()
		if c.up != nil {
			c.up.Close()
		}
	}
	a.mu.Unlock()
	a.handlers.Wait()
}

// relisten serves the socket again from the duplicate kept for an exec that
// did not happen, binding the path again if that fails.
func (a *agentServer) relisten(f *os.File) {
	l, err := transport.UnixListenerFromFile(f, a.path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "session worker: %v; binding the SSH agent socket again\n", err)
		var info os.FileInfo
		if l, info, err = listenAgentSocket(a.path); err != nil {
			fmt.Fprintf(os.Stderr, "session worker: the SSH agent socket is gone: %v\n", err)
			a.listener = nil
			return
		}
		a.ownSocket = info
	}
	a.listener = l
}

// removeOwnSocket removes the socket file while it is still this worker's.
func (a *agentServer) removeOwnSocket() {
	if cur, err := os.Stat(a.path); err == nil && os.SameFile(cur, a.ownSocket) {
		_ = os.Remove(a.path)
	}
}

func (a *agentServer) accept(l *transport.UnixListener, quit chan struct{}) {
	backoff := time.Duration(0)
	for {
		s, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// Out of descriptors, say: retry rather than give the socket up.
			backoff = min(max(2*backoff, 5*time.Millisecond), time.Second)
			fmt.Fprintf(os.Stderr, "session worker: accept on the SSH agent socket: %v; retrying in %v\n", err, backoff)
			time.Sleep(backoff)
			continue
		}
		backoff = 0
		c := a.track(s, quit)
		if c == nil {
			// Over the bound: the program carries on without an agent.
			s.Close()
			continue
		}
		go a.serve(c, quit)
	}
}

// track registers a program connection, unless maxAgentConns are open or
// the socket is closing.
func (a *agentServer) track(s transport.Stream, quit chan struct{}) *agentConn {
	a.mu.Lock()
	defer a.mu.Unlock()
	select {
	case <-quit:
		return nil
	default:
	}
	if len(a.conns) >= maxAgentConns {
		return nil
	}
	c := &agentConn{id: a.nextID.Add(1), conn: s}
	a.conns[c] = struct{}{}
	a.handlers.Add(1)
	return c
}

func (a *agentServer) untrack(c *agentConn) {
	a.mu.Lock()
	delete(a.conns, c)
	up := c.up
	a.mu.Unlock()
	c.conn.Close()
	if up != nil {
		up.Close()
	}
	a.handlers.Done()
}

// serve routes a program connection on its first message and serves it
// there until it closes.
func (a *agentServer) serve(c *agentConn, quit <-chan struct{}) {
	defer a.untrack(c)
	first, err := sshagent.ReadMessage(c.conn)
	if err != nil {
		return
	}
	for {
		f, err := a.route(quit)
		if err != nil {
			return
		}
		if f == nil {
			break
		}
		if a.serveForwarded(c, f, first, quit) {
			return
		}
		// The forwarder ended after it was picked; route again.
	}
	if a.inherited != "" && a.relayInherited(c, first) {
		return
	}
	a.serveUnavailable(c, first)
}

// route picks a new connection's forwarder, waiting while the session's
// size owner is a forwarding owner yet to reattach. A nil forwarder means
// none: the inherited agent, or no agent.
func (a *agentServer) route(quit <-chan struct{}) (*forwarder, error) {
	for {
		var f *forwarder
		var until time.Time
		var wake <-chan struct{}
		if err := a.rt.owner.do(func(s *ownerState) error {
			f, until, wake = s.agentRoute(time.Now())
			return nil
		}); err != nil {
			return nil, err
		}
		if f != nil || until.IsZero() {
			return f, nil
		}
		timer := time.NewTimer(time.Until(until))
		select {
		case <-wake:
		case <-timer.C:
		case <-quit:
			timer.Stop()
			return nil, errAgentStopping
		}
		timer.Stop()
	}
}

// serveForwarded serves a connection routed to forwarder f, whose first
// request is msg, until it closes. It reports false, having served nothing,
// if f had ended by then.
func (a *agentServer) serveForwarded(c *agentConn, f *forwarder, msg []byte, quit <-chan struct{}) bool {
	fc := f.add(c)
	if fc == nil {
		return false
	}
	defer f.remove(fc)
	for {
		reply, keep := a.exchange(f, fc, msg, quit)
		if reply != nil {
			if err := sshagent.WriteMessage(c.conn, reply); err != nil {
				return true
			}
		}
		if !keep {
			return true
		}
		var err error
		if msg, err = sshagent.ReadMessage(c.conn); err != nil {
			return true
		}
	}
}

// exchange passes one request from a program to the client, and returns the
// reply to write back and whether the connection carries on. A request a
// forwarded agent never takes is refused here. When the client cannot
// answer (the forwarder ended, it closed this connection, it took too long)
// the program gets SSH_AGENT_FAILURE and its connection is closed, since
// the client's agent connection, and any session binding on it, is gone.
func (a *agentServer) exchange(f *forwarder, fc *forwardedConn, msg []byte, quit <-chan struct{}) (reply []byte, keep bool) {
	if !sshagent.Forwardable(msg) {
		a.rt.recorder.event(db.NewEvent{Kind: session.EventSSHAgentRefused,
			Summary: fmt.Sprintf("refused a request to %s on the agent %s forwards: a forwarded agent only lists keys and signs", sshagent.Describe(msg), describeAgentClient(f.client))})
		return sshagent.Failure(), true
	}
	reply, err := f.request(fc, msg, quit)
	switch {
	case errors.Is(err, errAgentStopping):
		return nil, false
	case err != nil:
		return sshagent.Failure(), false
	}
	a.note(f, fc, msg, reply)
	return reply, true
}

// note records what a forwarded request did: the host a connection is bound
// to, and a signature given or refused, as session events.
func (a *agentServer) note(f *forwarder, fc *forwardedConn, msg, reply []byte) {
	switch sshagent.Type(msg) {
	case sshagent.MsgExtension:
		if b, err := sshagent.ParseSessionBind(msg); err == nil {
			fc.boundHost = b.HostKey
		}
	case sshagent.MsgSignRequest:
		req, err := sshagent.ParseSignRequest(msg)
		if err != nil {
			return
		}
		what, key := describeSigned(req.Data, fc.boundHost), sshagent.Fingerprint(req.Key)
		switch sshagent.Type(reply) {
		case sshagent.MsgSignResponse:
			a.rt.recorder.event(db.NewEvent{Kind: session.EventSSHAgentSigned,
				Summary: fmt.Sprintf("%s signed %s using key %s", describeAgentClient(f.client), what, key)})
		case sshagent.MsgFailure:
			a.rt.recorder.event(db.NewEvent{Kind: session.EventSSHAgentRefused,
				Summary: fmt.Sprintf("%s refused to sign %s using key %s", describeAgentClient(f.client), what, key)})
		}
	}
}

// describeAgentClient names a forwarder's client in an event.
func describeAgentClient(client string) string {
	if client == "" || client == "local" {
		return "a local client"
	}
	return client
}

// describeSigned says what signed data was for: a login, as whom and to
// where (the host key the connection's session binding names, from this
// machine's known_hosts when it can be), or an SSHSIG namespace.
func describeSigned(data, boundHost []byte) string {
	p := sshagent.ParsePurpose(data)
	switch {
	case p.Login && boundHost != nil:
		return fmt.Sprintf("a login as %s to %s", p.User, hostName(boundHost))
	case p.Login:
		return "a login as " + p.User
	case p.Namespace != "":
		return "data in the namespace " + p.Namespace
	}
	return "data"
}

// hostName names a host key: from the user's and the system's known_hosts,
// else by its fingerprint.
func hostName(key []byte) string {
	var files []string
	if home, err := os.UserHomeDir(); err == nil {
		files = append(files, filepath.Join(home, ".ssh", "known_hosts"))
	}
	files = append(files, "/etc/ssh/ssh_known_hosts")
	if name := sshagent.KnownHostName(key, files...); name != "" {
		return name
	}
	return "an unknown host (key " + sshagent.Fingerprint(key) + ")"
}

// relayInherited relays a connection to the agent the worker inherited, byte
// for byte and unfiltered: the session's programs could reach that agent
// before, so it gives them nothing new. It reports false, having written
// nothing to the program, when that agent cannot be reached.
func (a *agentServer) relayInherited(c *agentConn, first []byte) bool {
	up, err := net.DialTimeout("unix", a.inherited, agentDialTimeout)
	if err != nil {
		return false
	}
	a.mu.Lock()
	c.up = up
	a.mu.Unlock()
	if err := sshagent.WriteMessage(up, first); err != nil {
		return false
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(up, c.conn)
		if uc, ok := up.(*net.UnixConn); ok {
			_ = uc.CloseWrite()
		}
	}()
	_, _ = io.Copy(c.conn, up)
	c.conn.Close()
	<-done
	return true
}

// serveUnavailable answers a connection that has no agent behind it: no
// keys, and SSH_AGENT_FAILURE to anything else, as an agent with no keys
// would. ssh then carries on with its own key files.
func (a *agentServer) serveUnavailable(c *agentConn, msg []byte) {
	a.noteUnavailable()
	for {
		reply := sshagent.Failure()
		if sshagent.Type(msg) == sshagent.MsgRequestIdentities {
			reply = sshagent.NoIdentities()
		}
		if err := sshagent.WriteMessage(c.conn, reply); err != nil {
			return
		}
		var err error
		if msg, err = sshagent.ReadMessage(c.conn); err != nil {
			return
		}
	}
}

// noteUnavailable records that a program found no agent, once per gap in
// forwarding and only in a session that has been forwarded to: one that
// never asked for forwarding has nothing missing.
func (a *agentServer) noteUnavailable() {
	record := false
	_ = a.rt.owner.do(func(s *ownerState) error {
		if s.agentForwarded && !s.agentGap {
			s.agentGap, record = true, true
		}
		return nil
	})
	if record {
		a.rt.recorder.event(db.NewEvent{Kind: session.EventSSHAgentUnavailable,
			Summary: "a program asked for SSH keys and got none: no client that owns the session's size forwards an agent"})
	}
}

// forwarder is an attachment's SSH agent forwarder: the worker's end of an
// ssh-agent stream. The program connections routed to it go out as
// SSHAgentRequest frames tagged with their number, and the client's
// SSHAgentReply frames go back to whichever connection waits for them, one
// slot each since a connection has one request in flight.
type forwarder struct {
	conn     transport.Stream
	attachID string
	// client names the client (ForwardSSHAgent.Client, set by the daemon).
	client string

	writeMu sync.Mutex

	mu     sync.Mutex
	conns  map[uint32]*forwardedConn
	closed bool
	done   chan struct{}
}

// forwardedConn is a program connection routed to a forwarder.
type forwardedConn struct {
	id    uint32
	agent *agentConn
	reply chan []byte
	// gone is closed when the client closes the connection
	// (SSHAgentClose).
	gone chan struct{}
	// inFlight and clientClosed are guarded by forwarder.mu. A connection
	// with a request in flight is closed by its own goroutine once it has
	// answered the program; an idle one by whoever ends it.
	inFlight     bool
	clientClosed bool
	// boundHost is the host key of the connection's session binding; only
	// the connection's goroutine touches it.
	boundHost []byte
}

func newForwarder(conn transport.Stream, attachID, client string) *forwarder {
	return &forwarder{conn: conn, attachID: attachID, client: client, conns: map[uint32]*forwardedConn{}, done: make(chan struct{})}
}

// ended reports whether f has ended, though its attachment may not have
// forgotten it yet.
func (f *forwarder) ended() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// add routes a program connection to f, or returns nil if f has ended.
func (f *forwarder) add(c *agentConn) *forwardedConn {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	fc := &forwardedConn{id: c.id, agent: c, reply: make(chan []byte, 1), gone: make(chan struct{})}
	f.conns[c.id] = fc
	return fc
}

// remove forgets a program connection that closed, telling the client so it
// closes its agent connection for it.
func (f *forwarder) remove(fc *forwardedConn) {
	f.mu.Lock()
	delete(f.conns, fc.id)
	tell := !f.closed && !fc.clientClosed
	f.mu.Unlock()
	if tell {
		_ = f.write(&protocol.Response{SSHAgentClose: &protocol.SSHAgentConn{Conn: fc.id}})
	}
}

// request sends msg for connection fc and waits for the client's reply.
func (f *forwarder) request(fc *forwardedConn, msg []byte, quit <-chan struct{}) ([]byte, error) {
	f.mu.Lock()
	if f.closed || fc.clientClosed {
		f.mu.Unlock()
		return nil, errForwarderGone
	}
	fc.inFlight = true
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		fc.inFlight = false
		f.mu.Unlock()
	}()
	if err := f.write(&protocol.Response{SSHAgentRequest: &protocol.SSHAgentMessage{Conn: fc.id, Data: msg}}); err != nil {
		return nil, err
	}
	timer := time.NewTimer(agentReplyTimeout)
	defer timer.Stop()
	select {
	case reply := <-fc.reply:
		return reply, nil
	case <-fc.gone:
		return nil, errClientClosed
	case <-f.done:
		return nil, errForwarderGone
	case <-quit:
		return nil, errAgentStopping
	case <-timer.C:
		f.mu.Lock()
		fc.clientClosed = true
		f.mu.Unlock()
		_ = f.write(&protocol.Response{SSHAgentClose: &protocol.SSHAgentConn{Conn: fc.id}})
		return nil, errClientTimeout
	}
}

// write sends one frame to the client. A client that does not take it
// within agentWriteTimeout loses the forwarder: it is closed, and the
// requests routed to it fail.
func (f *forwarder) write(resp *protocol.Response) error {
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	f.conn.SetWriteDeadline(time.Now().Add(agentWriteTimeout))
	if err := protocol.WriteResponse(f.conn, resp); err != nil {
		f.close()
		return err
	}
	return nil
}

// close ends the forwarder: its stream is closed, requests in flight fail,
// and idle program connections routed to it are closed.
func (f *forwarder) close() {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.closed = true
	close(f.done)
	for _, fc := range f.conns {
		if !fc.inFlight {
			fc.agent.conn.Close()
		}
	}
	f.mu.Unlock()
	f.conn.Close()
}

// readLoop reads the client's frames until the stream ends or carries
// something else.
func (f *forwarder) readLoop(r *bufio.Reader) {
	for {
		req, err := protocol.ReadRequest(r)
		if err != nil || req == nil {
			return
		}
		switch {
		case req.SSHAgentReply != nil:
			m := req.SSHAgentReply
			if len(m.Data) == 0 || len(m.Data) > sshagent.MaxMessageLen {
				return
			}
			f.mu.Lock()
			fc := f.conns[m.Conn]
			f.mu.Unlock()
			if fc != nil {
				select {
				case fc.reply <- m.Data:
				default:
					// A reply nobody asked for: drop it.
				}
			}
		case req.SSHAgentClose != nil:
			f.mu.Lock()
			if fc := f.conns[req.SSHAgentClose.Conn]; fc != nil && !fc.clientClosed {
				fc.clientClosed = true
				close(fc.gone)
				if !fc.inFlight {
					fc.agent.conn.Close()
				}
			}
			f.mu.Unlock()
		default:
			return
		}
	}
}

// serveForwarder serves an ssh-agent stream: once the attachment it names is
// found live in this incarnation and has no forwarder yet, it is the
// attachment's forwarder until the stream or the attachment ends.
func (rt *runtime) serveForwarder(conn transport.Stream, reader *bufio.Reader, req *protocol.ForwardSSHAgent) error {
	refuse := func(err error) error {
		_ = protocol.WriteResponse(conn, protocol.ErrorResponsef("%v", err))
		return err
	}
	if rt.agent == nil {
		return refuse(fmt.Errorf("session `%s` has no SSH agent socket: it was started without one ([ssh_agent] session_socket = false, or by an older agtd)", rt.sessionID))
	}
	if req.ExpectUID != "" && req.ExpectUID != rt.uid {
		return refuse(errors.New(protocol.SessionReplacedMessage(rt.sessionID)))
	}
	f := newForwarder(conn, req.Attachment, req.Client)
	// Registered and acknowledged under writeMu, so no request reaches the
	// client before its acknowledgement does.
	f.writeMu.Lock()
	if err := rt.owner.do(func(s *ownerState) error { return s.addForwarder(req.Attachment, f) }); err != nil {
		f.writeMu.Unlock()
		return refuse(err)
	}
	defer rt.owner.post(func(s *ownerState) { s.removeForwarder(req.Attachment, f) })
	defer f.close()
	conn.SetWriteDeadline(time.Now().Add(agentWriteTimeout))
	err := protocol.WriteResponse(conn, &protocol.Response{SSHAgentForwarding: &protocol.SSHAgentForwarding{}})
	f.writeMu.Unlock()
	if err != nil {
		return err
	}
	f.readLoop(reader)
	return nil
}

// agentRoute is where a new connection to the agent socket goes: the size
// owner's forwarder; else, while the owner's id is reserved for the reattach
// of an owner that was forwarding, nowhere yet, until the time returned;
// else neither (the inherited agent, or none). wake is closed when the
// answer may have changed.
func (s *ownerState) agentRoute(now time.Time) (f *forwarder, until time.Time, wake <-chan struct{}) {
	if s.agentWake == nil {
		s.agentWake = make(chan struct{})
	}
	if a := s.attachments[s.sizeOwner]; a != nil && a.forwarder != nil && !a.forwarder.ended() {
		s.agentGap = false
		return a.forwarder, time.Time{}, s.agentWake
	}
	if s.sizeOwner != "" && s.sizeOwner == s.agentWaitFor && now.Before(s.agentWaitUntil) {
		return nil, s.agentWaitUntil, s.agentWake
	}
	return nil, time.Time{}, s.agentWake
}

// agentChanged wakes connections waiting for a route.
func (s *ownerState) agentChanged() {
	if s.agentWake != nil {
		close(s.agentWake)
	}
	s.agentWake = make(chan struct{})
}

// addForwarder makes f the forwarder of a live attachment.
func (s *ownerState) addForwarder(attachID string, f *forwarder) error {
	a, ok := s.attachments[attachID]
	switch {
	case !ok:
		return fmt.Errorf("attachment `%s` of session `%s` not found", attachID, s.sessionID)
	case a.forwarder != nil:
		return errForwarderExists
	}
	a.forwarder, a.forwarded = f, true
	s.agentForwarded = true
	s.agentChanged()
	return nil
}

// removeForwarder forgets an attachment's forwarder once it has ended.
func (s *ownerState) removeForwarder(attachID string, f *forwarder) {
	if a, ok := s.attachments[attachID]; ok && a.forwarder == f {
		a.forwarder = nil
		s.agentChanged()
	}
}

// agentLeft runs when attachment a, with id attachID, has gone. Its
// forwarder ends with it. If it owned the size and its id is now reserved
// for its reattach (size.go), and it was forwarding, new agent connections
// wait for the reattached owner's forwarder for as long as the reservation.
func (s *ownerState) agentLeft(attachID string, a *ownerAttachment) {
	if a.forwarder != nil {
		a.forwarder.close()
		a.forwarder = nil
	}
	if a.forwarded && s.sizeOwner == attachID {
		s.agentWaitFor, s.agentWaitUntil = attachID, time.Now().Add(sizeOwnerGrace)
	}
	s.agentChanged()
}

// agentHandoffWaitFor is the reserved owner whose forwarder agent
// connections are waiting for, to carry across a handoff; "" for none.
func (s *ownerState) agentHandoffWaitFor() string {
	if s.agentWaitFor != "" && s.agentWaitFor == s.sizeOwner && time.Now().Before(s.agentWaitUntil) {
		return s.agentWaitFor
	}
	return ""
}

// withEnv is env with key set to value, replacing any value it had.
func withEnv(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, key+"=") {
			out = append(out, kv)
		}
	}
	return append(out, key+"="+value)
}
