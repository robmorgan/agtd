//go:build ignore

// record captures representative PTY output streams for the worker's terminal
// fidelity tests. See streams/README.md for what each stream exercises and how
// to re-record one:
//
//	go run internal/worker/testdata/record.go [-only NAME] [-scrub STRING]...
//
// Each program runs under a real PTY at a fixed size in a scratch directory.
// The output is fed through libghostty-vt so terminal queries (cursor
// position, device attributes, colors) get answers, the way the worker
// answers them, and so the script can wait for text to appear on screen.
// The raw bytes are saved gzip-compressed in streams/NAME.vt.gz after
// replacing identifying strings with same-length placeholders.
package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
	"go.mitchellh.com/libghostty"
)

const (
	cols = 120
	rows = 40
	// workDir is fixed rather than random so re-recording gives the same
	// paths in the output.
	workDir = "/tmp/agtd-demo"
)

// step is one scripted action: wait until want is on screen (if set), then
// send keys (if set), then pause.
type step struct {
	want string
	// only, if set, skips the step unless the screen shows this text (for
	// prompts that appear only on a first run, such as folder trust).
	only  string
	send  string
	pause time.Duration
	// mark, if set, records where in the output this step ended, under
	// this name, in NAME.marks.json beside the stream: a test replays the
	// stream up to a mark to see the screen as it was then.
	mark string
	// within, if set, is how long to wait for want (30s otherwise).
	within time.Duration
}

type scenario struct {
	name  string
	argv  []string
	env   []string
	setup func(dir string) error
	steps []step
}

func main() {
	only := flag.String("only", "", "record only this stream")
	flag.BoolVar(&verbose, "v", false, "print the screen after each step")
	out := flag.String("out", "internal/worker/testdata/streams", "output directory")
	var extra multiFlag
	flag.Var(&extra, "scrub", "an extra string to replace in the output (repeatable)")
	flag.Parse()

	secrets := identifyingStrings(extra)
	for _, sc := range scenarios() {
		if *only != "" && sc.name != *only {
			continue
		}
		data, marks, err := record(sc)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", sc.name, err)
			os.Exit(1)
		}
		// Scrubbing keeps every offset, so the marks stay right.
		data = scrub(data, secrets)
		for _, s := range secrets {
			if bytes.Contains(asciiLower(data), asciiLower([]byte(s))) {
				fmt.Fprintf(os.Stderr, "%s: output still contains %q\n", sc.name, s)
				os.Exit(1)
			}
		}
		path := filepath.Join(*out, sc.name+".vt.gz")
		if err := writeGzip(path, data); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", sc.name, err)
			os.Exit(1)
		}
		if len(marks) > 0 {
			js, _ := json.MarshalIndent(marks, "", "  ")
			if err := os.WriteFile(filepath.Join(*out, sc.name+".marks.json"), append(js, '\n'), 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", sc.name, err)
				os.Exit(1)
			}
		}
		fmt.Printf("%s: %d bytes, %d marks\n", path, len(data), len(marks))
	}
}

var verbose bool

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(s string) error { *m = append(*m, s); return nil }

