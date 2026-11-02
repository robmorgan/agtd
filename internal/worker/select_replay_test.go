package worker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The selection prediction over the claude-select and pi-fullscreen-select
// streams: the painter gets the recorder's mouse reports as the CLI would,
// before the output that answers each. The first drag teaches it the
// agent's selection; over the second, every drag's guess must show what
// the agent then drew, and be judged right; at the top edge nothing is
// guessed; and the client's screen must end as the agent's.
func TestFramesPredictSelection(t *testing.T) {
	press := func(x, y int) string { return fmt.Sprintf("\x1b[<0;%d;%dM", x, y) }
	drag := func(x, y int) string { return fmt.Sprintf("\x1b[<32;%d;%dM", x, y) }
	release := func(x, y int) string { return fmt.Sprintf("\x1b[<0;%d;%dm", x, y) }
	click := press(30, 20) + release(30, 20)
	// The recorder's selectSteps, each with the mark its answer ends at
	// ("" for a step with none, answered by the next mark).
	steps := []struct{ input, mark string }{
		{press(10, 12), "learn-pressed"},
		{drag(14, 12), "learn-drag-1"},
		{drag(25, 12), "learn-drag-2"},
		{drag(25, 14), "learn-drag-3"},
		{drag(5, 16), "learn-drag-4"},
		{release(5, 16), "learn-released"},
		{click, "learn-cleared"},
		{press(12, 13), "select-pressed"},
		{drag(30, 13), "select-drag-1"},
		{drag(8, 17), "select-drag-2"},
		{drag(40, 15), "select-drag-3"},
		{drag(20, 11), "select-drag-4"},
		{release(20, 11), "select-released"},
		{click, "select-cleared"},
		{press(10, 12), ""},
		{drag(10, 1), "edge-held"},
		{release(10, 1), "edge-released"},
		{click, "edge-cleared"},
	}
	for _, name := range []string{"claude-select", "pi-fullscreen-select"} {
		t.Run(name, func(t *testing.T) {
			var data []byte
			for _, st := range loadStreams(t) {
				if st.name == name {
					data = st.data
				}
			}
			if data == nil {
				t.Fatalf("no %s stream", name)
			}
			var marks map[string]int
			if err := json.Unmarshal(must(os.ReadFile(filepath.Join("testdata", "streams", name+".marks.json"))), &marks); err != nil {
				t.Fatal(err)
			}
			r := newFrameRig(t, streamCols, streamRows)
			now := time.Unix(1e9, 0)
			r.p.SetPredictSelection(0)
			r.p.SetClock(func() time.Time { return now })
			off := 0
			feedTo := func(mark string) {
				r.feed(string(data[off:marks[mark]]))
				off = marks[mark]
			}
			feedTo("shell-2")
			r.frame()
			guessedBefore := 0
			for _, st := range steps {
				r.client.VTWrite(r.p.Select([]byte(st.input)))
				if st.mark == "" {
					continue
				}
				if st.mark == "edge-held" {
					guessedBefore = r.p.SelectStats().Guessed
				}
				feedTo(st.mark)
				if strings.HasPrefix(st.mark, "select-drag-") {
					// The guess, drawn before the answer was painted,
					// is the answer, over the rows the gesture went
					// over (rows 11 to 17; the agent's status rows go
					// on changing below).
					want, _, _, _, _ := cellsOf(r.src.term)
					got, _, _, _, _ := cellsOf(r.client)
					w, g := strings.Split(want, "\n")[10:17], strings.Split(got, "\n")[10:17]
					if d := strings.Join(w, "\n"); d != strings.Join(g, "\n") {
						d = firstDiff(d, strings.Join(g, "\n"))
						t.Fatalf("%s: the guess differs from the agent's selection at %s (from row 11)", st.mark, d)
					}
				}
				r.frame()
				if strings.HasSuffix(st.mark, "-released") {
					now = now.Add(time.Second)
					r.client.VTWrite(r.p.PredictExpire())
				}
				r.same(st.mark)
			}
			stats := r.p.SelectStats()
			t.Logf("%+v", stats)
			if stats.Gestures != 3 || stats.Wrong != 0 || stats.Right < 2 {
				t.Errorf("stats %+v: want 3 gestures, the first two guessed right", stats)
			}
			if stats.Guessed != guessedBefore {
				t.Errorf("a drag at the top edge was guessed")
			}
		})
	}
}
