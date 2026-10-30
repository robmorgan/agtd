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
//     was forwarding, or while the owner's forwarder has lately ended
//     (whether or not it owned the size then) and agt opens another, nowhere
//     yet: the connection waits, at most sizeOwnerGrace, for the owner's
//     next forwarder, and is dropped if the program gave up meanwhile;
//  3. else the agent the worker inherited (SSH_AUTH_SOCK in its own
//     environment, unless that is another session's socket: inheritedAgent),
//     relayed byte for byte, as sessions got before;
//  4. else no agent: an empty identities answer, and SSH_AGENT_FAILURE to
//     anything else.
//
// A connection keeps its route until it closes, even when ownership moves,
// since the client's agent keeps state per connection (session bindings).
// The worker refuses what a forwarded agent never takes (sshagent.Forwardable)
// itself, to spare the round trip; the client enforces its own policy too.
//
// Bounds: maxAgentConns program connections at once (more are closed as
// they arrive, before a goroutine starts for them, and logged);
// agentIdleTimeout for each request, the first one too, except on a
// connection relayed to the inherited agent after its first; messages of
// sshagent.MaxMessageLen, and
// frames from a forwarder of protocol.MaxSSHAgentFramePayload; one request
// in flight per connection, since the worker reads the next only after
// writing the reply; one forwarder per attachment; a client's reply waited
// for agentReplyTimeout; a write to a forwarder given agentWriteTimeout,
// after which the forwarder is dropped, and a reply to a program the same,
// after which its connection is closed. A connection relayed to the
// inherited agent has none of these past its first message: it is relayed
// byte for byte, as before. A forwarder that stops reading stalls only the
// connections routed to it, never the PTY, the attachments or other
// forwarders. Refusals are paced (record): one like another in its quiet
// time, which starts at agentEventQuiet and doubles while they keep coming,
// is held back and counted, and so is any once maxAgentEventKeys different
// ones are held, so a program looping on a refused request writes about
// ten events in its first hour and one an hour after, not one a request.
// Every signature is recorded.
//
// Goroutines: the accept loop, which ends when the listener closes (at the
// session's end or a handoff); one per program connection, ending with its
// connection, which stop closes; the timers that end the refusals' quiet
// times, at most maxAgentEventKeys and one more, each running briefly when
// its time is up; and for a connection relayed to the inherited agent one
// more, copying the other direction. A forwarder is served by a
// session-socket connection handler (serveForwarder), which reads the
// client's replies until the stream ends, as it does with its attachment.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"agtd.sh/internal/db"
	"agtd.sh/internal/paths"
	"agtd.sh/internal/protocol"
	"agtd.sh/internal/session"
	"agtd.sh/internal/sshagent"
	"agtd.sh/internal/transport"
)

const (
	// maxAgentConns bounds the program connections open on a session's
	// agent socket. It matches agt's bound on a forwarder's connections to
	// its agent.
	maxAgentConns = 64
	// agentDialTimeout bounds connecting to the inherited agent.
	agentDialTimeout = 2 * time.Second
	// maxAgentBindings bounds the session bindings remembered per
	// connection, as OpenSSH's agent bounds them.
	maxAgentBindings = 16
	// maxAgentEventKeys bounds the different refusals in their quiet time
	// at once (record); more are counted together. A flush writes at most
	// this many counts and one more, and as many first refusals can follow
	// it at once: twice it and one more must fit the recorder's queue
	// (maxPendingEvents), or events are dropped.
	maxAgentEventKeys = 16
	// agentAcceptBackoff bounds the wait before accepting again after the
	// agent socket failed to accept.
	agentAcceptBackoff = time.Second
	// agentQuiesceTimeout bounds the wait, at the session's end, for the
	// events of requests whose replies the programs have (quiesce).
	agentQuiesceTimeout = time.Second
)