func scenarios() []scenario {
	sh := []string{"TERM=xterm-256color", "COLORTERM=truecolor", "LANG=en_US.UTF-8", "PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"}
	return []scenario{
		{
			name: "shell",
			argv: []string{"/bin/bash", "--norc", "--noprofile", "-i"},
			env: append(sh, "HOME="+workDir, "BASH_SILENCE_DEPRECATION_WARNING=1", "CLICOLOR=1", "CLICOLOR_FORCE=1",
				`PS1=\[\e[1;32m\]demo\[\e[0m\]:\[\e[1;34m\]\w\[\e[0m\]\$ `),
			setup: setupRepo,
			steps: []step{
				{want: "demo:", send: "ls -G\r"},
				{want: "README", send: "git log --graph --color --oneline --decorate --all\r"},
				{want: "initial", send: "printf '\\e[1mbold\\e[0m \\e[3mitalic\\e[0m \\e[4munderline\\e[0m \\e[38;5;208m256\\e[0m \\e[38;2;10;200;120mtruecolor\\e[0m\\n'\r"},
				// Prompt redraws: edit a line in place, recall history, clear.
				{want: "truecolor", send: "echo this line gets edited", pause: 200 * time.Millisecond},
				{send: "\x15echo replaced\x1b[D\x1b[D\x7f\x7fXY\r", pause: 300 * time.Millisecond},
				{send: "\x1b[A", pause: 200 * time.Millisecond},
				{send: "\x1b[A\x1b[B\x03", pause: 200 * time.Millisecond},
				{send: "for i in $(seq 1 60); do echo \"row $i $(printf '%*s' $i '' | tr ' ' '=')\"; done\r", pause: 500 * time.Millisecond},
				{send: "echo " + strings.Repeat("wrapping-", 30) + "\r", pause: 300 * time.Millisecond},
				{send: "\x0c", pause: 300 * time.Millisecond},
				{send: "exit\r"},
			},
		},
		{
			name:  "vim",
			argv:  []string{"vim", "--clean", "-N", "main.go"},
			env:   append(sh, "HOME="+workDir),
			setup: setupSource,
			steps: editorSteps,
		},
		{
			name:  "nvim",
			argv:  []string{"nvim", "--clean", "main.go"},
			env:   append(sh, "HOME="+workDir, "XDG_CONFIG_HOME="+workDir+"/.config", "XDG_STATE_HOME="+workDir+"/.state", "XDG_DATA_HOME="+workDir+"/.data"),
			setup: setupSource,
			steps: editorSteps,
		},
		{
			name:  "go-test",
			argv:  []string{"go", "test", "-v", "-count=1", "./..."},
			env:   userEnv(),
			setup: setupGoTests,
			steps: []step{{pause: 0}},
		},
		{
			name:  "clang",
			argv:  []string{"clang", "-fsyntax-only", "-fcolor-diagnostics", "-ferror-limit=0", "-Wall", "-Wextra", "broken.c"},
			env:   sh,
			setup: setupBrokenC,
			steps: []step{{pause: 0}},
		},
		{
			// Claude Code with the user's own login: startup, the /help
			// screen, and quitting. Nothing is sent to the model.
			name:  "claude",
			argv:  []string{"claude"},
			env:   userEnv(),
			setup: setupEmpty,
			steps: []step{
				{want: "❯", pause: 1500 * time.Millisecond},
				// Accept the folder trust prompt, if shown.
				{only: "trust this folder", send: "\x1b[B", pause: 500 * time.Millisecond},
				{only: "trust this folder", send: "\r", pause: 3 * time.Second},
				{send: "/help", pause: time.Second},
				{send: "\r", pause: 2 * time.Second},
				{send: "\x1b", pause: time.Second},
				// Quit with Ctrl-C twice rather than /exit, whose completion
				// list would show the recording user's own commands.
				{send: "\x03", pause: 300 * time.Millisecond},
				{send: "\x03", pause: 2 * time.Second},
			},
		},
		{
			// Claude Code's input field (field.go): idle with the
			// placeholder, wrapped text, an Alt-Enter newline, a
			// bracketed paste, a word longer than a row. Nothing is sent
			// to the model. The first-run dialogs are answered if shown.
			name:  "claude-field",
			argv:  []string{"claude"},
			env:   userEnv(),
			setup: setupEmpty,
			steps: append([]step{
				{want: "❯", pause: 2 * time.Second},
				{only: "Keep xhigh", send: "\r", pause: 3 * time.Second},
				{only: "trust this folder", send: "\x1b[B", pause: 500 * time.Millisecond},
				{only: "trust this folder", send: "\r", pause: 4 * time.Second},
				{want: "Try \"", pause: 2 * time.Second, mark: "idle"},
			}, append(fieldSteps, step{send: "\x03", pause: 300 * time.Millisecond}, step{send: "\x03", pause: 2 * time.Second})...),
		},
		{
			// Claude Code's transcript scrolling on the alternate screen
			// (docs/designs/alt-screen-scrollback.md): rows of known,
			// unique text from bash mode until the viewport has
			// overflowed several times, the wheel up and down, the box
			// growing and shrinking, a click on the transcript, /clear.
			// Nothing is sent to the model.
			name:  "claude-scroll",
			argv:  []string{"claude"},
			env:   userEnv(),
			setup: setupEmpty,
			steps: append(append(append([]step{}, scrollStart...), scrollShellSteps...),
				step{send: wheel(64, 10), pause: 2 * time.Second, mark: "wheel-up"},
				step{send: wheel(65, 10), pause: 2 * time.Second, mark: "wheel-down"},
				step{send: wheel(64, 30), pause: 2 * time.Second, mark: "wheel-far-up"},
				step{send: "\x1b[<0;20;12M\x1b[<0;20;12m", pause: 2 * time.Second, mark: "clicked"},
				step{send: wheel(65, 40), pause: 2 * time.Second, mark: "wheel-far-down"},
				step{send: "first typed line\x1b\rsecond typed line\x1b\rthird typed line", pause: 1500 * time.Millisecond, mark: "typed"},
				step{send: strings.Repeat("\x7f", 60), pause: 1500 * time.Millisecond, mark: "untyped"},
				step{send: "/clear", pause: time.Second},
				step{send: "\r", pause: 3 * time.Second, mark: "cleared"},
				step{send: "\x03", pause: 300 * time.Millisecond},
				step{send: "\x03", pause: 2 * time.Second},
			),
		},
		{
			// The same transcript, then one prompt to the model whose
			// answer streams 200 known rows, with the wheel used while it
			// streams. It uses the recording user's account: record it
			// only with their consent.
			name:  "claude-scroll-stream",
			argv:  []string{"claude"},
			env:   userEnv(),
			setup: setupEmpty,
			steps: append(append(append([]step{}, scrollStart...), scrollShellSteps[:8]...),
				step{send: scrollPrompt, pause: 300 * time.Millisecond},
				step{send: "\r"},
				step{want: "model row 060", within: 3 * time.Minute, send: wheel(64, 6), pause: 6 * time.Second, mark: "scrolled-streaming"},
				step{send: wheel(65, 12), pause: time.Second},
				step{want: "model row 200", within: 3 * time.Minute, pause: 8 * time.Second, mark: "answered"},
				step{send: wheel(64, 8), pause: 2 * time.Second, mark: "answer-wheel-up"},
				step{send: wheel(65, 16), pause: 2 * time.Second, mark: "answer-wheel-down"},
				step{send: "\x03", pause: 300 * time.Millisecond},
				step{send: "\x03", pause: 2 * time.Second},
			),
		},
		{
			// Claude Code's own mouse selection on the alternate screen:
			// the transcript filled from bash mode, then the left button
			// pressed, dragged and released (SGR reports), twice, and a
			// drag held at the top edge. Nothing is sent to the model.
			name:  "claude-select",
			argv:  []string{"claude"},
			env:   userEnv(),
			setup: setupEmpty,
			steps: append(append(append(append([]step{}, scrollStart...), scrollShellSteps[:8]...), selectSteps...),
				step{send: "\x03", pause: 300 * time.Millisecond},
				step{send: "\x03", pause: 2 * time.Second},
			),
		},
		{
			// pi's own mouse selection in its fullscreen mode, the same
			// way, offline with an empty config directory as
			// pi-fullscreen-field.
			name: "pi-fullscreen-select",
			argv: []string{"pi", "--tui-mode", "fullscreen", "--offline", "--no-session",
				"--no-extensions", "--no-skills", "--no-prompt-templates", "--no-context-files"},
			env:   append(userEnv(), "PI_CODING_AGENT_DIR="+filepath.Join(workDir, ".pi-agent")),
			setup: setupEmpty,
			steps: append(append(append([]step{{pause: 6 * time.Second, mark: "idle"}}, scrollShellSteps[:8]...), selectSteps...),
				step{send: "\x03", pause: 500 * time.Millisecond},
				step{send: "\x03", pause: 500 * time.Millisecond},
				step{send: "\x04", pause: 2 * time.Second},
			),
		},
		{
			// Codex's input field, the same way.
			name:  "codex-field",
			argv:  []string{"codex", "-c", "check_for_update_on_startup=false"},
			env:   userEnv(),
			setup: setupEmpty,
			steps: append([]step{
				{want: "›", pause: 2 * time.Second},
				{only: "Trust this folder", send: "\r", pause: 4 * time.Second},
				{want: "›", pause: 3 * time.Second, mark: "idle"},
			}, append(fieldSteps, step{send: "\x03", pause: 500 * time.Millisecond}, step{send: "\x03", pause: 2 * time.Second})...),
		},
		{
			// pi's input field, the same way (pi draws on the primary
			// screen and paints its own cursor).
			name:  "pi-field",
			argv:  []string{"pi"},
			env:   userEnv(),
			setup: setupEmpty,
			steps: append([]step{
				{pause: 6 * time.Second, mark: "idle"},
			}, append(fieldSteps, step{send: "\x03", pause: 500 * time.Millisecond}, step{send: "\x03", pause: 500 * time.Millisecond}, step{send: "\x04", pause: 2 * time.Second})...),
		},
		{
			// pi's input field in its fullscreen mode (the alternate
			// screen), the same way. pi gets an empty config directory
			// and no network, extensions, skills or context files, so
			// its startup screen shows none of the recording user's
			// setup, logins or a changelog.
			name: "pi-fullscreen-field",
			argv: []string{"pi", "--tui-mode", "fullscreen", "--offline", "--no-session",
				"--no-extensions", "--no-skills", "--no-prompt-templates", "--no-context-files"},
			env:   append(userEnv(), "PI_CODING_AGENT_DIR="+filepath.Join(workDir, ".pi-agent")),
			setup: setupEmpty,
			steps: append([]step{
				{pause: 6 * time.Second, mark: "idle"},
			}, append(fieldSteps, step{send: "\x03", pause: 500 * time.Millisecond}, step{send: "\x03", pause: 500 * time.Millisecond}, step{send: "\x04", pause: 2 * time.Second})...),
		},
		{
			// Codex with the user's own login: startup, the slash command
			// popup, the local /model picker, and quitting.
			name:  "codex",
			argv:  []string{"codex", "-c", "check_for_update_on_startup=false"},
			env:   userEnv(),
			setup: setupEmpty,
			steps: []step{
				{want: "›", pause: 2 * time.Second},
				{send: "/", pause: time.Second},
				{send: "mod", pause: time.Second},
				{send: "\r", pause: 2 * time.Second},
				{send: "\x1b", pause: time.Second},
				{send: "\x03", pause: 2 * time.Second},
			},
		},
	}
}

