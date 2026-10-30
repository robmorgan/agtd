package cli

import (
	"errors"
	"os"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"agtd.sh/internal/protocol"
	"agtd.sh/internal/screen"
	"agtd.sh/internal/session"
	"agtd.sh/internal/transport"
)

func TestAttachSequences(t *testing.T) {
	if attachRestoreSequence != "\x1b[?2026l\x1b[?1049l\x1b[?9l\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1005l\x1b[?1006l\x1b[?1015l\x1b[?1016l\x1b[?2004l\x1b[?1004l\x1b[<32u\x1b[>4m\x1b[?7h\x1b[m\x1b[0 q\x1b[?25h" {
		t.Fatal("restore sequence changed")
	}
	// A snapshot is painted without pushing keyboard flags: the push is
	// once per attachment, and the detach empties the stack.
	got := string(attachStartupBytes([]byte("snapshot")))
	if got != "\x1b[?9l\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1005l\x1b[?1006l\x1b[?1015l\x1b[?1016l\x1b[2J\x1b[Hsnapshot" {
		t.Fatalf("startup %q", got)
	}
	// Every repaint turns mouse reporting off before the snapshot, which
	// turns back on the modes the agent has: one it left while its output
	// was held or unseen does not stay on here. Focus events stay as they
	// are, since turning them on makes the terminal report the focus.
	if resync := string(resyncBytes([]byte("snapshot"))); resync != got {
		t.Fatalf("resync %q", resync)
	}
	if attachEnterSequence != "\x1b[>1u" {
		t.Fatal("enter sequence changed")
	}
	if strings.Contains(got, "\x1b[?1049") {
		t.Fatal("attach enters the alternate screen")
	}
	if got := string(terminalTitleBytes("demo - " + attachExitTitle)); got != "\x1b]0;demo - agtd\x07\x1b]2;demo - agtd\x07" {
		t.Fatalf("title %q", got)
	}
}

func TestAdjacentSession(t *testing.T) {
	a, b, c := demo("alpha"), demo("beta"), demo("gamma")
	ordered := []*session.Record{&a, &b, &c}
	for _, tc := range []struct {
		current  string
		previous bool
		want     string
	}{{"alpha", false, "beta"}, {"gamma", false, "alpha"}, {"alpha", true, "gamma"}, {"beta", true, "alpha"}, {"missing", false, ""}} {
		if got := adjacentSessionIn(ordered, tc.current, tc.previous); got != tc.want {
			t.Errorf("%s previous=%v: got %q want %q", tc.current, tc.previous, got, tc.want)
		}
	}
	if got := adjacentSessionIn(ordered[:1], "alpha", false); got != "" {
		t.Fatalf("single session: %q", got)
	}
}

// Submit and switch goes to the running session, other than the current
// one, that most needs attention in list order, or nowhere.
func TestAttentionSession(t *testing.T) {
	a, b, c := demo("alpha"), demo("beta"), demo("gamma")
	b.Attention, c.Attention = session.AttentionAction, session.AttentionNotice
	ordered := []*session.Record{&b, &c, &a}
	if got := attentionSessionIn(ordered, "alpha"); got != "beta" {
		t.Fatalf("from alpha: %q", got)
	}
	if got := attentionSessionIn(ordered, "beta"); got != "gamma" {
		t.Fatalf("from beta: %q", got)
	}
	b.Status = session.StatusExited
	if got := attentionSessionIn(ordered, "alpha"); got != "gamma" {
		t.Fatalf("beta exited: %q", got)
	}
	c.Attention = session.AttentionInfo
	if got := attentionSessionIn(ordered, "alpha"); got != "" {
		t.Fatalf("nothing needs attention: %q", got)
	}
}

