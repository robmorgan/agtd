package worker

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"agtd.sh/internal/session"
)

func TestLoginShellKind(t *testing.T) {
	for path, want := range map[string]shellKind{
		"/bin/bash": shellPOSIX, "/usr/bin/zsh": shellPOSIX, "/bin/sh": shellPOSIX,
		"/usr/local/bin/fish": shellFish,
		"":                    shellUnknown, "bash": shellUnknown, "/usr/sbin/nologin": shellUnknown,
		"/usr/bin/nu": shellUnknown,
	} {
		if got := loginShellKind(path); got != want {
			t.Errorf("loginShellKind(%q) = %v, want %v", path, got, want)
		}
	}
}

// The command string passes the command, its arguments and the pinned
// values through untouched, whatever they contain.
func TestLoginShellArgsQuoting(t *testing.T) {
	pinned := []string{`SSH_AUTH_SOCK=/tmp/it's "a" $HOME \sock`}
	args := loginShellArgs(shellPOSIX, "/bin/sh",
		[]string{"-c", `printf '%s|' "$SSH_AUTH_SOCK" "$@"`, "x", "a b", "c'd", "$e"}, pinned)
	if got := args[:3]; strings.Join(got, " ") != "-l -i -c" {
		t.Fatalf("args start %q", got)
	}
	// Run without -l -i, so the test does not read this machine's profile.
	out, err := exec.Command("/bin/sh", args[2:]...).Output()
	if err != nil {
		t.Fatal(err)
	}
	if want := `/tmp/it's "a" $HOME \sock|a b|c'd|$e|`; string(out) != want {
		t.Fatalf("output %q, want %q", out, want)
	}

	fish := loginShellArgs(shellFish, "claude", []string{"--model", "x"}, pinned)
	want := []string{"-l", "-i", "-c", `set -gx SSH_AUTH_SOCK '/tmp/it\'s "a" $HOME \\sock'; exec $argv`, "claude", "--model", "x"}
	if strings.Join(fish, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("fish args %q, want %q", fish, want)
	}
}

// shellProbe prints what the agent got from the shell's rc files, and its
// parent's pid, one per line.
const shellProbe = `echo "probe:$LLMD_PROBE"; echo "auth:$SSH_AUTH_SOCK"; echo "ppid:$PPID"`

