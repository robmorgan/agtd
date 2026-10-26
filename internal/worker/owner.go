package worker

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/creack/pty"

	"agtd.sh/internal/protocol"
	"agtd.sh/internal/session"
)

// ownerState is only ever touched by the owner goroutine. All other
// goroutines reach it through owner.do, which serialises access.
type ownerState struct {
	sessionID   string
	ptmx        *os.File
	terminal    *terminalState
	geometry    protocol.Geometry
	attachments map[string]*ownerAttachment
	// sizeOwner is the id of the attachment that owns the size, empty for
	// none. After a handoff, or the owner's stream being cut, it may name
	// one that has not reattached yet; see size.go. sizeReserve counts
	// those reservations, so only the latest one's grace timer acts.
	sizeOwner   string
	sizeReserve int
	output      *broadcaster
	input       *ptyInput
	// outputBytes and outputChunks count the PTY output published
	// (session stats).
	outputBytes  uint64
	outputChunks uint64
	// activity tracks what the agent is doing; nil in tests that build
	// an ownerState without a session.
	activity *activityTracker
	// status keeps the program's own status records (OSC 7501), the
	// primary source of activity; see status.go.
	status *statusTracker
	// agentName and command are the session's agent as configured and
	// the command it runs, which pick the input field's profile (field.go).
	agentName, command string
	// colors are the default colours of the terminal the session was
	// created from or most recently attached by, which answer the
	// program's colour queries (colors.go); nil until one reports them.
	colors       *protocol.TerminalColors
	colorQueries colorQueries
	// colorFlushArmed is whether a timer will publish a held query start
	// that never completed.
	colorFlushArmed bool
	// lastOutputAt is when the PTY last produced output, for frames that
	// carry the field only once it has settled.
	lastOutputAt time.Time
	// owner is the goroutine this state belongs to, for timers that need
	// to get back onto it.
	owner *owner
	// groundWaiters run once the shadow terminal's parser is between
	// sequences; groundGen counts the times they have been run, so a
	// fallback timer only runs the ones it was started for. See atGround.
	groundWaiters []func()
	groundGen     uint64
	// restart is closed when a handoff begins, telling every attachment to
	// end (with SessionRestarting where the client asked for it); a
	// handoff that fails replaces it.
	restart chan struct{}
	// SSH agent forwarding (sshagent.go). agentWake is closed and replaced
	// whenever a new agent connection's route may have changed.
	// agentWaitFor is the size owner whose forwarder new connections wait
	// for until agentWaitUntil: a forwarding owner reserved for its
	// reattach. agentForwarded records that the session has been forwarded
	// to, and agentGap that the current gap in forwarding has been recorded.
	agentWake      chan struct{}
	agentWaitFor   string
	agentWaitUntil time.Time
	agentForwarded bool
	agentGap       bool
}

type ownerAttachment struct {
	kind        session.AttachmentKind
	connectedAt time.Time
	// geometry is the size the client last reported, applied only while
	// it owns the size; lastUsed is its attach or its last input, which
	// picks the next owner (size.go).
	geometry protocol.Geometry
	lastUsed time.Time
	// sizeNote (one slot, never blocks) signals a change of the size or
	// its owner to the attachment's handler.
	sizeNote chan struct{}
	// announcesLeave is set for a client that says when it leaves on
	// purpose (AttachLeave, with CapAttachSizeOwner), and left once it
	// has, or once it was detached: an owner that ends without leaving
	// was cut, and its reattach is waited for (size.go).
	announcesLeave bool
	left           bool
	// sub is the raw output queue; nil for a frame client, which has
	// frames instead (frames.go).
	sub        *subscriber
	frames     *frameState
	detach     chan struct{}
	detachOnce sync.Once
	// forwarder is the attachment's SSH agent forwarder, nil for none;
	// forwarded is set once it has had one (sshagent.go).
	forwarder *forwarder
	forwarded bool
}

func (a *ownerAttachment) signalDetach() {
	a.left = true
	a.detachOnce.Do(func() { close(a.detach) })
}

// owner serialises all access to ownerState on a single goroutine. It is
// stopped by closing done rather than cmds, because connection handlers and
// the PTY pump may still try to reach it after the session has ended (for
// example a client blocked writing to a slow socket past shutdownGrace).
// Once stopped, do returns errOwnerStopped and post is a no-op.
type owner struct {
	cmds chan func(*ownerState)
	done chan struct{}
}

var errOwnerStopped = errors.New("session worker is shutting down")

