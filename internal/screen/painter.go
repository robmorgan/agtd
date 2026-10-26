// Package screen paints the screen frames a session worker sends
// (protocol.Frame, under attach-frames) into a terminal, by absolute
// positioning: each changed row at its place, rows the program scrolled
// away pushed into the terminal's own scrollback, and the program's modes,
// keyboard state and cursor mirrored. It is pure Go: the CLI uses it, and
// the worker's tests replay its bytes into a libghostty terminal to check
// them cell for cell.
package screen

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"time"

	"agtd.sh/internal/protocol"
)

const (
	syncBegin = "\x1b[?2026h"
	syncEnd   = "\x1b[?2026l"
	// rowStart puts the pen back to its defaults and clears the row before
	// its contents are written, since a row's VT is formatted from a
	// default pen and may be shorter than what was there.
	rowStart = "\x1b[0m\x1b[K"
)

// Painter paints frames into one terminal and remembers what it painted.
type Painter struct {
	cols, lines int
	// rows is the VT last painted for each row; nil is blank.
	rows [][]byte
	// modes are the modes the last prefix mentioned ("?2004", "4") and
	// whether it set (h) or reset (l) them. Only changes are written: a
	// mode a new prefix no longer mentions is back at its default and is
	// put there, and one it mentions anew or differently is set so. The
	// rest of the prefix (tab stops, ending with CUP home) is written
	// when it changes.
	modes map[string]bool
	rest  []byte
	shape uint8
	seq   uint32
	// painted is whether any frame was painted yet.
	painted bool

	// altScrollback keeps the terminal on the primary screen while the
	// program is on the alternate one, and pushes the rows that leave the
	// top of its transcript into the terminal's own scrollback (see
	// SetAltScrollback). alt is the record of them, nil while the program
	// is on the primary screen; altEnd is how many rows from the top are
	// its transcript, from the last field seen (0: unknown).
	altScrollback bool
	altMax        int
	alt           *Transcript
	altEnd        int
	// pr is the wheel prediction (predict.go), and shown what the
	// terminal shows when a guess is drawn over the agent's screen (nil
	// when it shows rows as they are).
	pr    wheelPredict
	shown [][]byte
	clock func() time.Time
}

// SetAltScrollback turns on scrollback for programs on the alternate
// screen, keeping at most max rows of their transcript
// (docs/designs/alt-screen-scrollback.md, the client prototype): the
// alternate screen is never entered, its frames are painted on the
// primary screen, and the rows a Transcript finds leaving the top are
// scrolled into the terminal's scrollback as a primary screen's would be.
func (p *Painter) SetAltScrollback(max int) { p.altScrollback, p.altMax = true, max }

// AltScrollback reports whether the scrollback is on (SetAltScrollback).
func (p *Painter) AltScrollback() bool { return p.altScrollback }

// Leave is what to write before restoring the terminal at detach: under
// alt scrollback, while the program is on the alternate screen, its screen
// stays as a primary screen program's output would, so the cursor goes
// under it. Nil otherwise.
func (p *Painter) Leave() []byte {
	if p.alt == nil || p.lines == 0 {
		return nil
	}
	return []byte(cup(p.lines-1, 0) + "\r\n")
}

// altFrame turns an alternate screen frame into one painted on the
// primary screen: the screen it shows goes through the transcript record,
// and the rows recorded become a scroll (Scrolled, History) with every
// row repainted after it, since the terminal's rows moved. A look inside
// a synchronized-output block, at half a repaint, is skipped.
func (p *Painter) altFrame(f *protocol.Frame) *protocol.Frame {
	if !f.Alternate {
		if p.alt != nil {
			p.resetPredict()
		}
		p.alt, p.altEnd = nil, 0
		return f
	}
	lines := int(f.Lines)
	cur := make([][]byte, lines)
	if !f.Full && lines == p.lines && int(f.Cols) == p.cols && len(p.rows) == lines {
		copy(cur, p.rows)
	}
	for _, r := range f.Rows {
		if int(r.Index) < lines {
			cur[r.Index] = r.VT
		}
	}
	if fl := f.Field; fl != nil && fl.Found && fl.Alternate && fl.BoxTop > 0 {
		p.altEnd = int(fl.BoxTop) - 1
	}
	if p.alt == nil {
		p.alt = NewTranscript(p.altMax)
	}
	modes := p.modes
	if len(f.Prefix) > 0 {
		modes = parseModes(f.Prefix)
	}
	if modes["?2026"] {
		return f
	}
	pos, jumps := p.alt.Pos(), p.alt.Jumps
	out := p.alt.Update(cur, p.altEnd)
	p.observe(cur, p.alt.Pos()-pos, p.alt.Jumps != jumps, f)
	if len(out) == 0 {
		return f
	}
	g := *f
	g.Scrolled, g.History = uint16(min(len(out), 65535)), out
	g.Rows = make([]protocol.FrameRow, lines)
	for y := range cur {
		g.Rows[y] = protocol.FrameRow{Index: uint16(y), VT: cur[y]}
	}
	return &g
}

