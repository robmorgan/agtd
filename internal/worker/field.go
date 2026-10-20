package worker

import (
	"fmt"
	"path/filepath"
	"strings"

	"go.mitchellh.com/libghostty"

	"agtd.sh/internal/protocol"
)

// The input field.
//
// A client that echoes keystrokes locally (so typing never waits on the
// network) has to draw them where the program will. The worker holds the
// program's screen, so it finds the input field there and answers
// AttachField with a protocol.Field: where the text starts, how wide a row
// is, which way the box grows, what the program has drawn in it. The rules
// below were measured on Claude Code 2.1, Codex 0.160 and pi 0.72 at idle,
// typing, wrapping and pasting (docs/designs/smooth-remote-attach.md); what
// differs between them lives in fieldProfile. Anything the rules cannot
// place leaves Found false, and the client sends its keys raw, as it always
// did: slower, never wrong.

// fieldProfile is what a program does that the screen alone cannot tell.
type fieldProfile struct {
	// spare is how many columns at the right of the screen the program
	// leaves unused: a row holds Cols - TextCol - spare characters before
	// it wraps.
	spare int
	// wrapKeepsSpace: see protocol.Field.WrapKeepsSpace.
	wrapKeepsSpace bool
}

var defaultFieldProfile = fieldProfile{spare: 1}

// fieldProfiles is keyed by the program's reported app (OSC 7501), then by
// the session's agent name, then by its command's base name.
var fieldProfiles = map[string]fieldProfile{
	"claude-code": {spare: 2},
	"claude":      {spare: 2},
	"codex":       {spare: 1},
	"pi":          {spare: 1, wrapKeepsSpace: true},
}

// fieldProfileFor picks the profile for a session: the program's own name
// if it reports status, else the agent as configured, else its command.
func fieldProfileFor(app, agent, command string) fieldProfile {
	for _, key := range []string{app, agent, filepath.Base(command)} {
		if p, ok := fieldProfiles[key]; ok && key != "" {
			return p
		}
	}
	return defaultFieldProfile
}

const (
	// fieldScanRows bounds how far above the cursor the box is looked for.
	fieldScanRows = 24
	// fieldChromeRows is how many rows beyond the box Field.Chrome carries,
	// in the direction the box grows, so the client can grow it that far
	// and restore the rows afterwards.
	fieldChromeRows = 16
)

// promptGlyphs start the first row of a field, followed by a space.
const promptGlyphs = "❯›>$%#"

// fieldCell is one screen cell as detection reads it.
type fieldCell struct {
	// text is the cell's grapheme; " " for a blank, "" for the tail of a
	// wide character.
	text  string
	tail  bool
	blank bool
	// inverse and faint are the attributes detection looks at: a program
	// that hides the cursor draws its own as an inverse cell, and a
	// placeholder is faint text.
	inverse, faint bool
	bg             *libghostty.ColorRGB
}

// fieldScreen is the active screen's cells and cursor, read once per
// detection through libghostty's render state.
type fieldScreen struct {
	cols, lines int
	alternate   bool
	cursor      libghostty.RenderStateCursor
	rows        [][]fieldCell
}

func readFieldScreen(term *libghostty.Terminal) (*fieldScreen, error) {
	rs, err := libghostty.NewRenderState()
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	if err := rs.Update(term); err != nil {
		return nil, err
	}
	cols, err := rs.Cols()
	if err != nil {
		return nil, err
	}
	lines, err := rs.Rows()
	if err != nil {
		return nil, err
	}
	cur, err := rs.Cursor()
	if err != nil {
		return nil, err
	}
	screen, err := term.ActiveScreen()
	if err != nil {
		return nil, err
	}
	ri, err := libghostty.NewRenderStateRowIterator()
	if err != nil {
		return nil, err
	}
	defer ri.Close()
	if err := rs.RowIterator(ri); err != nil {
		return nil, err
	}
	rc, err := libghostty.NewRenderStateRowCells()
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	s := &fieldScreen{cols: int(cols), lines: int(lines), alternate: screen == libghostty.ScreenAlternate, cursor: *cur}
	var style libghostty.RenderCellStyle
	var cps []uint32
	var text strings.Builder
	for ri.Next() {
		if err := ri.Cells(rc); err != nil {
			return nil, err
		}
		row := make([]fieldCell, 0, s.cols)
		tail := false
		for rc.Next() {
			cps, err = rc.GraphemesInto(cps[:0])
			if err != nil {
				return nil, err
			}
			if err := rc.StyleInto(&style); err != nil {
				return nil, err
			}
			c := fieldCell{inverse: style.Inverse, faint: style.Faint}
			if style.HasBackground {
				bg := style.Background
				c.bg = &bg
			}
			switch {
			case tail:
				c.tail = true
				tail = false
			case len(cps) == 0 || (len(cps) == 1 && (cps[0] == ' ' || cps[0] == 0xa0)):
				// A no-break space (Claude Code puts one after its
				// prompt glyph) is a blank like any other.
				c.text, c.blank = " ", true
			default:
				text.Reset()
				for _, cp := range cps {
					text.WriteRune(rune(cp))
				}
				c.text = text.String()
				if _, width := libghostty.UnicodeGraphemeWidth(cps); width == 2 {
					tail = true
				}
			}
			row = append(row, c)
		}
		s.rows = append(s.rows, row)
	}
	return s, nil
}

