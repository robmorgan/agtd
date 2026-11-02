//go:build ignore

// probe_select shows how a coding agent with mouse reporting on draws a
// mouse selection: it fills the agent's transcript with known rows, then
// presses, drags and releases the left button (SGR reports), and after each
// step prints the rows whose cells changed since the step before, with
// each changed cell's style, and any clipboard write. Nothing is submitted
// to a model.
//
//	PKG_CONFIG_PATH=.build/ghostty-out/share/pkgconfig go run internal/worker/testdata/probe_select.go -only claude -dir /tmp/x
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
	"go.mitchellh.com/libghostty"
)

const (
	cols = 120
	rows = 40
)

type step struct {
	want  string
	only  string
	send  string
	pause time.Duration
	dump  string
}

// press, drag and release are SGR mouse reports for the left button at a
// 1-based column and row; drag is motion with the button held.
func press(x, y int) string   { return fmt.Sprintf("\x1b[<0;%d;%dM", x, y) }
func drag(x, y int) string    { return fmt.Sprintf("\x1b[<32;%d;%dM", x, y) }
func release(x, y int) string { return fmt.Sprintf("\x1b[<0;%d;%dm", x, y) }

// fill prints "shell row 001" to "shell row N" from the agent's shell
// escape (! in both agents).
func fill(n int, settle time.Duration) []step {
	return []step{
		{send: "!", pause: 500 * time.Millisecond},
		{send: fmt.Sprintf("seq -f 'shell row %%03g' 1 %d", n), pause: 300 * time.Millisecond},
		{send: "\r"},
		{want: fmt.Sprintf("shell row %03d", n), pause: settle, dump: "filled"},
	}
}

// gestures are the same for every agent: a drag within a row, one down
// across rows, a drag held past the top edge (does the agent scroll?),
// a click to clear, a double click, a triple click.
func gestures() []step {
	p := 400 * time.Millisecond
	return []step{
		{send: press(10, 12), pause: p, dump: "pressed"},
		{send: drag(14, 12), pause: p, dump: "drag-right-4"},
		{send: drag(25, 12), pause: p, dump: "drag-right-15"},
		{send: drag(25, 14), pause: p, dump: "drag-down-2"},
		{send: drag(5, 16), pause: p, dump: "drag-down-4-left"},
		{send: release(5, 16), pause: 1500 * time.Millisecond, dump: "released"},
		{send: press(30, 20) + release(30, 20), pause: time.Second, dump: "clicked-away"},
		{send: press(10, 12), pause: p},
		{send: drag(10, 8), pause: p, dump: "drag-up-4"},
		{send: drag(10, 1), pause: 300 * time.Millisecond, dump: "drag-top-edge-0.3s"},
		{pause: 1200 * time.Millisecond, dump: "drag-top-edge-1.5s"},
		{send: drag(10, 1), pause: 1500 * time.Millisecond, dump: "drag-top-edge-moved-again"},
		{send: release(10, 1), pause: 1500 * time.Millisecond, dump: "released-top"},
		{send: press(30, 20) + release(30, 20), pause: time.Second, dump: "clicked-away-2"},
		{send: press(8, 12) + release(8, 12) + press(8, 12) + release(8, 12), pause: time.Second, dump: "double-click"},
		{send: press(30, 20) + release(30, 20), pause: time.Second},
		{send: press(8, 12) + release(8, 12) + press(8, 12) + release(8, 12) + press(8, 12) + release(8, 12), pause: time.Second, dump: "triple-click"},
		{send: press(30, 20) + release(30, 20), pause: time.Second, dump: "clicked-away-3"},
	}
}

