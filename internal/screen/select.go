package screen

import (
	"bytes"
	"strconv"
	"strings"
	"time"

	"github.com/rivo/uniseg"
)

// Selection prediction: under frames, an agent that takes the mouse
// (Claude Code, pi in fullscreen) draws its own selection while the left
// button is dragged, a round trip after each motion. The painter reads the
// drag on its way to the agent and draws the highlight at once, over the
// agent's screen; the agent's frames stay the authority, and a guess is
// dropped once the agent has had time to answer the release.
//
// How the agent draws a selection is learned from its own answers, not
// assumed: the first frames of the first drag show which pen the
// selected cells gain (reverse video for pi, a background colour for
// Claude Code) and, once a selection spans three rows, the columns the
// rows between its ends run over (the whole row for pi, the transcript's
// text column for Claude Code). Until then nothing is drawn. A selection
// runs from the press cell to the pointer's, both included, through the
// rows between, as both agents draw it.
//
// No guess is drawn where the agent may do something else: once the
// pointer reaches the top or bottom row, or below the transcript (the
// agent scrolls, or the input box is not selectable), or the text under
// the selection changes (it scrolled, or new output arrived), for the
// rest of that gesture; while a wheel guess is drawn; and, after two
// wrong guesses in a row, until two gestures in a row would have been
// right.

// SelectStats counts the selection prediction's work.
type SelectStats struct {
	// Gestures are the drags seen; Guessed the ones drawn at once.
	Gestures, Guessed int
	// Right and Wrong are the guesses the agent's own selection matched,
	// or not, once it answered the release.
	Right, Wrong int
}

// selectPredict is the selection prediction's state, owned by the Painter.
type selectPredict struct {
	on  bool
	rtt time.Duration
	// sgr is the pen a selected cell gains, as an SGR sequence; nil until
	// learned. inverse is whether it is reverse video, else bg is its
	// background colour parameter ("48;2;38;79;120").
	sgr     []byte
	inverse bool
	bg      string
	// left and right are the columns the rows between a selection's ends
	// run over, inclusive; right is -1 until learned.
	left, right int

	// The gesture: whether the button is down, whether it moved since the
	// press, the press cell (anchor) and the pointer's, 0-based, and the
	// rows the pointer went over.
	down, moved    bool
	ax, ay, px, py int
	top, bottom    int
	// base is the agent's screen when the button went down, which the
	// first answers are compared with to learn the pen; text is its text,
	// which must not change under the selection for a guess to stand.
	base [][]byte
	text []string
	// off: no guess for the rest of this gesture. counted: the gesture
	// was counted as guessed.
	off, counted bool
	// settleBy is when the agent has had time to answer the release, and
	// the guess is judged and dropped; zero when nothing is pending.
	settleBy time.Time
	// misses counts wrong guesses in a row; at two, guessing pauses until
	// two gestures in a row would have been right (hits).
	misses, hits int
	paused       bool
	stats        SelectStats
}

// SetPredictSelection turns on the selection prediction; rtt is the
// link's round trip, which bounds how long a guess waits for the agent.
func (p *Painter) SetPredictSelection(rtt time.Duration) {
	p.sel.on, p.sel.rtt = true, rtt
	if p.sel.sgr == nil {
		p.sel.right = -1
	}
}

// SelectStats returns the selection prediction's counts.
func (p *Painter) SelectStats() SelectStats { return p.sel.stats }

// Select takes input on its way to the agent and draws the selection a
// left-button drag in it makes at once. It returns what to write to the
// terminal, nil for nothing.
func (p *Painter) Select(data []byte) []byte {
	s := &p.sel
	if !s.on {
		return nil
	}
	changed := false
	for _, m := range mouseReports(data) {
		changed = p.mouse(m) || changed
	}
	if !changed {
		return nil
	}
	return p.repaintSelect()
}