func newOwner() *owner {
	return &owner{cmds: make(chan func(*ownerState), 1024), done: make(chan struct{})}
}

// do runs fn on the owner goroutine and waits for it to finish.
func (o *owner) do(fn func(*ownerState) error) error {
	result := make(chan error, 1)
	select {
	case o.cmds <- func(s *ownerState) { result <- fn(s) }:
	case <-o.done:
		return errOwnerStopped
	}
	select {
	case err := <-result:
		return err
	case <-o.done:
		return errOwnerStopped
	}
}

// doAtGround is do for work that takes a snapshot: fn runs at the next
// point in the PTY output where the snapshot is an exact boundary (see
// ownerState.atGround), which may be after later output arrives.
func (o *owner) doAtGround(fn func(*ownerState) error) error {
	result := make(chan error, 1)
	if err := o.do(func(s *ownerState) error {
		s.atGround(func() { result <- fn(s) })
		return nil
	}); err != nil {
		return err
	}
	select {
	case err := <-result:
		return err
	case <-o.done:
		return errOwnerStopped
	}
}

// post runs fn on the owner goroutine without waiting.
func (o *owner) post(fn func(*ownerState)) {
	select {
	case o.cmds <- fn:
	case <-o.done:
	}
}

// stop makes run return after the command in progress, if any.
func (o *owner) stop() {
	close(o.done)
}

func (o *owner) run(state *ownerState) {
	for {
		select {
		case fn := <-o.cmds:
			fn(state)
		case <-o.done:
			return
		}
	}
}

func cellWidthPx(g protocol.Geometry) uint32 {
	if g.Cols == 0 || g.PixelWidth == 0 {
		return 0
	}
	return uint32(g.PixelWidth) / uint32(g.Cols)
}

func cellHeightPx(g protocol.Geometry) uint32 {
	if g.Rows == 0 || g.PixelHeight == 0 {
		return 0
	}
	return uint32(g.PixelHeight) / uint32(g.Rows)
}

func (s *ownerState) hasLiveAttachTerminal() bool {
	for _, a := range s.attachments {
		if a.kind == session.AttachmentAttach {
			return true
		}
	}
	return false
}

// writeInput queues client input for the PTY; see ptyInput.
func (s *ownerState) writeInput(data []byte) error {
	// A client terminal that also speaks the program status protocol
	// echoes the detection query back as input. The worker has already
	// answered it (afterFeed), so the copy is dropped: forwarded, it
	// would reach the program as a stray escape sequence after its
	// detection window, and it is a terminal's reflex, not the user
	// typing, so it must not count as answering a blocked record.
	if isStatusDetectionReply(data) {
		return nil
	}
	if err := s.input.enqueue(data); err != nil {
		return err
	}
	// Reports the client terminal sent by itself, or for a scroll or the
	// mouse crossing the window, reach the program but are not typing
	// (isPassiveInput): they answer nothing and are not activity. They do
	// show someone is looking, which is all a done or error record waits
	// for.
	if isPassiveInput(data) {
		if s.status != nil {
			s.status.userSaw()
			if s.activity != nil {
				s.activity.setNative(time.Now(), s.status.derive(), s.watched(), false)
			}
		}
		return nil
	}
	now := time.Now()
	// Typing answers whatever the session was waiting for, for both
	// sources: a blocked status record counts as answered until the
	// program reports again (silently, like the heuristic waiting flag).
	if s.status != nil {
		s.status.noteInput(now)
		if s.activity != nil {
			s.activity.setNative(now, s.status.derive(), s.watched(), false)
		}
	}
	if s.activity != nil {
		s.activity.input(now, s.watched())
	}
	return nil
}

// watched reports whether an interactive client is attached.
func (s *ownerState) watched() bool { return len(s.attachments) > 0 }

// tickActivity checks idleness and samples the PTY's foreground process,
// and compresses the scrollback of a terminal that has gone quiet.
func (s *ownerState) tickActivity() {
	now := time.Now()
	if s.activity != nil {
		s.activity.tick(now, foregroundPGID(s.ptmx), s.watched())
	}
	s.terminal.compressIdle(now)
}

// resize applies a client's geometry. A zero row or column count (a client
// whose terminal reports no size) keeps the current size instead: the PTY
// and libghostty both reject it.
func (s *ownerState) resize(g protocol.Geometry) error {
	if g.Cols == 0 || g.Rows == 0 {
		return nil
	}
	if err := pty.Setsize(s.ptmx, &pty.Winsize{Rows: g.Rows, Cols: g.Cols, X: g.PixelWidth, Y: g.PixelHeight}); err != nil {
		return fmt.Errorf("failed to resize pty: %w", err)
	}
	if err := s.terminal.resize(g.Cols, g.Rows, cellWidthPx(g), cellHeightPx(g)); err != nil {
		return err
	}
	changed := g != s.geometry
	s.geometry = g
	if changed {
		s.notifySize()
	}
	// Frame clients repaint at the new size.
	s.noteFrameOutput()
	return nil
}

