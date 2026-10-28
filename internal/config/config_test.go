package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// agentsOnPath leaves only the named commands on PATH.
func agentsOnPath(t *testing.T, names ...string) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
}

func TestLoad(t *testing.T) {
	agentsOnPath(t)
	dir := t.TempDir()
	cfg, err := Load(filepath.Join(dir, "missing.toml"))
	if err != nil || cfg.DefaultAgent != "claude" || cfg.Agents["codex"].Command != "codex" {
		t.Fatalf("defaults = %+v, %v", cfg, err)
	}
	path := filepath.Join(dir, "config.toml")
	os.WriteFile(path, []byte("default_agent = \"x\"\n[agents.y]\ncommand = \"y\"\n"), 0o600)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "default_agent `x`") {
		t.Fatalf("got %v", err)
	}
	os.WriteFile(path, []byte("[agents.y]\ncommand = \"y\"\nmodel_flag = \"\"\n[agents.z]\ncommand = \"z\"\n[agents.codex]\ncommand = \"codex\"\n"), 0o600)
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Agents["y"].Flag(); got != "" {
		t.Fatalf("empty model_flag = %q", got)
	}
	if got := cfg.Agents["z"].Flag(); got != "--model" {
		t.Fatalf("default model_flag = %q", got)
	}

	// Without default_agent: claude if configured, else the first agent.
	os.WriteFile(path, []byte("[agents.codex]\ncommand = \"codex\"\n[agents.claude]\ncommand = \"claude\"\n"), 0o600)
	if cfg, err := Load(path); err != nil || cfg.DefaultAgent != "claude" {
		t.Fatalf("with claude configured: %+v, %v", cfg, err)
	}
	os.WriteFile(path, []byte("[agents.zed]\ncommand = \"zed\"\n[agents.codex]\ncommand = \"codex\"\n"), 0o600)
	if cfg, err := Load(path); err != nil || cfg.DefaultAgent != "zed" {
		t.Fatalf("without claude: %+v, %v", cfg, err)
	}
}

func TestRequireAgentNamesTheConfigFile(t *testing.T) {
	_, err := defaultConfig().RequireAgent("missing", "/x/config.toml")
	if err == nil || !strings.Contains(err.Error(), "/x/config.toml") {
		t.Fatalf("got %v", err)
	}
}

