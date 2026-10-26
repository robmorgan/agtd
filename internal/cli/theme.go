package cli

import (
	"fmt"
	"strings"

	"github.com/rivo/uniseg"
)

// The renderer the overlay and the inbox share: the agtd design system's
// colours as SGR sequences, and lines of styled spans cut and padded to a
// width in terminal cells.
//
// Both follow the design system's dark theme: text and muted text, and
// amber (signal) for the one thing in focus: the selected row's bar, the
// active field, the Stop button. The overlay draws a Graphite surface panel
// edged in a hairline over the session; the inbox draws below the prompt on
// the terminal's own background (inboxTheme). Sessions keep the glyphs agt
// prints everywhere (● running, ⚠ needs you, ○ exited).

// overlayTheme holds the overlay's SGR sequences: foregrounds (FG) and
// backgrounds (BG). Every one is "" when colour is off.
type overlayTheme struct {
	surfaceBG, groundBG, lineBG, signalBG                              string
	textFG, mutedFG, quietFG, lineFG, signalFG, onSignalFG, okFG, bold string
}

func rgbFG(r, g, b uint8) string { return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", r, g, b) }
func rgbBG(r, g, b uint8) string { return fmt.Sprintf("\x1b[48;2;%d;%d;%dm", r, g, b) }

// The design system's tokens (dark theme), and their nearest 256-colour
// entries for terminals without 24-bit colour.
var (
	overlayTrueColor = overlayTheme{
		surfaceBG:  rgbBG(0x18, 0x1a, 0x1d), // surface
		groundBG:   rgbBG(0x0f, 0x11, 0x13), // ground: inputs sit inset
		lineBG:     rgbBG(0x2a, 0x2d, 0x32), // line: the selected row
		signalBG:   rgbBG(0xfd, 0xa9, 0x34), // signal
		textFG:     rgbFG(0xeb, 0xed, 0xef), // text
		mutedFG:    rgbFG(0x8b, 0x90, 0x98), // text-muted
		quietFG:    rgbFG(0x83, 0x86, 0x8c), // quiet
		lineFG:     rgbFG(0x3a, 0x3e, 0x44), // line, a step up so the edge holds over any screen
		signalFG:   rgbFG(0xfd, 0xa9, 0x34), // signal / signal-text
		onSignalFG: rgbFG(0x0f, 0x11, 0x13), // on-signal
		okFG:       rgbFG(0x8c, 0xc2, 0x65), // ok
		bold:       "\x1b[1m",
	}
	overlay256 = overlayTheme{
		surfaceBG: "\x1b[48;5;234m", groundBG: "\x1b[48;5;233m", lineBG: "\x1b[48;5;236m", signalBG: "\x1b[48;5;214m",
		textFG: "\x1b[38;5;255m", mutedFG: "\x1b[38;5;246m", quietFG: "\x1b[38;5;245m", lineFG: "\x1b[38;5;238m",
		signalFG: "\x1b[38;5;214m", onSignalFG: "\x1b[38;5;233m", okFG: "\x1b[38;5;107m", bold: "\x1b[1m",
	}
)

// overlayThemeFor picks 24-bit colour where the terminal says it takes
// it, 256 colours otherwise, and none under NO_COLOR or TERM=dumb.
func overlayThemeFor(getenv func(string) string) *overlayTheme {
	switch {
	case getenv("NO_COLOR") != "" || getenv("TERM") == "dumb":
		return &overlayTheme{}
	case supportsTrueColor(getenv):
		return &overlayTrueColor
	}
	return &overlay256
}

// inboxTheme is t for drawing below the prompt, on whatever background the
// terminal has: no panel, no inset input and the terminal's own text
// colour, so the inbox reads on a light terminal too. Only the selected row
// is raised, on line with the theme's text colour.
func inboxTheme(t *overlayTheme) *overlayTheme {
	out := *t
	out.surfaceBG, out.groundBG, out.textFG = "", "", ""
	out.lineBG = t.lineBG + t.textFG
	return &out
}

// span is text in one style; a line is a list of spans over a background
// ("" for the panel's surface).
type span struct{ style, text string }

type line struct {
	bg    string
	spans []span
}

func plain(text string) line { return line{spans: []span{{"", text}}} }

// render cuts l to width terminal cells and pads it with spaces, so it overwrites
// whatever was under it. Each span starts from the line's base colours, so
// a span's own style only adds to them.
func (l line) render(width int, t *overlayTheme) string {
	bg := l.bg
	if bg == "" {
		bg = t.surfaceBG
	}
	base := bg + t.textFG
	var b strings.Builder
	left := width
	for _, s := range l.spans {
		if left <= 0 {
			break
		}
		text := takeCells(s.text, left)
		left -= cellWidth(text)
		if style := base + s.style; style != "" {
			b.WriteString(style + text + ansiReset)
		} else {
			b.WriteString(text)
		}
	}
	if left > 0 {
		if base != "" {
			b.WriteString(base + strings.Repeat(" ", left) + ansiReset)
		} else {
			b.WriteString(strings.Repeat(" ", left))
		}
	}
	return b.String()
}

// listRow is a row of a list: a bar marking the selected row, then the
// row's own spans, then right-aligned text. The selected row is raised on
// line with an amber bar.
func (t *overlayTheme) listRow(width int, selected bool, spans []span, right span) line {
	l := line{spans: []span{{"", " "}}}
	if selected {
		l = line{bg: t.lineBG, spans: []span{{t.signalFG, "▌"}}}
	}
	l.spans = append(l.spans, span{"", " "})
	l.spans = append(l.spans, spans...)
	if room := width - l.width() - cellWidth(right.text) - 1; room > 0 && right.text != "" {
		l.spans = append(l.spans, span{"", strings.Repeat(" ", room)}, right)
	}
	return l
}

// input is a query or form field: an inset strip with a prompt glyph and
// a block cursor. An active field's glyph is amber.
func (t *overlayTheme) input(glyph, value, placeholder string, active bool) line {
	glyphStyle := t.quietFG
	if active {
		glyphStyle = t.signalFG
	}
	l := line{bg: t.groundBG, spans: []span{{"", " "}, {glyphStyle, glyph + " "}, {"", value}}}
	if active {
		l.spans = append(l.spans, span{"\x1b[7m", " "})
	}
	if value == "" && placeholder != "" {
		l.spans = append(l.spans, span{t.quietFG, placeholder})
	}
	return l
}

// width is the line's width in terminal cells.
func (l line) width() int {
	n := 0
	for _, s := range l.spans {
		n += cellWidth(s.text)
	}
	return n
}

// cellWidth is how many terminal cells s takes: two for a wide character
// (CJK, most emoji), none for a combining mark. Text drawn by the overlay
// and the inbox is measured this way, so a status message with a wide
// character never wraps a row. s must hold no escape sequences.
func cellWidth(s string) int { return uniseg.StringWidth(s) }

// takeCells is the longest prefix of s that fits in n cells, cut between
// grapheme clusters: a wide character that would straddle the edge is left
// out rather than split.
func takeCells(s string, n int) string {
	if n <= 0 {
		return ""
	}
	used := 0
	state := -1
	rest := s
	for len(rest) > 0 {
		var cluster string
		var w int
		cluster, rest, w, state = uniseg.FirstGraphemeClusterInString(rest, state)
		if used+w > n {
			return s[:len(s)-len(rest)-len(cluster)]
		}
		used += w
	}
	return s
}

// truncateCells cuts s to width cells, ending in "…" when it had to cut.
func truncateCells(s string, width int) string {
	if cellWidth(s) <= width {
		return s
	}
	if width <= 1 {
		return takeCells(s, width)
	}
	return takeCells(s, width-1) + "…"
}

// padCells cuts or pads s to exactly width cells.
func padCells(s string, width int) string {
	s = truncateCells(s, width)
	return s + strings.Repeat(" ", max(width-cellWidth(s), 0))
}