// publishOutput answers the program's colour queries (colors.go) and
// publishes the rest of the output (publishScanned).
func (s *ownerState) publishOutput(data []byte) error {
	data, replies := s.colorQueries.scan(data, s.colors)
	for _, reply := range replies {
		if err := s.input.enqueue(reply); err != nil {
			fmt.Fprintf(os.Stderr, "session worker: dropped colour query reply: %v\n", err)
		}
	}
	if len(s.colorQueries.held) > 0 {
		switch {
		case s.owner == nil:
			data = append(data, s.colorQueries.flush()...)
		case !s.colorFlushArmed:
			// A query start that the next chunk does not complete is
			// published as it is, as a snapshot's wait for a sequence
			// boundary is bounded (groundTimeout).
			s.colorFlushArmed = true
			time.AfterFunc(groundTimeout, func() {
				s.owner.post(func(s *ownerState) {
					s.colorFlushArmed = false
					if tail := s.colorQueries.flush(); len(tail) > 0 {
						if err := s.publishScanned(tail); err != nil {
							fmt.Fprintf(os.Stderr, "session worker: %v\n", err)
						}
					}
				})
			})
		}
	}
	if len(data) == 0 {
		return nil
	}
	return s.publishScanned(data)
}

// publishScanned feeds PTY output through the shadow terminal, answers any
// terminal queries on behalf of absent clients, then fans the raw bytes out.
//
// While snapshots are waiting for the parser to be between sequences (see
// atGround), it feeds only up to that point first, runs them, and then
// handles the rest: subscribers get the chunk in two pieces, and a client
// attaching there gets only the second, after its snapshot.
func (s *ownerState) publishScanned(data []byte) error {
	s.outputBytes += uint64(len(data))
	s.outputChunks++
	s.lastOutputAt = time.Now()
	defer s.noteFrameOutput()
	if len(s.groundWaiters) > 0 {
		n, writes, effects := s.terminal.feedUntilGround(data)
		s.afterFeed(writes, effects)
		if n > 0 {
			s.output.publish(data[:n])
		}
		if s.terminal.atGround() {
			s.runGroundWaiters()
		}
		data = data[n:]
		if len(data) == 0 {
			return nil
		}
	}
	s.afterFeed(s.terminal.feed(data))
	s.output.publish(data)
	return nil
}

// afterFeed passes the attention effects of output to activity tracking,
// and writes the shadow terminal's replies to terminal queries (cursor
// position, device attributes and so on) to the PTY when no client terminal
// is attached to answer them itself.
func (s *ownerState) afterFeed(writes [][]byte, effects terminalEffects) {
	now := time.Now()
	// The program status detection query is answered whether or not a
	// client terminal is attached: the worker consumes the reports itself,
	// and the client's terminal may not speak the protocol. A client
	// terminal that does answers too; the replies are identical bytes.
	for _, reply := range effects.statusReplies {
		if err := s.input.enqueue(reply); err != nil {
			fmt.Fprintf(os.Stderr, "session worker: dropped program status reply: %v\n", err)
		}
	}
	if s.status != nil {
		// In stream order: a prompt ends the records of the program
		// before it, never of one that reported after it in the same
		// read.
		for _, ev := range effects.statusEvents {
			if ev.report != nil {
				s.status.apply(now, *ev.report)
			} else {
				s.status.promptSeen()
			}
		}
	}
	s.frameEffects(effects)
	if s.activity != nil {
		title := ""
		if effects.titleChanged {
			title = s.terminal.title()
		}
		if s.status != nil {
			s.activity.setNative(now, s.status.derive(), s.watched(), true)
		}
		s.activity.output(now, effects, title, s.watched())
	}
	if s.hasLiveAttachTerminal() {
		return
	}
	for _, response := range writes {
		if err := s.input.enqueue(response); err != nil {
			fmt.Fprintf(os.Stderr, "session worker: dropped terminal reply: %v\n", err)
		}
	}
}

// groundTimeout bounds how long a snapshot waits for a sequence that output
// left unfinished. Programs write whole sequences, so the rest of one split
// across PTY reads follows at once; a program that stops mid-sequence gets
// a snapshot taken there instead (and its client may print the sequence's
// tail as text). A variable so tests can lengthen it.
var groundTimeout = 100 * time.Millisecond

