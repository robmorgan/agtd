package worker

import (
	"fmt"
	"path/filepath"
	"strings"
)

// An agent is started through the user's login shell, as an interactive
// login shell on the session's PTY, the way an ssh login or a terminal tab
// starts a program: the shell reads the user's profile and rc files and then
// execs the agent in its place. The agent so gets the environment the user
// sees in their own shells (PATH, credentials helpers such as llmd write
// into ~/.bashrc, version managers), read afresh for each session, rather
// than the one the daemon happened to start with: a daemon started by
// systemd or launchd never read the rc files, which interactive shells alone
// read. The exec keeps the shell's pid, so the agent is still the PTY
// session's leader and its process group, which kills, reaping, the
// foreground watch and handoffs rely on.
//
// A shell this file does not know how to drive, or no $SHELL at all, starts
// the agent directly with the daemon's environment, as [agents.NAME]
// login_shell = false does.

// shellKind is the syntax a login shell's command string is written in.
type shellKind int

const (
	shellUnknown shellKind = iota
	shellPOSIX
	shellFish
)

// loginShellKind is the kind of the shell at path, by its name.
func loginShellKind(path string) shellKind {
	if !filepath.IsAbs(path) {
		return shellUnknown
	}
	switch filepath.Base(path) {
	case "sh", "bash", "zsh", "dash", "ash", "ksh", "mksh", "oksh", "yash":
		return shellPOSIX
	case "fish":
		return shellFish
	}
	return shellUnknown
}

// loginShellArgs are the arguments that make shell, of kind, an interactive
// login shell that sets pinned (KEY=VALUE pairs) over whatever its rc files
// set and then execs command with args. pinned is what agtd itself gives the
// agent and an rc file must not undo: the session's SSH_AUTH_SOCK, say,
// which a profile pointing it at another agent would otherwise replace.
func loginShellArgs(kind shellKind, command string, args, pinned []string) []string {
	var script strings.Builder
	for _, kv := range pinned {
		key, value, _ := strings.Cut(kv, "=")
		if kind == shellFish {
			fmt.Fprintf(&script, "set -gx %s %s; ", key, fishQuote(value))
		} else {
			fmt.Fprintf(&script, "export %s=%s; ", key, posixQuote(value))
		}
	}
	if kind == shellFish {
		// fish puts every argument after the command string in $argv.
		script.WriteString("exec $argv")
	} else {
		// The first argument after the command string is $0, the rest "$@".
		script.WriteString(`exec "$0" "$@"`)
	}
	return append([]string{"-l", "-i", "-c", script.String(), command}, args...)
}

func posixQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func fishQuote(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

// launchFailureHint explains the exit codes a shell uses when it cannot run
// a command, for an agent started through shell: 127 for a command it did
// not find, 126 for one it could not execute. The agent itself may have
// exited with either, hence "may".
func launchFailureHint(shell, command string, code int32) string {
	switch code {
	case 127:
		return fmt.Sprintf(": `%s` may not be on the PATH the login shell %s sets up", command, shell)
	case 126:
		return fmt.Sprintf(": the login shell %s may not have been able to execute `%s`", shell, command)
	}
	return ""
}