// mouse notes one mouse report and reports whether the guess may have
// changed.
func (p *Painter) mouse(m mouseReport) bool {
	s := &p.sel
	x, y := m.x-1, m.y-1
	switch {
	case m.button == 32+3:
		// Motion with no button held (any-event tracking reports it all
		// the time) leaves a selection be.
		return false
	case m.button == 0 && m.press:
		drawn := p.endSelect()
		s.down, s.moved, s.off, s.counted = true, false, false, false
		s.ax, s.ay, s.px, s.py = x, y, x, y
		s.top, s.bottom = y, y
		s.base = append([][]byte(nil), p.rows...)
		s.text = make([]string, len(p.rows))
		for i, r := range p.rows {
			s.text[i] = rowText(r)
		}
		s.off = !p.selectable(y)
		return drawn
	case m.button == 32 && m.press && s.down:
		if !s.moved {
			s.stats.Gestures++
		}
		s.moved = true
		s.px, s.py = x, y
		s.top, s.bottom = min(s.top, y), max(s.bottom, y)
		if !p.selectable(y) {
			s.off = true
		}
		return true
	case m.button == 0 && !m.press && s.down:
		s.down = false
		if !s.moved {
			// A click: the agent clears its selection, or selects a word
			// on a double click; nothing is guessed.
			return false
		}
		s.px, s.py = x, y
		s.top, s.bottom = min(s.top, y), max(s.bottom, y)
		if !p.selectable(y) {
			s.off = true
		}
		s.settleBy = p.now().Add(2*s.rtt + predictWait)
		return true
	case m.button >= 64:
		// The wheel (Wheel's) and the extra buttons leave a selection be.
		return false
	case s.down || s.moved:
		// Another button, or one with a modifier: the agent may do
		// anything with it.
		s.off = true
		return true
	}
	return false
}

// selectable reports whether the pointer on row y keeps a guess: not the
// top or bottom row, where the agent scrolls, nor below the transcript.
func (p *Painter) selectable(y int) bool {
	if y <= 0 || y >= p.lines-1 {
		return false
	}
	return p.altEnd == 0 || y < p.altEnd
}

// endSelect ends a gesture, judging it if the agent has had time to answer
// it (a press can come sooner). It returns whether a guess was drawn.
func (p *Painter) endSelect() bool {
	s := &p.sel
	drawn := p.selShown != nil
	if s.moved && !s.off && s.sgr != nil && !s.down && !s.settleBy.IsZero() && !p.now().Before(s.settleBy) {
		right := p.selectMatches()
		if s.paused {
			if right {
				s.hits++
			} else {
				s.hits = 0
			}
			if s.hits >= 2 {
				s.paused, s.misses, s.hits = false, 0, 0
			}
		} else if drawn {
			if right {
				s.stats.Right++
				s.misses = 0
			} else {
				s.stats.Wrong++
				s.misses++
				if s.misses >= 2 {
					s.paused, s.hits = true, 0
				}
			}
		}
	}
	s.down, s.moved, s.off = false, false, false
	s.base, s.text = nil, nil
	s.settleBy = time.Time{}
	return drawn
}

// selectExpire judges and drops a guess the agent has had time to answer.
func (p *Painter) selectExpire() []byte {
	s := &p.sel
	if s.settleBy.IsZero() || p.now().Before(s.settleBy) {
		return nil
	}
	if !p.endSelect() {
		return nil
	}
	return p.repaintSelect()
}

// selectFrame looks at a frame's rows for the gesture: it learns the
// selection's pen and bounds from the agent's answers, and turns the
// guess off when the text under the selection changed.
func (p *Painter) selectFrame() {
	s := &p.sel
	if !s.moved || s.base == nil {
		return
	}
	for y := s.top; y <= s.bottom && y < len(p.rows); y++ {
		if y >= len(s.text) || rowText(p.rows[y]) != s.text[y] {
			s.off = true
			return
		}
	}
	if s.sgr == nil {
		p.learnPen()
	}
	if s.sgr != nil && s.right < 0 {
		p.learnBounds()
	}
}

// learnPen learns the pen selected cells gain: every cell over the rows
// the pointer went over whose pen changed since the press, with its text
// the same, gained reverse video alone, or one background colour alone.
func (p *Painter) learnPen() {
	s := &p.sel
	inverse, gains := 0, 0
	bg := ""
	for y := s.top; y <= s.bottom && y < len(p.rows) && y < len(s.base); y++ {
		was, now := rowCells(s.base[y], p.cols), rowCells(p.rows[y], p.cols)
		for x := range now {
			b, c := was[x], now[x]
			if b.text != c.text || b.pen == c.pen {
				continue
			}
			switch {
			case !b.pen.inverse && c.pen.inverse && b.pen.bg == c.pen.bg:
				inverse++
			case c.pen.bg != "" && c.pen.bg != b.pen.bg && b.pen.inverse == c.pen.inverse && (bg == "" || bg == c.pen.bg):
				bg = c.pen.bg
			default:
				return
			}
			gains++
		}
	}
	switch {
	case gains < 3:
	case inverse == gains:
		s.inverse, s.sgr = true, []byte("\x1b[7m")
	case inverse == 0:
		s.bg, s.sgr = bg, []byte("\x1b["+bg+"m")
	}
}

