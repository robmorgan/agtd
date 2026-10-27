package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"agtd.sh/internal/config"
	"agtd.sh/internal/protocol"
	"agtd.sh/internal/screen"
	"agtd.sh/internal/session"
	"agtd.sh/internal/transport"
)

const (
	// attachEnterSequence pushes the kitty keyboard protocol's
	// "disambiguate" flag, so Ctrl-[ and Esc arrive as different keys.
	// It is written once per attachment: the agent's own flags, which
	// the snapshot and its output set on top, live on this level.
	attachEnterSequence = "\x1b[>1u"
	// attachRestoreSequence undoes what an agent may have turned on: an
	// open synchronized-output block, the alternate screen, mouse reporting
	// (every mode and encoding, as crossterm's EnableMouseCapture sets
	// 1015 too), bracketed paste, focus events, the kitty keyboard flags,
	// xterm's modifyOtherKeys (which Claude Code sets, and which leaves a
	// shell reading Ctrl-R as "27;5;114~"), disabled line wrap, text
	// attributes, the cursor shape and a hidden cursor. The keyboard flag
	// stack is emptied (popping more levels than it holds empties it): the
	// level attach pushed, and any an agent pushed and never popped,
	// relayed to this terminal as output. pi pushes release reporting,
	// which left a shell printing ":3A" for every arrow key after a detach
	// that popped one level. The stack is per screen, so the pop follows
	// the return to the main screen, where attach pushed its level.
	attachRestoreSequence = "\x1b[?2026l\x1b[?1049l" + attachMouseOffSequence + "\x1b[?2004l\x1b[?1004l\x1b[<32u\x1b[>4m\x1b[?7h\x1b[m\x1b[0 q\x1b[?25h"
	// attachMouseOffSequence turns mouse reporting off, in every mode and
	// encoding.
	attachMouseOffSequence = "\x1b[?9l\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1005l\x1b[?1006l\x1b[?1015l\x1b[?1016l"
	attachClearSequence    = "\x1b[2J\x1b[H"
	// attachRepaintSequence starts every paint of a snapshot: mouse
	// reporting off, then a clear. A snapshot sets only the modes that
	// differ from the defaults, so without this a mouse mode the agent
	// turned off while its output was held (local echo) or not seen (a
	// reconnect) would stay on here, and the terminal would keep sending
	// reports it no longer wants. Focus events (1004) are left alone:
	// some terminals (Ghostty) report the focus whenever the mode is
	// turned on, and the snapshot already turns it on again; with local
	// echo the echo holds back that repeated report (echo.focus), without
	// it the agent gets it, as it always has.
	attachRepaintSequence = attachMouseOffSequence + attachClearSequence
	// attachDetachSequence clears the session's screen after a detach, so
	// the shell's prompt does not land in the middle of the agent's UI. The
	// session's output stays in the terminal's scrollback.
	attachDetachSequence = "\x1b[H\x1b[2J"
	attachExitTitle      = "agtd"

	// attachOwnScreenSequence enters the alternate screen for an attach
	// from the inbox (client.fromInbox), so the primary screen, the
	// inbox's block on it, is kept until attachRestoreSequence leaves it.
	// The keyboard flags attach pushes are per screen, so they are popped
	// before that (attachOwnRestorePrefix).
	attachOwnScreenSequence = "\x1b[?1049h"
	attachOwnRestorePrefix  = "\x1b[<32u"

	// frameQueue bounds the daemon frames read ahead of the terminal.
	frameQueue = 16
)

type attachOutcome int

const (
	outcomeDetached attachOutcome = iota
	outcomeEnded
	outcomeSwitch
)

type attachResult struct {
	outcome attachOutcome
	ended   *protocol.SessionEnded // outcomeEnded
	next    string                 // outcomeSwitch
}

// sessionAddress is how to name a session of this client's host to attach
// to it again: HOST/NAME, or local/NAME on the local daemon.
func (c *client) sessionAddress(id string) string {
	host := c.remoteName()
	if host == "" {
		host = localHostName
	}
	return escapeControls(host + "/" + id)
}

// attachSession attaches the terminal to a session until the user detaches
// or the session ends, following switches to other sessions on the way.
func (c *client) attachSession(id string) error {
	return c.attachSessionUID(id, "")
}

// attachSessionUID is attachSession for a session known by its UID too
// (one just created), refusing any other incarnation under its name.
func (c *client) attachSessionUID(id, uid string) error {
	ended, err := c.attachLoop(id, uid)
	if ended != nil {
		fmt.Println(formatSessionEnd(ended))
	}
	return err
}

// attachLoop attaches until the user detaches or the session ends (then
// ended says how), following switches to other sessions on the way.
func (c *client) attachLoop(id, uid string) (ended *protocol.SessionEnded, err error) {
	if !c.fromInbox {
		// Said before the attach takes the screen, and again once it has
		// given it back, since its first paint clears the screen. The
		// inbox shows it itself.
		if w := sshAgentWarning(c.paths); w != "" {
			fmt.Fprintln(os.Stderr, "agt: "+w)
			defer fmt.Fprintln(os.Stderr, "agt: "+w)
		}
	}
	titled := false
	defer func() {
		if titled {
			writeOut(terminalTitleBytes(attachExitTitle))
		}
	}()
	for {
		res, err := c.attachOnce(id, uid, &titled)
		if err != nil {
			return nil, err
		}
		switch res.outcome {
		case outcomeEnded:
			return res.ended, nil
		case outcomeSwitch:
			id, uid = res.next, ""
		default:
			if !c.fromInbox {
				fmt.Fprintf(os.Stderr, "detached from %s\n", c.sessionAddress(id))
			}
			return nil, nil
		}
	}
}

