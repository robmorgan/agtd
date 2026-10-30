package worker

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.mitchellh.com/libghostty"

	"agtd.sh/internal/protocol"
)

// continuationMaxBytes bounds the unfinished escape sequence (or UTF-8
// character) the shadow terminal tracks so a handoff can carry it to the next
// worker image exactly. A longer one (a huge OSC 52 clipboard write, say)
// makes the continuation unavailable until the parser is back at ground, and
// a handoff attempted meanwhile is refused rather than restored inexactly.
const continuationMaxBytes = 1 << 20

// terminalState wraps a libghostty terminal that shadows the PTY stream so a
// reattaching client can be rehydrated with a snapshot of the screen. Its
// parser also reports the effects agents use to ask for attention (bell,
// desktop notifications, title changes); they are collected during a feed
// and returned with it, so the FFI boundary stays one call per PTY read.
type terminalState struct {
	term    *libghostty.Terminal
	pending [][]byte
	size    libghostty.SizeReportSize
	effects terminalEffects

	// lastFeed is when PTY output last reached the terminal, which
	// decides when its scrollback is idle enough to compress.
	lastFeed time.Time
	// compressed reports that a compression pass finished while the
	// terminal's compression activity token was compressedToken; until
	// the token changes there is nothing more to compress.
	compressed      bool
	compressedToken uint64
}

// Scrollback compression. libghostty can compress the pages of scrollback
// it holds, which a session that has scrolled far keeps for as long as it
// runs: a full 10 MB scrollback goes from about 9.4 MiB resident to under
// 1 MiB. Formatting a snapshot (an attach, a resync, history) still works on
// compressed pages, takes about 10% longer, and leaves them decompressed
// until the next quiet tick compresses them again.
//
// Compression runs on the owner goroutine (the terminal is not safe to share)
// from the worker's one-second activity tick, once output has been quiet for
// compressIdleAfter, in incremental steps that stop after compressStepBudget
// so PTY output and requests queued behind it wait at most that long. A busy
// session is never compressed: its pages would be decompressed again at once.
const (
	compressIdleAfter  = 3 * time.Second
	compressStepBudget = 5 * time.Millisecond
)

// maxNotificationsPerFeed bounds the notifications kept from one PTY read; a
// program flooding them gains nothing past the first few.
const maxNotificationsPerFeed = 4

// terminalEffects are the attention-related effects of one feed.
type terminalEffects struct {
	bells         int
	notifications []libghostty.TerminalDesktopNotification
	titleChanged  bool
	// statusEvents are the program status reports (OSC 7501, already
	// validated by libghostty) and primary shell prompt starts (OSC 133 A)
	// of the feed, in stream order: a prompt ends the records of the
	// program before it but not of one that reported after it, so the
	// order must survive a coalesced read. Unlike notifications they are
	// not capped: replaying them into the record store needs them all
	// (dropping a clear, say, leaves stale records), and each costs the
	// program its own sequence bytes of PTY output, so the feed bounds
	// them.
	statusEvents []statusEvent
	// statusReplies are the terminal's answers to the program status
	// protocol's detection query. They are kept apart from the other
	// terminal replies (pending) because the worker answers this query
	// even while a client terminal is attached: the worker consumes the
	// reports itself, and the client's terminal may not support the
	// protocol. See ownerState.afterFeed.
	statusReplies [][]byte
	// clipboard is the feed's last clipboard write (OSC 52, or another
	// protocol libghostty normalizes) as the OSC 52 sequence that repeats
	// it, for frame clients to relay; nil for none. A later write
	// supersedes an earlier one, so one is all a client needs.
	clipboard []byte
}

// maxClipboardBytes bounds the text of a clipboard write relayed to frame
// clients: 75 KB is 100 KB of base64, the most pi writes through OSC 52.
// A larger write is refused rather than cut.
const maxClipboardBytes = 75_000