func TestFormatSessionEnd(t *testing.T) {
	code := int32(3)
	msg := "spawn failed"
	hostile := "bad\x1b]0;owned\x07"
	for _, tc := range []struct {
		e    protocol.SessionEnded
		want string
	}{
		{protocol.SessionEnded{SessionID: "a", Status: session.StatusExited, ExitCode: &code}, "session a finished (exit 3)"},
		{protocol.SessionEnded{SessionID: "a", Status: session.StatusExited}, "session a finished"},
		{protocol.SessionEnded{SessionID: "a", Status: session.StatusFailed, Error: &msg}, "session a failed: spawn failed"},
		{protocol.SessionEnded{SessionID: "a", Status: session.StatusFailed}, "session a failed"},
		{protocol.SessionEnded{SessionID: "a", Status: session.StatusFailed, Error: &hostile}, "session a failed: " + escapeControls(hostile)},
		{protocol.SessionEnded{SessionID: "a", Status: session.StatusUnknownRecovered, ExitCode: &code}, "session a was lost (its worker exited while agtd was down)"},
		{protocol.SessionEnded{SessionID: "a", Status: session.StatusRunning}, "session a ended"},
	} {
		if got := formatSessionEnd(&tc.e); got != tc.want {
			t.Errorf("got %q want %q", got, tc.want)
		}
	}
}

func TestOverlayKeys(t *testing.T) {
	o := newOverlay(testClient(t), "alpha", nil, nil)
	if out, err := o.handle(keyEvent{code: keyChar, r: 'y', mods: modCtrl}); err != nil || out.kind != overlayStay {
		t.Fatalf("ctrl-y: %+v %v", out, err)
	}
	if out, _ := o.handle(key(keyEsc)); out.kind != overlayClose {
		t.Fatalf("esc: %+v", out)
	}
	if items := o.filteredPalette(); items[0].title == submitSwitchItem.title {
		t.Fatal("Submit and switch offered without [attach] submit_and_switch")
	}
	// Ctrl-Y, Enter is submit and switch, when offered.
	o.submitSwitch = true
	if out, err := o.handle(key(keyEnter)); err != nil || out.kind != overlaySubmit {
		t.Fatalf("enter on the fresh palette: %+v %v", out, err)
	}
	o.handle(char('n'))
	o.handle(char('e'))
	if items := o.filteredPalette(); len(items) != 1 || items[0].title != "New session" {
		t.Fatalf("filtered %+v", items)
	}
	o.handle(key(keyEnter))
	if o.mode != overlayNewSession || o.agentInput != "claude" {
		t.Fatalf("new session: %v %q", o.mode, o.agentInput)
	}
	o.handle(char('B'))
	o.handle(key(keyTab))
	o.handle(key(keyBackspace))
	if o.nameInput != "B" || o.agentInput != "claud" {
		t.Fatalf("fields %q %q", o.nameInput, o.agentInput)
	}
	o.handle(key(keyEnter))
	if !strings.Contains(o.toast, "invalid session name") {
		t.Fatalf("toast %q", o.toast)
	}
	o.handle(key(keyEsc))
	if o.mode != overlayPalette {
		t.Fatalf("esc from form: %v", o.mode)
	}
}