var (
	// errSessionRestarting: the session's worker is restarting (a live
	// handoff to a new agtd binary) and ended the attach stream; the
	// session is still there to attach to again.
	errSessionRestarting = errors.New("the session is restarting")
	// errAttachDropped: the attach stream ended without a final frame, so
	// it was cut (the daemon stopped, say) rather than ended by the worker.
	errAttachDropped = errors.New("the attach stream ended unexpectedly")
)

// attachIdentity is what a reattach carries over from the attachment it
// replaces, so that it reaches the same session incarnation and the daemon
// can drop the old attachment at once (see protocol.CapAttachReplace).
type attachIdentity struct {
	// uid is the session incarnation, when the daemon reports it.
	uid string
	// attachID is the attachment the connection had.
	attachID string
}

// update records what the daemon answered an attach with.
func (a *attachIdentity) update(attached *protocol.Attached) {
	if attached.SessionUID != "" {
		a.uid = attached.SessionUID
	}
	a.attachID = attached.AttachID
}

// frame is one response read from an attach stream.
type frame struct {
	resp *protocol.Response
	err  error // nil resp and nil err: the stream ended
}

// attachStream is an attach session's stream, and the goroutine reading its
// frames.
//
// The reader goroutine belongs to the attachStream: stop closes the stream,
// which ends any read in progress, and waits for the goroutine to finish, so
// switching sessions never leaves one behind. The reader never blocks on a
// full channel after stop, since every send also watches done. While the
// channel is full it stops reading, and the stream's flow control pushes
// back on the daemon, which drops output for a lagging attachment rather
// than stall the PTY.
type attachStream struct {
	s      transport.Stream
	frames chan frame
	done   chan struct{}
	exited chan struct{}
	// announcesLeave: the worker takes AttachLeave (CapAttachSizeOwner).
	announcesLeave bool
	// composes: the worker takes AttachCompose (CapAttachCompose).
	composes bool
}

func startAttachStream(s transport.Stream, r *bufio.Reader) *attachStream {
	a := &attachStream{s: s, frames: make(chan frame, frameQueue), done: make(chan struct{}), exited: make(chan struct{})}
	go func() {
		defer close(a.exited)
		for {
			resp, err := protocol.ReadResponse(r)
			select {
			case a.frames <- frame{resp, err}:
			case <-a.done:
				return
			}
			if resp == nil || err != nil {
				return
			}
		}
	}()
	return a
}

func (a *attachStream) send(req *protocol.Request) error { return protocol.WriteRequest(a.s, req) }

// leave tells the worker this client is leaving on purpose (a detach, a
// switch, the session ending), so that if it owns the session's size the
// next attachment takes over at once, then stops the stream. A stream that
// was cut is stopped without it, which keeps the size reserved for its
// reattach.
//
// With flush, it first waits, at most leaveFlushTimeout, for the worker to
// end the stream, which it does once it has read everything this client
// sent: a client about to close its QUIC connection must, since closing it
// drops whatever QUIC has not sent yet, and an AttachLeave lost that way
// reads as a cut, leaving the size and the SSH agent waiting for a
// reattach that never comes.
func (a *attachStream) leave(flush bool) {
	if a.announcesLeave {
		// A stalled stream must not hold up the detach.
		a.s.SetWriteDeadline(time.Now().Add(time.Second))
		if err := a.send(&protocol.Request{AttachLeave: protocol.Empty}); err == nil && flush {
			a.s.CloseWrite()
			a.awaitEnd(leaveFlushTimeout)
		}
	}
	a.stop()
}

// leaveFlushTimeout bounds how long a leaving client waits for the worker
// to end its stream; a round trip, normally.
const leaveFlushTimeout = time.Second

// awaitEnd waits, at most timeout, for the stream's last frame.
func (a *attachStream) awaitEnd(timeout time.Duration) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case f := <-a.frames:
			if f.err != nil || f.resp == nil || f.resp.EndOfStream != nil || f.resp.SessionEnded != nil ||
				f.resp.SessionRestarting != nil || f.resp.Error != nil {
				return
			}
		case <-timer.C:
			return
		}
	}
}

// stop half-closes the stream, which tells the daemon this client detached,
// then closes it and waits for the reader.
func (a *attachStream) stop() {
	a.s.CloseWrite()
	close(a.done)
	a.s.Close()
	<-a.exited
}