// userEnv is a minimal environment around the recording user's real home
// directory, so Claude Code and Codex find their logins and settings but
// nothing else from the recording shell leaks in.
func userEnv() []string {
	env := []string{"TERM=xterm-256color", "COLORTERM=truecolor", "LANG=en_US.UTF-8", "DISABLE_AUTOUPDATER=1"}
	for _, key := range []string{"HOME", "USER", "LOGNAME", "PATH", "SHELL", "TMPDIR"} {
		if v, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+v)
		}
	}
	return env
}

var editorSteps = []step{
	{want: "package main", pause: 300 * time.Millisecond},
	{send: "G", pause: 300 * time.Millisecond},
	{send: "gg", pause: 300 * time.Millisecond},
	{send: "/func\r", pause: 300 * time.Millisecond},
	{send: "ototal := 0 // edited\x1b", pause: 300 * time.Millisecond},
	{send: "\x04\x04", pause: 300 * time.Millisecond}, // scroll down half a page twice
	{send: "\x15", pause: 300 * time.Millisecond},     // and back up
	{send: ":split\r", pause: 300 * time.Millisecond},
	{send: ":vsplit\r", pause: 300 * time.Millisecond},
	{send: "\x17w50G", pause: 300 * time.Millisecond},
	{send: ":qa!\r"},
}

