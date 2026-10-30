package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agtd.sh/internal/protocol"
	"agtd.sh/internal/session"
)

// cloneRequest asks for a session that clones url into path as workspace ws.
func cloneRequest(ws, url, path string) *protocol.Request {
	return &protocol.Request{CreateSession: &protocol.CreateSession{Clone: &protocol.CloneWorkspace{Workspace: ws, URL: url, Path: path}}}
}

// A clone runs `git clone -- URL PATH` from the home directory, in a session
// named after the workspace, with the session's own agent socket; it records
// no base commit and registers nothing: the client adds the workspace once
// the clone has succeeded.
func TestCloneSession(t *testing.T) {
	bin := t.TempDir()
	// A stand-in for git: what it was asked, where, with which agent, then
	// the checkout.
	stub := "#!/bin/sh\ncase \"$3\" in *missing*) echo 'fatal: repository not found'; exit 128;; esac\necho \"git $*\"; echo \"pwd:$(pwd -P)\"; echo \"auth:$SSH_AUTH_SOCK\"; mkdir -p \"$4/.git\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	h := newHarness(t)
	t.Setenv("HOME", h.cwd)
	c := h.control()

	resp := c.call(cloneRequest("api", "git@example.com:org/api.git", "~/src/api"))
	if resp.CreateSession == nil {
		t.Fatalf("clone = %#v", resp.Error)
	}
	if resp.CreateSession.SessionID != "clone-api" || resp.CreateSession.Cwd != h.cwd {
		t.Fatalf("clone session = %#v", resp.CreateSession)
	}
	dest := filepath.Join(h.cwd, "src", "api")
	h.eventually("the clone to finish", func() bool { return h.session("clone-api").Status == session.StatusExited })
	rec := h.session("clone-api")
	if rec.ExitCode == nil || *rec.ExitCode != 0 || rec.Agent != "git" {
		t.Fatalf("clone record = %#v", rec)
	}
	if _, err := os.Stat(filepath.Join(dest, ".git")); err != nil {
		t.Fatalf("checkout: %v", err)
	}
	hist := h.request(&protocol.Request{GetHistory: &protocol.GetHistory{SessionID: "clone-api"}})
	if hist.History == nil {
		t.Fatalf("history = %#v", hist.Error)
	}
	real, _ := filepath.EvalSymlinks(h.cwd)
	for _, want := range []string{"git clone -- git@example.com:org/api.git " + dest, "pwd:" + real, "auth:" + h.paths.SSHAgentSocketPath(resp.CreateSession.UID)} {
		if !strings.Contains(hist.History.Data, want) {
			t.Fatalf("no %q in the clone's output:\n%s", want, hist.History.Data)
		}
	}
	if ws := h.workspaces(); len(ws) != 0 {
		t.Fatalf("the daemon registered %+v itself", ws)
	}

	// The path exists now; the next clone of the name is numbered.
	wantError(t, c.call(cloneRequest("api", "git@example.com:org/api.git", dest)), "already exists; to use it as is, add it with `agt workspace add api "+dest+"`")
	if resp := c.call(cloneRequest("api", "git@example.com:org/api.git", "~/src/api2")); resp.CreateSession == nil || resp.CreateSession.SessionID != "clone-api-2" {
		t.Fatalf("second clone = %#v, %#v", resp.CreateSession, resp.Error)
	}

	// A clone that fails at once, perhaps before its worker is seen
	// ready, was still created: its end is for the client to report.
	gone := c.call(cloneRequest("gone", "git@example.com:org/missing.git", "~/gone"))
	if gone.CreateSession == nil {
		t.Fatalf("failing clone = %#v", gone.Error)
	}
	h.eventually("the failing clone to end", func() bool { return h.session("clone-gone").Status == session.StatusFailed })
	if rec := h.session("clone-gone"); rec.Error == nil || !strings.Contains(*rec.Error, "exited with code 128") {
		t.Fatalf("failing clone record = %#v", rec)
	}
}

func TestCloneRefusals(t *testing.T) {
	h := newHarness(t)
	t.Setenv("HOME", h.cwd)
	if resp := h.addWorkspace("taken", h.cwd); resp.Workspace == nil {
		t.Fatalf("add workspace: %#v", resp.Error)
	}
	c := h.control()
	for _, tc := range []struct{ ws, url, path, want string }{
		{"taken", "git@h:r", "~/x", "workspace `taken` already exists"},
		{"Bad", "git@h:r", "~/x", "invalid workspace name"},
		{"api", "https://github.com/org/api.git", "~/x", "only SSH URLs"},
		{"api", "file:///srv/repo", "~/x", "only SSH URLs"},
		{"api", "/srv/repo", "~/x", "only SSH URLs"},
		{"api", "ext::sh -c touch% /tmp/pwned", "~/x", "spaces or control characters"},
		{"api", "ext::ssh", "~/x", "only SSH URLs"},
		{"api", "-oProxyCommand=x:r", "~/x", "not a URL"},
		{"api", "ssh://-oProxyCommand=x/r", "~/x", "no host"},
		{"api", "ssh://host", "~/x", "needs a repository path"},
		{"api", "git@h:r", "src/x", "must be an absolute path"},
		{"api", "git@h:r", "~", "already exists"},
	} {
		wantError(t, c.call(cloneRequest(tc.ws, tc.url, tc.path)), tc.want)
	}
	for _, ok := range []string{"git@github.com:org/api.git", "github.com:org/api", "ssh://git@github.com/org/api.git", "ssh://git@host:2222/org/api"} {
		if err := checkCloneURL(ok); err != nil {
			t.Fatalf("checkCloneURL(%q) = %v", ok, err)
		}
	}
	bad := cloneRequest("api", "git@h:r", "~/x")
	bad.CreateSession.Agent = "sh"
	wantError(t, c.call(bad), "a clone takes no working directory, agent")
	if ss := h.request(&protocol.Request{ListSessions: protocol.Empty}); ss.Sessions == nil || len(*ss.Sessions) != 0 {
		t.Fatalf("refused clones left sessions: %#v", ss)
	}
}

// A daemon whose sessions get no agent socket cannot forward, so it refuses
// to clone rather than clone with whatever agent it has.
func TestCloneNeedsTheSessionSocket(t *testing.T) {
	h := newHarnessWithConfig(t, "\n[ssh_agent]\nsession_socket = false\n")
	t.Setenv("HOME", h.cwd)
	wantError(t, h.control().call(cloneRequest("api", "git@h:r", "~/x")), "`[ssh_agent] session_socket = false` turns off")
}