// runLoginShellSession runs a session whose agent is /bin/sh -c script
// started through shell, with HOME (and ZDOTDIR) at home, to its end, and
// returns its rendered history.
func runLoginShellSession(t *testing.T, shell, home, script string) (*harness, string) {
	t.Helper()
	if _, err := os.Stat(shell); err != nil {
		t.Skipf("no %s", shell)
	}
	dir, h := newRoot(t)
	t.Setenv("SHELL", shell)
	t.Setenv("HOME", home)
	t.Setenv("ZDOTDIR", home)
	for _, k := range []string{"ENV", "BASH_ENV", "TERM_PROGRAM", "LLMD_PROBE"} {
		t.Setenv(k, "")
	}
	t.Setenv("SSH_AUTH_SOCK", "/inherited/agent.sock")
	err := Run(Args{
		SessionID: h.sessionID, Cwd: dir, CreatedAt: h.createdAt, UID: h.uid,
		AgentName: "sh", Command: "/bin/sh", Args: []string{"-c", script},
		SSHAgentSocket: true, LoginShell: true,
	})
	h.exited = true
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	logged, err := os.ReadFile(h.paths.RenderedLogPath(h.sessionID))
	if err != nil {
		t.Fatal(err)
	}
	return h, string(logged)
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// checkProbe checks that the agent saw what the rc file exported, kept the
// session's own SSH_AUTH_SOCK over the rc file's, and is the worker's own
// child (the worker runs in this process): the shell exec'd it in its place
// rather than forking it.
func checkProbe(t *testing.T, h *harness, logged string) {
	t.Helper()
	if !hasLine(logged, "probe:from-rc") {
		t.Errorf("the agent did not get the rc file's export:\n%s", logged)
	}
	if want := "auth:" + h.paths.SSHAgentSocketPath(h.uid); !hasLine(logged, want) {
		t.Errorf("the agent's SSH_AUTH_SOCK is not the session's (%q):\n%s", want, logged)
	}
	if want := "ppid:" + strconv.Itoa(os.Getpid()); !hasLine(logged, want) {
		t.Errorf("the agent is not the worker's child (%q):\n%s", want, logged)
	}
	if rec, _ := h.store.GetSession(h.sessionID); rec == nil || rec.Status != session.StatusExited {
		t.Errorf("record = %#v", rec)
	}
}

// What Debian and Ubuntu give a user: ~/.profile sources ~/.bashrc, which
// returns at once unless the shell is interactive. Exports below that guard,
// where installers append theirs, reach only interactive shells; a daemon
// started by systemd through `bash -l -c` never saw them.
func TestLoginShellBashReadsInteractiveRC(t *testing.T) {
	home := t.TempDir()
	writeFiles(t, home, map[string]string{
		".profile": `if [ -n "$BASH_VERSION" ] && [ -f "$HOME/.bashrc" ]; then . "$HOME/.bashrc"; fi` + "\n",
		".bashrc": "case $- in\n  *i*) ;;\n  *) return;;\nesac\n" +
			"export LLMD_PROBE=from-rc\nexport SSH_AUTH_SOCK=/rc/agent.sock\n",
	})
	h, logged := runLoginShellSession(t, "/bin/bash", home, shellProbe)
	checkProbe(t, h, logged)
}

func TestLoginShellZshReadsInteractiveRC(t *testing.T) {
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("no zsh")
	}
	home := t.TempDir()
	writeFiles(t, home, map[string]string{
		".zshrc": "export LLMD_PROBE=from-rc\nexport SSH_AUTH_SOCK=/rc/agent.sock\n",
	})
	h, logged := runLoginShellSession(t, zsh, home, shellProbe)
	checkProbe(t, h, logged)
}

// A command the login shell cannot find ends the session failed, with a
// hint about the PATH the shell sets up.
func TestLoginShellMissingCommandHint(t *testing.T) {
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("no /bin/bash")
	}
	dir, h := newRoot(t)
	t.Setenv("SHELL", "/bin/bash")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BASH_ENV", "")
	err := Run(Args{
		SessionID: h.sessionID, Cwd: dir, CreatedAt: h.createdAt, UID: h.uid,
		AgentName: "missing", Command: "agtd-no-such-agent", LoginShell: true,
	})
	h.exited = true
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rec, _ := h.store.GetSession(h.sessionID)
	if rec == nil || rec.Status != session.StatusFailed || rec.Error == nil ||
		!strings.Contains(*rec.Error, "code 127: `agtd-no-such-agent` may not be on the PATH the login shell /bin/bash sets up") {
		t.Fatalf("record = %#v (error %v)", rec, rec.Error)
	}
}

// A $SHELL the worker cannot drive starts the agent directly.
func TestLoginShellUnknownShellStartsDirectly(t *testing.T) {
	_, logged := runLoginShellSessionUnchecked(t, "/usr/bin/false")
	if !hasLine(logged, "probe:") {
		t.Fatalf("the agent did not run directly:\n%s", logged)
	}
}

func runLoginShellSessionUnchecked(t *testing.T, shell string) (*harness, string) {
	t.Helper()
	dir, h := newRoot(t)
	t.Setenv("SHELL", shell)
	t.Setenv("LLMD_PROBE", "")
	err := Run(Args{
		SessionID: h.sessionID, Cwd: dir, CreatedAt: h.createdAt, UID: h.uid,
		AgentName: "sh", Command: "/bin/sh", Args: []string{"-c", shellProbe}, LoginShell: true,
	})
	h.exited = true
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	logged, err := os.ReadFile(h.paths.RenderedLogPath(h.sessionID))
	if err != nil {
		t.Fatal(err)
	}
	return h, string(logged)
}

// hasLine reports whether text has line, trailing blanks aside.
func hasLine(text, line string) bool {
	for l := range strings.Lines(text) {
		if strings.TrimRight(l, " \r\n") == line {
			return true
		}
	}
	return false
}
