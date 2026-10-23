package worker

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"agtd.sh/internal/screen"
	"go.mitchellh.com/libghostty"
)

// The claude-scroll stream is Claude Code filling its transcript with
// "shell row 001" to "shell row 120" on the alternate screen, scrolling it
// with the wheel, growing its input box and clearing it
// (testdata/streams/README.md). Replayed through the transcript record at
// several granularities, from every repaint to what a frame client on a
// slow link would see, never inside a synchronized repaint, every row must
// be recorded once, in order, as long as no more than a screen scrolls
// between two looks.
func TestTranscriptRecordsClaudeScroll(t *testing.T) {
	var data []byte
	for _, st := range loadStreams(t) {
		if st.name == "claude-scroll" {
			data = st.data
		}
	}
	if data == nil {
		t.Skip("no claude-scroll stream")
	}
	repaints := splitAfter(data, []byte("\x1b[?2026l"))
	cases := []struct {
		name   string
		pieces [][]byte
		// exact: nothing scrolls more than a screen between two looks.
		exact bool
	}{
		{"every repaint", repaints, true},
		{"every 4 repaints", group(repaints, 4), true},
		{"every 16 repaints", group(repaints, 16), true},
		// The worker looks after every read and at every synchronized
		// block's end inside one (decision 1), skipping a read that ends
		// inside a block.
		{"2 KiB reads", readLooks(data, 2048), true},
		{"16 KiB reads", readLooks(data, 16384), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec, looks := replayTranscript(t, c.pieces)
			seen := map[int]int{}
			last, order := 0, true
			var text []string
			for _, row := range rec.Rows(0) {
				line := plainRow(row)
				text = append(text, line)
				var n int
				if i := strings.Index(line, "shell row "); i >= 0 {
					if _, err := fmt.Sscanf(line[i:], "shell row %d", &n); err == nil {
						seen[n]++
						if n < last {
							order = false
						}
						last = n
					}
				}
			}
			var missing, dup []int
			for n := 1; n <= 120; n++ {
				switch {
				case seen[n] == 0:
					missing = append(missing, n)
				case seen[n] > 1:
					dup = append(dup, n)
				}
			}
			t.Logf("%d looks, %d rows recorded, %d jumps, missing %v, duplicated %v, in order %v", looks, rec.Recorded(), rec.Jumps, missing, dup, order)
			if dir := os.Getenv("TRANSCRIPT_DUMP"); dir != "" {
				os.WriteFile(dir+"/"+strings.ReplaceAll(c.name, " ", "-")+".txt", []byte(strings.Join(text, "\n")+"\n"), 0o644)
				var raw []string
				for _, row := range rec.Rows(0) {
					raw = append(raw, fmt.Sprintf("%q", row))
				}
				os.WriteFile(dir+"/"+strings.ReplaceAll(c.name, " ", "-")+".raw", []byte(strings.Join(raw, "\n")+"\n"), 0o644)
			}
			if c.exact && (len(missing) > 0 || len(dup) > 0 || !order) {
				t.Errorf("missing %v, duplicated %v, in order %v", missing, dup, order)
			}
		})
	}
}

// replayTranscript feeds the pieces to a terminal and the alternate
// screen's rows after each piece to a transcript record.
func replayTranscript(t *testing.T, pieces [][]byte) (*screen.Transcript, int) {
	term, err := libghostty.NewTerminal(libghostty.WithSize(streamCols, streamRows))
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	rec := screen.NewTranscript(0)
	looks := 0
	for _, p := range pieces {
		term.VTWrite(p)
		active, err := term.ActiveScreen()
		if err != nil {
			t.Fatal(err)
		}
		if active != libghostty.ScreenAlternate {
			continue
		}
		// A repaint is looked at whole, as the worker does: never inside
		// a synchronized-output block.
		if syncing, err := term.Mode(libghostty.ModeSyncOutput); err != nil || syncing {
			continue
		}
		rows, err := frameRows(term, streamCols, streamRows)
		if err != nil {
			t.Fatal(err)
		}
		rec.Update(rows, transcriptEnd(t, term))
		looks++
	}
	return rec, looks
}

// transcriptEnd is the rows above Claude Code's input box and the hint row
// over it, or 0 when no field is found.
func transcriptEnd(t *testing.T, term *libghostty.Terminal) int {
	f, err := detectField(term, fieldProfiles["claude"], false)
	if err != nil {
		t.Fatal(err)
	}
	if f == nil || !f.Found || f.BoxTop < 1 {
		return 0
	}
	return int(f.BoxTop) - 1
}

func splitAfter(data, sep []byte) [][]byte {
	var out [][]byte
	for len(data) > 0 {
		i := bytes.Index(data, sep)
		if i < 0 {
			return append(out, data)
		}
		out = append(out, data[:i+len(sep)])
		data = data[i+len(sep):]
	}
	return out
}

func group(pieces [][]byte, n int) [][]byte {
	var out [][]byte
	for i := 0; i < len(pieces); i += n {
		out = append(out, bytes.Join(pieces[i:min(i+n, len(pieces))], nil))
	}
	return out
}

// readLooks cuts data into reads of n bytes, and each read after every
// end of a synchronized block in it.
func readLooks(data []byte, n int) [][]byte {
	var out [][]byte
	for _, c := range chunks(data, n) {
		out = append(out, splitAfter(c, []byte("\x1b[?2026l"))...)
	}
	return out
}

func chunks(data []byte, n int) [][]byte {
	var out [][]byte
	for i := 0; i < len(data); i += n {
		out = append(out, data[i:min(i+n, len(data))])
	}
	return out
}

var vtEscape = regexp.MustCompile(`\x1b(\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(\x07|\x1b\\)|.)`)

// plainRow is a row's text without its escape sequences.
func plainRow(row []byte) string {
	return strings.TrimRight(vtEscape.ReplaceAllString(screen.RowKey(row), ""), " ")
}