// clipboardSequence is the OSC 52 sequence that writes w's text to the same
// clipboard on a client terminal, or nil when w carries no text (a clear,
// or only other MIME types) or too much of it. OSC 52 carries plain text
// only.
func clipboardSequence(w libghostty.ClipboardWrite) []byte {
	var sel byte
	switch w.Location {
	case libghostty.ClipboardLocationStandard:
		sel = 'c'
	case libghostty.ClipboardLocationPrimary:
		sel = 'p'
	case libghostty.ClipboardLocationSelection:
		sel = 's'
	default:
		return nil
	}
	for _, c := range w.Contents {
		if !strings.HasPrefix(c.MIME, "text/plain") {
			continue
		}
		if len(c.Data) == 0 || len(c.Data) > maxClipboardBytes {
			return nil
		}
		seq := append([]byte("\x1b]52;"), sel, ';')
		seq = base64.StdEncoding.AppendEncode(seq, c.Data)
		return append(seq, "\x1b\\"...)
	}
	return nil
}

// statusEvent is one entry of a feed's program status stream: a report,
// or (report nil) a primary shell prompt start.
type statusEvent struct {
	report *libghostty.ProgramStatus
}

// statusDetectionPrefix starts every reply to the program status detection
// query: OSC 7501 ; ? and, since revision 0.4 of the protocol, the states
// and kinds the terminal accepts (":states=idle,working,...:kinds=...").
// Older terminals reply with the bare "?".
const statusDetectionPrefix = "\x1b]7501;?"

// isStatusDetectionReply reports whether data is exactly one detection
// reply, ended by ST or BEL, as libghostty writes it and as a client
// terminal sends it back. writeInput matches it against whole input chunks:
// a client terminal writes its reply as its own standalone write, which
// arrives as its own chunk. A reply that ever coalesced with keystrokes (or
// split across reads) would pass through as ordinary input — the
// pre-existing behavior for every other terminal reply.
func isStatusDetectionReply(data []byte) bool {
	body, ok := bytes.CutPrefix(data, []byte(statusDetectionPrefix))
	if !ok {
		return false
	}
	if b, ok := bytes.CutSuffix(body, []byte("\x1b\\")); ok {
		body = b
	} else if b, ok := bytes.CutSuffix(body, []byte("\x07")); ok {
		body = b
	} else {
		return false
	}
	// Anything else that ends or starts a sequence means more than one.
	return !bytes.ContainsAny(body, "\x1b\x07")
}

// isPassiveInput reports whether data is nothing but reports a client
// terminal sends on its own or for a gesture that answers nothing: wheel
// scrolls and mouse motion (X10, urxvt 1015 and SGR 1006 forms), focus
// changes (1004), key releases (kitty) and CSI replies to the program's
// queries. writeInput forwards such a chunk but does not count it as the
// user typing: scrolling a fullscreen agent's transcript, or the mouse
// crossing its window, must not mark a blocked prompt as answered. A click
// or any key counts, and so does a chunk mixing reports with typing.
func isPassiveInput(data []byte) bool {
	// A read of reports is short; anything longer is not classified at
	// all, so a client cannot make the worker scan a huge frame before
	// the input limit refuses it (noteInput runs first).
	if len(data) == 0 || len(data) > maxPassiveInput {
		return false
	}
	for len(data) > 0 {
		n := passiveReportLen(data)
		if n == 0 {
			return false
		}
		data = data[n:]
	}
	return true
}

// maxPassiveInput bounds the chunks isPassiveInput classifies: a burst of
// mouse motion in one read is a few hundred bytes.
const maxPassiveInput = 4096