// connectAttach opens an attach stream. A session that has already ended
// answers with SessionEnded instead. Cancelling ctx abandons the attempt,
// closing the stream if it was opened.
//
// The attach stream asks for the attach features the daemon's Welcome
// offered (protocol.AttachFeatures): the session's UID, replacing prev's
// attachment when prev knows the UID, resyncs after lag, and at most
// scrollback rows of scrollback in the snapshot (-1 for all).
func (c *client) connectAttach(ctx context.Context, id string, prev attachIdentity, scrollback int, takeSize bool) (*attachStream, *protocol.Attached, *protocol.SessionEnded, error) {
	cs, err := c.controlStream(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	s, err := c.open(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	stopWatch := context.AfterFunc(ctx, func() { s.Close() })
	defer stopWatch()
	cols, rows, pw, ph := terminalGeometry()
	attach := &protocol.AttachSession{
		SessionID: id,
		Kind:      session.AttachmentAttach,
		Geometry:  protocol.Geometry{Cols: cols, Rows: rows, PixelWidth: pw, PixelHeight: ph},
		Features:  protocol.AttachFeatures(cs.features),
	}
	if prev.uid != "" && attach.HasFeature(protocol.CapSessionUID) {
		attach.ExpectUID = prev.uid
		if attach.HasFeature(protocol.CapAttachReplace) {
			attach.Replaces = prev.attachID
		}
	}
	if attach.HasFeature(protocol.CapAttachScrollback) {
		attach.ScrollbackRows = protocol.AllScrollbackRows
		if scrollback >= 0 {
			attach.ScrollbackRows = uint32(scrollback)
		}
	}
	attach.TakeSize = takeSize && attach.HasFeature(protocol.CapAttachSizeOwner)
	if attach.HasFeature(protocol.CapTerminalColors) {
		attach.Colors = terminalColors()
	}
	if !c.wantsFrames() {
		attach.Features = slices.DeleteFunc(attach.Features, func(f string) bool {
			return f == protocol.CapAttachFrames || f == protocol.CapAttachFrameWindow
		})
	}
	req := &protocol.Request{AttachSession: attach}
	if err := protocol.WriteRequest(s, req); err != nil {
		s.Close()
		return nil, nil, nil, err
	}
	r := bufio.NewReader(s)
	resp, err := protocol.ReadResponse(r)
	switch {
	case err != nil:
		s.Close()
		return nil, nil, nil, err
	case resp == nil:
		s.Close()
		return nil, nil, nil, errors.New("agtd closed the connection")
	case !stopWatch():
		// ctx was cancelled after the answer arrived, closing s.
		return nil, nil, nil, ctx.Err()
	case resp.Attached != nil:
		a := startAttachStream(s, r)
		a.announcesLeave = slices.Contains(resp.Attached.Features, protocol.CapAttachSizeOwner)
		a.composes = slices.Contains(resp.Attached.Features, protocol.CapAttachCompose)
		return a, resp.Attached, nil, nil
	case resp.SessionEnded != nil:
		s.Close()
		return nil, nil, resp.SessionEnded, nil
	case resp.Error != nil:
		s.Close()
		return nil, nil, nil, errors.New(resp.Error.Message)
	}
	s.Close()
	return nil, nil, nil, fmt.Errorf("unexpected attach response: %s", describeResponse(resp))
}

// attachOnce attaches to one session. Everything runs on this goroutine,
// fed by three sources: stdin chunks (the process-wide reader), the attach
// stream's frames, and SIGWINCH. Output is written to stdout as it
// arrives; while stdout is slow, the loop, and with it input, waits.
//
// When the attachment is cut while the session lives on (the connection to
// the daemon is lost, the daemon restarts, or the session's worker restarts
// for an upgrade) the terminal stays in raw mode while attachOnce attaches
// to the same session again, repainting the screen from the new snapshot;
// see reattach.
func (c *client) attachOnce(id, uid string, titled *bool) (res attachResult, err error) {
	ident := attachIdentity{uid: uid}
	// --take-size applies to the session first attached; a switch to
	// another one does not take its size.
	takeSize := c.takeSize
	c.takeSize = false
	stream, attached, ended, err := c.connectAttach(context.Background(), id, ident, c.attachScrollbackRows(), takeSize)
	if err != nil {
		return attachResult{}, err
	}
	if ended != nil {
		return attachResult{outcome: outcomeEnded, ended: ended}, nil
	}
	ident.update(attached)
	// Each attachment has its own SSH agent forwarder, if any: a reattach
	// is a new attachment, and needs a new one.
	var fwd *agentForwarder
	defer func() {
		if stream != nil {
			// A switch keeps the connection for the next attachment;
			// anything else may be followed by closing it.
			stream.leave(c.host != nil && res.outcome != outcomeSwitch)
		}
		// The forwarder stops after the leave, so the worker has handed
		// the size on before agent requests stop finding it.
		fwd.stop()
	}()
	fwd = c.forwardAgent(id, attached)

	writeOut(terminalTitleBytes(id + " - " + attachExitTitle))
	*titled = true
	if !c.fromInbox {
		fmt.Fprintf(os.Stderr, "attached to %s (%s); Ctrl-Y overlay, Ctrl-\\\\ detaches, Ctrl-[/Ctrl-] switch running sessions\n", id, attached.AttachID)
	}
	raw, err := enterRaw(true)
	if err != nil {
		return attachResult{}, fmt.Errorf("failed to set raw terminal mode: %w", err)
	}
	if c.fromInbox {
		writeOut([]byte(attachOwnScreenSequence))
	}
	var painter *screen.Painter
	defer func() {
		raw.restore()
		if painter != nil {
			writeOut(painter.Leave())
		}
		if c.fromInbox {
			// Leaving the alternate screen restores the inbox's: no
			// clear after a detach.
			writeOut([]byte(attachOwnRestorePrefix + attachRestoreSequence))
			return
		}
		writeOut([]byte(attachRestoreSequence))
		if err == nil && res.outcome == outcomeDetached {
			writeOut([]byte(attachDetachSequence))
		}
	}()
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	// Closing the terminal (SIGHUP) or a kill detaches cleanly, so the
	// worker hears this client leave rather than lose its stream.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGHUP, syscall.SIGTERM)
	defer signal.Stop(quit)

	keys := stdinChunks()
	var parser attachParser
	snapshot := attached.Snapshot
	ech := c.newEchoFor(attached)
	painter = c.newPainterFor(attached)
	if ech != nil {
		ech.frames = painter != nil
	}
	writeOut([]byte(attachEnterSequence))
	for {
		writeOut(attachStartupBytes(snapshot))
		ech.repainted()
		res, err := c.runAttachment(id, stream, keys, &parser, winch, quit, sizeOwnership(attached), ech, painter)
		if err == nil || !c.reconnectable(err) {
			return res, err
		}
		stream.stop()
		stream = nil
		fwd.stop()
		fwd = nil
		var r *attachResult
		stream, attached, r, err = c.reattach(id, ident, keys, &parser, err)
		if stream == nil {
			return deref(r), err
		}
		ident.update(attached)
		fwd = c.forwardAgent(id, attached)
		snapshot = attached.Snapshot
		painter = c.newPainterFor(attached)
		// An overlay that was open when the connection dropped may have
		// hidden the cursor.
		writeOut([]byte("\x1b[?25h"))
		// The echo keeps the keys it had not seen the agent draw; once
		// the new snapshot is up it asks the field which ones landed.
		switch {
		case ech == nil:
		case attached.HasFeature(protocol.CapAttachField):
			ech.frames = painter != nil
			ech.reattached()
		default:
			// The new worker does not report the field (a handoff to
			// an older one): the keys the echo still holds were never
			// sent, and go now, unpredicted. If this stream has failed
			// too, they are lost with it, as keys sent on it would be.
			if held := ech.suspend(); len(held) > 0 {
				_ = stream.send(&protocol.Request{AttachInput: &protocol.Bytes{Data: held}})
			}
			ech = nil
		}
	}
}

// newEchoFor is the local echo for an attachment, nil when the worker
// does not report the input field (CapAttachField) or the echo is off
// ([composer] enabled = false).
func (c *client) newEchoFor(attached *protocol.Attached) *echo {
	if !attached.HasFeature(protocol.CapAttachField) {
		return nil
	}
	cfg, err := config.Load(c.paths.Config)
	if err != nil || !cfg.Composer.EchoEnabled() {
		return nil
	}
	return newEcho(echoConfig{flushAfter: cfg.Composer.EchoFlushAfter()})
}

// wantsFrames is whether the screen should arrive as frames
// (attach-frames) rather than raw output: [attach] mode, or the mode set
// on the client (agt bench echo --mode). "auto" is frames from a remote
// daemon, where a round trip is worth saving, and raw output from the
// local one, where the relay is cheaper.
func (c *client) wantsFrames() bool {
	mode := c.attachMode
	if mode == "" {
		if cfg, err := config.Load(c.paths.Config); err == nil {
			mode = cfg.Attach.AttachMode()
		}
	}
	switch mode {
	case "frames":
		return true
	case "raw":
		return false
	}
	return c.host != nil
}

// newPainterFor is the screen painter for an attachment, nil unless the
// screen arrives as frames (CapAttachFrames).
func (c *client) newPainterFor(attached *protocol.Attached) *screen.Painter {
	if !attached.HasFeature(protocol.CapAttachFrames) {
		return nil
	}
	p := screen.New()
	if cfg, err := config.Load(c.paths.Config); err == nil {
		if cfg.Attach.AltScrollback {
			p.SetAltScrollback()
		}
		if cfg.Attach.WheelPredicted() {
			p.SetPredictScroll(c.latency())
		}
	}
	return p
}

// sizeOwnership is the attachment's size ownership as it attached, nil when
// the worker does not track it.
func sizeOwnership(attached *protocol.Attached) *protocol.SizeOwnership {
	if !attached.HasFeature(protocol.CapAttachSizeOwner) {
		return nil
	}
	o := attached.SizeOwner
	return &o
}

// attachScrollbackRows is the configured cap on the scrollback an attach
// copies into the terminal ([attach] scrollback_rows; -1 for all of it).
func (c *client) attachScrollbackRows() int {
	cfg, err := config.Load(c.paths.Config)
	if err != nil {
		return config.DefaultAttachScrollbackRows
	}
	return cfg.Attach.AttachScrollbackRows()
}

// runAttachment runs one attach stream until it ends, the user detaches or
// switches, or it fails.
//
// size is the attachment's size ownership (nil if not tracked), kept up to
// date from AttachSizeOwner frames for the overlay. ech is the local echo
// of typing (nil for none): typed bytes go through it, and so do the
// worker's answers about the input field.
func (c *client) runAttachment(id string, stream *attachStream, keys <-chan []byte, parser *attachParser, winch, quit <-chan os.Signal, size *protocol.SizeOwnership, ech *echo, painter *screen.Painter) (attachResult, error) {
	var ov *overlay
	// An attachment that ends with the overlay open (a switch, the
	// session's end) leaves the alternate screen the overlay drew on.
	defer func() {
		if ov != nil {
			writeOut(ov.leave())
		}
	}()
	redraw := false
	// sendInput writes typed bytes to the stream, through the echo when
	// there is one (which may hold them back for a moment, and draw).
	sendInput := func(data []byte) error {
		if ech != nil {
			send, paint := ech.input(data)
			writeOut(paint)
			if painter != nil && len(paint) > 0 {
				painter.Overdrawn()
			}
			data = send
		}
		if len(data) == 0 {
			return nil
		}
		return stream.send(&protocol.Request{AttachInput: &protocol.Bytes{Data: data}})
	}
	// flushEcho sends what the echo holds before the attachment is left
	// or the overlay takes the keys.
	flushEcho := func() error {
		if ech == nil {
			return nil
		}
		if send := ech.suspend(); len(send) > 0 {
			return stream.send(&protocol.Request{AttachInput: &protocol.Bytes{Data: send}})
		}
		return nil
	}
	// The wheel prediction's timer takes back a guess the agent has not
	// answered (screen/predict.go); it is set after anything that may
	// have drawn or answered one.
	predictTimer := time.NewTimer(time.Hour)
	predictTimer.Stop()
	defer predictTimer.Stop()
	schedulePredict := func() {
		if painter == nil {
			return
		}
		predictTimer.Stop()
		if due := painter.PredictDue(); !due.IsZero() {
			predictTimer.Reset(max(time.Until(due), 0))
		}
	}
	for {
		// The overlay is drawn over the session's screen, which nothing
		// else writes to while it is open, so it only changes after input,
		// a resize, or the frames that follow a resize.
		if ov != nil && redraw {
			ov.draw()
		}
		redraw = false
		select {
		case chunk, ok := <-keys:
			if !ok {
				_ = flushEcho()
				return attachResult{outcome: outcomeDetached}, nil
			}
			redraw = true
			if ov != nil {
				res, closed, err := c.overlayInput(ov, chunk, stream, size, ech, painter)
				if err != nil || res != nil {
					return deref(res), err
				}
				if closed {
					ov = nil
				}
				continue
			}
			for _, action := range parser.push(chunk) {
				switch action.kind {
				case actionData:
					if painter != nil {
						writeOut(painter.Wheel(action.data))
						schedulePredict()
					}
					if err := sendInput(action.data); err != nil {
						return attachResult{}, err
					}
				case actionDetach:
					_ = flushEcho()
					return attachResult{outcome: outcomeDetached}, nil
				case actionOpenOverlay:
					if err := flushEcho(); err != nil {
						return attachResult{}, err
					}
					ov = newOverlay(c, id, size, painter)
					if err := ov.open(); err != nil {
						return attachResult{}, err
					}
				case actionPreviousSession, actionNextSession:
					next, err := c.adjacentLiveSession(id, action.kind == actionPreviousSession)
					if err != nil {
						return attachResult{}, err
					}
					if next != "" {
						_ = flushEcho()
						return attachResult{outcome: outcomeSwitch, next: next}, nil
					}
				}
				if ov != nil {
					// Like the terminal, the overlay takes over from here;
					// the rest of this chunk is dropped.
					break
				}
			}
		case <-ech.timerC():
			send, reqs := ech.tick(time.Now())
			if len(send) > 0 {
				if err := stream.send(&protocol.Request{AttachInput: &protocol.Bytes{Data: send}}); err != nil {
					return attachResult{}, err
				}
			}
			for _, req := range reqs {
				if err := stream.send(req); err != nil {
					return attachResult{}, err
				}
			}
		case <-quit:
			_ = flushEcho()
			return attachResult{outcome: outcomeDetached}, nil
		case <-winch:
			redraw = true
			if painter != nil {
				painter.Resized()
			}
			if ov != nil {
				ov.resized()
			}
			if ech != nil {
				ech.resized()
			}
			if err := stream.send(resizeRequest()); err != nil {
				return attachResult{}, err
			}
		case f := <-stream.frames:
			res, done, err := handleFrame(f, ov != nil, size, ech, stream, painter)
			if err != nil || done {
				return res, err
			}
			if ov != nil && ov.framed() {
				redraw = true
			}
			schedulePredict()
		case <-predictTimer.C:
			if paint := painter.PredictExpire(); ov == nil {
				writeOut(paint)
			}
			schedulePredict()
		}
	}
}

// Reconnection backs off from the first retry up to the maximum, and keeps
// trying until it succeeds, fails for good, or the user detaches. A laptop
// that slept for hours reconnects when it wakes.
const (
	reconnectFirstRetry = 500 * time.Millisecond
	reconnectMaxRetry   = 5 * time.Second
	// reconnectStartDaemon is the attempt at which a local CLI starts the
	// daemon itself, as any command would, if it has not come back (it
	// crashed, say). A restart or upgrade brings it back well before.
	reconnectStartDaemon = 4
)

// reconnectable reports whether an attachment that failed with err can be
// resumed by attaching again: the session is fine, only the path to it
// broke. Errors a retry cannot fix (a refused or changed key, a session that
// is gone or replaced, a protocol error) are not.
func (c *client) reconnectable(err error) bool {
	switch {
	case errors.Is(err, errSessionRestarting), errors.Is(err, errAttachDropped):
		return true
	case c.host != nil:
		return transport.IsConnectionLost(err)
	}
	// The local daemon's socket is refused or missing while it restarts.
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, io.ErrUnexpectedEOF)
}

// reattach attaches to session id again after its attachment was cut
// (cause): the connection was lost, the daemon restarted, or the session's
// worker restarted for an upgrade. It shows a status line on the
// bottom row, which the new snapshot paints over. Typed input is dropped
// rather than sent late to an agent the user cannot see; only the detach
// key acts, at any point. It gives up on errors a retry cannot fix: a
// refused or changed key, a session that is gone or was replaced by
// another under the same name (prev's UID), a protocol error.
//
// It returns the new stream and its Attached, or with a nil stream, the
// result or error that ends the attachment.
func (c *client) reattach(id string, prev attachIdentity, keys <-chan []byte, parser *attachParser, cause error) (*attachStream, *protocol.Attached, *attachResult, error) {
	detached := &attachResult{outcome: outcomeDetached}
	delay := time.Duration(0)
	for try := 1; ; try++ {
		if !errors.Is(cause, errSessionRestarting) {
			// The old connection is dead; the next stream dials a new
			// one. A restarting worker leaves the connection alone.
			c.close()
		}
		if c.host == nil && try == reconnectStartDaemon && !c.localDaemonAnswers() {
			_ = c.spawnDaemon()
		}
		writeOut(reconnectStatusBytes(c.reconnectStatus(cause), try, cause))

		retry := make(chan struct{})
		timer := time.AfterFunc(delay, func() { close(retry) })
		if !waitUnlessDetached(retry, keys, parser) {
			timer.Stop()
			return nil, nil, detached, nil
		}

		// The attempt runs on its own goroutine so the detach key works
		// while it waits; cancelling ctx ends it promptly, and it is
		// always waited for, so none outlives reattach.
		type attempt struct {
			stream   *attachStream
			attached *protocol.Attached
			ended    *protocol.SessionEnded
			err      error
		}
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan attempt, 1)
		done := make(chan struct{})
		go func() {
			defer close(done)
			var a attempt
			// The terminal already holds the scrollback the first
			// snapshot copied, so a reattach repaints only the screen.
			a.stream, a.attached, a.ended, a.err = c.connectAttach(ctx, id, prev, 0, false)
			result <- a
		}()
		finished := waitUnlessDetached(done, keys, parser)
		cancel()
		a := <-result
		switch {
		case !finished:
			if a.stream != nil {
				a.stream.leave(c.host != nil)
			}
			return nil, nil, detached, nil
		case a.err == nil && a.ended != nil:
			return nil, nil, &attachResult{outcome: outcomeEnded, ended: a.ended}, nil
		case a.err == nil:
			return a.stream, a.attached, nil, nil
		case !c.reconnectable(a.err):
			return nil, nil, nil, a.err
		}
		cause = a.err
		delay = min(max(2*delay, reconnectFirstRetry), reconnectMaxRetry)
	}
}