// fieldSteps type into an agent's input field; fieldText is what the
// field fixture tests expect to find.
var (
	fieldText  = strings.Repeat("the quick brown fox jumps over the lazy dog ", 4)
	fieldClear = strings.Repeat("\x7f", 220)
	fieldSteps = []step{
		{send: fieldText, pause: 1500 * time.Millisecond, mark: "typed"},
		{send: "\x1b\rsecond line", pause: 1200 * time.Millisecond, mark: "newline"},
		{send: fieldClear, pause: 1200 * time.Millisecond, mark: "cleared"},
		{send: "\x1b[200~line one\nline two\nline three\x1b[201~", pause: 1200 * time.Millisecond, mark: "pasted"},
		{send: fieldClear, pause: 1200 * time.Millisecond},
		{send: strings.Repeat("k", 130), pause: 1200 * time.Millisecond, mark: "longword"},
		{send: fieldClear, pause: 1200 * time.Millisecond},
	}
)

// scrollStart starts Claude Code and answers its first-run dialogs.
var scrollStart = []step{
	{want: "❯", pause: 2 * time.Second},
	{only: "Keep xhigh", send: "\r", pause: 3 * time.Second},
	{only: "trust this folder", send: "\x1b[B", pause: 500 * time.Millisecond},
	{only: "trust this folder", send: "\r", pause: 4 * time.Second},
	{want: "Try \"", pause: time.Second, mark: "idle"},
}