var (
	// agentReplyTimeout bounds the wait for a client's reply, leaving room
	// for its agent to ask the user to confirm (a key's touch, 1Password)
	// over a slow link. A variable for tests.
	agentReplyTimeout = 90 * time.Second
	// agentWriteTimeout bounds a write to a forwarder, or to a program; one
	// that takes longer is dropped.
	agentWriteTimeout = 10 * time.Second
	// agentIdleTimeout bounds the wait for each of a program connection's
	// requests, so one that never speaks, or stays open unused, cannot hold
	// one of the socket's places for good. Its first request gets as long:
	// ssh-add connects before it asks for a passphrase or a PIN. A variable
	// for tests.
	agentIdleTimeout = 15 * time.Minute
	// agentEventQuiet is how long a refusal holds back those like it at
	// first, and agentEventQuietMax how long at most, as the time doubles
	// while they keep coming (record). Variables for tests.
	agentEventQuiet    = 10 * time.Second
	agentEventQuietMax = time.Hour
	// requireForwarderWait is how long, from its start, a session that
	// requires a forwarded agent (Args.RequireForwarder) keeps programs
	// waiting for the first forwarder: the client creates the session,
	// attaches, then opens one. A variable for tests.
	requireForwarderWait = 30 * time.Second
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

	// events paces refusals (record): those in their quiet time, by
	// summary; overflow is the quiet time of those held back for want of
	// room, nil while there is none.
	eventsMu sync.Mutex
	events   map[string]*agentEvent
	overflow *agentEvent
	// recording counts forwarded requests between their reply and their
	// event: from just before the reply reaches the program until note has
	// recorded what the client's agent did. recorded is closed when it
	// falls to zero. The session's end waits for them (quiesce) before the
	// recorder closes. Every other event, a flush's counts too, goes to the
	// recorder under eventsMu, or before the program has its reply.
	recording int
	recorded  chan struct{}
}

// agentEvent is a quiet time: how long it is, how many refusals it has held
// back so far, and the timer that ends it (endQuiet).
type agentEvent struct {
	quiet time.Duration
	held  int
	timer *time.Timer
}

// agentConn is one program connection to the agent socket.
type agentConn struct {
	id   uint32
	conn transport.Stream
	// r reads conn, so a program's next message can be looked for without
	// taking it.
	r *bufio.Reader
	// up is the inherited agent's connection, for a connection relayed
	// there; guarded by agentServer.mu.
	up net.Conn
}

// inheritedAgent is the agent a worker relays to when no forwarder serves:
// SSH_AUTH_SOCK in its environment, unless that is itself another session's
// socket, as a daemon started from inside a session hands its workers.
// Relaying there would let one session sign with the keys forwarded to
// another, recorded under the other's name. A symlink is kept: where it
// leads is judged at each dial (resolveAgentSocket), since it moves.
func inheritedAgent(p *paths.AppPaths) string {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock != "" && inAgentSocketDir(p, sock) {
		return ""
	}
	return sock
}

// inAgentSocketDir reports whether path is in the runtime's ssh/ directory,
// where sessions' agent sockets are.
func inAgentSocketDir(p *paths.AppPaths, path string) bool {
	dir := filepath.Dir(filepath.Clean(path))
	if dirInfo, err := os.Stat(dir); err == nil {
		if sshInfo, err := os.Stat(p.SSHDir); err == nil {
			return os.SameFile(dirInfo, sshInfo)
		}
	}
	return dir == filepath.Clean(p.SSHDir)
}

// resolveAgentSocket follows path through any symlinks to the socket it
// names now, and reports false when that is a session's agent socket. A link
// is the usual way to keep a fixed SSH_AUTH_SOCK (`ln -sf "$SSH_AUTH_SOCK"
// ~/.ssh/ssh_auth_sock` in a shell's startup), a shell in a session points
// it at that session's socket, and the next login points it back.
func resolveAgentSocket(p *paths.AppPaths, path string) (string, bool) {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	} else if target, err := os.Readlink(path); err == nil {
		// A link to a socket that is not there: judge it by where it
		// points.
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		path = target
	}
	return path, !inAgentSocketDir(p, path)
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
	return &agentServer{rt: rt, path: path, ownSocket: ownSocket, inherited: inherited, listener: l,
		conns: map[*agentConn]struct{}{}, events: map[string]*agentEvent{}}
}