// passiveReportLen is the length of the passive report at the start of b,
// or 0 when b does not start with one.
func passiveReportLen(b []byte) int {
	if len(b) < 3 || b[0] != 0x1b || b[1] != '[' {
		return 0
	}
	if b[2] == 'M' {
		if len(b) < 6 || !passiveMouseButton(int(b[3])-32) {
			return 0
		}
		return 6
	}
	i := 2
	for i < len(b) && b[i] >= 0x30 && b[i] <= 0x3f {
		i++
	}
	params := string(b[2:i])
	for i < len(b) && b[i] >= 0x20 && b[i] <= 0x2f {
		i++
	}
	if i >= len(b) || b[i] < 0x40 || b[i] > 0x7e {
		return 0
	}
	final, n := b[i], i+1
	switch {
	case params == "" && (final == 'I' || final == 'O'):
		return n // focus in or out
	case strings.HasPrefix(params, "<") && (final == 'M' || final == 'm'):
		if button, ok := leadingInt(params[1:]); ok && passiveMouseButton(button) {
			return n
		}
		return 0
	case final == 'M':
		if button, ok := leadingInt(params); ok && passiveMouseButton(button-32) {
			return n
		}
		return 0
	case strings.HasPrefix(params, "?") || strings.HasPrefix(params, ">"),
		final == 'R', final == 'n', final == 't', final == 'y' && b[i-1] == '$':
		// A reply to a query. A legacy modified F3 (CSI 1;5R) has the
		// form of a cursor position report and counts here too, as in
		// the CLI's decoder (internal/cli/input.go), whose tables these
		// mirror.
		return n
	}
	if _, rest, ok := strings.Cut(params, ";"); ok {
		mods, _, _ := strings.Cut(rest, ";")
		if strings.HasSuffix(mods, ":3") {
			return n // a key release
		}
	}
	return 0
}

// passiveMouseButton: wheel buttons (64 and up) and motion (32 added) edit
// nothing; presses and releases of a button may answer a prompt.
func passiveMouseButton(button int) bool {
	return button >= 0 && button&(64|32) != 0
}

// leadingInt reads the decimal number at the start of s, up to a ';'. As in
// the CLI's decoder, a number past 32 bits is not one.
func leadingInt(s string) (int, bool) {
	if end := strings.IndexByte(s, ';'); end >= 0 {
		s = s[:end]
	}
	v, err := strconv.ParseUint(s, 10, 32)
	return int(v), err == nil
}

func newTerminalState(cols, rows uint16, maxScrollbackBytes uint) (*terminalState, error) {
	s := &terminalState{size: libghostty.SizeReportSize{Rows: rows, Columns: cols}}
	term, err := libghostty.NewTerminal(
		libghostty.WithSize(cols, rows),
		libghostty.WithMaxScrollbackBytes(maxScrollbackBytes),
		libghostty.WithContinuationMaxBytes(continuationMaxBytes),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create terminal state: %w", err)
	}
	s.adopt(term)
	return s, nil
}

// adopt makes term the shadow terminal, wiring the callbacks through which
// it answers terminal queries and reports bells, notifications, title
// changes and clipboard writes.
func (s *terminalState) adopt(term *libghostty.Terminal) {
	term.SetEffectWritePty(func(_ *libghostty.Terminal, data []byte) {
		if isStatusDetectionReply(data) {
			// Bounded like notifications: a program spamming detection
			// queries gains nothing past the first few per read (it can
			// always query again), and the input queue is spared.
			if len(s.effects.statusReplies) < maxNotificationsPerFeed {
				s.effects.statusReplies = append(s.effects.statusReplies, append([]byte(nil), data...))
			}
			return
		}
		s.pending = append(s.pending, append([]byte(nil), data...))
	})
	term.SetEffectSize(func(_ *libghostty.Terminal) (libghostty.SizeReportSize, bool) {
		return s.size, true
	})
	term.SetEffectBell(func(_ *libghostty.Terminal) { s.effects.bells++ })
	term.SetEffectDesktopNotification(func(_ *libghostty.Terminal, n libghostty.TerminalDesktopNotification) {
		if len(s.effects.notifications) < maxNotificationsPerFeed {
			s.effects.notifications = append(s.effects.notifications, n)
		}
	})
	term.SetEffectTitleChanged(func(_ *libghostty.Terminal) { s.effects.titleChanged = true })
	// A clipboard write reaches a raw client in the output itself; frame
	// clients get it again from here (frameEffects). The worker has no
	// clipboard of its own, and the client terminal's own policy decides
	// whether the write lands.
	term.SetEffectClipboardWrite(func(_ *libghostty.Terminal, w libghostty.ClipboardWrite) libghostty.ClipboardWriteReply {
		seq := clipboardSequence(w)
		if seq == nil {
			return libghostty.ClipboardWriteReply{Result: libghostty.ClipboardWriteUnsupported}
		}
		s.effects.clipboard = seq
		return libghostty.ClipboardWriteReply{Result: libghostty.ClipboardWriteSuccess}
	})
	// Setting a program status handler also makes the terminal answer the
	// protocol's detection query, so programs know they can report.
	term.SetEffectProgramStatus(func(_ *libghostty.Terminal, r libghostty.ProgramStatus) {
		s.effects.statusEvents = append(s.effects.statusEvents, statusEvent{report: &r})
	})
	term.SetEffectSemanticPrompt(func(_ *libghostty.Terminal, p libghostty.TerminalSemanticPrompt) {
		if p.Kind == libghostty.SemanticPromptStart && p.PromptKind == libghostty.PromptPrimary {
			s.effects.statusEvents = append(s.effects.statusEvents, statusEvent{})
		}
	})
	s.term = term
}