// scrollShellSteps fill Claude Code's transcript from bash mode with rows
// that are unique, ordered and known ("shell row 001" to "shell row 120"),
// a batch of 30 at a time, marked after each.
var scrollShellSteps = func() []step {
	var steps []step
	for b := range 4 {
		first, last := b*30+1, b*30+30
		steps = append(steps,
			step{send: "!", pause: 500 * time.Millisecond},
			step{send: fmt.Sprintf("seq -f 'shell row %%03g' %d %d", first, last), pause: 300 * time.Millisecond},
			step{send: "\r"},
			step{want: fmt.Sprintf("shell row %03d", last), pause: 2 * time.Second, mark: fmt.Sprintf("shell-%d", b+1)})
	}
	return steps
}()

// Mouse reports for the left button at a 1-based column and row (SGR):
// a press, motion with the button held, a release.
func mousePress(x, y int) string   { return fmt.Sprintf("\x1b[<0;%d;%dM", x, y) }
func mouseDrag(x, y int) string    { return fmt.Sprintf("\x1b[<32;%d;%dM", x, y) }
func mouseRelease(x, y int) string { return fmt.Sprintf("\x1b[<0;%d;%dm", x, y) }

// selectSteps select over the transcript twice, each gesture marked after
// every report: within a row, down across rows and back left, then
// (select-*) down, back up short of where it was, and above the press;
// a click away clears each. Then a drag held at the top edge, where the
// agent scrolls.
var selectSteps = func() []step {
	p := 400 * time.Millisecond
	click := mousePress(30, 20) + mouseRelease(30, 20)
	return []step{
		{send: mousePress(10, 12), pause: p, mark: "learn-pressed"},
		{send: mouseDrag(14, 12), pause: p, mark: "learn-drag-1"},
		{send: mouseDrag(25, 12), pause: p, mark: "learn-drag-2"},
		{send: mouseDrag(25, 14), pause: p, mark: "learn-drag-3"},
		{send: mouseDrag(5, 16), pause: p, mark: "learn-drag-4"},
		{send: mouseRelease(5, 16), pause: 1500 * time.Millisecond, mark: "learn-released"},
		{send: click, pause: time.Second, mark: "learn-cleared"},
		{send: mousePress(12, 13), pause: p, mark: "select-pressed"},
		{send: mouseDrag(30, 13), pause: p, mark: "select-drag-1"},
		{send: mouseDrag(8, 17), pause: p, mark: "select-drag-2"},
		{send: mouseDrag(40, 15), pause: p, mark: "select-drag-3"},
		{send: mouseDrag(20, 11), pause: p, mark: "select-drag-4"},
		{send: mouseRelease(20, 11), pause: 1500 * time.Millisecond, mark: "select-released"},
		{send: click, pause: time.Second, mark: "select-cleared"},
		{send: mousePress(10, 12), pause: p},
		{send: mouseDrag(10, 1), pause: 1500 * time.Millisecond, mark: "edge-held"},
		{send: mouseRelease(10, 1), pause: time.Second, mark: "edge-released"},
		{send: click, pause: time.Second, mark: "edge-cleared"},
	}
}()