func TestOverlayDraws(t *testing.T) {
	o := newOverlay(testClient(t), "alpha", nil, nil)
	o.submitSwitch = true
	o.sessions = []session.Record{demo("alpha"), demo("beta")}
	body := o.body(60, 20)
	if got := stripANSI(body[1].render(60, o.theme)); !strings.HasPrefix(got, " Commands ") || !strings.Contains(got, "/alpha  ") || strings.Contains(got, "╱") {
		t.Fatalf("title %q", got)
	}
	o.latency = func() time.Duration { return 8240 * time.Microsecond }
	if got := stripANSI(o.body(60, 20)[1].render(60, o.theme)); !strings.Contains(got, "/alpha  8.2ms  ") {
		t.Fatalf("title with latency %q", got)
	}
	for d, want := range map[time.Duration]string{
		400 * time.Microsecond: "0.4ms",
		143 * time.Millisecond: "143ms",
	} {
		if got := formatOverlayLatency(d); got != want {
			t.Errorf("formatOverlayLatency(%v) = %q, want %q", d, got, want)
		}
	}
	if got := stripANSI(body[3].render(60, o.theme)); !strings.HasPrefix(got, " ❯ ") || runeLen(got) != 60 {
		t.Fatalf("prompt %q", got)
	}
	if got := stripANSI(body[5].render(60, o.theme)); !strings.HasPrefix(got, "▌ Submit and switch") || !strings.HasSuffix(got, "↵ ") {
		t.Fatalf("first item %q", got)
	}
	if got := stripANSI(body[6].render(60, o.theme)); !strings.HasPrefix(got, "  Switch session") || !strings.HasSuffix(got, "s ") {
		t.Fatalf("second item %q", got)
	}
	if got := stripANSI(body[7].render(60, o.theme)); !strings.HasPrefix(got, "  New session") {
		t.Fatalf("third item %q", got)
	}
	if got := stripANSI(body[18].render(60, o.theme)); !strings.Contains(got, "enter run") {
		t.Fatalf("hints %q", got)
	}
	o.mode = overlaySwitcher
	if got := stripANSI(o.body(60, 20)[5].render(60, o.theme)); !strings.Contains(got, "beta") || !strings.Contains(got, "/tmp/beta") {
		t.Fatalf("switcher row %q", got)
	}
	// Value: protects=an overlay row with wide characters fills its width in cells and never wraps; fails_when=the shared renderer counts runes again; why_new=theme_test covers render, not the overlay's rows; seam=none; no_claim=a terminal's own width table
	wide := demo("beta")
	wide.Cwd = "/tmp/日本語のとても長いディレクトリの名前です/もっと深いところ"
	o.sessions = []session.Record{demo("alpha"), wide}
	if got := stripANSI(o.body(60, 20)[6].render(60, o.theme)); cellWidth(got) != 60 || !strings.Contains(got, "日本語") {
		t.Fatalf("switcher row with wide characters is %d cells: %q", cellWidth(got), got)
	}
	for d, want := range map[time.Duration]string{
		40 * time.Second:                              "40s",
		12 * time.Minute:                              "12m",
		3*time.Hour + 5*time.Minute:                   "3h5m",
		51*time.Hour + 44*time.Minute + 9*time.Second: "2d3h44m",
		48 * time.Hour:                                "2d",
	} {
		if got := formatUptime(d); got != want {
			t.Errorf("formatUptime(%v) = %q, want %q", d, got, want)
		}
	}
	o.mode = overlayStopConfirm
	if got := stripANSI(o.body(60, 20)[5].render(60, o.theme)); !strings.Contains(got, "Stop") || !strings.Contains(got, "Cancel") {
		t.Fatalf("stop buttons %q", got)
	}
	if x, y, w, h := overlayRect(100, 30); x != 10 || y != 3 || w != 80 || h != 24 {
		t.Fatalf("rect %d %d %d %d", x, y, w, h)
	}
	if got := wrapText("Press Enter to stop the current session, or Esc to cancel.", 20); len(got) != 3 || got[0] != "Press Enter to stop" {
		t.Fatalf("wrap %q", got)
	}
}

// The overlay offers to take the size only while another attachment owns
// it, and choosing that closes it with overlayTookSize.
func TestOverlayTakeSize(t *testing.T) {
	titles := func(o *overlay) []string {
		var out []string
		for _, item := range o.filteredPalette() {
			out = append(out, item.title)
		}
		return out
	}
	if got := titles(newOverlay(testClient(t), "alpha", nil, nil)); slices.Contains(got, takeSizeItem.title) {
		t.Fatalf("take-size offered without size ownership: %v", got)
	}
	owner := &protocol.SizeOwnership{Owner: true, OwnerID: "attach-1"}
	if got := titles(newOverlay(testClient(t), "alpha", owner, nil)); slices.Contains(got, takeSizeItem.title) {
		t.Fatalf("take-size offered to the owner: %v", got)
	}
	pending := &protocol.SizeOwnership{OwnerID: "attach-1", Geometry: protocol.Geometry{Cols: 100, Rows: 30}}
	o := newOverlay(testClient(t), "alpha", pending, nil)
	for _, r := range "take" {
		o.handle(char(r))
	}
	if got := titles(o); len(got) != 1 || got[0] != takeSizeItem.title {
		t.Fatalf("filtered %v", got)
	}
	if out, err := o.handle(key(keyEnter)); err != nil || out.kind != overlayTookSize {
		t.Fatalf("enter: %+v %v", out, err)
	}
	if got := len(paletteItems); got != 4 {
		t.Fatalf("the take-size item leaked into the shared palette: %d items", got)
	}

	if got := sizeDetails(pending); !strings.Contains(got, "100x30, set by attach-1") {
		t.Fatalf("pending details %q", got)
	}
	if got := sizeDetails(&protocol.SizeOwnership{Owner: true, Geometry: protocol.Geometry{Cols: 80, Rows: 24}}); !strings.Contains(got, "80x24, set by this terminal") {
		t.Fatalf("owner details %q", got)
	}
}

