package worker

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The field fixtures: each agent recorded under a real PTY at 120x40 with
// its input field idle, typed into past a wrap, given an Alt-Enter newline,
// cleared, pasted into and filled with a word longer than a row (record.go,
// the *-field scenarios). The marks say where in the stream each state is;
// replaying the stream to a mark shows the screen as it was then, and the
// detector must read the field off it as the measurements in
// docs/designs/smooth-remote-attach.md say. When an agent changes its box,
// this fails and the fixture is re-recorded.

var fieldFixtureText = strings.Repeat("the quick brown fox jumps over the lazy dog ", 4)

type fieldExpectation struct {
	rows     []string
	col      uint16 // the cursor's column
	rowBelow int    // the cursor's row below the field's first
}

func TestFieldFixtures(t *testing.T) {
	twoRows := []string{
		"the quick brown fox jumps over the lazy dog the quick brown fox jumps over the lazy dog the quick brown fox jumps",
		"over the lazy dog the quick brown fox jumps over the lazy dog",
	}
	piRows := []string{
		"the quick brown fox jumps over the lazy dog the quick brown fox jumps over the lazy dog the quick brown fox jumps over",
		"the lazy dog the quick brown fox jumps over the lazy dog",
	}
	pasted := []string{"line one", "line two", "line three"}
	// The typed text ends with a space and the cursor sits after it: the
	// cursor's row keeps it.
	typedRows := []string{twoRows[0], twoRows[1] + " "}
	piTypedRows := []string{piRows[0], piRows[1] + " "}
	for _, agent := range []struct {
		name          string
		profile       fieldProfile
		textCol       uint16
		width         uint16
		growsUp       bool
		inverseCursor bool
		marks         map[string]fieldExpectation
	}{
		{
			name: "claude", profile: fieldProfileFor("claude-code", "", ""), textCol: 2, width: 116, growsUp: true,
			marks: map[string]fieldExpectation{
				"idle":     {rows: []string{""}, col: 2},
				"typed":    {rows: typedRows, col: 64, rowBelow: 1},
				"newline":  {rows: append(append([]string(nil), twoRows...), "second line"), col: 13, rowBelow: 2},
				"cleared":  {rows: []string{""}, col: 2},
				"pasted":   {rows: pasted, col: 12, rowBelow: 2},
				"longword": {rows: []string{strings.Repeat("k", 116), strings.Repeat("k", 14)}, col: 16, rowBelow: 1},
			},
		},
		{
			name: "codex", profile: fieldProfileFor("", "codex", ""), textCol: 2, width: 117, growsUp: true,
			marks: map[string]fieldExpectation{
				"idle":     {rows: []string{""}, col: 2},
				"typed":    {rows: typedRows, col: 64, rowBelow: 1},
				"newline":  {rows: append(append([]string(nil), twoRows...), "second line"), col: 13, rowBelow: 2},
				"cleared":  {rows: []string{""}, col: 2},
				"pasted":   {rows: pasted, col: 12, rowBelow: 2},
				"longword": {rows: []string{strings.Repeat("k", 117), strings.Repeat("k", 13)}, col: 15, rowBelow: 1},
			},
		},
		{
			name: "pi", profile: fieldProfileFor("", "pi", ""), textCol: 0, width: 119, inverseCursor: true,
			marks: map[string]fieldExpectation{
				"idle":     {rows: []string{""}, col: 0},
				"typed":    {rows: piTypedRows, col: 57, rowBelow: 1},
				"newline":  {rows: append(append([]string(nil), piRows...), "second line"), col: 11, rowBelow: 2},
				"cleared":  {rows: []string{""}, col: 0},
				"pasted":   {rows: pasted, col: 10, rowBelow: 2},
				"longword": {rows: []string{strings.Repeat("k", 119), strings.Repeat("k", 11)}, col: 11, rowBelow: 1},
			},
		},
		{
			// pi --tui-mode fullscreen: the same box on the alternate screen.
			name: "pi-fullscreen", profile: fieldProfileFor("", "pi", ""), textCol: 0, width: 119, growsUp: true, inverseCursor: true,
			marks: map[string]fieldExpectation{
				"idle":     {rows: []string{""}, col: 0},
				"typed":    {rows: piTypedRows, col: 57, rowBelow: 1},
				"newline":  {rows: append(append([]string(nil), piRows...), "second line"), col: 11, rowBelow: 2},
				"cleared":  {rows: []string{""}, col: 0},
				"pasted":   {rows: pasted, col: 10, rowBelow: 2},
				"longword": {rows: []string{strings.Repeat("k", 119), strings.Repeat("k", 11)}, col: 11, rowBelow: 1},
			},
		},
	} {
		t.Run(agent.name, func(t *testing.T) {
			data, marks := loadFieldFixture(t, agent.name)
			for mark, want := range agent.marks {
				t.Run(mark, func(t *testing.T) {
					offset, ok := marks[mark]
					if !ok || offset > len(data) {
						t.Fatalf("no mark %q in the fixture", mark)
					}
					ts := newTestTerminal(t, 120, 40)
					ts.feed(data[:offset])
					f, err := detectField(ts.term, agent.profile, false)
					if err != nil {
						t.Fatal(err)
					}
					if !f.Found {
						t.Fatalf("no field found; screen:\n%s", formatContent(ts.term, 0, true))
					}
					if f.TextCol != agent.textCol || f.Width != agent.width || f.GrowsUp != agent.growsUp || f.InverseCursor != agent.inverseCursor ||
						f.WrapKeepsSpace != agent.profile.wrapKeepsSpace || f.Cols != 120 || f.Lines != 40 {
						t.Errorf("geometry: %+v", f)
					}
					if !reflect.DeepEqual(f.Rows, want.rows) {
						t.Errorf("rows\n got %q\nwant %q", f.Rows, want.rows)
					}
					if f.Col != want.col || int(f.Row)-int(f.Top) != want.rowBelow {
						t.Errorf("cursor at (%d, %d), top %d; want column %d on row %d of the field", f.Row, f.Col, f.Top, want.col, want.rowBelow)
					}
					if int(f.Top) < 20 {
						t.Errorf("the field sits at row %d, above the bottom half", f.Top)
					}
					if len(f.Chrome) == 0 || string(f.Style) != "\x1b[0m" {
						t.Errorf("chrome %d rows, style %q", len(f.Chrome), f.Style)
					}
				})
			}
		})
	}
}

// loadFieldFixture reads streams/NAME-field.vt.gz and its marks.
func loadFieldFixture(t *testing.T, name string) ([]byte, map[string]int) {
	t.Helper()
	dir := filepath.Join("testdata", "streams")
	f, err := os.Open(filepath.Join(dir, name+"-field.vt.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	js, err := os.ReadFile(filepath.Join(dir, name+"-field.marks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var marks map[string]int
	if err := json.Unmarshal(js, &marks); err != nil {
		t.Fatal(err)
	}
	return data, marks
}