// start serves the socket's listener. Each connection is admitted (track)
// before a goroutine starts for it, so one over maxAgentConns costs no more
// than closing it.
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
		var backoff time.Duration
		// Connections refused over the bound are logged at most once a
		// minute, with how many there were since.
		var dropped int
		var droppedLogged time.Time
		for {
			s, err := l.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				backoff = min(max(2*backoff, 5*time.Millisecond), agentAcceptBackoff)
				fmt.Fprintf(os.Stderr, "session worker: accept on the SSH agent socket: %v; retrying in %v\n", err, backoff)
				time.Sleep(backoff)
				continue
			}
			backoff = 0
			c, full := a.track(s, quit)
			if c == nil {
				// Over the bound, or closing: the program carries on
				// without an agent.
				if full {
					dropped++
					if time.Since(droppedLogged) >= time.Minute {
						fmt.Fprintf(os.Stderr, "session worker: refused %d SSH agent connections: %d are open\n", dropped, maxAgentConns)
						dropped, droppedLogged = 0, time.Now()
					}
				}
				s.Close()
				continue
			}
			a.handlers.Add(1)
			go func() {
				defer a.handlers.Done()
				a.serve(c, quit)
			}()
		}
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
	// The counts of refusals held back go to the recorder before a
	// handoff's exec, which loses what it has not written yet. At the
	// session's end those were written before the recorder closed
	// (onChildExited); any since are not recorded.
	a.flushEvents()
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

// track registers a program connection. It returns nil when the socket is
// closing, or, with full set, when maxAgentConns are open.
func (a *agentServer) track(s transport.Stream, quit chan struct{}) (c *agentConn, full bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	select {
	case <-quit:
		return nil, false
	default:
	}
	if len(a.conns) >= maxAgentConns {
		return nil, true
	}
	c = &agentConn{id: a.nextID.Add(1), conn: s, r: bufio.NewReader(s)}
	a.conns[c] = struct{}{}
	return c, false
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
}