// waitUnlessDetached waits until done fires, dropping typed input. It
// returns false if the user pressed the detach key first, or stdin ended.
func waitUnlessDetached(done <-chan struct{}, keys <-chan []byte, parser *attachParser) bool {
	for {
		select {
		case <-done:
			return true
		case chunk, ok := <-keys:
			if !ok {
				return false
			}
			for _, action := range parser.push(chunk) {
				if action.kind == actionDetach {
					return false
				}
			}
		}
	}
}

// reconnectStatus says what happened to the attachment.
func (c *client) reconnectStatus(cause error) string {
	switch {
	case errors.Is(cause, errSessionRestarting):
		return "session restarting, reattaching"
	case c.host != nil:
		return fmt.Sprintf("connection to %s lost, reconnecting", c.host.Name)
	}
	return "connection to agtd lost, reconnecting"
}

// reconnectStatusBytes draws the reconnect status on the bottom row,
// leaving the cursor where it was.
func reconnectStatusBytes(status string, try int, cause error) []byte {
	cols, rows, _, _ := terminalGeometry()
	msg := status
	if try > 1 {
		msg += fmt.Sprintf(" (attempt %d)", try)
	}
	msg += fmt.Sprintf(": %v. Ctrl-\\ detaches", cause)
	line := []rune(" " + strings.Join(strings.Fields(msg), " ") + " ")
	if cols > 0 && len(line) > int(cols) {
		line = line[:cols]
	}
	return []byte(fmt.Sprintf("\x1b7\x1b[%d;1H\x1b[2K\x1b[7m%s\x1b[0m\x1b8", rows, string(line)))
}