// Size notes update the attachment's ownership without ending it.
func TestSizeOwnerFrames(t *testing.T) {
	size := &protocol.SizeOwnership{OwnerID: "attach-1"}
	note := protocol.SizeOwnership{Owner: true, OwnerID: "attach-2", Geometry: protocol.Geometry{Cols: 90, Rows: 20}}
	for _, overlay := range []bool{false, true} {
		*size = protocol.SizeOwnership{}
		if _, done, err := handleFrame(frame{resp: &protocol.Response{AttachSizeOwner: &note}}, overlay, size, nil, nil, nil); done || err != nil || *size != note {
			t.Fatalf("size note (overlay %v): done %v err %v size %+v", overlay, done, err, size)
		}
	}
	if _, done, err := handleFrame(frame{resp: &protocol.Response{AttachSizeOwner: &note}}, false, nil, nil, nil, nil); done || err != nil {
		t.Fatalf("size note without tracking: done %v err %v", done, err)
	}
}

// The palette's Detach detaches, as Ctrl-\ does.
func TestOverlayDetach(t *testing.T) {
	o := newOverlay(testClient(t), "alpha", nil, nil)
	for _, r := range "detach" {
		o.handle(char(r))
	}
	if got := o.filteredPalette(); len(got) != 1 || got[0].title != "Detach" {
		t.Fatalf("filtered %v", got)
	}
	// Detach is the last command, whatever else is offered.
	pending := &protocol.SizeOwnership{OwnerID: "attach-1"}
	all := newOverlay(testClient(t), "alpha", pending, nil)
	all.submitSwitch = true
	if got := all.filteredPalette(); got[len(got)-1].title != "Detach" || got[len(got)-2].title != takeSizeItem.title {
		t.Fatalf("palette %v", got)
	}
	if out, err := o.handle(key(keyEnter)); err != nil || out.kind != overlayDetach {
		t.Fatalf("enter: %+v %v", out, err)
	}
	if got := testClient(t).sessionAddress("alpha"); got != "local/alpha" {
		t.Fatalf("address %q", got)
	}
	// Value: protects="detached from HOST/NAME" names the remote host to
	// attach again, with its controls escaped; fails_when=sessionAddress
	// says local/ for a remote session or writes a host name's controls to
	// the terminal; why_new=only the local address was checked; seam=none;
	// no_claim=the end-to-end detach line (TestAttachEndToEnd).
	remote := testClient(t)
	remote.host = &transport.Host{Name: "devbox\x1b]0;x\a"}
	if got := remote.sessionAddress("alpha"); !strings.HasPrefix(got, "devbox") || !strings.HasSuffix(got, "/alpha") || strings.ContainsAny(got, "\x1b\a") {
		t.Fatalf("remote address %q", got)
	}
}

// The overlay opens the alternate screen only over a session painted on
// the primary one, whose view would scroll the overlay away, and leaves
// it as it closes; over an agent on the alternate screen it stays there.
func TestOverlayOwnsTheAlternateScreen(t *testing.T) {
	// Value: protects=closing the overlay over Claude Code (alternate
	// screen) does not drop the terminal to the primary screen, and over a
	// shell it returns from the alternate screen it opened;
	// fails_when=ownsAlt ignores the painter's screen or leave() writes
	// ?1049l without having opened it; why_new=no test covered the
	// overlay's alternate screen; seam=none; no_claim=the backdrop drawn
	// under it (draw writes to the terminal; manual/e2e).
	frame := func(prefix string) *screen.Painter {
		p := screen.New()
		p.Apply(&protocol.Frame{Seq: 1, Full: true, Cols: 10, Lines: 2, Prefix: []byte(prefix), Rows: []protocol.FrameRow{{Index: 0, VT: []byte("$ ")}}}, nil)
		return p
	}
	for _, c := range []struct {
		name    string
		painter *screen.Painter
		leave   string
	}{
		{"raw output", nil, ""},
		{"no frame yet", screen.New(), ""},
		{"primary screen", frame("\x1b[H"), "\x1b[?1049l"},
		{"alternate screen", frame("\x1b[?1049h\x1b[H"), ""},
	} {
		o := newOverlay(testClient(t), "alpha", nil, c.painter)
		if got := string(o.leave()); got != c.leave {
			t.Errorf("%s: leave %q, want %q", c.name, got, c.leave)
		}
		if got := o.framed(); got != (c.painter != nil) {
			t.Errorf("%s: framed %v", c.name, got)
		}
	}
}