// atGround runs fn on the owner goroutine at the next point in the PTY
// output where the shadow terminal's parser is between escape sequences and
// UTF-8 characters: now, if it already is. A snapshot is a boundary in the
// stream (the client gets the snapshot, then the output after it), and a
// boundary inside a sequence would hand the client the sequence's tail
// without its start, which it would print as text. PTY reads split
// sequences routinely under heavy output, so snapshots wait for the next
// boundary, at most groundTimeout.
func (s *ownerState) atGround(fn func()) {
	if len(s.groundWaiters) == 0 && s.terminal.atGround() {
		fn()
		return
	}
	s.groundWaiters = append(s.groundWaiters, fn)
	if len(s.groundWaiters) == 1 {
		gen := s.groundGen
		time.AfterFunc(groundTimeout, func() {
			s.owner.post(func(s *ownerState) {
				if s.groundGen == gen {
					s.runGroundWaiters()
				}
			})
		})
	}
}

func (s *ownerState) runGroundWaiters() {
	waiters := s.groundWaiters
	s.groundWaiters = nil
	s.groundGen++
	for _, fn := range waiters {
		fn()
	}
}

func (s *ownerState) history(vt bool) (string, error) {
	data, err := s.terminal.format(vt)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (s *ownerState) snapshot(scrollback int) ([]byte, error) {
	return s.terminal.snapshot(scrollback)
}

// field finds the program's input field (field.go). There is none while
// the program reports it is blocked on a dialog, or while something other
// than the agent or a shell holds the PTY's foreground (an editor takes
// keys as commands).
func (s *ownerState) field() (*protocol.Field, error) {
	app, blocked := "", false
	if s.status != nil {
		if n := s.status.derive(); n != nil {
			app = n.app
			blocked = n.activity == session.ActivityBlocked
		}
	}
	if s.activity != nil && !s.activity.foregroundTakesText() {
		blocked = true
	}
	return detectField(s.terminal.term, fieldProfileFor(app, s.agentName, s.command), blocked)
}

type attachResult struct {
	attachID string
	snapshot []byte
	detach   chan struct{}
	restart  chan struct{}
	sizeNote chan struct{}
	// sub is the raw output queue, nil for a frame client, whose frames
	// are its state instead.
	sub    *subscriber
	frames *frameState
}

// attach registers a new attachment under attachID, which the caller
// numbered (see runtime.nextAttachSeq), and takes its snapshot, with up to
// scrollback rows of scrollback (allScrollback for all). If replaces names
// a live attachment, that one is dropped at once: the client says it was
// its own, on a connection it has lost.
func (s *ownerState) attach(attachID string, kind session.AttachmentKind, g protocol.Geometry, replaces string, scrollback int, takeSize, announcesLeave, frames bool) (*attachResult, error) {
	connectedAt := time.Now().UTC()
	if old, ok := s.attachments[replaces]; ok && replaces != "" {
		// Its handler may be blocked writing to the dead connection; it
		// ends within shutdownGrace and cleans up after itself, but the
		// attachment stops receiving output and leaves the list now.
		old.signalDetach()
		s.output.unsubscribe(old.sub)
		if old.frames != nil {
			old.frames.close()
		}
		delete(s.attachments, replaces)
		// Its forwarder ends with it, and if it owned the size, agent
		// connections wait for its replacement's, as after a cut.
		s.agentLeft(replaces, old)
	}

	// The size owner's replacement inherits the size (also a reserved
	// owner's, after a handoff or a cut); with no owner, the first attachment with
	// a size takes it; otherwise this one is pending (size.go).
	owns := hasSize(g) && (takeSize || s.sizeOwner == "" || (replaces != "" && replaces == s.sizeOwner))
	a := &ownerAttachment{
		kind: kind, connectedAt: connectedAt, geometry: g, lastUsed: connectedAt,
		sizeNote: make(chan struct{}, 1), detach: make(chan struct{}), announcesLeave: announcesLeave,
	}
	if owns {
		// Resize first, so the snapshot is laid out for the client's
		// terminal: one formatted at the previous size lands wrapped and
		// with the cursor on the wrong row in a terminal of another size.
		if err := s.resize(g); err != nil {
			return nil, err
		}
		if replaces != "" && replaces == s.agentWaitFor {
			// Agent connections waiting for the owner's reattach wait
			// for this attachment's forwarder now.
			s.agentWaitFor = attachID
		}
		if s.sizeOwner != attachID {
			s.sizeOwner = attachID
			s.notifySize()
		}
	} else if replaces != "" && replaces == s.sizeOwner {
		// The owner came back without a size: the next one takes over.
		s.promote()
	}
	// A frame client gets the scrollback only; its first frame paints the
	// screen (frames.go).
	var snapshot []byte
	var err error
	if frames {
		snapshot, err = s.terminal.historySnapshot(scrollback)
	} else {
		snapshot, err = s.snapshot(scrollback)
	}
	if err == nil && frames {
		if a.frames, err = newFrameState(); err == nil {
			// What the snapshot covered; the first frame scrolls nothing.
			a.frames.history, err = s.terminal.term.ScrollbackRows()
		}
	}
	if err != nil {
		if owns {
			s.sizeOwner = ""
			s.promote()
		}
		return nil, err
	}

	if !frames {
		a.sub = s.output.subscribe()
	}
	s.attachments[attachID] = a
	// An interactive attach shows the user the screen: done and error
	// records have been seen (the same moment the daemon acknowledges
	// attention), so they no longer decide the session's activity. A
	// reattach that replaces a lost connection is the CLI reconnecting by
	// itself — no human looked — so it does not count.
	if kind == session.AttachmentAttach && replaces == "" && s.status != nil && s.activity != nil {
		s.status.userSaw()
		s.activity.setNative(connectedAt, s.status.derive(), s.watched(), false)
	}
	return &attachResult{
		attachID: attachID,
		snapshot: snapshot,
		detach:   a.detach,
		restart:  s.restart,
		sizeNote: a.sizeNote,
		sub:      a.sub,
		frames:   a.frames,
	}, nil
}

func (s *ownerState) removeAttachment(attachID string) {
	a, ok := s.attachments[attachID]
	if !ok {
		// Already replaced by its reattach, which took over its
		// ownership too.
		return
	}
	delete(s.attachments, attachID)
	if a.frames != nil {
		a.frames.close()
	}
	if attachID == s.sizeOwner {
		s.ownerLeft(a.announcesLeave && !a.left)
	}
	s.agentLeft(attachID, a)
}

func (s *ownerState) listAttachments() []session.AttachmentRecord {
	out := make([]session.AttachmentRecord, 0, len(s.attachments))
	for id, a := range s.attachments {
		out = append(out, session.AttachmentRecord{
			AttachID: id, SessionID: s.sessionID, Kind: a.kind, ConnectedAt: a.connectedAt,
			SizeOwner: id == s.sizeOwner, Cols: a.geometry.Cols, Rows: a.geometry.Rows,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ConnectedAt.Before(out[j].ConnectedAt) })
	return out
}

func (s *ownerState) detachAttachment(attachID string) error {
	a, ok := s.attachments[attachID]
	if !ok {
		return fmt.Errorf("attachment `%s` not found", attachID)
	}
	a.signalDetach()
	return nil
}

func (s *ownerState) detachAll() {
	for _, a := range s.attachments {
		a.signalDetach()
	}
}

// outputBatch is PTY output read by the pump and not yet taken by the
// owner goroutine.
type outputBatch struct {
	mu   sync.Mutex
	data []byte
	// posted is set while a take is queued on the owner goroutine and has
	// not run; data added meanwhile goes out with it.
	posted bool
	// taken is signalled (without blocking) whenever the owner takes the
	// batch, to wake a pump waiting for room.
	taken chan struct{}
}

// add appends data to the batch, queueing a take on the owner unless one is
// already queued. It returns false if the owner has stopped.
func (b *outputBatch) add(data []byte, o *owner) bool {
	b.mu.Lock()
	for b.posted && len(b.data)+len(data) > maxOutputBatch {
		b.mu.Unlock()
		select {
		case <-b.taken:
		case <-o.done:
			return false
		}
		b.mu.Lock()
	}
	// A fresh slice per batch: subscribers keep the one they were given.
	b.data = append(b.data, data...)
	post := !b.posted
	b.posted = true
	b.mu.Unlock()
	if post {
		o.post(func(s *ownerState) {
			if err := s.publishOutput(b.take()); err != nil {
				fmt.Fprintf(os.Stderr, "session worker: failed to publish output: %v\n", err)
			}
		})
	}
	return true
}

func (b *outputBatch) take() []byte {
	b.mu.Lock()
	data := b.data
	b.data = nil
	b.posted = false
	b.mu.Unlock()
	select {
	case b.taken <- struct{}{}:
	default:
	}
	return data
}
