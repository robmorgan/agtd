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
}

// New makes a painter for a terminal that has just been cleared.
func New() *Painter { return &Painter{} }

// Reset forgets the rows painted, for a terminal that was cleared; the
// modes and the cursor shape the terminal still has are remembered, so
// only their changes are written.
func (p *Painter) Reset() {
	p.rows, p.seq, p.painted = nil, 0, false
}

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
	var b bytes.Buffer
	b.WriteString(syncBegin)
	cols, lines := int(f.Cols), int(f.Lines)
	resized := p.painted && (cols != p.cols || lines != p.lines)
	if f.Full || resized || len(p.rows) != lines {
		p.rows = make([][]byte, lines)
	}
	p.cols, p.lines = cols, lines
	if len(f.Prefix) > 0 {
		modes, rest := splitPrefix(f.Prefix)
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