// handoffState is the shadow terminal's state carried across a worker
// handoff: libghostty's binary snapshot, which is exact (every screen and
// its scrollback, cursor, modes, and the parser's unfinished input), and as
// a fallback the VT rendering a reattaching client gets, in case the next
// image cannot decode the snapshot (its format is not yet stable across
// libghostty versions).
type terminalHandoff struct {
	Snapshot []byte
	VT       []byte
	Size     libghostty.SizeReportSize
}

func (s *terminalState) handoff() (*terminalHandoff, error) {
	snapshot, err := s.term.Snapshot()
	if err != nil {
		// Most likely an unfinished sequence longer than
		// continuationMaxBytes, which the snapshot cannot carry.
		return nil, fmt.Errorf("failed to snapshot the terminal (it may be in the middle of a very long escape sequence; try again): %w", err)
	}
	vt, err := s.format(true)
	if err != nil {
		return nil, err
	}
	return &terminalHandoff{Snapshot: snapshot, VT: vt, Size: s.size}, nil
}

// restoreTerminal rebuilds the shadow terminal from a handoff, from the
// exact binary snapshot when it decodes. Otherwise it rebuilds it from the VT
// rendering, which keeps the screen and scrollback text and most modes but
// not, for example, an unfinished escape sequence, and reports why in
// inexact. err is set only if neither worked.
func restoreTerminal(h *terminalHandoff) (s *terminalState, inexact error, err error) {
	term, snapErr := decodeSnapshot(h.Snapshot)
	if snapErr == nil {
		limit := maxScrollbackBytes
		if snapErr = term.SetScrollbackMaxBytes(&limit); snapErr != nil {
			term.Close()
		}
	}
	if snapErr == nil {
		s = &terminalState{size: h.Size}
		s.adopt(term)
		return s, nil, nil
	}
	cols, rows := h.Size.Columns, h.Size.Rows
	if cols == 0 || rows == 0 {
		cols, rows = defaultPtyCols, defaultPtyRows
	}
	s, err = newTerminalState(cols, rows, maxScrollbackBytes)
	if err != nil {
		return nil, nil, errors.Join(snapErr, err)
	}
	s.size = h.Size
	s.feed(h.VT)
	return s, snapErr, nil
}

func decodeSnapshot(data []byte) (*libghostty.Terminal, error) {
	if len(data) == 0 {
		return nil, errors.New("no terminal snapshot")
	}
	d, err := libghostty.NewSnapshotDecoderBytes(data)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	// Keep tracking the parser's unfinished input in the restored
	// terminal, so the next handoff carries it too.
	if err := d.SetMaxContinuationBytes(continuationMaxBytes); err != nil {
		return nil, err
	}
	if err := d.SetRetainContinuation(true); err != nil {
		return nil, err
	}
	// A restored terminal would otherwise hold all its scrollback
	// uncompressed, however compressed it was in the previous image.
	if err := d.SetCompressHistory(true); err != nil {
		return nil, err
	}
	term, err := d.Decode()
	if err != nil {
		return nil, fmt.Errorf("failed to decode the terminal snapshot: %w", err)
	}
	return term, nil
}

