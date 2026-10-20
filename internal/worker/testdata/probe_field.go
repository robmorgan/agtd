//go:build ignore

// probe_field shows what a coding agent's input box looks like in the shadow
// terminal: the cursor, the bottom rows with their cell attributes, then the
// same after typing, after an Alt-Enter newline, after a bracketed paste, and
// after clearing. Nothing is submitted to a model.
//
//	PKG_CONFIG_PATH=.build/ghostty-out/share/pkgconfig go run internal/worker/testdata/probe_field.go -only claude -dir /tmp/x
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

var clear = strings.Repeat("\x7f", 220)

func boundary(n int) []step {
	return []step{
		{send: strings.Repeat("m", n) + " zz", pause: 1200 * time.Millisecond, dump: fmt.Sprintf("boundary-%d", n)},
		{send: clear, pause: 1200 * time.Millisecond},
	}
}

func steps(name string) (argv []string, st []step) {
	common := []step{
		{send: "one\x1b[13;2utwo", pause: 1200 * time.Millisecond, dump: "shift-enter"},
		{send: "\x7f\x7f\x7f", pause: 1200 * time.Millisecond, dump: "deleted-line-two"},
		{send: "\x7f", pause: 1200 * time.Millisecond, dump: "joined"},
		{send: "\x1b[13;2u\x1b[13;2uz", pause: 1200 * time.Millisecond, dump: "two-blank-lines"},
		{send: clear, pause: 1200 * time.Millisecond},
		{send: "a\x1b[106;5ub", pause: 1200 * time.Millisecond, dump: "ctrl-j"},
		{send: clear, pause: 1200 * time.Millisecond},
		{send: "a\x1b[13;5ub", pause: 1200 * time.Millisecond, dump: "ctrl-enter"},
		{send: clear, pause: 1200 * time.Millisecond, dump: "cleared"},
	}
	switch name {
	case "claude":
		return []string{"claude"}, append([]step{
			{want: "❯", pause: 2 * time.Second},
			{only: "Keep xhigh", send: "\r", pause: 3 * time.Second},
			{only: "trust this folder", send: "\x1b[B", pause: 500 * time.Millisecond},
			{only: "trust this folder", send: "\r", pause: 4 * time.Second},
			{want: "Try \"", pause: 3 * time.Second, dump: "idle"},
		}, append(common, step{send: "\x03", pause: 300 * time.Millisecond}, step{send: "\x03", pause: 2 * time.Second})...)
	case "codex":
		return []string{"codex", "-c", "check_for_update_on_startup=false"}, append([]step{
			{want: "›", pause: 2 * time.Second},
			{only: "Trust this folder", send: "\r", pause: 4 * time.Second},
			{want: "›", pause: 3 * time.Second, dump: "idle"},
		}, append(common, step{send: "\x03", pause: 500 * time.Millisecond}, step{send: "\x03", pause: 2 * time.Second})...)
	case "pi":
		return []string{"pi"}, append([]step{
			{pause: 6 * time.Second, dump: "idle"},
		}, append(common, step{send: "\x03", pause: 500 * time.Millisecond}, step{send: "\x03", pause: 500 * time.Millisecond}, step{send: "\x04", pause: 2 * time.Second})...)
	}
	return nil, nil
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
	dir := flag.String("dir", "", "working directory (a git repository)")
	flag.Parse()
	argv, st := steps(*only)
	if argv == nil || *dir == "" {
		fmt.Fprintln(os.Stderr, "usage: -only claude|codex|pi -dir DIR")
		os.Exit(2)
	}
	if err := run(argv, st, *dir); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(argv []string, st []step, dir string) error {
	var replies [][]byte
	term, err := libghostty.NewTerminal(
		libghostty.WithSize(cols, rows),
		libghostty.WithWritePty(func(_ *libghostty.Terminal, data []byte) {
			replies = append(replies, append([]byte(nil), data...))
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
	cmd.Env = userEnv()
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

// dump prints the cursor and the bottom rows: text, then one attribute
// character per cell (R inverse, B bold, F faint, U underline, I italic,
// b background colour, f foreground colour, . plain).
func dump(term *libghostty.Terminal, label string) error {
	rs, err := libghostty.NewRenderState()
	if err != nil {
		return err
	}
	defer rs.Close()
	if err := rs.Update(term); err != nil {
		return err
	}
	cur, err := rs.Cursor()
	if err != nil {
		return err
	}
	screen, _ := term.ActiveScreen()
	fmt.Printf("===== %s: screen=%v cursor visible=%v shape=%v at (x=%d,y=%d) inViewport=%v\n", label, screen, cur.Visible, cur.VisualStyle, cur.ViewportX, cur.ViewportY, cur.ViewportHasValue)
	ri, err := libghostty.NewRenderStateRowIterator()
	if err != nil {
		return err
	}
	defer ri.Close()
	if err := rs.RowIterator(ri); err != nil {
		return err
	}
	rc, err := libghostty.NewRenderStateRowCells()
	if err != nil {
		return err
	}
	defer rc.Close()
	y := -1
	for ri.Next() {
		y++
		if err := ri.Cells(rc); err != nil {
			return err
		}
		var text, attrs strings.Builder
		var colors []string
		var st libghostty.RenderCellStyle
		x := -1
		for rc.Next() {
			x++
			g, _ := rc.Graphemes()
			if len(g) == 0 {
				text.WriteByte(' ')
			} else {
				for _, cp := range g {
					text.WriteRune(rune(cp))
				}
			}
			if err := rc.StyleInto(&st); err != nil {
				return err
			}
			c := byte('.')
			switch {
			case st.Inverse:
				c = 'R'
			case st.Bold:
				c = 'B'
			case st.Faint:
				c = 'F'
			case st.Underline:
				c = 'U'
			case st.Italic:
				c = 'I'
			case st.HasBackground:
				c = 'b'
			case st.HasForeground:
				c = 'f'
			}
			attrs.WriteByte(c)
			if (st.HasForeground || st.HasBackground) && len(colors) < 6 {
				tag := fmt.Sprintf("x%d:", x)
				if st.HasForeground {
					tag += fmt.Sprintf("fg#%02x%02x%02x", st.Foreground.R, st.Foreground.G, st.Foreground.B)
				}
				if st.HasBackground {
					tag += fmt.Sprintf("bg#%02x%02x%02x", st.Background.R, st.Background.G, st.Background.B)
				}
				if len(colors) == 0 || !strings.HasSuffix(colors[len(colors)-1], tag[strings.Index(tag, ":")+1:]) {
					colors = append(colors, tag)
				}
			}
		}
		t := strings.TrimRight(text.String(), " ")
		a := strings.TrimRight(attrs.String(), ".")
		if t == "" && a == "" {
			continue
		}
		fmt.Printf("%2d|%s\n", y, t)
		if a != "" {
			fmt.Printf("  |%s   %s\n", a, strings.Join(colors, " "))
		}
	}
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
