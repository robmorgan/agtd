package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"agtd.sh/internal/db"
	"agtd.sh/internal/protocol"
	"agtd.sh/internal/session"
)

// Workspaces are named directories on this machine, stored in state.db and
// managed by clients over the protocol. They exist so a client that does not
// share this machine's filesystem can still say where a session runs. They
// are addressing only: agtd does not sync them, and creates one only by
// starting a clone session (cloneLaunch) at a client's request; the client
// registers the clone once it has succeeded.

func (s *Server) addWorkspace(req *protocol.AddWorkspace) (*session.Workspace, error) {
	name := strings.TrimSpace(req.Name)
	if !session.ValidName(name) {
		return nil, fmt.Errorf("invalid workspace name `%s`: %s", name, session.NameRules)
	}
	path, err := expandHome(req.Path)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("workspace path `%s` must be an absolute path or start with `~/`", req.Path)
	}
	path = filepath.Clean(path)
	if info, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("workspace path `%s` does not exist", path)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("workspace path `%s` is not a directory", path)
	}
	w, err := s.db.AddWorkspace(name, path)
	if errors.Is(err, db.ErrWorkspaceExists) {
		return nil, fmt.Errorf("workspace `%s` already exists", name)
	}
	if err != nil {
		return nil, err
	}
	return &w, nil
}

func (s *Server) removeWorkspace(name string) error {
	removed, err := s.db.RemoveWorkspace(name)
	if err != nil {
		return err
	}
	if !removed {
		return fmt.Errorf("workspace `%s` not found", name)
	}
	return nil
}

// resolveCwd turns a create request's Cwd and Workspace into the absolute
// directory the agent runs in, and the workspace name to record. Everything
// is resolved on this machine: the client may be elsewhere, so its paths mean
// nothing here unless they are absolute or relative to a named workspace.
func (s *Server) resolveCwd(req *protocol.CreateSession) (string, *string, error) {
	var name string
	if req.Workspace != nil {
		name = strings.TrimSpace(*req.Workspace)
	}
	if name == "" {
		cwd, err := expandHome(req.Cwd)
		if err != nil {
			return "", nil, err
		}
		if !filepath.IsAbs(cwd) {
			return "", nil, fmt.Errorf("working directory `%s` must be an absolute path", req.Cwd)
		}
		return filepath.Clean(cwd), nil, nil
	}

	w, err := s.db.GetWorkspace(name)
	if err != nil {
		return "", nil, err
	}
	if w == nil {
		return "", nil, fmt.Errorf("workspace `%s` not found; add it with `agt workspace add`", name)
	}
	if req.Cwd == "" {
		return w.Path, &name, nil
	}
	if filepath.IsAbs(req.Cwd) || strings.HasPrefix(req.Cwd, "~") {
		return "", nil, fmt.Errorf("working directory `%s` must be relative to workspace `%s`", req.Cwd, name)
	}
	// A lexical check, to catch `../` mistakes; symlinks inside the
	// workspace are followed like anywhere else.
	cwd := filepath.Join(w.Path, req.Cwd)
	if rel, err := filepath.Rel(w.Path, cwd); err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", nil, fmt.Errorf("working directory `%s` is outside workspace `%s`", req.Cwd, name)
	}
	return cwd, &name, nil
}

// expandHome replaces a leading `~` or `~/` with the daemon user's home
// directory. Paths are resolved on the daemon's machine, never the client's.
// `~user` is not supported and is returned unchanged.
func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot expand `%s`: %w", path, err)
	}
	return filepath.Join(home, path[1:]), nil
}

