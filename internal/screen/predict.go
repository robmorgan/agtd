package screen

import (
	"bytes"
	"math"
	"regexp"
	"time"

	"agtd.sh/internal/protocol"
)

// Wheel prediction (docs/designs/alt-screen-scrollback.md, decision 12):
// under frames, with or without alt scrollback, a wheel tick over the
// transcript of an agent on the alternate screen is drawn at once, the transcript rows shifted by the rows the agent moves per tick
// and filled from the record, while the tick goes to the agent as before.
// The worker's frames stay the authority: the guess is drawn over them
// until they show the agent has moved as far, and taken back when the
// agent does otherwise. A guess never touches the rows under the
// transcript (the input box and status rows).

const (
	// predictSettle is how long the agent gets to scroll once a frame
	// shows the worker has read the tick, before the guess is taken back.
	predictSettle = 150 * time.Millisecond
	// predictWait is how long, beyond two round trips, a guess stands
	// after its tick without such a frame: none comes while nothing moves.
	predictWait = 300 * time.Millisecond
	// predictMaxStep bounds the rows per tick learned.
	predictMaxStep = 10
	// predictMaxPinned is the most transcript rows a guess at the right
	// place may get wrong for them to be taken as the agent's chrome
	// (pinned) rather than a wrong guess.
	predictMaxPinned = 2
	// recordRows bounds the transcript record, kept for alt scrollback,
	// the prediction, or both.
	recordRows = 10000
	// predictSeenRows bounds the rows kept as last seen, on each side of
	// the agent's screen.
	predictSeenRows = 2000
)

// wheelPredict is the wheel prediction's state, owned by the Painter.
type wheelPredict struct {
	on  bool
	rtt time.Duration
	// resized: the terminal's size changed since the last frame, whose
	// rows a guess would lay out for the old size.
	resized bool
	// stepF is the rows the agent moves per tick, learned and smoothed.
	stepF float64
	// debt is how far the shown transcript is ahead of the agent's, in
	// rows, positive when the content moved up (the wheel down), as
	// Shift counts it.
	debt int64
	// seqAtTick is the last frame painted when the newest tick went: a
	// frame built after the worker read a later ack has seen the tick.
	seqAtTick uint32
	// settleBy is when a guess still unanswered is taken back, or a
	// gesture that drew nothing is judged; zero when nothing is pending.
	settleBy time.Time
	// The gesture: ticks since the debt was last paid off (signed), the
	// rows the agent moved meanwhile, and whether a tick met an edge of
	// what the record holds.
	ticks, rows int64
	clamped     bool
	// settled: the last frame paid the debt off exactly, and Apply judges
	// the guess against it.
	settled bool
	// misses counts guesses in a row the agent showed wrong; at two,
	// guessing pauses until two gestures in a row would have been right.
	misses, hits int
	paused       bool
	// seen holds each transcript row by logical number as the agent last
	// drew it, within predictSeenRows of its screen: the record keeps a
	// row as it was when it left the top, and an agent redraws rows (a
	// tool's "Running…" becomes its result), and the live end of the
	// transcript never left the top at all.
	seen map[int64][]byte
	// pinned are the transcript rows the agent draws over what is under
	// them (Claude Code's "Jump to bottom" while scrolled up): a guess
	// leaves them as the agent last drew them. A row is pinned once two
	// judged guesses in a row moved it from the agent's screen and got it
	// wrong (suspect: the last one's): the agent did not move it with the
	// rest. A row filled from seen or the record and wrong is stale
	// content, which the frame corrects in seen, not chrome. pinnedEnd is
	// the transcript's end they were learned at.
	pinned, suspect map[int]bool
	pinnedEnd       int
	// shownDebt is the debt the guess drawn (Painter.shown) was made for.
	shownDebt int64
	// lastDir is the direction of the last tick (-1 up, 1 down, 0 none
	// yet). Claude Code does not move for the first tick after the wheel
	// turns the other way, so neither does the guess, unless the agent
	// was seen to (reversalMoves). swallowed is the direction of a first
	// tick the guess left undrawn and the agent has not answered yet
	// (0: none), and reversed whether the gesture's first tick turned
	// the wheel around.
	lastDir, swallowed int64
	reversalMoves      bool
	reversed           bool
	stats              PredictStats
}

// PredictStats counts the wheel prediction's work.
type PredictStats struct {
	// Ticks are the wheel ticks over the transcript; Guesses the ones
	// drawn at once.
	Ticks, Guesses int
	// TakenBack are the guesses the agent did not follow: it did not
	// move in time, moved further or the other way, or jumped.
	TakenBack int
	// Corrections are the guesses the agent followed to the same place
	// with rows drawn otherwise; Rows those rows, and Edge the ones of
	// them the guess filled from what scrolled in rather than moved.
	Corrections, Rows, Edge int
}