func deref(r *attachResult) attachResult {
	if r == nil {
		return attachResult{}
	}
	return *r
}

// handleFrame handles one frame of a live attachment. While the overlay is
// open, output is dropped: the screen is redrawn from a snapshot when it
// closes. So is it while the echo holds keys the agent has not been seen to
// draw (echo.go). Size ownership notes update size; the worker's answers
// about the input field go to the echo.
func handleFrame(f frame, overlayOpen bool, size *protocol.SizeOwnership, ech *echo, stream *attachStream, painter *screen.Painter) (attachResult, bool, error) {
	switch {
	case f.err != nil:
		return attachResult{}, true, f.err
	case f.resp == nil:
		return attachResult{}, true, errAttachDropped
	case f.resp.AttachFrame != nil:
		if painter == nil {
			return attachResult{}, true, errors.New("unexpected screen frame")
		}
		return attachResult{}, false, paintFrame(f.resp.AttachFrame, painter, ech, stream, overlayOpen)
	case f.resp.EndOfStream != nil:
		return attachResult{outcome: outcomeDetached}, true, nil
	case f.resp.SessionRestarting != nil:
		return attachResult{}, true, errSessionRestarting
	case f.resp.PtyOutput != nil:
		data := f.resp.PtyOutput.Data
		if !overlayOpen && !ech.holding() {
			writeOut(data)
		}
		if ech != nil {
			ech.output(len(data))
		}
		return attachResult{}, false, nil
	case f.resp.SessionEnded != nil:
		return attachResult{outcome: outcomeEnded, ended: f.resp.SessionEnded}, true, nil
	case f.resp.AttachResync != nil:
		// The worker dropped output while this client lagged; the
		// snapshot replaces the screen, and output continues from it.
		if !overlayOpen && !ech.holding() {
			writeOut(resyncBytes(f.resp.AttachResync.Data))
			ech.repainted()
		}
		if ech != nil {
			ech.output(len(f.resp.AttachResync.Data))
		}
		return attachResult{}, false, nil
	case f.resp.AttachSizeOwner != nil:
		noteSizeOwner(size, *f.resp.AttachSizeOwner, ech)
		return attachResult{}, false, nil
	case f.resp.Field != nil:
		if ech == nil {
			return attachResult{}, true, errors.New("unexpected input field response")
		}
		send, paint := ech.fieldAnswer(f.resp.Field)
		if !overlayOpen {
			writeOut(paint)
		}
		if painter != nil && len(paint) > 0 {
			painter.Overdrawn()
		}
		if len(send) > 0 {
			if err := stream.send(&protocol.Request{AttachInput: &protocol.Bytes{Data: send}}); err != nil {
				return attachResult{}, true, err
			}
		}
		return attachResult{}, false, nil
	case f.resp.AttachSnapshot != nil:
		if ech == nil {
			return attachResult{}, true, errors.New("unexpected attach snapshot response during passthrough")
		}
		// Asked for by the echo, to repaint the screen once the agent
		// has drawn everything typed.
		if paint := ech.snapshotAnswer(f.resp.AttachSnapshot.Data); !overlayOpen {
			writeOut(paint)
		}
		return attachResult{}, false, nil
	case f.resp.Error != nil:
		return attachResult{}, true, errors.New(f.resp.Error.Message)
	}
	return attachResult{}, true, fmt.Errorf("unexpected response: %s", describeResponse(f.resp))
}