// scrollPrompt asks the model for 200 rows of known text.
const scrollPrompt = "Reply with exactly 200 lines and nothing else, no code block: line k is the words model row followed by k as three digits, so the first line is model row 001 and the last is model row 200."

// wheel is n SGR mouse wheel reports (button 64 up, 65 down) at row 12,
// column 20, over the transcript.
func wheel(button, n int) string {
	return strings.Repeat(fmt.Sprintf("\x1b[<%d;20;12M", button), n)
}

func setupEmpty(string) error { return nil }

func setupRepo(dir string) error {
	for name, body := range map[string]string{
		"README.md": "# demo\n", "main.go": "package main\n", "notes.txt": "notes\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			return err
		}
	}
	for _, d := range []string{"cmd", "internal", "scripts"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "scripts", "build.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		return err
	}
	git := func(args ...string) error {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = []string{"HOME=" + dir, "PATH=/opt/homebrew/bin:/usr/bin:/bin",
			"GIT_AUTHOR_NAME=Demo", "GIT_AUTHOR_EMAIL=demo@example.com",
			"GIT_COMMITTER_NAME=Demo", "GIT_COMMITTER_EMAIL=demo@example.com",
			"GIT_AUTHOR_DATE=2026-01-02T03:04:05Z", "GIT_COMMITTER_DATE=2026-01-02T03:04:05Z"}
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("git %v: %v: %s", args, err, out)
		}
		return nil
	}
	steps := [][]string{
		{"init", "-q", "-b", "main"}, {"add", "."}, {"commit", "-qm", "initial import"},
		{"checkout", "-qb", "feature"}, {"commit", "-q", "--allow-empty", "-m", "add the feature"},
		{"commit", "-q", "--allow-empty", "-m", "fix review comments"},
		{"checkout", "-q", "main"}, {"commit", "-q", "--allow-empty", "-m", "unrelated fix on main"},
		{"merge", "-q", "--no-ff", "-m", "merge feature", "feature"},
		{"commit", "-q", "--allow-empty", "-m", "release 0.1.0"}, {"tag", "v0.1.0"},
	}
	for _, s := range steps {
		if err := git(s...); err != nil {
			return err
		}
	}
	return nil
}

func setupSource(dir string) error {
	var b strings.Builder
	b.WriteString("package main\n\nimport \"fmt\"\n\n")
	for i := range 40 {
		fmt.Fprintf(&b, "// step%d prints its number and returns it doubled.\nfunc step%d(n int) int {\n\tfmt.Println(\"step\", %d, n)\n\treturn n * 2\n}\n\n", i, i, i)
	}
	b.WriteString("func main() {\n\tn := 1\n")
	for i := range 40 {
		fmt.Fprintf(&b, "\tn = step%d(n)\n", i)
	}
	b.WriteString("}\n")
	return os.WriteFile(filepath.Join(dir, "main.go"), []byte(b.String()), 0o644)
}