// serve routes a program connection on its first message and serves it
// there until it closes.
func (a *agentServer) serve(c *agentConn, quit <-chan struct{}) {
	defer a.untrack(c)
	c.conn.SetReadDeadline(time.Now().Add(agentIdleTimeout))
	first, err := sshagent.ReadMessage(c.r)
	if err != nil {
		return
	}
	c.conn.SetReadDeadline(time.Time{})
	for {
		f, waited, err := a.route(quit)
		if err != nil {
			return
		}
		if waited && c.gone() {
			// The program gave up while it waited: its request goes
			// nowhere, so no agent asks anyone to confirm a signature
			// nobody waits for.
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
	if a.inherited != "" && a.relayInherited(c, first, quit) {
		return
	}
	a.serveUnavailable(c, first)
}

// answer writes a reply to the program. One that does not take it within
// agentWriteTimeout (it sends requests and never reads the answers) loses
// its connection, so it cannot hold one of the socket's places by never
// reading.
func (c *agentConn) answer(reply []byte) error {
	c.conn.SetWriteDeadline(time.Now().Add(agentWriteTimeout))
	return sshagent.WriteMessage(c.conn, reply)
}

// gone reports whether the program has closed its connection, without
// taking anything it sent.
func (c *agentConn) gone() bool {
	c.conn.SetReadDeadline(time.Now().Add(time.Millisecond))
	_, err := c.r.Peek(1)
	c.conn.SetReadDeadline(time.Time{})
	return err != nil && !errors.Is(err, os.ErrDeadlineExceeded)
}

// route picks a new connection's forwarder, waiting while the size owner's
// next forwarder is awaited (agentRoute), and reports whether it waited. A
// nil forwarder means none: the inherited agent, or no agent.
func (a *agentServer) route(quit <-chan struct{}) (f *forwarder, waited bool, err error) {
	for {
		var until time.Time
		var wake <-chan struct{}
		if err := a.rt.owner.do(func(s *ownerState) error {
			f, until, wake = s.agentRoute(time.Now())
			return nil
		}); err != nil {
			return nil, waited, err
		}
		if f != nil || until.IsZero() {
			return f, waited, nil
		}
		waited = true
		timer := time.NewTimer(time.Until(until))
		select {
		case <-wake:
		case <-timer.C:
		case <-quit:
			timer.Stop()
			return nil, waited, errAgentStopping
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
		reply, keep, answered := a.exchange(f, fc, msg, quit)
		if answered {
			// What the client's agent did is recorded once the program has
			// its reply, since naming where a login goes reads known_hosts,
			// which must not hold up a signature. The session's end waits
			// for it (quiesce).
			a.beginRecording()
		}
		var err error
		if reply != nil {
			err = c.answer(reply)
		}
		if f.settle(fc) {
			// The client closed the connection, or the forwarder ended,
			// while the reply was on its way: the program has its answer,
			// then its connection closes.
			keep = false
		}
		if answered {
			a.note(f, fc, msg, reply)
			a.endRecording()
		}
		if err != nil || !keep {
			return true
		}
		c.conn.SetReadDeadline(time.Now().Add(agentIdleTimeout))
		if msg, err = sshagent.ReadMessage(c.r); err != nil {
			return true
		}
	}
}

// exchange passes one request from a program to the client, and returns the
// reply to write back, whether the connection carries on, and whether the
// client answered. A request a forwarded agent never takes is refused here.
// When the client cannot answer (the forwarder ended, it closed this
// connection, it took too long) the program gets SSH_AGENT_FAILURE and its
// connection is closed, since the client's agent connection, and any session
// binding on it, is gone.
func (a *agentServer) exchange(f *forwarder, fc *forwardedConn, msg []byte, quit <-chan struct{}) (reply []byte, keep, answered bool) {
	if !sshagent.Forwardable(msg) {
		a.record(session.EventSSHAgentRefused,
			fmt.Sprintf("refused a request to %s on the agent %s forwards: a forwarded agent only lists keys and signs", sshagent.Describe(msg), describeAgentClient(f.client)))
		return sshagent.Failure(), true, false
	}
	reply, err := f.request(fc, msg, quit)
	switch {
	case errors.Is(err, errAgentStopping):
		return nil, false, false
	case err != nil:
		return sshagent.Failure(), false, false
	}
	return reply, true, true
}

// note records what a forwarded request did: the host a connection is bound
// to, and a signature given or refused, as session events.
func (a *agentServer) note(f *forwarder, fc *forwardedConn, msg, reply []byte) {
	switch sshagent.Type(msg) {
	case sshagent.MsgExtension:
		// A binding counts once the client's agent has accepted it, which
		// OpenSSH's does only after checking the host key's signature over
		// the session id.
		if b, err := sshagent.ParseSessionBind(msg); err == nil && sshagent.Type(reply) == sshagent.MsgSuccess {
			fc.bind(b.SessionID, b.HostKey)
		}
	case sshagent.MsgSignRequest:
		req, err := sshagent.ParseSignRequest(msg)
		if err != nil {
			return
		}
		what, key := describeSigned(req.Data, fc.bindings), sshagent.Fingerprint(req.Key)
		switch sshagent.Type(reply) {
		case sshagent.MsgSignResponse:
			a.record(session.EventSSHAgentSigned, fmt.Sprintf("%s signed %s using key %s", describeAgentClient(f.client), what, key))
		case sshagent.MsgFailure:
			a.record(session.EventSSHAgentRefused, fmt.Sprintf("%s refused to sign %s using key %s", describeAgentClient(f.client), what, key))
		}
	}
}

// record records one of the socket's events. Every signature is recorded:
// it is the trail of what the client's agent signed. Refusals are paced: a
// refusal starts a quiet time of agentEventQuiet, and those like it that
// come within it are held back. When it ends an event counts them, and if
// there were any, another quiet time follows, twice as long, up to
// agentEventQuietMax; if there were none, the refusal is forgotten. While
// maxAgentEventKeys refusals are in their quiet time, any other is held
// back in the overflow's quiet time instead, counted with the rest of them
// on the same doubling clock, and while that holds any back, other
// refusals join it rather than start their own. So a program looping on a
// refused request writes about ten events in its first hour and one an
// hour after, and one varying what it asks maxAgentEventKeys more. Events
// go to the recorder under eventsMu, so none is between the two when the
// session's end flushes the rest.
func (a *agentServer) record(kind session.EventKind, summary string) {
	if kind != session.EventSSHAgentRefused {
		a.rt.recorder.event(db.NewEvent{Kind: kind, Summary: summary})
		return
	}
	a.eventsMu.Lock()
	defer a.eventsMu.Unlock()
	switch e := a.events[summary]; {
	case e != nil:
		e.held++
	case a.overflow == nil && len(a.events) < maxAgentEventKeys:
		a.events[summary] = startQuiet(func(e *agentEvent) { a.endQuiet(summary, e) })
		a.rt.recorder.event(db.NewEvent{Kind: kind, Summary: summary})
	default:
		if a.overflow == nil {
			a.overflow = startQuiet(a.endOverflow)
		}
		a.overflow.held++
	}
}

// startQuiet starts a quiet time of agentEventQuiet, which calls end when
// it is over.
func startQuiet(end func(*agentEvent)) *agentEvent {
	e := &agentEvent{quiet: agentEventQuiet}
	e.timer = time.AfterFunc(e.quiet, func() { end(e) })
	return e
}

// again starts the quiet time after e, twice as long, up to
// agentEventQuietMax, once what e held back is counted.
func (e *agentEvent) again() {
	e.held, e.quiet = 0, min(2*e.quiet, agentEventQuietMax)
	e.timer.Reset(e.quiet)
}

// endQuiet ends quiet time e of the refusal summary: those it held back are
// counted, and another follows; if it held none back, the refusal is
// forgotten.
func (a *agentServer) endQuiet(summary string, e *agentEvent) {
	a.eventsMu.Lock()
	defer a.eventsMu.Unlock()
	switch {
	case a.events[summary] != e:
		// Flushed meanwhile (flushEvents).
	case e.held == 0:
		delete(a.events, summary)
	default:
		a.countHeld(summary, e.held)
		e.again()
	}
}

// endOverflow ends the overflow's quiet time e likewise.
func (a *agentServer) endOverflow(e *agentEvent) {
	a.eventsMu.Lock()
	defer a.eventsMu.Unlock()
	switch {
	case a.overflow != e:
	case e.held == 0:
		a.overflow = nil
	default:
		a.countOverflow(e.held)
		e.again()
	}
}

// countHeld and countOverflow record how many refusals a quiet time held
// back. Called with eventsMu held.
func (a *agentServer) countHeld(summary string, n int) {
	a.rt.recorder.event(db.NewEvent{Kind: session.EventSSHAgentRefused,
		Summary: fmt.Sprintf("%s (and %d more like it)", summary, n)})
}

func (a *agentServer) countOverflow(n int) {
	a.rt.recorder.event(db.NewEvent{Kind: session.EventSSHAgentRefused,
		Summary: fmt.Sprintf("refused %d more requests, too many different ones to list each", n)})
}

// flushEvents counts every refusal held back, and forgets them all: at the
// session's end, and when the socket stops.
func (a *agentServer) flushEvents() {
	a.eventsMu.Lock()
	defer a.eventsMu.Unlock()
	for summary, e := range a.events {
		e.timer.Stop()
		if e.held > 0 {
			a.countHeld(summary, e.held)
		}
		delete(a.events, summary)
	}
	if e := a.overflow; e != nil {
		e.timer.Stop()
		if e.held > 0 {
			a.countOverflow(e.held)
		}
		a.overflow = nil
	}
}

// beginRecording marks a forwarded request between its reply and its event
// (recording); endRecording marks its event recorded.
func (a *agentServer) beginRecording() {
	a.eventsMu.Lock()
	if a.recording == 0 {
		a.recorded = make(chan struct{})
	}
	a.recording++
	a.eventsMu.Unlock()
}

func (a *agentServer) endRecording() {
	a.eventsMu.Lock()
	if a.recording--; a.recording == 0 {
		close(a.recorded)
	}
	a.eventsMu.Unlock()
}

// quiesce waits, at most d, for the forwarded requests whose replies the
// programs have but whose events are not yet recorded, so a signature given
// before the session's end is on record. It waits on nothing else: not the
// connections' goroutines, which may wait for the owner goroutine that
// calls it, or for a client's reply.
func (a *agentServer) quiesce(d time.Duration) {
	a.eventsMu.Lock()
	if a.recording == 0 {
		a.eventsMu.Unlock()
		return
	}
	recorded := a.recorded
	a.eventsMu.Unlock()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-recorded:
	case <-t.C:
	}
}

// describeAgentClient names a forwarder's client in an event.
func describeAgentClient(client string) string {
	if client == "" || client == "local" {
		return "a local client"
	}
	return client
}

// describeSigned says what signed data was for: a login, as whom, and to
// where when the login's session is one the client's agent accepted a
// binding for (the host key it names, from this machine's known_hosts when
// it can be), or an SSHSIG namespace.
func describeSigned(data []byte, bindings map[string][]byte) string {
	p := sshagent.ParsePurpose(data)
	switch {
	case p.Login:
		if host, ok := bindings[string(p.SessionID)]; ok {
			return fmt.Sprintf("a login as %s to %s", p.User, hostName(host))
		}
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
	name, hashed := sshagent.KnownHostName(key, files...)
	switch {
	case name != "":
		return name
	case hashed:
		return "a known host (hashed in known_hosts; key " + sshagent.Fingerprint(key) + ")"
	}
	return "an unknown host (key " + sshagent.Fingerprint(key) + ")"
}

// relayInherited relays a connection to the agent the worker inherited, byte
// for byte and unfiltered: the session's programs could reach that agent
// before, so it gives them nothing new. It reports false, having written
// nothing to the program, when that agent cannot be reached, or when its
// path now leads to a session's socket: a link moved since the worker
// started.
func (a *agentServer) relayInherited(c *agentConn, first []byte, quit <-chan struct{}) bool {
	// The socket the path names now is the one dialled, so a link moved
	// between the check and the dial cannot lead elsewhere.
	target, ok := resolveAgentSocket(a.rt.paths, a.inherited)
	if !ok {
		return false
	}
	up, err := net.DialTimeout("unix", target, agentDialTimeout)
	if err != nil {
		return false
	}
	a.mu.Lock()
	select {
	case <-quit:
		// stop has closed the connections it knew of; this one would
		// escape it, and could hold stop up.
		a.mu.Unlock()
		up.Close()
		return true
	default:
	}
	c.up = up
	a.mu.Unlock()
	if err := sshagent.WriteMessage(up, first); err != nil {
		return false
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(up, c.r)
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
		if err := c.answer(reply); err != nil {
			return
		}
		c.conn.SetReadDeadline(time.Now().Add(agentIdleTimeout))
		var err error
		if msg, err = sshagent.ReadMessage(c.r); err != nil {
			return
		}
	}
}

// noteUnavailable records that a program found no agent, once per gap in
// forwarding and only in a session that has been forwarded to, or that
// requires a forwarder: one that never asked for forwarding has nothing
// missing.
func (a *agentServer) noteUnavailable() {
	record := false
	_ = a.rt.owner.do(func(s *ownerState) error {
		if (s.agentForwarded || s.agentRequired) && !s.agentGap {
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
	conn transport.Stream
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
	// bindings are the session bindings the client's agent accepted on the
	// connection, host keys by session id; only the connection's goroutine
	// touches them.
	bindings map[string][]byte
}

// bind remembers a session binding the client's agent accepted.
func (fc *forwardedConn) bind(sessionID, hostKey []byte) {
	if fc.bindings == nil {
		fc.bindings = map[string][]byte{}
	}
	if len(fc.bindings) < maxAgentBindings {
		fc.bindings[string(sessionID)] = hostKey
	}
}

func newForwarder(conn transport.Stream, client string) *forwarder {
	return &forwarder{conn: conn, client: client, conns: map[uint32]*forwardedConn{}, done: make(chan struct{})}
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
	select {
	case <-fc.reply:
		// A duplicate of the last reply, taken before that request ended.
	default:
	}
	// In flight until the program has the answer (settle), so the
	// client's close cannot take the connection from under the reply.
	fc.inFlight = true
	f.mu.Unlock()
	if err := f.write(&protocol.Response{SSHAgentRequest: &protocol.SSHAgentMessage{Conn: fc.id, Data: msg}}); err != nil {
		return nil, err
	}
	timer := time.NewTimer(agentReplyTimeout)
	defer timer.Stop()
	var err error
	select {
	case reply := <-fc.reply:
		return reply, nil
	case <-fc.gone:
		err = errClientClosed
	case <-f.done:
		err = errForwarderGone
	case <-quit:
		return nil, errAgentStopping
	case <-timer.C:
		f.mu.Lock()
		fc.clientClosed = true
		f.mu.Unlock()
		_ = f.write(&protocol.Response{SSHAgentClose: &protocol.SSHAgentConn{Conn: fc.id}})
		return nil, errClientTimeout
	}
	// The client's reply came before its close or the stream's end
	// (readLoop takes frames in order), though the select picked the
	// latter: it still answers the request, a signature the client made.
	select {
	case reply := <-fc.reply:
		return reply, nil
	default:
		return nil, err
	}
}

// settle ends fc's request once its answer has been written to the
// program, and reports whether fc takes no more: f ended, or the client
// closed fc, meanwhile.
func (f *forwarder) settle(fc *forwardedConn) (finished bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fc.inFlight = false
	return f.closed || fc.clientClosed
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
		req, err := protocol.ReadSSHAgentRequest(r)
		if err != nil || req == nil {
			return
		}
		switch {
		case req.SSHAgentReply != nil:
			m := req.SSHAgentReply
			if len(m.Data) == 0 || len(m.Data) > sshagent.MaxMessageLen {
				return
			}
			// Only a connection waiting for its reply takes one; any other
			// reply is dropped, or it would answer the next request.
			f.mu.Lock()
			if fc := f.conns[m.Conn]; fc != nil && fc.inFlight {
				select {
				case fc.reply <- m.Data:
				default:
					// A second reply to one request: drop it.
				}
			}
			f.mu.Unlock()
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
// found live in this incarnation and has no live forwarder (one that has
// ended gives way), it is the attachment's forwarder until the stream or the
// attachment ends.
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
	f := newForwarder(conn, req.Client)
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
// owner's forwarder; else nowhere yet, until the time returned, while the
// owner's next forwarder is awaited: its forwarder ended within
// sizeOwnerGrace (removed or not yet, and whether or not it owned the size
// then), so agt opens another, or its id is reserved for the reattach of an
// owner that was forwarding (agentWaitFor), or the session requires a
// forwarder and agentRequiredUntil has yet to pass; else neither (the
// inherited agent, or none). wake is closed when the answer may have changed.
func (s *ownerState) agentRoute(now time.Time) (f *forwarder, until time.Time, wake <-chan struct{}) {
	if s.agentWake == nil {
		s.agentWake = make(chan struct{})
	}
	if a := s.attachments[s.sizeOwner]; a != nil {
		switch {
		case a.forwarder != nil && !a.forwarder.ended():
			return a.forwarder, time.Time{}, s.agentWake
		case a.forwarder != nil:
			// Ended, and yet to be removed (removeForwarder).
			return nil, now.Add(sizeOwnerGrace), s.agentWake
		case now.Before(a.forwarderEnded.Add(sizeOwnerGrace)):
			return nil, a.forwarderEnded.Add(sizeOwnerGrace), s.agentWake
		}
	}
	if s.sizeOwner != "" && s.sizeOwner == s.agentWaitFor && now.Before(s.agentWaitUntil) {
		return nil, s.agentWaitUntil, s.agentWake
	}
	if s.agentRequired && now.Before(s.agentRequiredUntil) {
		return nil, s.agentRequiredUntil, s.agentWake
	}
	return nil, time.Time{}, s.agentWake
}

// agentChanged runs whenever a new connection's route may have changed: it
// ends the gap in forwarding once the size owner forwards again, and wakes
// connections waiting for a route.
func (s *ownerState) agentChanged() {
	if a := s.attachments[s.sizeOwner]; a != nil && a.forwarder != nil && !a.forwarder.ended() {
		// The next program to find no agent starts a new gap.
		s.agentGap = false
	}
	if s.agentWake != nil {
		close(s.agentWake)
	}
	s.agentWake = make(chan struct{})
}

// addForwarder makes f the forwarder of a live attachment. One that has
// ended, which its stream's handler has yet to remove, gives way to f, as
// when agt opens a forwarder again.
func (s *ownerState) addForwarder(attachID string, f *forwarder) error {
	a, ok := s.attachments[attachID]
	switch {
	case !ok:
		return fmt.Errorf("attachment `%s` of session `%s` not found", attachID, s.sessionID)
	case a.forwarder != nil && !a.forwarder.ended():
		return errForwarderExists
	}
	a.forwarder, a.forwarded = f, true
	s.agentForwarded = true
	s.agentChanged()
	return nil
}

// removeForwarder forgets an attachment's forwarder once it has ended. If
// the attachment lives on, agt opens another (a stream that failed, a write
// that timed out): while the attachment owns the size, or once it takes it,
// new agent connections wait for that one (agentRoute), as for an owner's
// reattach, rather than turn to the devbox's own agent meanwhile.
func (s *ownerState) removeForwarder(attachID string, f *forwarder) {
	if a, ok := s.attachments[attachID]; ok && a.forwarder == f {
		a.forwarder, a.forwarderEnded = nil, time.Now()
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