// text is the text on row y from column from, trailing blanks trimmed; to
// cuts it before column to (the whole row when to < 0).
func (s *fieldScreen) text(y, from, to int) string {
	if y < 0 || y >= len(s.rows) {
		return ""
	}
	var b strings.Builder
	for x, c := range s.rows[y] {
		if x < from || (to >= 0 && x >= to) {
			continue
		}
		b.WriteString(c.text)
	}
	return strings.TrimRight(b.String(), " ")
}

// firstNonBlank is the column of the first cell on row y that is not blank,
// or -1.
func (s *fieldScreen) firstNonBlank(y int) int {
	if y < 0 || y >= len(s.rows) {
		return -1
	}
	for x, c := range s.rows[y] {
		if !c.blank && !c.tail {
			return x
		}
	}
	return -1
}

// blankBefore reports whether every cell of row y before column col is blank.
func (s *fieldScreen) blankBefore(y, col int) bool {
	if y < 0 || y >= len(s.rows) {
		return false
	}
	for x, c := range s.rows[y] {
		if x >= col {
			return true
		}
		if !c.blank && !c.tail {
			return false
		}
	}
	return true
}

// isRule reports whether row y is a rule the program draws above or below
// its field: at least three box-drawing cells and nothing else.
func (s *fieldScreen) isRule(y int) bool {
	if y < 0 || y >= len(s.rows) {
		return false
	}
	n := 0
	for _, c := range s.rows[y] {
		if c.blank || c.tail {
			continue
		}
		r := []rune(c.text)
		if len(r) != 1 || !(r[0] >= 0x2500 && r[0] <= 0x259f) {
			return false
		}
		n++
	}
	return n >= 3
}

// promptGlyph is the column of a prompt glyph that starts row y (its first
// non-blank cell, followed by a blank), or -1.
func (s *fieldScreen) promptGlyph(y int) int {
	x := s.firstNonBlank(y)
	if x < 0 || x+1 >= s.cols {
		return -1
	}
	c := s.rows[y][x]
	if !strings.Contains(promptGlyphs, c.text) || !s.rows[y][x+1].blank {
		return -1
	}
	return x
}

// anchor is the insertion point: the terminal's cursor when it is shown,
// else the one inverse-video cell a program that hides the cursor draws as
// its own. A menu's highlight is a whole inverse row and never counts. A
// program drawing inline on the primary screen keeps its field wherever its
// output has reached, near the top of a tall terminal at first, so the
// whole screen is searched, from the bottom; a cell on a row that a rule
// borders (a drawn box) is taken over one anywhere else, which only counts
// in the bottom half.
func (s *fieldScreen) anchor() (y, x int, inverse, ok bool) {
	if s.cursor.Visible && s.cursor.ViewportHasValue {
		return int(s.cursor.ViewportY), int(s.cursor.ViewportX), false, true
	}
	loose := -1
	for y := s.lines - 1; y >= 0; y-- {
		n, at := 0, -1
		for x, c := range s.rows[y] {
			if c.inverse && !c.tail {
				n++
				at = x
			}
		}
		if n != 1 {
			continue
		}
		if s.isRule(y-1) || s.isRule(y+1) {
			return y, at, true, true
		}
		if loose < 0 && y >= s.lines/2 {
			loose = y
		}
	}
	if loose >= 0 {
		for x, c := range s.rows[loose] {
			if c.inverse && !c.tail {
				return loose, x, true, true
			}
		}
	}
	return 0, 0, false, false
}