func setupGoTests(dir string) error {
	files := map[string]string{
		"go.mod": "module example.com/demo\n\ngo 1.22\n",
		"calc/calc.go": `package calc

func Add(a, b int) int { return a + b }
func Div(a, b int) int { return a / b }
`,
		"calc/calc_test.go": `package calc

import (
	"testing"
	"time"
)

func TestAdd(t *testing.T) {
	for _, c := range []struct{ a, b, want int }{{1, 2, 3}, {2, 2, 4}, {-1, 1, 0}} {
		t.Run("", func(t *testing.T) {
			if got := Add(c.a, c.b); got != c.want {
				t.Fatalf("Add(%d, %d) = %d, want %d", c.a, c.b, got, c.want)
			}
		})
	}
}

func TestAddIsWrong(t *testing.T) {
	t.Log("checking a deliberately wrong expectation")
	if got := Add(2, 2); got != 5 {
		t.Errorf("Add(2, 2) = %d, want 5", got)
	}
}

func TestSlow(t *testing.T) {
	time.Sleep(150 * time.Millisecond)
}

func TestSkipped(t *testing.T) { t.Skip("not on this platform") }

func TestDivPanics(t *testing.T) {
	Div(1, 0)
}
`,
		"text/text_test.go": `package text

import (
	"strings"
	"testing"
)

func TestUpper(t *testing.T) {
	for _, w := range []string{"alpha", "beta", "gamma", "delta"} {
		t.Run(w, func(t *testing.T) {
			if strings.ToUpper(w) == w {
				t.Fatal("unchanged")
			}
		})
	}
}
`,
	}
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// setupBrokenC writes a C file with a few thousand diagnostics of varied
// kinds, so clang prints several hundred KB of colored output.
func setupBrokenC(dir string) error {
	var b strings.Builder
	b.WriteString("#include <stdio.h>\n\n")
	for i := range 250 {
		fmt.Fprintf(&b, "static int unused_%d(int a, int b) {\n", i)
		fmt.Fprintf(&b, "    int x%d = \"not an int\";\n", i)
		fmt.Fprintf(&b, "    undeclared_%d(a);\n", i)
		fmt.Fprintf(&b, "    if (a = b) printf(\"%%s\\n\", a);\n")
		fmt.Fprintf(&b, "    struct missing_%d s;\n", i)
		b.WriteString("}\n\n")
	}
	return os.WriteFile(filepath.Join(dir, "broken.c"), []byte(b.String()), 0o644)
}

// record runs one scenario and returns everything it wrote to the PTY, and
// the marked offsets in it.
func record(sc scenario) ([]byte, map[string]int, error) {
	if err := os.RemoveAll(workDir); err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(workDir)
	if err := sc.setup(workDir); err != nil {
		return nil, nil, fmt.Errorf("setup: %w", err)
	}
	marks := map[string]int{}

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
		return nil, nil, err
	}
	defer term.Close()

	cmd := exec.Command(sc.argv[0], sc.argv[1:]...)
	cmd.Dir = workDir
	cmd.Env = sc.env
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: rows, Cols: cols, X: cols * 8, Y: rows * 16})
	if err != nil {
		return nil, nil, err
	}
	defer ptmx.Close()

	// The reader goroutine owns nothing but its buffer; the terminal is only
	// touched on this goroutine.
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
	go func() {
		cmd.Wait()
		close(exited)
	}()

	var out bytes.Buffer
	consume := func(chunk []byte) {
		out.Write(chunk)
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
	// pump consumes output until cond holds or the deadline passes.
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
					// EOF: keep waiting on the ticker for the
					// program to be reaped.
					chunks = nil
					continue
				}
				consume(c)
			case <-tick.C:
			}
		}
		return true
	}

	for _, s := range sc.steps {
		if s.want != "" {
			wants := strings.Split(s.want, "|")
			within := 30 * time.Second
			if s.within > 0 {
				within = s.within
			}
			ok := pump(time.Now().Add(within), func() bool {
				text := screen()
				for _, w := range wants {
					if strings.Contains(text, w) {
						return true
					}
				}
				return false
			})
			if !ok {
				return nil, nil, fmt.Errorf("timed out waiting for %q; screen:\n%s", s.want, screen())
			}
		}
		if s.only != "" && !strings.Contains(screen(), s.only) {
			continue
		}
		if s.send != "" {
			// Type like a person would, a key at a time, so programs
			// redraw per keystroke.
			for _, k := range splitKeys(s.send) {
				if _, err := ptmx.Write([]byte(k)); err != nil {
					return nil, nil, err
				}
				pump(time.Now().Add(15*time.Millisecond), func() bool { return false })
			}
		}
		pump(time.Now().Add(s.pause), func() bool { return false })
		if s.mark != "" {
			marks[s.mark] = out.Len()
		}
		if verbose {
			fmt.Fprintf(os.Stderr, "--- after %q:\n%s\n", s.send, screen())
		}
	}

	// Let the program exit on its own, then drain the PTY.
	if !pump(time.Now().Add(60*time.Second), func() bool {
		select {
		case <-exited:
			return true
		default:
			return false
		}
	}) {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-exited
		return nil, nil, errors.New("program did not exit; screen:\n" + screen())
	}
	pump(time.Now().Add(500*time.Millisecond), func() bool { return false })
	return out.Bytes(), marks, nil
}