// The written default parses to the built-in defaults, is private, and is
// never overwritten.
func TestWriteDefault(t *testing.T) {
	agentsOnPath(t, "pi")
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := WriteDefault(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode: %v %v", info, err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := defaultConfig()
	if cfg.DefaultAgent != want.DefaultAgent || !slices.Equal(cfg.AgentNames(), want.AgentNames()) {
		t.Fatalf("got %+v", cfg)
	}
	for name, agent := range cfg.Agents {
		if agent.Command != want.Agents[name].Command || agent.Flag() != "--model" {
			t.Fatalf("%s: %+v", name, agent)
		}
	}
	if got, want := cfg.Attach.AttachScrollbackRows(), want.Attach.AttachScrollbackRows(); got != want {
		t.Fatalf("attach scrollback rows = %d, built-in default %d", got, want)
	}
	if cfg.SSHAgent.SessionSocketEnabled() != want.SSHAgent.SessionSocketEnabled() || cfg.SSHAgent.Forward != want.SSHAgent.Forward {
		t.Fatalf("ssh agent = %+v, built-in default %+v", cfg.SSHAgent, want.SSHAgent)
	}
	os.WriteFile(path, []byte("default_agent = \"zed\"\n"), 0o600)
	if err := WriteDefault(path); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != "default_agent = \"zed\"\n" {
		t.Fatalf("overwrote: %q", data)
	}
}

// The default agent is the first of claude, codex and pi on PATH, in the
// built-in defaults and in the file written from them.
func TestDefaultAgentIsDetected(t *testing.T) {
	for _, tc := range []struct {
		installed []string
		want      string
	}{
		{nil, "claude"},
		{[]string{"pi"}, "pi"},
		{[]string{"codex", "pi"}, "codex"},
		{[]string{"pi", "claude"}, "claude"},
	} {
		agentsOnPath(t, tc.installed...)
		if got := defaultConfig().DefaultAgent; got != tc.want {
			t.Errorf("%v installed: built-in default %q, want %q", tc.installed, got, tc.want)
		}
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := WriteDefault(path); err != nil {
			t.Fatal(err)
		}
		agentsOnPath(t) // what the file says, not what is on PATH now
		cfg, err := Load(path)
		if err != nil || cfg.DefaultAgent != tc.want {
			t.Errorf("%v installed: written default %+v, %v; want %q", tc.installed, cfg, err, tc.want)
		}
	}
}

func TestAgentNamesKeepFileOrder(t *testing.T) {
	if got := defaultConfig().AgentNames(); !slices.Equal(got, []string{"claude", "codex", "pi"}) {
		t.Fatalf("defaults: %v", got)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(path, []byte("default_agent = \"claude\"\n[agents.claude]\ncommand = \"claude\"\n[agents.zed]\ncommand = \"zed\"\n[agents.codex]\ncommand = \"codex\"\n"), 0o600)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.AgentNames(); !slices.Equal(got, []string{"claude", "zed", "codex"}) {
		t.Fatalf("got %v", got)
	}
}

func TestAttachScrollbackRows(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		text string
		want int
		bad  bool
	}{
		{"", DefaultAttachScrollbackRows, false},
		{"[attach]\nscrollback_rows = 0\n", 0, false},
		{"[attach]\nscrollback_rows = -1\n", -1, false},
		{"[attach]\nscrollback_rows = 250\n", 250, false},
		{"[attach]\nscrollback_rows = -2\n", 0, true},
	} {
		path := filepath.Join(dir, "config.toml")
		if err := os.WriteFile(path, []byte(tc.text), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if tc.bad {
			if err == nil {
				t.Fatalf("%q: loaded", tc.text)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q: %v", tc.text, err)
		}
		if got := cfg.Attach.AttachScrollbackRows(); got != tc.want {
			t.Fatalf("%q: scrollback rows = %d, want %d", tc.text, got, tc.want)
		}
	}
}

func TestSSHAgent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/ssh-x/agent.1")
	home, _ := os.UserHomeDir()
	fp := "SHA256:" + strings.Repeat("A", 43)
	for _, tc := range []struct {
		text          string
		sessionSocket bool
		forward       bool
		agent         string
		keys          []string
		bad           string
	}{
		{text: "", sessionSocket: true, agent: "/tmp/ssh-x/agent.1"},
		{text: "[ssh_agent]\nsession_socket = false\n", agent: "/tmp/ssh-x/agent.1"},
		{text: "[ssh_agent]\nforward = true\nagent = \"~/.1password/agent.sock\"\nkeys = [\"" + fp + "\"]\n",
			sessionSocket: true, forward: true, agent: filepath.Join(home, ".1password/agent.sock"), keys: []string{fp}},
		{text: "[ssh_agent]\nagent = \"/run/agent.sock\"\n", sessionSocket: true, agent: "/run/agent.sock"},
		{text: "[ssh_agent]\nkeys = [\"MD5:aa:bb\"]\n", bad: "`MD5:aa:bb` is not a key fingerprint"},
		{text: "[ssh_agent]\nkeys = [\"SHA256:short\"]\n", bad: "`SHA256:short` is not a key fingerprint"},
	} {
		if err := os.WriteFile(path, []byte(tc.text), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if tc.bad != "" {
			if err == nil || !strings.Contains(err.Error(), tc.bad) {
				t.Fatalf("%q: got %v", tc.text, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q: %v", tc.text, err)
		}
		a := cfg.SSHAgent
		if a.SessionSocketEnabled() != tc.sessionSocket || a.Forward != tc.forward || a.AgentSocket() != tc.agent || !slices.Equal(a.Keys, tc.keys) {
			t.Fatalf("%q: %+v (session socket %v, agent %q)", tc.text, a, a.SessionSocketEnabled(), a.AgentSocket())
		}
	}
	// Without $SSH_AUTH_SOCK or an agent there is nothing to forward.
	t.Setenv("SSH_AUTH_SOCK", "")
	if got := (SSHAgentConfig{Forward: true}).AgentSocket(); got != "" {
		t.Fatalf("agent socket = %q", got)
	}
}

func TestAgentLoginShell(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(path, []byte("[agents.a]\ncommand = \"a\"\n[agents.b]\ncommand = \"b\"\nlogin_shell = false\n[agents.c]\ncommand = \"c\"\nlogin_shell = true\n"), 0o600)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"a": true, "b": false, "c": true} {
		if got := cfg.Agents[name].LoginShellEnabled(); got != want {
			t.Errorf("agent %s: LoginShellEnabled() = %v, want %v", name, got, want)
		}
	}
	if !defaultConfig().Agents["claude"].LoginShellEnabled() {
		t.Error("built-in claude does not start through the login shell")
	}
}