func steps(name, pi string) (argv []string, env []string, st []step) {
	switch name {
	case "claude":
		return []string{"claude"}, userEnv(), append([]step{
			{want: "❯", pause: 2 * time.Second},
			{only: "Keep xhigh", send: "\r", pause: 3 * time.Second},
			{only: "trust this folder", send: "\x1b[B", pause: 500 * time.Millisecond},
			{only: "trust this folder", send: "\r", pause: 4 * time.Second},
			{want: "Try \"", pause: time.Second},
		}, append(append(fill(60, 2*time.Second), gestures()...), step{send: "\x03", pause: 300 * time.Millisecond}, step{send: "\x03", pause: 2 * time.Second})...)
	case "pi":
		dir, _ := os.MkdirTemp("", "pi-agent")
		return []string{pi, "--tui-mode", "fullscreen", "--offline", "--no-session",
				"--no-extensions", "--no-skills", "--no-prompt-templates", "--no-context-files"},
			append(userEnv(), "PI_CODING_AGENT_DIR="+dir), append([]step{
				{pause: 6 * time.Second},
			}, append(append(fill(60, 2*time.Second), gestures()...), step{send: "\x03", pause: 500 * time.Millisecond}, step{send: "\x03", pause: 500 * time.Millisecond}, step{send: "\x04", pause: 2 * time.Second})...)
	}
	return nil, nil, nil
}

func userEnv() []string {
	env := []string{"TERM=xterm-256color", "COLORTERM=truecolor", "LANG=en_US.UTF-8", "DISABLE_AUTOUPDATER=1"}
	for _, key := range []string{"HOME", "USER", "LOGNAME", "PATH", "SHELL", "TMPDIR"} {
		if v, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+v)
		}
	}
	return env
}

func main() {
	only := flag.String("only", "", "agent to probe")
	dir := flag.String("dir", "", "working directory")
	pi := flag.String("pi", "pi", "pi binary")
	flag.Parse()
	argv, env, st := steps(*only, *pi)
	if argv == nil || *dir == "" {
		fmt.Fprintln(os.Stderr, "usage: -only claude|pi -dir DIR [-pi PATH]")
		os.Exit(2)
	}
	if err := run(argv, env, st, *dir); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(argv []string, env []string, st []step, dir string) error {
	var replies [][]byte
	term, err := libghostty.NewTerminal(
		libghostty.WithSize(cols, rows),
		libghostty.WithWritePty(func(_ *libghostty.Terminal, data []byte) {
			replies = append(replies, append([]byte(nil), data...))
		}),
		libghostty.WithClipboardWrite(func(_ *libghostty.Terminal, w libghostty.ClipboardWrite) libghostty.ClipboardWriteReply {
			for _, c := range w.Contents {
				fmt.Printf("----- clipboard write (%s): %q\n", c.MIME, c.Data)
			}
			return libghostty.ClipboardWriteReply{Result: libghostty.ClipboardWriteSuccess}
		}),
		libghostty.WithSizeReport(func(*libghostty.Terminal) (libghostty.SizeReportSize, bool) {
			return libghostty.SizeReportSize{Rows: rows, Columns: cols, CellWidth: 8, CellHeight: 16}, true
		}),
	)
	if err != nil {
		return err
	}
	defer term.Close()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: rows, Cols: cols, X: cols * 8, Y: rows * 16})
	if err != nil {
		return err
	}
	defer ptmx.Close()
	chunks := make(chan []byte, 64)
	go func() {
		defer close(chunks)
		buf := make([]byte, 32*1024)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				chunks <- append([]byte(nil), buf[:n]...)
			}
			if err != nil {
				return
			}
		}
	}()
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()

	consume := func(chunk []byte) {
		term.VTWrite(chunk)
		for _, r := range replies {
			ptmx.Write(r)
		}
		replies = nil
	}
	screen := func() string {
		f, err := libghostty.NewFormatter(term, libghostty.WithFormatterFormat(libghostty.FormatterFormatPlain))
		if err != nil {
			return ""
		}
		defer f.Close()
		s, _ := f.FormatString()
		return s
	}
	pump := func(deadline time.Time, cond func() bool) bool {
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for !cond() {
			if time.Now().After(deadline) {
				return false
			}
			select {
			case c, ok := <-chunks:
				if !ok {
					chunks = nil
					continue
				}
				consume(c)
			case <-tick.C:
			}
		}
		return true
	}
	for _, s := range st {
		if s.want != "" {
			if !pump(time.Now().Add(40*time.Second), func() bool { return strings.Contains(screen(), s.want) }) {
				return fmt.Errorf("timed out waiting for %q; screen:\n%s", s.want, screen())
			}
		}
		if s.only != "" && !strings.Contains(screen(), s.only) {
			continue
		}
		if s.send != "" {
			for _, k := range splitKeys(s.send) {
				if _, err := ptmx.Write([]byte(k)); err != nil {
					return fmt.Errorf("%w; screen:\n%s", err, screen())
				}
				pump(time.Now().Add(8*time.Millisecond), func() bool { return false })
			}
		}
		pump(time.Now().Add(s.pause), func() bool { return false })
		if s.dump != "" {
			if err := dump(term, s.dump); err != nil {
				return err
			}
		}
	}
	if !pump(time.Now().Add(10*time.Second), func() bool {
		select {
		case <-exited:
			return true
		default:
			return false
		}
	}) {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-exited
		fmt.Println("(killed; did not exit on its own)")
	}
	return nil
}