// noteSizeOwner records a size ownership note (size is nil when it is not
// tracked). A new session size (another attachment took it, or this one
// did from the overlay) moves the agent's field as a SIGWINCH does, so the
// echo hears of it.
func noteSizeOwner(size *protocol.SizeOwnership, note protocol.SizeOwnership, ech *echo) {
	if size == nil {
		return
	}
	if ech != nil && (size.Geometry.Cols != note.Geometry.Cols || size.Geometry.Rows != note.Geometry.Rows) {
		ech.resized()
	}
	*size = note
}

// paintFrame paints a screen frame: the field in it reconciles the echo,
// whose predicted rows go over the frame while keys are still in flight,
// then the frame is acknowledged, asking for the field in the next ones
// while there is an echo. Keys a field proves missing after a reattach
// are sent again.
func paintFrame(fr *protocol.Frame, painter *screen.Painter, ech *echo, stream *attachStream, overlayOpen bool) error {
	var send, overlay []byte
	if ech != nil {
		if fr.Field != nil {
			send, _ = ech.frameField(fr.Field, fr.AckedBy())
		}
		overlay = ech.overlay()
	}
	if paint := painter.Apply(fr, overlay); !overlayOpen {
		writeOut(paint)
	}
	if ech != nil && fr.Field != nil {
		ech.screenShown(fr.Field, painter.Rows())
	}
	if len(send) > 0 {
		if err := stream.send(&protocol.Request{AttachInput: &protocol.Bytes{Data: send}}); err != nil {
			return err
		}
	}
	return ackFrame(stream, fr.Seq, ech, painter)
}