// Backdrop paints the screen as last shown, for a view drawn over it on
// the alternate screen (the attach overlay), where the terminal's view
// cannot scroll into its scrollback and carry the view off with it. It is
// nil when there is nothing painted or the terminal is already on the
// alternate screen.
func (p *Painter) Backdrop() []byte {
	if !p.painted {
		return nil
	}
	for _, m := range altModes {
		if p.modes[m] {
			return nil
		}
	}
	var b bytes.Buffer
	b.WriteString(syncBegin)
	for y, row := range p.rows {
		if p.shown != nil && y < len(p.shown) {
			row = p.shown[y]
		}
		b.WriteString(cup(y, 0) + rowStart)
		b.Write(row)
	}
	b.WriteString(syncEnd)
	return b.Bytes()
}

// altModes are the modes that switch to the alternate screen.
var altModes = []string{"?47", "?1047", "?1049"}

// New makes a painter for a terminal that has just been cleared.
func New() *Painter { return &Painter{} }

// Reset forgets the rows painted, for a terminal that was cleared; the
// modes and the cursor shape the terminal still has are remembered, so
// only their changes are written. The mouse modes are forgotten: a repaint
// turns them off first (attachRepaintSequence), so the next frame must set
// them again.
func (p *Painter) Reset() {
	p.rows, p.seq, p.painted = nil, 0, false
	p.resetPredict()
	for _, m := range mouseModes {
		delete(p.modes, m)
	}
}

// mouseModes are the modes a repaint turns off, in every encoding.
var mouseModes = []string{"?9", "?1000", "?1002", "?1003", "?1005", "?1006", "?1015", "?1016"}

// Seq is the sequence number of the last frame painted (0 for none).
func (p *Painter) Seq() uint32 { return p.seq }

// Row is the VT last painted for row y, nil when blank or unknown.
func (p *Painter) Row(y int) []byte {
	if y < 0 || y >= len(p.rows) {
		return nil
	}
	return p.rows[y]
}