// feed parses PTY output and returns any responses the terminal wants to
// write back to the PTY (mode reports, size reports, and so on), and the
// attention effects the output had.
func (s *terminalState) feed(data []byte) ([][]byte, terminalEffects) {
	s.lastFeed = time.Now()
	s.term.VTWrite(data)
	return s.take()
}

// title is the title the program last set (OSC 0/2).
func (s *terminalState) title() string {
	t, err := s.term.Title()
	if err != nil {
		return ""
	}
	return t
}

// feedUntilGround parses only as much of data as it takes for the parser to
// be between sequences again, and returns how much that was (all of data if
// it never gets there) along with any responses and effects. See
// ownerState.atGround.
func (s *terminalState) feedUntilGround(data []byte) (int, [][]byte, terminalEffects) {
	s.lastFeed = time.Now()
	stop, _ := s.term.VTWriteUntil(data, libghostty.TerminalBoundaryGround)
	n := stop.Consumed
	writes, effects := s.take()
	return n, writes, effects
}

// take returns and resets the responses and effects collected by a feed.
func (s *terminalState) take() ([][]byte, terminalEffects) {
	writes, effects := s.pending, s.effects
	s.pending, s.effects = nil, terminalEffects{}
	return writes, effects
}

// atGround reports whether the parser is between sequences and UTF-8
// characters, so a snapshot taken now is an exact boundary in the stream.
func (s *terminalState) atGround() bool {
	ground, err := s.term.VTGround()
	return err != nil || ground
}

func (s *terminalState) resize(cols, rows uint16, cellWidthPx, cellHeightPx uint32) error {
	s.size = libghostty.SizeReportSize{Rows: rows, Columns: cols, CellWidth: cellWidthPx, CellHeight: cellHeightPx}
	return s.term.Resize(cols, rows, cellWidthPx, cellHeightPx)
}

// format renders the active screen and its scrollback for history: plain
// text, or VT sequences that reproduce the styles.
func (s *terminalState) format(vt bool) ([]byte, error) {
	// Formatting reads, and so decompresses, every page of scrollback
	// without changing the compression activity token: compress again
	// at the next quiet tick.
	s.compressed = false
	if !vt {
		return formatScreen(s.term, libghostty.FormatterFormatPlain, true, allScrollback)
	}
	colors, err := changedColors(s.term)
	if err != nil {
		return nil, err
	}
	screen, err := formatScreen(s.term, libghostty.FormatterFormatVT, true, allScrollback)
	if err != nil {
		return nil, err
	}
	return append(colors, screen...), nil
}

// allScrollback asks for every retained row of scrollback.
const allScrollback = -1

// snapshot returns the VT bytes that repaint a client's terminal, already
// cleared and of the same size, as this terminal is now: the primary screen
// and up to scrollback rows of its scrollback (allScrollback for all of
// it), the alternate screen when it is active, the cursor (position,
// visibility, pen style), terminal modes, scrolling region, tab stops,
// keyboard modes, charsets and any colors the program changed.
// ARCHITECTURE.md lists what a VT stream cannot restore.
func (s *terminalState) snapshot(scrollback int) ([]byte, error) {
	out, err := changedColors(s.term)
	if err != nil {
		return nil, err
	}
	screen, err := s.term.ActiveScreen()
	if err != nil {
		return nil, err
	}
	// Each screen has its own cursor shape, and entering the alternate
	// screen can copy the primary one's: once the primary screen's shape
	// is set, the alternate one's is set too, block included.
	primaryShape := false
	if screen == libghostty.ScreenAlternate {
		primary, shape, err := s.primaryUnderAlternate(scrollback)
		if err != nil {
			return nil, err
		}
		out, primaryShape = append(out, primary...), shape
	}
	active, err := formatScreen(s.term, libghostty.FormatterFormatVT, true, scrollback)
	if err != nil {
		return nil, err
	}
	if screen == libghostty.ScreenAlternate {
		active = resetPenAfterSwitch(active)
	}
	out = append(out, active...)
	out, _, err = appendCursorShape(out, s.term, primaryShape)
	return out, err
}