// ackFrame acknowledges frame seq, asking for the field in the next ones
// while there is an echo, which notes the ack: the frames that follow it
// show the keys sent before it.
func ackFrame(stream *attachStream, seq uint32, ech *echo, painter *screen.Painter) error {
	if err := stream.send(&protocol.Request{AttachFrameAck: &protocol.FrameAck{Seq: seq, Field: ech != nil || (painter != nil && painter.WantsField())}}); err != nil {
		return err
	}
	ech.frameAcked(seq)
	return nil
}

// overlayInput feeds input to the open overlay. closed reports that it
// closed and the screen was restored.
func (c *client) overlayInput(ov *overlay, chunk []byte, stream *attachStream, size *protocol.SizeOwnership, ech *echo, painter *screen.Painter) (res *attachResult, closed bool, err error) {
	for _, ev := range ov.keys.push(chunk) {
		out, err := ov.handle(ev)
		if err != nil {
			return nil, false, err
		}
		switch out.kind {
		case overlayTookSize:
			// The worker resizes before it answers the snapshot asked
			// for next, so the restored screen has the new size.
			cols, rows, pw, ph := terminalGeometry()
			if err := stream.send(&protocol.Request{AttachTakeSize: &protocol.Geometry{Cols: cols, Rows: rows, PixelWidth: pw, PixelHeight: ph}}); err != nil {
				return nil, false, err
			}
			return closeOverlay(ov, stream, size, ech, painter)
		case overlayClose:
			return closeOverlay(ov, stream, size, ech, painter)
		case overlaySubmit:
			// Enter goes on the attach stream, behind the keys the echo
			// sent when the overlay opened. The worker encodes it for
			// the program's keyboard mode when it can; an older one gets
			// a carriage return.
			req := &protocol.Request{AttachInput: &protocol.Bytes{Data: []byte("\r")}}
			if stream.composes {
				req = &protocol.Request{AttachCompose: &protocol.Bytes{}}
			}
			if err := stream.send(req); err != nil {
				return nil, false, err
			}
			next, err := c.attentionSession(ov.session)
			if err != nil {
				return nil, false, err
			}
			if next != "" {
				return &attachResult{outcome: outcomeSwitch, next: next}, false, nil
			}
			// Nothing else needs attention: stay, and watch the answer.
			return closeOverlay(ov, stream, size, ech, painter)
		case overlaySwitch:
			return &attachResult{outcome: outcomeSwitch, next: out.session}, false, nil
		case overlayDetach:
			return &attachResult{outcome: outcomeDetached}, false, nil
		}
	}
	return nil, false, nil
}

// closeOverlay restores the session's screen once the overlay has closed.
func closeOverlay(ov *overlay, stream *attachStream, size *protocol.SizeOwnership, ech *echo, painter *screen.Painter) (*attachResult, bool, error) {
	writeOut(ov.leave())
	restored, ended, err := restoreSnapshot(stream, size, ech, painter)
	if err != nil {
		return nil, false, err
	}
	if ended != nil {
		return &attachResult{outcome: outcomeEnded, ended: ended}, false, nil
	}
	writeOut([]byte("\x1b[?25h"))
	writeOut(attachStartupBytes(restored))
	ech.repainted()
	return nil, true, nil
}