// learnBounds learns the columns the rows between a selection's ends run
// over, from an answer whose selection spans three rows or more.
func (p *Painter) learnBounds() {
	s := &p.sel
	var spans [][2]int
	first := -1
	for y := s.top; y <= s.bottom && y < len(p.rows); y++ {
		lo, hi := -1, -1
		for x, c := range rowCells(p.rows[y], p.cols) {
			if p.selected(c.pen) {
				if lo < 0 {
					lo = x
				}
				hi = x
			}
		}
		if lo < 0 {
			if first >= 0 {
				break
			}
			continue
		}
		if first < 0 {
			first = y
		}
		spans = append(spans, [2]int{lo, hi})
	}
	if len(spans) < 3 {
		return
	}
	left, right := spans[1][0], spans[1][1]
	for _, sp := range spans[1 : len(spans)-1] {
		if sp[0] != left || sp[1] != right {
			return
		}
	}
	s.left, s.right = left, right
}

// selected reports whether a cell's pen shows the learned selection.
func (p *Painter) selected(pen cellPen) bool {
	if p.sel.inverse {
		return pen.inverse
	}
	return p.sel.bg != "" && pen.bg == p.sel.bg
}

// selectSpans are the columns the guess selects on each row, inclusive,
// from the press to the pointer; ok is false when it cannot be drawn.
func (p *Painter) selectSpans() (spans map[int][2]int, ok bool) {
	s := &p.sel
	x0, y0, x1, y1 := s.ax, s.ay, s.px, s.py
	if y1 < y0 || (y1 == y0 && x1 < x0) {
		x0, y0, x1, y1 = x1, y1, x0, y0
	}
	if y1 > y0 && s.right < 0 {
		return nil, false
	}
	spans = map[int][2]int{}
	for y := y0; y <= y1; y++ {
		lo, hi := s.left, s.right
		if y == y0 {
			lo = x0
		}
		if y == y1 {
			hi = x1
		}
		if y1 > y0 && y == y0 {
			hi = s.right
		}
		if y1 > y0 && y == y1 {
			lo = s.left
		}
		lo, hi = max(lo, 0), min(hi, p.cols-1)
		if lo <= hi {
			spans[y] = [2]int{lo, hi}
		}
	}
	return spans, true
}

// selectMatches reports whether the agent's screen shows the selection
// the guess would draw now, over the rows the pointer went over.
func (p *Painter) selectMatches() bool {
	spans, ok := p.selectSpans()
	if !ok {
		return false
	}
	s := &p.sel
	for y := min(s.top, s.ay); y <= max(s.bottom, s.ay) && y < len(p.rows); y++ {
		sp, in := spans[y]
		for x, c := range rowCells(p.rows[y], p.cols) {
			if p.selected(c.pen) != (in && x >= sp[0] && x <= sp[1]) {
				return false
			}
		}
	}
	return true
}

// selectGuess is the screen with the guess drawn over the agent's, nil for
// no guess.
func (p *Painter) selectGuess() [][]byte {
	s := &p.sel
	if !s.moved || s.off || s.paused || s.sgr == nil || !p.painted || p.shown != nil || p.pr.debt != 0 {
		return nil
	}
	if !p.modes["?1006"] || !(p.modes["?1002"] || p.modes["?1003"]) {
		return nil
	}
	if !s.down && s.settleBy.IsZero() {
		return nil
	}
	spans, ok := p.selectSpans()
	if !ok {
		return nil
	}
	out := make([][]byte, len(p.rows))
	copy(out, p.rows)
	for y := min(s.top, s.ay); y <= max(s.bottom, s.ay) && y < len(out); y++ {
		row := p.unselect(out[y])
		if sp, in := spans[y]; in {
			row = highlightRow(row, sp[0], sp[1], s.sgr)
		}
		out[y] = row
	}
	if !s.counted {
		s.counted = true
		s.stats.Guessed++
	}
	return out
}

// repaintSelect writes, outside any frame, the rows where the terminal
// differs from the guess (or, with none, from the agent's screen), leaving
// the cursor and pen as they were.
func (p *Painter) repaintSelect() []byte {
	want := p.selectGuess()
	var b bytes.Buffer
	for y := range p.rows {
		have, final := p.rows[y], p.rows[y]
		if p.selShown != nil && y < len(p.selShown) {
			have = p.selShown[y]
		}
		if want != nil {
			final = want[y]
		}
		if bytes.Equal(have, final) {
			continue
		}
		if b.Len() == 0 {
			b.WriteString(syncBegin + "\x1b7")
		}
		b.WriteString(cup(y, 0) + rowStart)
		b.Write(final)
	}
	p.selShown = want
	if b.Len() == 0 {
		return nil
	}
	b.WriteString("\x1b8" + syncEnd)
	return b.Bytes()
}