// splitKeys splits scripted input into keystrokes, keeping escape
// sequences such as arrow keys, bracketed pastes and Alt-Enter (ESC CR)
// whole: a lone ESC followed by a CR a moment later reads as Esc then
// Enter, which submits.
func splitKeys(s string) []string {
	var keys []string
	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], "\x1b[200~"):
			end := strings.Index(s[i:], "\x1b[201~")
			if end >= 0 {
				keys = append(keys, s[i:i+end+6])
				i += end + 6
				continue
			}
		case s[i] == 0x1b && i+2 < len(s) && s[i+1] == '[':
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
				j++
			}
			keys = append(keys, s[i:j+1])
			i = j + 1
			continue
		case s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\r':
			keys = append(keys, s[i:i+2])
			i += 2
			continue
		}
		keys = append(keys, s[i:i+1])
		i++
	}
	return keys
}

// identifyingStrings lists what must not appear in a committed stream: the
// user's name and home directory, the host name, and git identity.
func identifyingStrings(extra []string) []string {
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if len(s) >= 3 {
			out = append(out, s)
		}
	}
	if u, err := user.Current(); err == nil {
		add(u.HomeDir)
		add(u.Username)
		add(u.Name)
	}
	if h, err := os.Hostname(); err == nil {
		add(h)
		add(strings.Split(h, ".")[0])
	}
	for _, key := range []string{"user.email", "user.name"} {
		if v, err := exec.Command("git", "config", "--global", key).Output(); err == nil {
			add(string(v))
		}
	}
	for _, e := range extra {
		add(e)
	}
	return out
}

// scrub replaces each identifying string with a placeholder of the same
// length, so cursor positions and line wrapping are unchanged.
func scrub(data []byte, secrets []string) []byte {
	for _, s := range secrets {
		placeholder := []byte(s)
		for i, c := range placeholder {
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
				placeholder[i] = 'x'
			case c >= '0' && c <= '9':
				placeholder[i] = '0'
			}
		}
		data = replaceFold(data, []byte(s), placeholder)
	}
	// Session ids and the like are not identifying, but they are not
	// worth keeping either.
	return uuidPattern.ReplaceAllFunc(data, func(m []byte) []byte {
		return []byte("00000000-0000-0000-0000-000000000000")
	})
}

var uuidPattern = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

func replaceFold(data, old, repl []byte) []byte {
	lower := asciiLower(data)
	needle := asciiLower(old)
	for i := 0; ; {
		j := bytes.Index(lower[i:], needle)
		if j < 0 {
			return data
		}
		copy(data[i+j:], repl)
		i += j + len(old)
	}
}

// asciiLower lowercases ASCII letters only, so offsets stay byte-for-byte
// the same as the input's.
func asciiLower(b []byte) []byte {
	out := make([]byte, len(b))
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return out
}

func writeGzip(path string, data []byte) error {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return err
	}
	// No name or timestamp in the header, so re-recording identical output
	// gives an identical file.
	if _, err := io.Copy(zw, bytes.NewReader(data)); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}
