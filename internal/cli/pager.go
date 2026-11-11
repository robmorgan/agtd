package cli

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
)

// The inbox's pager: a session's diff or history, shown on the alternate
// screen over the inbox and read with the inbox's own keys. An external
// pager cannot be run instead: the inbox's stdin reader (stdinChunks)
// would take its keys.

// inboxPageMax bounds what the pager holds: what comes after it is cut, and
// the page says where to get all of it.
const inboxPageMax = 4 << 20

var errPageFull = errors.New("page full")

// pageBuffer takes what an artifact stream writes, up to limit bytes, then
// fails with errPageFull, which ends the stream.
type pageBuffer struct {
	buf       []byte
	limit     int
	truncated bool
}

func (w *pageBuffer) Write(p []byte) (int, error) {
	if room := w.limit - len(w.buf); len(p) > room {
		w.buf = append(w.buf, p[:room]...)
		w.truncated = true
		return room, errPageFull
	}
	w.buf = append(w.buf, p...)
	return len(p), nil
}

// inboxPage is what the pager shows: lines escaped and with tabs expanded,
// each in its own style.
type inboxPage struct {
	title  string
	lines  []string
	styles []string
	// note ends the page: what was cut, if anything.
	note string
	// top is the first line on screen, and height how many the last
	// drawing had room for (what a page key moves by).
	top, height int
}

// newPage splits text into the page's lines; diff colours them as `agt
// diff` does (renderPage drops the colours when colour is off).
func newPage(title string, text []byte, diff bool) *inboxPage {
	p := &inboxPage{title: title}
	text = bytes.TrimSuffix(text, []byte("\n"))
	var c diffColorizer
	for raw := range bytes.SplitSeq(text, []byte("\n")) {
		raw = bytes.TrimSuffix(raw, []byte("\r"))
		style := ""
		if diff {
			style = c.classify(raw)
		}
		p.lines = append(p.lines, escapeControls(expandTabs(string(raw))))
		p.styles = append(p.styles, style)
	}
	return p
}

// expandTabs replaces tabs with spaces up to the next multiple of eight
// cells, as a terminal would.
func expandTabs(s string) string {
	if !strings.Contains(s, "\t") {
		return s
	}
	var b strings.Builder
	col := 0
	for _, r := range s {
		if r == '\t' {
			n := 8 - col%8
			b.WriteString(strings.Repeat(" ", n))
			col += n
			continue
		}
		b.WriteRune(r)
		col += cellWidth(string(r))
	}
	return b.String()
}

// lastTop is the top line that shows the end of the page.
func (p *inboxPage) lastTop() int {
	n := len(p.lines)
	if p.note != "" {
		n++
	}
	return max(n-max(p.height, 1), 0)
}

// handlePagerKey moves through the page; q or Esc closes it, back to the
// list.
func (i *inbox) handlePagerKey(ev keyEvent) {
	p := i.page
	page := max(p.height-1, 1)
	switch {
	case ev.code == keyEsc, ev.code == keyLeft, ev.code == keyChar && ev.mods == 0 && ev.r == 'q':
		i.mode, i.page = inboxBrowse, nil
		return
	case ev.code == keyUp, ev.code == keyChar && ev.mods == 0 && ev.r == 'k':
		p.top--
	case ev.code == keyDown, ev.code == keyEnter, ev.code == keyChar && ev.mods == 0 && ev.r == 'j':
		p.top++
	case ev.code == keyPageDown, ev.code == keyChar && ev.mods == 0 && (ev.r == ' ' || ev.r == 'f'):
		p.top += page
	case ev.code == keyPageUp, ev.code == keyChar && ev.mods == 0 && ev.r == 'b':
		p.top -= page
	case ev.code == keyHome, ev.code == keyChar && ev.mods == 0 && ev.r == 'g':
		p.top = 0
	case ev.code == keyEnd, ev.code == keyChar && ev.mods == 0 && ev.r == 'G':
		p.top = p.lastTop()
	}
	p.top = max(min(p.top, p.lastTop()), 0)
}

// renderPage is the pager's screen: a title, the page, and its keys, rows
// lines of cols-1 cells.
func (i *inbox) renderPage(cols, rows int) []string {
	t := i.theme
	p := i.page
	width := lineWidth(cols)
	p.height = max(rows-2, 1)
	p.top = max(min(p.top, p.lastTop()), 0)
	total := len(p.lines)
	last := min(p.top+p.height, total)
	where := fmt.Sprintf("  lines %d–%d of %d", min(p.top+1, total), last, total)
	out := []line{{spans: []span{{"", " "}, {t.bold, p.title}, {t.mutedFG, where}}}}
	for n := p.top; n < p.top+p.height; n++ {
		switch {
		case n < total:
			style := p.styles[n]
			if t.mutedFG == "" {
				style = "" // NO_COLOR or TERM=dumb
			}
			out = append(out, line{spans: []span{{style, p.lines[n]}}})
		case n == total && p.note != "":
			out = append(out, line{spans: []span{{t.signalFG, p.note}}})
		default:
			out = append(out, line{})
		}
	}
	out = append(out, i.hintLine([][2]string{{"↑↓", "scroll"}, {"space/b", "page"}, {"g/G", "top/end"}, {"q", "back"}}))
	lines := make([]string, 0, rows)
	for _, l := range out[:min(len(out), max(rows, 1))] {
		lines = append(lines, l.render(width, t))
	}
	return lines
}

// fullScreen draws lines over the whole (alternate) screen.
type fullScreen struct {
	frame string
}

func (s *fullScreen) draw(lines []string) {
	frame := "\x1b[H" + strings.Join(lines, "\x1b[K\r\n") + "\x1b[K\x1b[J"
	if frame == s.frame {
		return
	}
	writeOut([]byte(frame))
	s.frame = frame
}
