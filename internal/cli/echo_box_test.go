package cli

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"agtd.sh/internal/protocol"
	"agtd.sh/internal/screen"
)

// gridTerm is just enough of a terminal to replay what the echo and the
// painter write: absolute cursor moves, text, erase, line feeds at the
// bottom row. Every glyph is one cell wide.
type gridTerm struct {
	rows       [][]rune
	row, col   int
	cols, size int
}

func newGridTerm(cols, lines int) *gridTerm {
	g := &gridTerm{cols: cols, size: lines}
	for range lines {
		g.rows = append(g.rows, []rune(strings.Repeat(" ", cols)))
	}
	return g
}

func (g *gridTerm) write(b []byte) {
	s := []rune(string(b))
	for i := 0; i < len(s); i++ {
		switch r := s[i]; {
		case r == 0x1b && i+1 < len(s) && s[i+1] == '[':
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
				j++
			}
			if j >= len(s) {
				return
			}
			g.csi(string(s[i+2:j]), s[j])
			i = j
		case r == '\n':
			if g.row == g.size-1 {
				g.rows = append(g.rows[1:], []rune(strings.Repeat(" ", g.cols)))
			} else {
				g.row++
			}
		case r < 0x20:
		default:
			if g.row < g.size && g.col < g.cols {
				g.rows[g.row][g.col] = r
			}
			g.col++
		}
	}
}

func (g *gridTerm) csi(params string, final rune) {
	nums := func() []int {
		var out []int
		for _, p := range strings.Split(strings.TrimLeft(params, "?=>"), ";") {
			n, _ := strconv.Atoi(p)
			out = append(out, n)
		}
		return out
	}
	switch final {
	case 'H':
		n := nums()
		g.row, g.col = max(n[0]-1, 0), 0
		if len(n) > 1 {
			g.col = max(n[1]-1, 0)
		}
	case 'X':
		for c := g.col; c < min(g.col+max(nums()[0], 1), g.cols); c++ {
			g.rows[g.row][c] = ' '
		}
	case 'K':
		from, to := g.col, g.cols
		if nums()[0] == 2 {
			from = 0
		}
		for c := from; c < to; c++ {
			g.rows[g.row][c] = ' '
		}
	}
}

// box describes the composer on the screen: the rows of its rules and of
// the glyph line.
func (g *gridTerm) box() string {
	var rules, glyphs []int
	for y, r := range g.rows {
		switch {
		case strings.HasPrefix(string(r), "───"):
			rules = append(rules, y)
		case strings.HasPrefix(string(r), "❯"):
			glyphs = append(glyphs, y)
		}
	}
	return fmt.Sprintf("rules %v glyph rows %v", rules, glyphs)
}

// claudeScreen is Claude Code's screen with the composer showing rows (an
// empty one shows its placeholder) and the box growing up from row 37.
func claudeScreen(seq uint32, rows []string) *protocol.Frame {
	f := claudeField(rows)
	n := len(rows)
	frame := &protocol.Frame{Seq: seq, Full: true, Alternate: true, Cols: 120, Lines: 40, Field: f,
		CursorRow: f.Row, CursorCol: f.Col}
	rule := []byte(strings.Repeat("─", 120))
	frame.Rows = append(frame.Rows, protocol.FrameRow{Index: uint16(36 - n), VT: rule})
	for i, r := range rows {
		vt := []byte("  " + r)
		if i == 0 {
			vt = []byte("❯ " + r)
			if r == "" && n == 1 {
				vt = []byte("❯ Try it")
			}
		}
		frame.Rows = append(frame.Rows, protocol.FrameRow{Index: uint16(37 - n + i), VT: vt})
	}
	frame.Rows = append(frame.Rows, protocol.FrameRow{Index: 37, VT: rule})
	return frame
}

// paintFrame as the attach loop does it: the frame's field, the overlay
// over the frame, the ack.
func replayFrame(e *echo, p *screen.Painter, g *gridTerm, fr *protocol.Frame) {
	e.frameField(fr.Field, fr.Seq)
	g.write(p.Apply(fr, e.overlay()))
	e.frameAcked(fr.Seq)
}

// The first Shift-Enter in the composer must grow the box and keep it
// grown through the frames that follow: the frame built before the key
// reached the agent (which shows the old composer), then the one that
// shows the new line.
func TestEchoFirstShiftEnterNeverCollapsesTheBox(t *testing.T) {
	for _, typed := range []string{"", "one"} {
		t.Run(fmt.Sprintf("typed=%q", typed), func(t *testing.T) {
			e, _ := fixedClockEcho(echoFlushAfter)
			e.frames = true
			p, g := screen.New(), newGridTerm(120, 40)
			before := []string{typed}
			replayFrame(e, p, g, claudeScreen(1, before))
			if typed != "" {
				e.input([]byte(typed))
				replayFrame(e, p, g, claudeScreen(2, before))
			}
			start := g.box()
			t.Logf("start:           %s", start)

			_, paint := e.input([]byte("\x1b[13;2u"))
			g.write(paint)
			grown := g.box()
			t.Logf("after the key:   %s", grown)

			// The next frame was built before the key arrived.
			replayFrame(e, p, g, claudeScreen(3, before))
			lagging := g.box()
			t.Logf("lagging frame:   %s", lagging)

			after := []string{typed, ""}
			replayFrame(e, p, g, claudeScreen(4, after))
			replayFrame(e, p, g, claudeScreen(5, after))
			final := g.box()
			t.Logf("agent's frame:   %s", final)

			if grown != lagging {
				t.Errorf("the lagging frame changed the box: %s -> %s", grown, lagging)
			}
			if grown != final {
				t.Errorf("the agent's frame disagrees with the prediction: %s vs %s", grown, final)
			}
		})
	}
}