// penReset puts the pen back to its defaults: SGR, hyperlink, charsets
// (G0 ASCII, shifted in) and character protection.
const penReset = "\x1b[0m\x1b]8;;\x1b\\\x1b(B\x0f\x1b[0\"q"

// resetPenAfterSwitch makes the alternate screen's contents start from a
// default pen. The formatter writes screen contents assuming one, but the
// client still has the pen the primary screen's formatting just restored:
// switching screens keeps it, and 1049 also saves it with the cursor, so it
// has to stay until the switch. The formatter emits the modes (which make
// the switch) and then the tab stops, which start by clearing them all
// (TBC, CSI 3 g); the reset goes in between.
func resetPenAfterSwitch(formatted []byte) []byte {
	i := bytes.Index(formatted, []byte("\x1b[3g"))
	if i < 0 {
		return formatted
	}
	out := make([]byte, 0, len(formatted)+len(penReset))
	out = append(out, formatted[:i]...)
	out = append(out, penReset...)
	return append(out, formatted[i:]...)
}

// appendCursorShape appends DECSCUSR for the active screen's cursor shape,
// which the formatter does not emit, and reports whether it did. A block
// cursor is left alone unless block is set: it is also what a terminal
// shows when no program has asked for a shape, so the client keeps its own
// default. The shape is only exposed through a render state, which copies
// the visible screen once.
func appendCursorShape(out []byte, term *libghostty.Terminal, block bool) ([]byte, bool, error) {
	rs, err := libghostty.NewRenderState()
	if err != nil {
		return nil, false, err
	}
	defer rs.Close()
	if err := rs.Update(term); err != nil {
		return nil, false, err
	}
	shape, err := rs.CursorVisualStyle()
	if err != nil {
		return nil, false, err
	}
	var n int
	switch shape {
	case libghostty.CursorVisualStyleBar:
		n = 6
	case libghostty.CursorVisualStyleUnderline:
		n = 4
	default:
		if !block {
			return out, false, nil
		}
		n = 2
	}
	if blinking, _ := term.Mode(libghostty.ModeCursorBlinking); blinking {
		n-- // the blinking variant of each shape
	}
	return fmt.Appendf(out, "\x1b[%d q", n), true, nil
}

// primaryUnderAlternate formats the primary screen while a program (an
// editor, or a TUI such as Claude Code) has the alternate screen up.
// libghostty's formatter only formats the active screen, so this copies the
// terminal through its binary snapshot, leaves the alternate screen in the
// copy (which restores the cursor 1049 saved), and formats that. The client
// gets the primary screen first; the alternate screen's own formatting then
// switches it over with the same mode the program used, so the primary
// screen and its scrollback are there again when the program exits.
//
// The primary screen's cursor shape is included, and shape reports whether
// it was set (see snapshot).
//
// This costs a copy of the whole terminal, scrollback included, on each
// snapshot taken while the alternate screen is up; BENCHMARKS.md has the
// numbers.
func (s *terminalState) primaryUnderAlternate(scrollback int) (out []byte, shape bool, err error) {
	data, err := s.term.Snapshot()
	if err != nil {
		return nil, false, fmt.Errorf("failed to copy terminal state: %w", err)
	}
	dec, err := libghostty.NewSnapshotDecoderBytes(data)
	if err != nil {
		return nil, false, fmt.Errorf("failed to copy terminal state: %w", err)
	}
	defer dec.Close()
	cp, err := dec.Decode()
	if err != nil {
		return nil, false, fmt.Errorf("failed to copy terminal state: %w", err)
	}
	defer cp.Close()

	// Leave the alternate screen with 1049, which switches screens without
	// copying the alternate screen's cursor (47 and 1047 copy it, shape
	// included, over the primary screen's own), then clear the other
	// alternate screen modes: a program can set more than one, and one left
	// set would make this primary screen's formatting enter the alternate
	// screen again. 1049 restores the saved cursor, which a client restores
	// the same way when the program leaves with 1049.
	cp.VTWrite([]byte("\x1b[?1049l\x1b[?1047l\x1b[?47l"))
	// Restoring the saved cursor also restores origin mode, which belongs
	// to the terminal rather than a screen: put it back as it is now, or
	// this formatting would set it and the alternate screen's (which only
	// sets modes that differ from the defaults) would leave it set.
	on, _ := s.term.Mode(libghostty.ModeOrigin)
	if copied, _ := cp.Mode(libghostty.ModeOrigin); copied != on {
		if on {
			cp.VTWrite([]byte("\x1b[?6h"))
		} else {
			cp.VTWrite([]byte("\x1b[?6l"))
		}
	}
	// The scrolling region belongs to the terminal, not to a screen, and
	// the alternate screen's formatting sets it. Set here as well, it
	// would already be in force while the alternate screen's contents are
	// written from the top, and scroll them.
	out, err = formatScreen(cp, libghostty.FormatterFormatVT, false, scrollback)
	if err != nil {
		return nil, false, err
	}
	return appendCursorShape(out, cp, false)
}

