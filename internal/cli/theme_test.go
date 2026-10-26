package cli

import (
	"strings"
	"testing"
)

func TestCellWidths(t *testing.T) {
	for _, c := range []struct {
		s    string
		want int
	}{{"", 0}, {"abc", 3}, {"日本", 4}, {"ok 👍", 5}, {"é", 1}, {"●⚠○", 3}} {
		if got := cellWidth(c.s); got != c.want {
			t.Errorf("cellWidth(%q) = %d, want %d", c.s, got, c.want)
		}
	}
	for _, c := range []struct {
		s    string
		n    int
		want string
	}{
		{"abcdef", 3, "abc"},
		{"日本語", 3, "日"}, // the second character would straddle the edge
		{"日本語", 4, "日本"},
		{"ab👍cd", 3, "ab"},
		{"éx", 1, "é"}, // a combining mark stays with its letter
		{"abc", 0, ""},
		{"abc", 10, "abc"},
	} {
		if got := takeCells(c.s, c.n); got != c.want {
			t.Errorf("takeCells(%q, %d) = %q, want %q", c.s, c.n, got, c.want)
		}
	}
	if got := truncateCells("needs 日本語 review", 10); got != "needs 日…" || cellWidth(got) != 9 {
		t.Errorf("truncateCells = %q (%d cells)", got, cellWidth(got))
	}
	if got := padCells("日本", 6); got != "日本  " {
		t.Errorf("padCells = %q", got)
	}
}

// A line renders to exactly its width in cells, whatever its spans hold,
// so the terminal never wraps it.
func TestRenderFillsWidthInCells(t *testing.T) {
	th := &overlayTheme{}
	for _, l := range []line{
		plain("short"),
		plain(strings.Repeat("x", 50)),
		{spans: []span{{"", "waiting "}, {"", "レビューをお願いします 👍👍👍"}}},
		{spans: []span{{"", "abc"}, {"", "日"}}},
	} {
		got := l.render(20, th)
		if w := cellWidth(got); w != 20 {
			t.Errorf("render(%v) = %q, %d cells", l.spans, got, w)
		}
	}
	// A wide character that does not fit is replaced by padding.
	if got := (line{spans: []span{{"", strings.Repeat("a", 19) + "日"}}}).render(20, th); got != strings.Repeat("a", 19)+" " {
		t.Errorf("straddling character: %q", got)
	}
}

// The empty theme (NO_COLOR, TERM=dumb) emits no escape sequences at all,
// and the inbox theme leaves the terminal's background and text alone.
func TestThemes(t *testing.T) {
	empty := overlayThemeFor(func(k string) string {
		if k == "NO_COLOR" {
			return "1"
		}
		return ""
	})
	l := empty.listRow(30, true, []span{{empty.bold, "demo"}}, span{empty.quietFG, "3m"})
	if got := l.render(30, empty); strings.Contains(got, "\x1b") || cellWidth(got) != 30 || !strings.HasPrefix(got, "▌ demo") || !strings.HasSuffix(got, "3m ") {
		t.Errorf("empty theme row = %q", got)
	}
	// The block cursor is reverse video, which is not colour.
	got := empty.input("❯", "", "Filter", true).render(20, empty)
	if strings.Contains(strings.NewReplacer("\x1b[7m", "", ansiReset, "").Replace(got), "\x1b") {
		t.Errorf("empty theme input = %q", got)
	}
	in := inboxTheme(&overlayTrueColor)
	if got := plain("x").render(5, in); strings.Contains(got, "\x1b[48;") || strings.Contains(got, overlayTrueColor.textFG) {
		t.Errorf("inbox theme paints the terminal's background or text: %q", got)
	}
	sel := in.listRow(10, true, []span{{"", "x"}}, span{})
	if got := sel.render(10, in); !strings.Contains(got, overlayTrueColor.lineBG) || !strings.Contains(got, overlayTrueColor.textFG) {
		t.Errorf("inbox selected row = %q", got)
	}
}