// reconcileSelect writes, inside Apply's synchronized output, the rows
// where the terminal differs from what should be shown now: the guess over
// the frame just painted, or the frame alone. painted are the rows the
// terminal now shows as the agent has them (nil for none).
func (p *Painter) reconcileSelect(b *bytes.Buffer, painted []bool) {
	p.selectFrame()
	want := p.selectGuess()
	for y := range p.rows {
		have, final := p.rows[y], p.rows[y]
		if p.selShown != nil && y < len(p.selShown) && (painted == nil || !painted[y]) {
			have = p.selShown[y]
		}
		if want != nil {
			final = want[y]
		}
		if !bytes.Equal(have, final) {
			b.WriteString(cup(y, 0) + rowStart)
			b.Write(final)
		}
	}
	p.selShown = want
}

// resetSelect forgets the gesture and the guess (the terminal was cleared
// or resized); what was learned is kept.
func (p *Painter) resetSelect() {
	s := &p.sel
	s.down, s.moved, s.off = false, false, false
	s.base, s.text = nil, nil
	s.settleBy = time.Time{}
	p.selShown = nil
}

// unselect is row without the learned selection pen, so a guess is drawn
// over the agent's screen as it was before its own selection.
func (p *Painter) unselect(row []byte) []byte {
	s := &p.sel
	if bytes.IndexByte(row, 0x1b) < 0 {
		return row
	}
	var out []byte
	for i := 0; i < len(row); {
		if row[i] != 0x1b {
			j := i
			for j < len(row) && row[j] != 0x1b {
				j++
			}
			out = append(out, row[i:j]...)
			i = j
			continue
		}
		seq := row[i : i+escapeLen(row[i:])]
		i += len(seq)
		items, ok := sgrItems(seq)
		if !ok {
			out = append(out, seq...)
			continue
		}
		var kept []string
		for _, it := range items {
			if (s.inverse && it == "7") || (!s.inverse && it == s.bg) {
				continue
			}
			kept = append(kept, it)
		}
		if len(kept) > 0 {
			out = append(out, "\x1b["+strings.Join(kept, ";")+"m"...)
		}
	}
	return out
}

// highlightRow is row with cells x0 to x1 (inclusive) drawn with sgr over
// their own pen, the empty cells among them as spaces, and the row padded
// with them when it ends before x1.
func highlightRow(row []byte, x0, x1 int, sgr []byte) []byte {
	var out, pen []byte
	col, in := 0, false
	enter := func() {
		if !in {
			out = append(out, sgr...)
			in = true
		}
	}
	leave := func() {
		if in {
			out = append(out, "\x1b[0m"...)
			out = append(out, pen...)
			in = false
		}
	}
	for i := 0; i < len(row); {
		if row[i] == 0x1b {
			seq := row[i : i+escapeLen(row[i:])]
			i += len(seq)
			if n, ok := cufCount(seq); ok {
				for n > 0 {
					switch {
					case col < x0:
						k := min(n, x0-col)
						out = append(out, cuf(k)...)
						col, n = col+k, n-k
					case col <= x1:
						enter()
						k := min(n, x1-col+1)
						out = append(out, strings.Repeat(" ", k)...)
						col, n = col+k, n-k
					default:
						leave()
						out = append(out, cuf(n)...)
						col, n = col+n, 0
					}
				}
				continue
			}
			if items, ok := sgrItems(seq); ok {
				if len(items) == 1 && items[0] == "0" {
					pen = nil
				} else {
					pen = append(pen, seq...)
				}
				out = append(out, seq...)
				if in {
					out = append(out, sgr...)
				}
				continue
			}
			out = append(out, seq...)
			continue
		}
		j := i
		for j < len(row) && row[j] != 0x1b {
			j++
		}
		g := uniseg.NewGraphemes(string(row[i:j]))
		for g.Next() {
			if col >= x0 && col <= x1 {
				enter()
			} else if col > x1 {
				leave()
			}
			out = append(out, g.Str()...)
			col += max(g.Width(), 1)
		}
		i = j
	}
	if col <= x1 {
		if col < x0 {
			out = append(out, cuf(x0-col)...)
			col = x0
		}
		enter()
		out = append(out, strings.Repeat(" ", x1-col+1)...)
	}
	if in {
		out = append(out, "\x1b[0m"...)
	}
	return out
}