// Apply returns what to write to the terminal for f, inside synchronized
// output: the modes that changed, the scroll, the changed rows, keyboard
// state, colours and effects, then overlay (the local echo's rows, already
// positioned, ending with the cursor where the echo predicts it) or, with
// no overlay, the frame's cursor. The frame's cursor is the program's,
// which lags the keys still in flight: putting it after the overlay would
// drag the cursor back a few cells on every frame while typing. A full
// frame, or one of another size, starts the row cache afresh.
func (p *Painter) Apply(f *protocol.Frame, overlay []byte) []byte {
	if p.altScrollback {
		f = p.altFrame(f)
	}
	var b bytes.Buffer
	b.WriteString(syncBegin)
	cols, lines := int(f.Cols), int(f.Lines)
	resized := p.painted && (cols != p.cols || lines != p.lines)
	if f.Full || resized || len(p.rows) != lines {
		p.rows = make([][]byte, lines)
		if p.shown != nil || resized {
			p.resetPredict()
		}
	}
	// written are the rows this frame writes, while a guess is drawn
	// over the rows it leaves.
	var written []bool
	if p.shown != nil {
		written = make([]bool, lines)
	}
	p.cols, p.lines = cols, lines
	if len(f.Prefix) > 0 {
		modes, rest := splitPrefix(f.Prefix)
		if p.altScrollback {
			for _, m := range altModes {
				delete(modes, m)
			}
		}
		for _, m := range sortedModes(p.modes) {
			if _, still := modes[m]; !still {
				b.WriteString(defaultMode(m, p.modes[m]))
			}
		}
		for _, m := range sortedModes(modes) {
			if old, had := p.modes[m]; !had || old != modes[m] {
				b.WriteString(modeSequence(m, modes[m]))
			}
		}
		if !bytes.Equal(rest, p.rest) {
			b.Write(rest)
		}
		p.modes, p.rest = modes, rest
	}
	if resized {
		b.WriteString("\x1b[2J")
	}
	if k := int(f.Scrolled); k > 0 && lines > 0 {
		// Line feeds at the bottom row scroll the terminal, pushing the
		// top rows into its scrollback as the program's screen did. The
		// rows leaving are painted with their final contents first, a
		// screen at a time from the top; rows the frame has no contents
		// for (more scrolled than it carries) go first, as they were.
		if extra := k - len(f.History); extra > 0 {
			b.WriteString(cup(lines-1, 0) + strings.Repeat("\n", extra))
		}
		for start := 0; start < len(f.History); start += lines {
			chunk := f.History[start:min(start+lines, len(f.History))]
			for i, h := range chunk {
				b.WriteString(cup(i, 0) + rowStart)
				b.Write(h)
			}
			b.WriteString(cup(lines-1, 0) + strings.Repeat("\n", len(chunk)))
		}
		if k >= lines || f.Full {
			p.rows = make([][]byte, lines)
		} else {
			if p.shown != nil {
				copy(p.shown, p.shown[k:])
				for i := lines - k; i < lines; i++ {
					p.shown[i] = nil
				}
			}
			copy(p.rows, p.rows[k:])
			for i := lines - k; i < lines; i++ {
				p.rows[i] = nil
			}
		}
	}
	for _, r := range f.Rows {
		y := int(r.Index)
		if y >= lines {
			continue
		}
		b.WriteString(cup(y, 0) + rowStart)
		b.Write(r.VT)
		p.rows[y] = r.VT
		if written != nil {
			written[y] = true
		}
	}
	if p.shown != nil || p.pr.debt != 0 || p.pr.settled {
		p.reconcileShown(&b, written)
	}
	b.Write(f.Keyboard)
	b.Write(f.Colors)
	b.Write(f.Effects)
	b.Write(overlay)
	if f.CursorShape != p.shape || !p.painted {
		if f.CursorShape != 0 || p.painted {
			fmt.Fprintf(&b, "\x1b[%d q", f.CursorShape)
		}
		p.shape = f.CursorShape
	}
	if len(overlay) == 0 {
		b.WriteString(cup(int(f.CursorRow), int(f.CursorCol)))
	}
	b.WriteString(syncEnd)
	p.seq = f.Seq
	p.painted = true
	return b.Bytes()
}

func cup(row, col int) string { return fmt.Sprintf("\x1b[%d;%dH", row+1, col+1) }

// parseModes lists the modes a prefix mentions, CSI [?] Pm h (true) or l
// (false), with "?" kept in the key.
func parseModes(prefix []byte) map[string]bool {
	modes, _ := splitPrefix(prefix)
	return modes
}

// splitPrefix separates a prefix into the modes it mentions and the rest
// (tab stops, CUP), in order.
func splitPrefix(prefix []byte) (map[string]bool, []byte) {
	modes := map[string]bool{}
	var rest []byte
	for i := 0; i < len(prefix); i++ {
		if prefix[i] != 0x1b || i+1 >= len(prefix) || prefix[i+1] != '[' {
			rest = append(rest, prefix[i])
			continue
		}
		j := i + 2
		for j < len(prefix) && (prefix[j] >= '0' && prefix[j] <= '9' || prefix[j] == ';' || prefix[j] == '?') {
			j++
		}
		if j >= len(prefix) {
			rest = append(rest, prefix[i:]...)
			break
		}
		if final := prefix[j]; final == 'h' || final == 'l' {
			params := string(prefix[i+2 : j])
			private := strings.HasPrefix(params, "?")
			for _, n := range strings.Split(strings.TrimPrefix(params, "?"), ";") {
				if n == "" {
					continue
				}
				if private {
					n = "?" + n
				}
				modes[n] = final == 'h'
			}
		} else {
			rest = append(rest, prefix[i:j+1]...)
		}
		i = j
	}
	return modes, rest
}

// sortedModes lists modes in a fixed order, so output is reproducible.
func sortedModes(modes map[string]bool) []string {
	out := make([]string, 0, len(modes))
	for m := range modes {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// modeSequence sets or resets a mode.
func modeSequence(m string, set bool) string {
	if set {
		return "\x1b[" + m + "h"
	}
	return "\x1b[" + m + "l"
}

// defaultMode puts a mode the prefix set (or reset) back to its default,
// which is the opposite.
func defaultMode(m string, set bool) string { return modeSequence(m, !set) }