// SetPredictScroll turns on the wheel prediction, with or without alt
// scrollback (without it, the record is kept for the guesses alone); rtt
// is the link's round trip, which bounds how long a guess waits for the
// agent.
func (p *Painter) SetPredictScroll(rtt time.Duration) {
	p.pr.on, p.pr.rtt = true, rtt
	if p.pr.stepF == 0 {
		p.pr.stepF = 1
	}
}

// SetClock replaces the painter's clock (time.Now), for replays.
func (p *Painter) SetClock(now func() time.Time) { p.clock = now }

// PredictStats returns the wheel prediction's counts.
func (p *Painter) PredictStats() PredictStats { return p.pr.stats }

// PredictDue is when PredictExpire has work, zero for never.
func (p *Painter) PredictDue() time.Time { return p.pr.settleBy }

func (p *Painter) now() time.Time {
	if p.clock != nil {
		return p.clock()
	}
	return time.Now()
}

// Wheel takes input on its way to the agent and draws the wheel ticks in
// it at once: SGR mouse reports of the plain wheel over the transcript.
// It returns what to write to the terminal, nil for nothing.
func (p *Painter) Wheel(data []byte) []byte {
	if !p.pr.on || p.alt == nil {
		return nil
	}
	drawn := false
	for _, dir := range wheelTicks(data, p.altEnd) {
		drawn = p.tick(dir) || drawn
	}
	if !drawn {
		return nil
	}
	return p.repaintShown()
}

// PredictExpire takes back a guess the agent has not answered in time
// (see predictSettle and predictWait), and judges a gesture that drew
// nothing. It returns what to write to the terminal.
func (p *Painter) PredictExpire() []byte {
	pr := &p.pr
	if pr.settleBy.IsZero() || p.now().Before(pr.settleBy) {
		return nil
	}
	if pr.debt == 0 && pr.clamped && !pr.paused {
		// Nothing drawn, every tick at the edge of what the record
		// holds (above it, after attaching to a long transcript): no
		// guess was made, so the gesture neither confirms nor faults
		// the guessing.
		pr.ticks, pr.rows, pr.clamped, pr.settled, pr.reversed, pr.settleBy = 0, 0, false, false, false, time.Time{}
		return nil
	}
	if pr.debt == 0 {
		// Nothing drawn (paused, or ticks that drew nothing unclamped):
		// the gesture would have been right if the agent moved as far as
		// guessed.
		p.endGesture(pr.ticks != 0 && !pr.clamped && pr.rows == pr.ticks*p.step())
		return nil
	}
	if pr.reversed && pr.rows == 0 && abs(pr.ticks) == 1 {
		// Drawn for a first tick after a reversal, which the agent did
		// not move for: from now on such a tick is left undrawn.
		pr.reversalMoves = false
	}
	pr.debt = 0
	pr.stats.TakenBack++
	p.endGesture(false)
	return p.repaintShown()
}

// predictable reports whether a tick can be drawn now.
func (p *Painter) predictable() bool {
	return !p.pr.resized && p.alt != nil && p.altEnd > 0 && p.painted && len(p.rows) > p.altEnd && !p.modes["?2026"]
}

func (p *Painter) step() int64 {
	return min(max(int64(math.Round(p.pr.stepF)), 1), predictMaxStep)
}

// tick notes a wheel tick (dir -1 up, 1 down) and reports whether the
// guess drawn changed.
func (p *Painter) tick(dir int64) bool {
	pr := &p.pr
	// The agent sees every tick, drawn or not, so the direction is kept
	// first.
	reversal := pr.lastDir != 0 && dir != pr.lastDir
	pr.lastDir, pr.swallowed = dir, 0
	if !p.predictable() {
		return false
	}
	pr.stats.Ticks++
	if reversal {
		if !pr.reversalMoves {
			pr.swallowed = dir
			return false
		}
		pr.reversed = pr.reversed || pr.ticks == 0
	}
	pr.ticks += dir
	pr.seqAtTick = p.seq
	pr.settleBy = p.now().Add(2*pr.rtt + predictWait)
	if pr.paused {
		return false
	}
	full := pr.debt + dir*p.step()
	want := full
	for want != pr.debt && p.guess(want) == nil {
		want -= dir
	}
	pr.clamped = pr.clamped || want != full
	if want == pr.debt {
		return false
	}
	pr.debt = want
	pr.stats.Guesses++
	return true
}