// Value: protects=the overlay's Session Details never hand a daemon's session name, agent or size owner to the terminal raw; fails_when=sessionDetails or sizeDetails drops an escapeControls; why_new=details tests use clean fields only; seam=none; no_claim=attention level (only known levels reach the line)
// Session Details show a daemon's fields escaped, so a hostile record
// cannot clear the screen or set the title from the overlay.
func TestSessionDetailsAreEscaped(t *testing.T) {
	now := time.Now()
	s := demo("ev\x1b[2Jil")
	s.Agent = "co\x1b]0;t\x07dex"
	details := sessionDetails(&s, now) + "\n" + sizeDetails(&protocol.SizeOwnership{OwnerID: "at\x1b[1Atach-1", Geometry: protocol.Geometry{Cols: 80, Rows: 24}})
	if strings.ContainsAny(details, "\x1b\x07") {
		t.Fatalf("controls reached the details: %q", details)
	}
	for _, want := range []string{"ev", "il", "co", "dex", "at", "tach-1"} {
		if !strings.Contains(details, want) {
			t.Fatalf("details lost %q: %q", want, details)
		}
	}
}

// Value: protects=a write that failed because the worker closed the stream ends the attachment as the frames before the close say (the session's end, a detach, a worker restart, the worker's error, a read error) and otherwise reattaches (errAttachDropped, so the daemon decides), waiting while frames keep coming and never past the detach key, stdin's end or the terminal closing; fails_when=a frame is misread, an unexplained failure is not a drop, a frame an exited reader queued is missed, an exited reader or a silent stream is waited on too long, a slow backlog is cut at a fixed timeout, typed input ends the wait, or a detach is ignored; why_new=the e2e Stop test covers only the ended case, with real timing; seam=none; no_claim=reconnectable's classification of the error (not called here)
func TestExplainFailure(t *testing.T) {
	exited := make(chan struct{})
	close(exited)
	readErr := errors.New("connection reset")
	explain := func(stream *attachStream, keys <-chan []byte, quit <-chan os.Signal) (*attachResult, error) {
		return stream.explainFailure(syscall.EPIPE, false, keys, &attachParser{}, quit)
	}
	for _, c := range []struct {
		name      string
		last      frame
		hasResult bool
		outcome   attachOutcome
		wantErr   error
	}{
		{"session ended", frame{resp: &protocol.Response{SessionEnded: &protocol.SessionEnded{SessionID: "demo"}}}, true, outcomeEnded, nil},
		{"detached elsewhere", frame{resp: &protocol.Response{EndOfStream: protocol.Empty}}, true, outcomeDetached, nil},
		{"session restarting", frame{resp: &protocol.Response{SessionRestarting: &protocol.SessionRestarting{}}}, false, 0, errSessionRestarting},
		{"read error", frame{err: readErr}, false, 0, readErr},
		{"no final frame", frame{}, false, 0, errAttachDropped},
	} {
		// The reader still running, then already exited with the frames
		// queued: then both cases are ready, and select picks either, so
		// each is run more than once.
		for i, readerExited := range []chan struct{}{nil, exited, exited, exited, exited} {
			stream := &attachStream{frames: make(chan frame, 2), exited: readerExited}
			stream.frames <- frame{resp: &protocol.Response{PtyOutput: &protocol.Bytes{Data: []byte("tail")}}}
			stream.frames <- c.last
			res, err := explain(stream, nil, nil)
			if (res != nil) != c.hasResult || (res != nil && (res.outcome != c.outcome || (c.outcome == outcomeEnded) != (res.ended != nil))) || !errors.Is(err, c.wantErr) {
				t.Errorf("%s (run %d): result %+v, err %v; want result %v (%v), err %v", c.name, i, res, err, c.hasResult, c.outcome, c.wantErr)
			}
		}
	}
	stream := &attachStream{frames: make(chan frame, 1)}
	stream.frames <- frame{resp: protocol.ErrorResponsef("worker refused")}
	if res, err := explain(stream, nil, nil); res != nil || err == nil || err.Error() != "worker refused" {
		t.Errorf("error frame: result %+v, err %v; want the worker's error", res, err)
	}

	// A reader that exited with nothing queued: the stream ended without a
	// word, at once, so it was dropped, the write's error kept for the record.
	stream = &attachStream{frames: make(chan frame, 1), exited: exited}
	start := time.Now()
	if res, err := explain(stream, nil, nil); res != nil || !errors.Is(err, errAttachDropped) || !errors.Is(err, syscall.EPIPE) {
		t.Errorf("exited reader: result %+v, err %v; want a drop", res, err)
	}
	if waited := time.Since(start); waited >= explainIdle { // gstack-timing-ok: asserts the wait was skipped, not a speed
		t.Errorf("waited %v on a reader that had exited", waited)
	}

	// A write that found the connection gone reads only what is queued,
	// at once, though the reader has not noticed yet.
	start = time.Now()
	stream = &attachStream{frames: make(chan frame)}
	if res, err := stream.explainFailure(syscall.EPIPE, true, nil, &attachParser{}, nil); res != nil || !errors.Is(err, errAttachDropped) {
		t.Errorf("lost connection: result %+v, err %v; want a drop", res, err)
	}
	if waited := time.Since(start); waited >= explainIdle { // gstack-timing-ok: asserts the wait was skipped, not a speed
		t.Errorf("waited %v on a lost connection", waited)
	}
	for i := range 4 {
		stream = &attachStream{frames: make(chan frame, 1)}
		stream.frames <- frame{resp: &protocol.Response{SessionEnded: &protocol.SessionEnded{SessionID: "demo"}}}
		if res, err := stream.explainFailure(syscall.EPIPE, true, nil, &attachParser{}, nil); res == nil || res.outcome != outcomeEnded || err != nil {
			t.Errorf("lost connection, end queued (run %d): result %+v, err %v; want the session's end", i, res, err)
		}
	}

	// A live stream that says nothing is a drop once explainIdle passes.
	stream = &attachStream{frames: make(chan frame)}
	start = time.Now()
	if res, err := explain(stream, nil, nil); res != nil || !errors.Is(err, errAttachDropped) {
		t.Errorf("silent stream: result %+v, err %v; want a drop", res, err)
	}
	if waited := time.Since(start); waited >= explainMax { // gstack-timing-ok: bounds the wait by the idle period, not a speed
		t.Errorf("waited %v on a silent stream", waited)
	}

	// Typed input is dropped, and the wait goes on to the stream's word.
	keys := make(chan []byte, 1)
	keys <- []byte("a")
	stream = &attachStream{frames: make(chan frame, 1)}
	stream.frames <- frame{resp: &protocol.Response{SessionEnded: &protocol.SessionEnded{SessionID: "demo"}}}
	if res, err := explain(stream, keys, nil); res == nil || res.outcome != outcomeEnded || err != nil {
		t.Errorf("typed key: result %+v, err %v; want the session's end", res, err)
	}

	// The detach key, stdin's end and the terminal closing each detach
	// while the stream says nothing.
	detachKey := make(chan []byte, 1)
	detachKey <- []byte("\x1c")
	closedKeys := make(chan []byte)
	close(closedKeys)
	hangup := make(chan os.Signal, 1)
	hangup <- syscall.SIGHUP
	for _, c := range []struct {
		name string
		keys <-chan []byte
		quit <-chan os.Signal
	}{
		{"detach key", detachKey, nil},
		{"stdin ended", closedKeys, nil},
		{"terminal closed", nil, hangup},
	} {
		stream = &attachStream{frames: make(chan frame)}
		if res, err := explain(stream, c.keys, c.quit); res == nil || res.outcome != outcomeDetached || err != nil {
			t.Errorf("%s: result %+v, err %v; want detached", c.name, res, err)
		}
	}

	// A backlog that keeps arriving is read to its end, past explainIdle.
	stream = &attachStream{frames: make(chan frame)}
	go func() {
		for range 3 {
			time.Sleep(explainIdle * 2 / 5) // gstack-timing-ok: paces a backlog slower than one idle period in total, asserts only the outcome
			stream.frames <- frame{resp: &protocol.Response{PtyOutput: &protocol.Bytes{Data: []byte("backlog")}}}
		}
		stream.frames <- frame{resp: &protocol.Response{SessionEnded: &protocol.SessionEnded{SessionID: "demo"}}}
	}()
	if res, err := explain(stream, nil, nil); res == nil || res.outcome != outcomeEnded || err != nil {
		t.Errorf("slow backlog: result %+v, err %v; want the session's end", res, err)
	}
}