// detectField finds the input field on term's active screen. blocked says
// the program reports it is waiting on a dialog, or something other than
// the agent or a shell holds the foreground: no field then.
func detectField(term *libghostty.Terminal, prof fieldProfile, blocked bool) (*protocol.Field, error) {
	s, err := readFieldScreen(term)
	if err != nil {
		return nil, err
	}
	f := &protocol.Field{Cols: uint16(s.cols), Lines: uint16(s.lines), Alternate: s.alternate}
	if blocked || s.cols == 0 || s.lines == 0 {
		return f, nil
	}
	ay, ax, inverse, ok := s.anchor()
	if !ok || ay >= s.lines || ax >= s.cols || s.isRule(ay) {
		return f, nil
	}

	// The first row of the field: the nearest row at or above the cursor
	// that starts with a prompt glyph, within the rule above if there is
	// one. Text starts after the glyph and the space that follows it.
	glyphRow, textCol, ruleAbove := -1, -1, -1
	for y := ay; y >= 0 && y >= ay-fieldScanRows; y-- {
		if y != ay && s.isRule(y) {
			ruleAbove = y
			break
		}
		if g := s.promptGlyph(y); g >= 0 && (y != ay || g < ax) {
			glyphRow, textCol = y, g+2
			break
		}
	}
	top := ay
	switch {
	case glyphRow >= 0:
		top = glyphRow
		if s.isRule(glyphRow - 1) {
			ruleAbove = glyphRow - 1
		}
	case ruleAbove >= 0:
		// A box between rules with no glyph (pi): the text starts at the
		// leftmost text on the rows between them, at the cursor if none.
		top = ruleAbove + 1
		textCol = ax
		for y := top; y < s.lines && !s.isRule(y); y++ {
			if x := s.firstNonBlank(y); x >= 0 && x < textCol {
				textCol = x
			}
		}
	default:
		// No glyph, no rule: a shell prompt. Its text is part of the
		// row, and readline wraps at column 0.
		textCol = 0
	}
	if textCol > ax {
		return f, nil
	}
	// The rows below the first: continuation rows (blank before the text
	// column), up to the cursor's row at least, then while they hold
	// text, stopping at a rule, at a status row (text before the text
	// column) or at a blank row past the cursor.
	bottom, ruleBelow := top, -1
	for y := top; y < s.lines; y++ {
		if y > top && s.isRule(y) {
			ruleBelow = y
			break
		}
		if y > top && !s.blankBefore(y, textCol) {
			break
		}
		if y > ay && s.firstNonBlank(y) < 0 {
			break
		}
		bottom = y
	}
	if bottom < ay {
		return f, nil
	}

	for y := top; y <= bottom; y++ {
		to := -1
		if y == ay && s.faintOrBlankFrom(y, ax) {
			// What follows the cursor is not text the user typed: a
			// placeholder in an empty field, a shell's suggestion.
			to = ax
		}
		text := s.text(y, textCol, to)
		if y == ay {
			// Blanks between the text and the cursor are spaces the
			// user typed (the program put the cursor after them): kept,
			// or a client's prediction would lose each space it typed
			// before a pause.
			if end := s.lastNonBlank(y, textCol, to) + 1; ax > end {
				text += strings.Repeat(" ", ax-max(end, textCol))
			}
		}
		f.Rows = append(f.Rows, text)
	}

	f.Found = true
	f.Row, f.Col, f.Top, f.TextCol = uint16(ay), uint16(ax), uint16(top), uint16(textCol)
	f.Width = uint16(max(s.cols-textCol-prof.spare, 1))
	f.GrowsUp = s.alternate
	f.InverseCursor = inverse
	f.WrapKeepsSpace = prof.wrapKeepsSpace
	f.Style = []byte("\x1b[0m")
	if bg := s.rows[ay][ax].bg; bg != nil {
		f.Style = fmt.Appendf(f.Style, "\x1b[48;2;%d;%d;%dm", bg.R, bg.G, bg.B)
	}

	boxTop, boxBottom := top, bottom
	if ruleAbove >= 0 {
		boxTop = ruleAbove
	}
	if ruleBelow >= 0 {
		boxBottom = ruleBelow
	}
	f.BoxTop, f.BoxBottom = uint16(boxTop), uint16(boxBottom)
	first, last := boxTop, boxBottom
	if f.GrowsUp {
		first = max(boxTop-fieldChromeRows, 0)
	} else {
		last = min(boxBottom+fieldChromeRows, s.lines-1)
	}
	for y := first; y <= last; y++ {
		vt, err := formatRow(term, uint16(s.cols), uint32(y))
		if err != nil {
			return nil, err
		}
		f.Chrome = append(f.Chrome, protocol.ScreenRow{Index: uint16(y), VT: vt})
	}
	return f, nil
}

// lastNonBlank is the last column on row y, from column from and before
// column to (the whole row when to < 0), holding something other than a
// blank, or from-1 for none.
func (s *fieldScreen) lastNonBlank(y, from, to int) int {
	last := from - 1
	for x, c := range s.rows[y] {
		if x < from || (to >= 0 && x >= to) {
			continue
		}
		if !c.blank {
			last = x
		}
	}
	return last
}

// faintOrBlankFrom reports whether every cell of row y from column from on
// is blank or faint.
func (s *fieldScreen) faintOrBlankFrom(y, from int) bool {
	for x, c := range s.rows[y] {
		if x >= from && !c.blank && !c.tail && !c.faint {
			return false
		}
	}
	return true
}

// formatRow renders row y of the active screen as VT bytes that draw it
// from column 0 in a default pen: libghostty's formatter over a selection of
// that row alone, with none of the terminal state a snapshot carries.
func formatRow(term *libghostty.Terminal, cols uint16, y uint32) ([]byte, error) {
	first, err := term.GridRef(libghostty.Point{Tag: libghostty.PointTagActive, X: 0, Y: y})
	if err != nil {
		return nil, err
	}
	last, err := term.GridRef(libghostty.Point{Tag: libghostty.PointTagActive, X: cols - 1, Y: y})
	if err != nil {
		return nil, err
	}
	f, err := libghostty.NewFormatter(term,
		libghostty.WithFormatterSelection(&libghostty.Selection{Start: *first, End: *last}),
		libghostty.WithFormatterFormat(libghostty.FormatterFormatVT),
		libghostty.WithFormatterTrim(false),
		libghostty.WithFormatterUnwrap(false),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create formatter: %w", err)
	}
	defer f.Close()
	return f.Format()
}