// guess is the screen with the transcript moved debt rows further than
// the agent has it: rows still on its screen are moved, the others come
// from the rows as last seen, or else the record. Nil when a row needed
// is not held.
func (p *Painter) guess(debt int64) [][]byte {
	end := p.altEnd
	pos, mark, oldest := p.alt.Pos(), p.alt.Recorded(), p.alt.Oldest()
	out := make([][]byte, len(p.rows))
	copy(out, p.rows)
	for y := 0; y < end; y++ {
		if p.pr.pinned[y] {
			continue
		}
		src := int64(y) + debt
		if src >= 0 && src < int64(end) && !p.pr.pinned[int(src)] {
			out[y] = p.rows[src]
			continue
		}
		n := pos + src
		if r, ok := p.pr.seen[n]; ok {
			out[y] = r
			continue
		}
		if src >= 0 && src < int64(end) {
			// Under the agent's own chrome (a pinned row): not known
			// until the agent draws it, so left as it is for the frame
			// to correct.
			continue
		}
		if n < oldest || n >= mark {
			return nil
		}
		out[y] = p.alt.Rows(n)[0]
	}
	return out
}

// observe takes the agent's screen as the record placed it: cur moved
// moved rows (Shift's sign), or jumped to content the record does not
// hold. It keeps the rows as seen, and pays the guess off by what the
// agent moved. Called by altFrame after the record's Update.
func (p *Painter) observe(cur [][]byte, moved int64, jumped bool, f *protocol.Frame) {
	pr := &p.pr
	if !pr.on {
		return
	}
	pos := p.alt.Pos()
	if jumped || pr.seen == nil {
		// Rows past the record's end belonged to the screen left.
		mark := p.alt.Recorded()
		for n := range pr.seen {
			if n >= mark {
				delete(pr.seen, n)
			}
		}
		if pr.seen == nil {
			pr.seen = map[int64][]byte{}
		}
	}
	if p.altEnd != pr.pinnedEnd {
		pr.pinned, pr.suspect, pr.pinnedEnd = nil, nil, p.altEnd
	}
	if p.altEnd > 0 {
		for y := 0; y < min(p.altEnd, len(cur)); y++ {
			if !pr.pinned[y] {
				pr.seen[pos+int64(y)] = cur[y]
			}
		}
	}
	if len(pr.seen) > 3*predictSeenRows {
		for n := range pr.seen {
			if n < pos-predictSeenRows || n > pos+predictSeenRows {
				delete(pr.seen, n)
			}
		}
	}
	if pr.swallowed != 0 && moved != 0 {
		// The agent moved for a first tick after a reversal after all.
		pr.reversalMoves = (moved > 0) == (pr.swallowed > 0)
		pr.swallowed = 0
	}
	if pr.debt == 0 && pr.ticks == 0 {
		return
	}
	pr.rows += moved
	switch {
	case jumped:
		if pr.debt != 0 {
			pr.debt = 0
			pr.stats.TakenBack++
			p.endGesture(false)
		}
		return
	case pr.debt == 0 || moved == 0:
	case (moved > 0) == (pr.debt > 0) && abs(moved) <= abs(pr.debt):
		pr.debt -= moved
		pr.settled = pr.debt == 0
	default:
		// Overtaken, or moved the other way: what the agent shows wins.
		pr.debt = 0
		pr.stats.TakenBack++
		p.endGesture((moved > 0) == (pr.ticks > 0))
		return
	}
	if pr.debt != 0 && f.AckedBy() > pr.seqAtTick {
		if by := p.now().Add(predictSettle); by.Before(pr.settleBy) {
			pr.settleBy = by
		}
	}
}

// endGesture learns the rows per tick from the gesture, notes whether
// its guess was right, and starts the next.
func (p *Painter) endGesture(right bool) {
	pr := &p.pr
	if pr.ticks != 0 && !pr.clamped && pr.rows != 0 && (pr.rows > 0) == (pr.ticks > 0) {
		pr.stepF = 0.75*pr.stepF + 0.25*float64(pr.rows)/float64(pr.ticks)
	}
	switch {
	case right && pr.paused:
		if pr.hits++; pr.hits >= 2 {
			pr.paused, pr.hits, pr.misses = false, 0, 0
		}
	case right:
		pr.misses = 0
	default:
		pr.hits = 0
		if pr.misses++; pr.misses >= 2 {
			pr.paused = true
		}
	}
	pr.ticks, pr.rows, pr.clamped, pr.settled, pr.reversed = 0, 0, false, false, false
	if pr.debt == 0 {
		pr.settleBy = time.Time{}
	}
}

// resetPredict drops the guess and what the prediction knows of the
// screen, for a screen that was replaced (full, resized, left).
// Resized drops the guess, for a terminal whose size just changed: until
// a frame arrives, the rows the painter has are laid out for the old size,
// and a guess drawn from them would wrap or land on the input box.
func (p *Painter) Resized() {
	p.resetPredict()
	p.pr.resized = true
}