type cell struct {
	text  string
	style string
}

var last [][]cell

// grid reads the screen's cells with a style signature each.
func grid(term *libghostty.Terminal) ([][]cell, error) {
	rs, err := libghostty.NewRenderState()
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	if err := rs.Update(term); err != nil {
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
	var g [][]cell
	for ri.Next() {
		if err := ri.Cells(rc); err != nil {
			return nil, err
		}
		var row []cell
		var st libghostty.RenderCellStyle
		for rc.Next() {
			cps, _ := rc.Graphemes()
			t := " "
			if len(cps) > 0 {
				var b strings.Builder
				for _, cp := range cps {
					b.WriteRune(rune(cp))
				}
				t = b.String()
			}
			if err := rc.StyleInto(&st); err != nil {
				return nil, err
			}
			s := ""
			if st.Inverse {
				s += "R"
			}
			if st.Bold {
				s += "B"
			}
			if st.Faint {
				s += "F"
			}
			if st.Underline {
				s += "U"
			}
			if st.Italic {
				s += "I"
			}
			if st.HasForeground {
				s += fmt.Sprintf(" fg#%02x%02x%02x", st.Foreground.R, st.Foreground.G, st.Foreground.B)
			}
			if st.HasBackground {
				s += fmt.Sprintf(" bg#%02x%02x%02x", st.Background.R, st.Background.G, st.Background.B)
			}
			row = append(row, cell{t, s})
		}
		g = append(g, row)
	}
	return g, nil
}

// dump prints the rows whose cells changed since the last dump: the text,
// then a line marking changed cells (* text and style, s style only, t
// text only), and the styles changed cells went from and to.
func dump(term *libghostty.Terminal, label string) error {
	g, err := grid(term)
	if err != nil {
		return err
	}
	screen, _ := term.ActiveScreen()
	var modes []string
	for _, m := range []struct {
		name string
		mode libghostty.Mode
	}{{"1000", libghostty.ModeNormalMouse}, {"1002", libghostty.ModeButtonMouse}, {"1003", libghostty.ModeAnyMouse}, {"1006", libghostty.ModeSGRMouse}} {
		if on, _ := term.Mode(m.mode); on {
			modes = append(modes, m.name)
		}
	}
	fmt.Printf("===== %s: screen=%v mouse=%v\n", label, screen, modes)
	for y, row := range g {
		var prev []cell
		if y < len(last) {
			prev = last[y]
		}
		var text, marks strings.Builder
		changes := map[string]int{}
		changed := false
		for x, c := range row {
			text.WriteString(c.text)
			var p cell
			if x < len(prev) {
				p = prev[x]
			}
			m := byte(' ')
			switch {
			case p.text != c.text && p.style != c.style:
				m = '*'
			case p.style != c.style:
				m = 's'
			case p.text != c.text:
				m = 't'
			}
			if m != ' ' {
				changed = true
				if p.style != c.style {
					changes[fmt.Sprintf("[%s] -> [%s]", p.style, c.style)]++
				}
			}
			marks.WriteByte(m)
		}
		if !changed {
			continue
		}
		fmt.Printf("%2d|%s\n", y+1, strings.TrimRight(text.String(), " "))
		fmt.Printf("  |%s\n", strings.TrimRight(marks.String(), " "))
		for k, n := range changes {
			fmt.Printf("  |   %d cells %s\n", n, k)
		}
	}
	last = g
	return nil
}

func splitKeys(s string) []string {
	var keys []string
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+2 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
				j++
			}
			keys = append(keys, s[i:j+1])
			i = j + 1
			continue
		}
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\r' {
			keys = append(keys, s[i:i+2])
			i += 2
			continue
		}
		keys = append(keys, s[i:i+1])
		i++
	}
	return keys
}