func cuf(n int) string {
	if n == 1 {
		return "\x1b[C"
	}
	return "\x1b[" + strconv.Itoa(n) + "C"
}

// cellPen is what a cell's pen says about a selection: reverse video, and
// its background colour parameter ("" for the default).
type cellPen struct {
	inverse bool
	bg      string
}

type rowCell struct {
	text string
	pen  cellPen
}

// rowCells are a row's first cols cells, an empty cell as a space with
// the default pen; a wide character's second cell repeats it.
func rowCells(row []byte, cols int) []rowCell {
	cells := make([]rowCell, cols)
	for i := range cells {
		cells[i].text = " "
	}
	var pen cellPen
	col := 0
	for i := 0; i < len(row) && col < cols; {
		if row[i] == 0x1b {
			seq := row[i : i+escapeLen(row[i:])]
			i += len(seq)
			if n, ok := cufCount(seq); ok {
				col += n
				continue
			}
			if items, ok := sgrItems(seq); ok {
				for _, it := range items {
					switch {
					case it == "0":
						pen = cellPen{}
					case it == "7":
						pen.inverse = true
					case it == "27":
						pen.inverse = false
					case it == "49":
						pen.bg = ""
					case isBackground(it):
						pen.bg = it
					}
				}
			}
			continue
		}
		j := i
		for j < len(row) && row[j] != 0x1b {
			j++
		}
		g := uniseg.NewGraphemes(string(row[i:j]))
		for g.Next() && col < cols {
			for k := 0; k < max(g.Width(), 1) && col+k < cols; k++ {
				cells[col+k] = rowCell{text: g.Str(), pen: pen}
			}
			col += max(g.Width(), 1)
		}
		i = j
	}
	return cells
}

// rowText is a row's text, without its pen, the trailing blanks trimmed.
func rowText(row []byte) string {
	var b strings.Builder
	for _, c := range rowCells(row, maxCols) {
		b.WriteString(c.text)
	}
	return strings.TrimRight(b.String(), " ")
}

// isBackground reports whether an SGR item sets the background colour.
func isBackground(it string) bool {
	if strings.HasPrefix(it, "48;") || strings.HasPrefix(it, "48:") {
		return true
	}
	n, err := strconv.Atoi(it)
	return err == nil && ((n >= 40 && n <= 47) || (n >= 100 && n <= 107))
}

// sgrItems splits an SGR sequence into its items, an extended colour
// (38, 48 or 58 with its 5;n or 2;r;g;b) as one; ok is false for any
// other sequence. An empty SGR is a reset ("0").
func sgrItems(seq []byte) (items []string, ok bool) {
	if len(seq) < 3 || seq[1] != '[' || seq[len(seq)-1] != 'm' {
		return nil, false
	}
	body := string(seq[2 : len(seq)-1])
	if body == "" {
		return []string{"0"}, true
	}
	if strings.ContainsAny(body, "<=>?") {
		return nil, false
	}
	params := strings.Split(body, ";")
	for i := 0; i < len(params); i++ {
		it := params[i]
		if (it == "38" || it == "48" || it == "58") && i+1 < len(params) {
			n := 0
			switch params[i+1] {
			case "5":
				n = 2
			case "2":
				n = 4
			}
			if i+n < len(params) && n > 0 {
				it = strings.Join(params[i:i+n+1], ";")
				i += n
			}
		}
		items = append(items, it)
	}
	return items, true
}

// mouseReport is an SGR mouse report: the button code, the 1-based cell,
// and whether it is a press or motion (M) or a release (m).
type mouseReport struct {
	button, x, y int
	press        bool
}

// mouseReports finds the SGR mouse reports (ESC [ < b ; x ; y M or m) in
// data.
func mouseReports(data []byte) []mouseReport {
	var out []mouseReport
	for {
		i := bytes.Index(data, []byte("\x1b[<"))
		if i < 0 {
			return out
		}
		data = data[i+3:]
		var f [3]int
		k, n, j := 0, 0, 0
		for ; j < len(data); j++ {
			c := data[j]
			if c >= '0' && c <= '9' && n < 6 {
				f[k] = f[k]*10 + int(c-'0')
				n++
				continue
			}
			if c == ';' && k < 2 && n > 0 {
				k, n = k+1, 0
				continue
			}
			break
		}
		if j < len(data) && k == 2 && n > 0 && (data[j] == 'M' || data[j] == 'm') {
			out = append(out, mouseReport{button: f[0], x: f[1], y: f[2], press: data[j] == 'M'})
		}
		data = data[j:]
	}
}