func (p *Painter) resetPredict() {
	pr := &p.pr
	pr.debt, pr.ticks, pr.rows, pr.clamped, pr.settled, pr.reversed = 0, 0, 0, false, false, false
	pr.swallowed = 0
	pr.settleBy = time.Time{}
	pr.seen, pr.pinned, pr.suspect = nil, nil, nil
	p.shown = nil
}

// reconcileShown writes, inside Apply's synchronized output, the rows
// where the terminal differs from what should be shown now: the guess
// over the frame just painted, or the frame alone. written are the rows
// Apply just wrote (nil for none). A guess paid off exactly is judged
// here: rows it got wrong under the transcript's right place are the
// agent's chrome, pinned, if they are few.
func (p *Painter) reconcileShown(b *bytes.Buffer, written []bool) {
	pr := &p.pr
	var want [][]byte
	if pr.debt != 0 {
		if want = p.guess(pr.debt); want == nil {
			pr.debt = 0
			pr.stats.TakenBack++
			p.endGesture(false)
		}
	}
	var moved, fetched []int
	for y := range p.rows {
		final := p.rows[y]
		if want != nil {
			final = want[y]
		}
		if pr.settled && p.shown != nil && y < p.altEnd && y < len(p.shown) && !pr.pinned[y] && plainKey(p.shown[y]) != plainKey(final) {
			if src := int64(y) + pr.shownDebt; src >= 0 && src < int64(p.altEnd) && !pr.pinned[int(src)] {
				moved = append(moved, y)
			} else {
				fetched = append(fetched, y)
			}
		}
		have := p.rows[y]
		if p.shown != nil && y < len(p.shown) && (written == nil || !written[y]) {
			have = p.shown[y]
		}
		if !bytes.Equal(have, final) {
			b.WriteString(cup(y, 0) + rowStart)
			b.Write(final)
		}
	}
	if pr.settled {
		var fresh []int
		suspect := map[int]bool{}
		for _, y := range moved {
			if pr.suspect[y] {
				if pr.pinned == nil {
					pr.pinned = map[int]bool{}
				}
				pr.pinned[y] = true
				delete(pr.seen, p.alt.Pos()+int64(y))
				continue
			}
			suspect[y] = true
			fresh = append(fresh, y)
		}
		pr.suspect = suspect
		if n := len(moved) + len(fetched); n > 0 {
			pr.stats.Corrections++
			pr.stats.Rows += n
			pr.stats.Edge += len(fetched)
		}
		p.endGesture(len(fresh) <= predictMaxPinned)
	}
	p.shown, pr.shownDebt = want, pr.debt
}

// repaintShown writes, outside any frame, the rows where the terminal
// differs from the guess (or, with none, from the agent's screen), leaving
// the cursor and pen as they were.
func (p *Painter) repaintShown() []byte {
	var want [][]byte
	if p.pr.debt != 0 {
		want = p.guess(p.pr.debt)
	}
	var b bytes.Buffer
	for y := range p.rows {
		have, final := p.rows[y], p.rows[y]
		if p.shown != nil && y < len(p.shown) {
			have = p.shown[y]
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
	p.shown, p.pr.shownDebt = want, p.pr.debt
	if b.Len() == 0 {
		return nil
	}
	b.WriteString("\x1b8" + syncEnd)
	return b.Bytes()
}

// wheelTicks finds the plain wheel's SGR mouse reports (ESC [ < 64 or 65
// ; x ; y M) in data over the top end rows: -1 for each tick up, 1 down.
// A wheel with a modifier, which a terminal keeps for itself or the
// agent may treat otherwise, is not counted.
func wheelTicks(data []byte, end int) []int64 {
	var out []int64
	for {
		i := bytes.Index(data, []byte("\x1b[<"))
		if i < 0 {
			return out
		}
		data = data[i+3:]
		var f [3]int
		n := 0
		j := 0
		for ; j < len(data) && n < 3; j++ {
			c := data[j]
			switch {
			case c >= '0' && c <= '9':
				f[n] = f[n]*10 + int(c-'0')
			case c == ';' && n < 2:
				n++
			case (c == 'M' || c == 'm') && n == 2:
				n = 3
			default:
				n = 4
			}
		}
		if n != 3 {
			continue
		}
		data = data[j:]
		if f[2] < 1 || f[2] > end {
			continue
		}
		switch f[0] {
		case 64:
			out = append(out, -1)
		case 65:
			out = append(out, 1)
		}
	}
}

// plainKey is a row's text as seen: RowKey without its styling, since an
// agent redraws the same text in differently split runs of colour.
func plainKey(row []byte) string { return sgr.ReplaceAllString(RowKey(row), "") }

var sgr = regexp.MustCompile("\x1b\\[[0-9;:]*m")

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