// cloneLaunch checks a CreateSession that asks for a clone and returns the
// session that runs it: `git clone -- URL PATH`, from the daemon user's home,
// through the login shell like an agent, with SSH_AUTH_SOCK answered only by
// a client's forwarder (worker.Args.RequireForwarder), so the clone uses the
// keys of the client that asked for it or none, never an agent agtd
// inherited. It records no base commit: there is no repository yet.
func (s *Server) cloneLaunch(req *protocol.CreateSession, name *string) (launch, error) {
	c := req.Clone
	if req.Cwd != "" || req.Agent != "" || req.Model != nil || req.Workspace != nil {
		return launch{}, errors.New("a clone takes no working directory, agent, model or workspace")
	}
	if !s.config.SSHAgent.SessionSocketEnabled() {
		return launch{}, fmt.Errorf("cloning needs SSH agent forwarding, which `[ssh_agent] session_socket = false` turns off in %s", s.paths.Config)
	}
	ws := strings.TrimSpace(c.Workspace)
	if !session.ValidName(ws) {
		return launch{}, fmt.Errorf("invalid workspace name `%s`: %s", ws, session.NameRules)
	}
	if w, err := s.db.GetWorkspace(ws); err != nil {
		return launch{}, err
	} else if w != nil {
		return launch{}, fmt.Errorf("workspace `%s` already exists, at %s", ws, w.Path)
	}
	if err := checkCloneURL(c.URL); err != nil {
		return launch{}, err
	}
	path, err := expandHome(c.Path)
	if err != nil {
		return launch{}, err
	}
	if !filepath.IsAbs(path) {
		return launch{}, fmt.Errorf("clone path `%s` must be an absolute path or start with `~/`", c.Path)
	}
	path = filepath.Clean(path)
	if _, err := os.Lstat(path); err == nil {
		return launch{}, fmt.Errorf("`%s` already exists; to use it as is, add it with `agt workspace add %s %s`", path, ws, path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return launch{}, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return launch{}, err
	}
	if name == nil {
		name = s.cloneSessionName(ws)
	}
	return launch{
		name: name, agentName: "git", cwd: home,
		command: "git", args: []string{"clone", "--", c.URL, path},
		agentSocket: true, requireForwarder: true, loginShell: true, endedIsStarted: true,
	}, nil
}

// cloneSessionName names the session that clones workspace ws: clone-WS, or
// clone-WS-2 and so on while those are taken. Nil, for a generated name,
// when none fits the name rules.
func (s *Server) cloneSessionName(ws string) *string {
	for i := 1; i <= 100; i++ {
		name := "clone-" + ws
		if i > 1 {
			name = fmt.Sprintf("%s-%d", name, i)
		}
		if !session.ValidName(name) {
			return nil
		}
		if rec, err := s.db.GetSession(name); err == nil && rec == nil {
			return &name
		}
	}
	return nil
}

// checkCloneURL accepts SSH URLs only, since a clone authenticates with a
// forwarded agent: ssh://[user@]host[:port]/path, or git's scp-like
// [user@]host:path. Anything else (https, git://, file, a local path, a
// remote helper such as ext::) is refused, as is anything git or ssh could
// take for an option.
func checkCloneURL(url string) error {
	refuse := func(why string) error {
		return fmt.Errorf("cannot clone `%s`: %s", url, why)
	}
	for _, r := range url {
		if r <= ' ' || r == 0x7f {
			return refuse("it has spaces or control characters")
		}
	}
	if url == "" || strings.HasPrefix(url, "-") {
		return refuse("not a URL")
	}
	var host string
	if rest, ok := strings.CutPrefix(url, "ssh://"); ok {
		host, _, _ = strings.Cut(rest, "/")
		if !strings.Contains(rest, "/") || strings.HasSuffix(rest, "/") {
			return refuse("an ssh:// URL needs a repository path")
		}
	} else {
		if strings.Contains(url, "://") || strings.Contains(url, "::") {
			return refuse("only SSH URLs (ssh://host/path or user@host:path) can be cloned with a forwarded agent")
		}
		before, path, ok := strings.Cut(url, ":")
		if !ok || strings.Contains(before, "/") || path == "" {
			return refuse("only SSH URLs (ssh://host/path or user@host:path) can be cloned with a forwarded agent")
		}
		host = before
	}
	if i := strings.LastIndex(host, "@"); i >= 0 {
		host = host[i+1:]
	}
	if host == "" || strings.HasPrefix(host, "-") {
		return refuse("no host")
	}
	return nil
}