// restoreSnapshot asks for the current screen after the overlay closes,
// skipping output that arrives first. Input waits (bounded, in the stdin
// channel) until it is done. A snapshot the echo asked for before the
// overlay opened is answered first, and is not the one wanted.
func restoreSnapshot(stream *attachStream, size *protocol.SizeOwnership, ech *echo, painter *screen.Painter) ([]byte, *protocol.SessionEnded, error) {
	if err := stream.send(&protocol.Request{AttachSnapshot: protocol.Empty}); err != nil {
		return nil, nil, err
	}
	for f := range stream.frames {
		switch {
		case f.err != nil:
			return nil, nil, f.err
		case f.resp == nil:
			return nil, nil, errAttachDropped
		case f.resp.AttachFrame != nil:
			// Under frames the snapshot is a full frame; one in flight
			// before the request is acknowledged and superseded.
			fr := f.resp.AttachFrame
			if painter == nil {
				return nil, nil, errors.New("unexpected screen frame")
			}
			if ech != nil && fr.Field != nil {
				ech.frameField(fr.Field, fr.AckedBy())
			}
			if err := ackFrame(stream, fr.Seq, ech, painter); err != nil {
				return nil, nil, err
			}
			if fr.Full {
				painter.Reset()
				return painter.Apply(fr, nil), nil, nil
			}
		case f.resp.SessionRestarting != nil:
			return nil, nil, errSessionRestarting
		case f.resp.AttachSnapshot != nil:
			if ech != nil && ech.snapshotDue {
				ech.snapshotAnswer(nil)
				continue
			}
			return f.resp.AttachSnapshot.Data, nil, nil
		case f.resp.Field != nil:
			if ech != nil {
				ech.fieldAnswer(f.resp.Field)
			}
		case f.resp.PtyOutput != nil, f.resp.AttachResync != nil:
			// Superseded by the snapshot asked for.
		case f.resp.AttachSizeOwner != nil:
			noteSizeOwner(size, *f.resp.AttachSizeOwner, ech)
		case f.resp.SessionEnded != nil:
			return nil, f.resp.SessionEnded, nil
		case f.resp.EndOfStream != nil:
			return nil, nil, errors.New("attach stream ended while restoring snapshot")
		case f.resp.Error != nil:
			return nil, nil, errors.New(f.resp.Error.Message)
		default:
			return nil, nil, fmt.Errorf("unexpected response while restoring attach snapshot: %s", describeResponse(f.resp))
		}
	}
	return nil, nil, errors.New("agtd closed the attach connection")
}

func resizeRequest() *protocol.Request {
	cols, rows, pw, ph := terminalGeometry()
	return &protocol.Request{AttachResize: &protocol.Geometry{Cols: cols, Rows: rows, PixelWidth: pw, PixelHeight: ph}}
}

// attachStartupBytes paints a snapshot after attachRepaintSequence (mouse
// reporting off, a clear): at attach, after
// a reattach, and after the overlay closes. The keyboard flags are not
// pushed here (attachEnterSequence is written once per attachment): the
// snapshot sets the agent's flags on the level already pushed, and every
// push would be one more level for the detach to pop.
func attachStartupBytes(snapshot []byte) []byte {
	return append([]byte(attachRepaintSequence), snapshot...)
}

// resyncBytes repaints the screen from a resync snapshot.
func resyncBytes(snapshot []byte) []byte {
	return append([]byte(attachRepaintSequence), snapshot...)
}

// terminalTitleBytes sets both the window and icon titles.
func terminalTitleBytes(title string) []byte {
	return []byte("\x1b]0;" + title + "\x07\x1b]2;" + title + "\x07")
}

// writeOut writes to the terminal. Errors are ignored: there is nowhere to
// report them, and a terminal that went away ends the attachment through
// its input.
func writeOut(b []byte) {
	for len(b) > 0 {
		n, err := os.Stdout.Write(b)
		if err != nil {
			return
		}
		b = b[n:]
	}
}

// attentionSession is the running session other than id that most needs
// attention: the first in list order (attention first, the most recently
// raised first) with attention raised, or "" when none has.
func (c *client) attentionSession(id string) (string, error) {
	sessions, err := c.listSessions(0)
	if err != nil {
		return "", err
	}
	return attentionSessionIn(orderedSessions(sessions), id), nil
}

func attentionSessionIn(sessions []*session.Record, id string) string {
	for _, s := range sessions {
		if s.SessionID != id && s.Status == session.StatusRunning && s.Attention.Rank() > 0 {
			return s.SessionID
		}
	}
	return ""
}

// adjacentLiveSession is the running session before or after id in list
// order, wrapping around, or "" when there is none.
func (c *client) adjacentLiveSession(id string, previous bool) (string, error) {
	sessions, err := c.listSessions(0)
	if err != nil {
		return "", err
	}
	var running []session.Record
	for _, s := range sessions {
		if s.Status == session.StatusRunning {
			running = append(running, s)
		}
	}
	return adjacentSessionIn(orderedSessions(running), id, previous), nil
}

func adjacentSessionIn(sessions []*session.Record, id string, previous bool) string {
	if len(sessions) <= 1 {
		return ""
	}
	current := -1
	for i, s := range sessions {
		if s.SessionID == id {
			current = i
		}
	}
	if current < 0 {
		return ""
	}
	target := (current + 1) % len(sessions)
	if previous {
		target = (current + len(sessions) - 1) % len(sessions)
	}
	if target == current {
		return ""
	}
	return sessions[target].SessionID
}

func formatSessionEnd(e *protocol.SessionEnded) string {
	switch e.Status {
	case session.StatusFailed:
		if e.Error != nil {
			return fmt.Sprintf("session %s failed: %s", e.SessionID, *e.Error)
		}
		return fmt.Sprintf("session %s failed", e.SessionID)
	case session.StatusExited, session.StatusUnknownRecovered:
		if e.ExitCode != nil {
			return fmt.Sprintf("session %s finished (exit %d)", e.SessionID, *e.ExitCode)
		}
		return fmt.Sprintf("session %s finished", e.SessionID)
	}
	return fmt.Sprintf("session %s ended", e.SessionID)
}