// formatScreen formats term's active screen. In VT form it also emits the
// state that affects how later output renders (modes, cursor, pen, the
// scrolling region if region is set, and so on), but not the palette:
// changedColors emits only the colors a program
// changed, where the formatter would overwrite all 256 of the client
// terminal's colors with libghostty's defaults. It does not emit the
// working directory (OSC 7) either, which names a directory on the
// session's host, not the client's.
//
// Every row is written, blank rows at the bottom included (the formatter
// keeps them in VT form without trimming), with CRLF between rows and no
// other CRLF: written into a client from the top, the rows land where
// they are here, however much scrollback comes before them.
// TestSnapshotWritesEveryRow pins that.
//
// With scrollback other than allScrollback, only the last scrollback rows
// of scrollback are formatted, followed by the screen.
func formatScreen(term *libghostty.Terminal, format libghostty.FormatterFormat, region bool, scrollback int) ([]byte, error) {
	sel, _, err := scrollbackSelection(term, scrollback)
	if err != nil {
		return nil, err
	}
	return runFormatter(term, format, region, sel)
}

var crlf = []byte("\r\n")

// scrollbackSelection selects the last scrollback rows of term's
// scrollback and the whole screen, and returns the number of rows selected.
// It returns a nil selection, for the whole of both, when scrollback is
// allScrollback or covers all of it.
func scrollbackSelection(term *libghostty.Terminal, scrollback int) (*libghostty.Selection, uint, error) {
	total, err := term.TotalRows()
	if err != nil {
		return nil, 0, err
	}
	history, err := term.ScrollbackRows()
	if err != nil {
		return nil, 0, err
	}
	if scrollback < 0 || uint(scrollback) >= history {
		return nil, total, nil
	}
	cols, err := term.Cols()
	if err != nil {
		return nil, 0, err
	}
	screenRows := total - history
	start := libghostty.Point{Tag: libghostty.PointTagActive}
	if scrollback > 0 {
		start = libghostty.Point{Tag: libghostty.PointTagHistory, Y: uint32(history - uint(scrollback))}
	}
	first, err := term.GridRef(start)
	if err != nil {
		return nil, 0, err
	}
	last, err := term.GridRef(libghostty.Point{Tag: libghostty.PointTagActive, X: cols - 1, Y: uint32(screenRows - 1)})
	if err != nil {
		return nil, 0, err
	}
	return &libghostty.Selection{Start: *first, End: *last}, uint(scrollback) + screenRows, nil
}

// runFormatter formats term's active screen. The modes and tab stops come
// before the screen contents, and the state emitted after them (the
// scrolling region if region is set, keyboard modes, then cursor, pen,
// hyperlink, protection, kitty keyboard flags and charsets) follows.
func runFormatter(term *libghostty.Terminal, format libghostty.FormatterFormat, region bool, sel *libghostty.Selection) ([]byte, error) {
	f, err := libghostty.NewFormatter(term,
		libghostty.WithFormatterSelection(sel),
		libghostty.WithFormatterFormat(format),
		libghostty.WithFormatterTrim(false),
		libghostty.WithFormatterUnwrap(false),
		libghostty.WithFormatterExtraModes(true),
		// Tab stops also matter for the alternate screen: setting them
		// ends with CUP home, which is where the screen contents start.
		libghostty.WithFormatterExtraTabstops(true),
		libghostty.WithFormatterExtraScrollingRegion(region),
		libghostty.WithFormatterExtraKeyboard(true),
		libghostty.WithFormatterExtraCursor(true),
		libghostty.WithFormatterExtraStyle(true),
		libghostty.WithFormatterExtraHyperlink(true),
		libghostty.WithFormatterExtraProtection(true),
		libghostty.WithFormatterExtraKittyKeyboard(true),
		libghostty.WithFormatterExtraCharsets(true),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create formatter: %w", err)
	}
	defer f.Close()
	return f.Format()
}

// compressIdle compresses scrollback if output has been quiet long enough
// and the scrollback changed (or was read by format) since the last complete
// pass; see compressIdleAfter.
func (s *terminalState) compressIdle(now time.Time) {
	if now.Sub(s.lastFeed) < compressIdleAfter {
		return
	}
	token, err := s.term.CompressionActivity()
	if err != nil || (s.compressed && token == s.compressedToken) {
		return
	}
	deadline := now.Add(compressStepBudget)
	for {
		result, err := s.term.Compress(libghostty.TerminalCompressionIncremental)
		if err != nil || result != libghostty.TerminalCompressionPending {
			// Done, or not possible on this platform (or failing):
			// nothing more to do until the scrollback changes.
			s.compressed = true
			s.compressedToken, _ = s.term.CompressionActivity()
			return
		}
		if time.Now().After(deadline) {
			return
		}
	}
}

// memory is what the terminal's screens hold, from libghostty.
func (s *terminalState) memory() (*protocol.TerminalMemory, error) {
	m, err := s.term.MemoryUsage()
	if err != nil {
		return nil, err
	}
	return &protocol.TerminalMemory{
		Pages:           m.Primary.Pages + m.Alternate.Pages,
		ResidentBytes:   m.Primary.ResidentBytes + m.Alternate.ResidentBytes,
		CompressedPages: m.Primary.CompressedPages + m.Alternate.CompressedPages,
		CompressedBytes: m.Primary.CompressedBytes + m.Alternate.CompressedBytes,
	}, nil
}

// scrollbackRows is the number of rows of history above the screen.
func (s *terminalState) scrollbackRows() uint64 {
	n, err := s.term.ScrollbackRows()
	if err != nil {
		return 0
	}
	return uint64(n)
}

// changedColors returns OSC sequences setting the palette entries and the
// default foreground, background and cursor colors that the program has
// changed (OSC 4, 10, 11, 12), and nothing for colors it left alone, so the
// client keeps its own theme.
func changedColors(term *libghostty.Terminal) ([]byte, error) {
	current, err := term.ColorPalette()
	if err != nil {
		return nil, err
	}
	defaults, err := term.ColorPaletteDefault()
	if err != nil {
		return nil, err
	}
	var out []byte
	for i := range current {
		if current[i] != defaults[i] {
			c := current[i]
			out = fmt.Appendf(out, "\x1b]4;%d;rgb:%02x/%02x/%02x\x1b\\", i, c.R, c.G, c.B)
		}
	}
	for _, dyn := range []struct {
		osc               int
		current, fallback func() (*libghostty.ColorRGB, error)
	}{
		{10, term.ColorForeground, term.ColorForegroundDefault},
		{11, term.ColorBackground, term.ColorBackgroundDefault},
		{12, term.ColorCursor, term.ColorCursorDefault},
	} {
		c, err := dyn.current()
		if err != nil {
			return nil, err
		}
		d, err := dyn.fallback()
		if err != nil {
			return nil, err
		}
		if c != nil && (d == nil || *c != *d) {
			out = fmt.Appendf(out, "\x1b]%d;rgb:%02x/%02x/%02x\x1b\\", dyn.osc, c.R, c.G, c.B)
		}
	}
	return out, nil
}

func (s *terminalState) close() {
	if s.term != nil {
		s.term.Close()
		s.term = nil
	}
}
