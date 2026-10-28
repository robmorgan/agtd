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
		// Mistakes turn forwarding off, and say why, without failing the
		// file: agtd still starts, and agt's other settings hold.
		{text: "[ssh_agent]\nkeys = [\"MD5:aa:bb\"]\n", bad: "`MD5:aa:bb` is not a key fingerprint"},
		{text: "[ssh_agent]\nkeys = [\"SHA256:short\"]\n", bad: "`SHA256:short` is not a key fingerprint"},
		// A misspelled or misplaced setting would leave every key offered.
		{text: "[ssh_agent]\nforward = true\nkey = [\"" + fp + "\"]\n", bad: "[ssh_agent] has no setting `key`"},
		{text: "[ssh_agent]\nforward = true\n[remote]\nkeys = [\"" + fp + "\"]\n", bad: "`keys` is under [remote]"},
		{text: "keys = [\"" + fp + "\"]\n[ssh_agent]\nforward = true\n", bad: "`keys` is at the top of the file"},
		{text: "Keys = [\"" + fp + "\"]\n[ssh_agent]\nforward = true\n", bad: "`Keys` is at the top of the file"},
		{text: "[ssh-agent]\nforward = true\n", bad: "not [ssh-agent]"},
		// The decoder matches names whatever their case: a table spelled so
		// would replace [ssh_agent], and a setting spelled so is not the one
		// the docs name.
		{text: "[SSH_Agent]\nforward = true\n", bad: "not [SSH_Agent]"},
		{text: "[ssh_agent]\nforward = true\n[SSH_Agent]\nkeys = [\"" + fp + "\"]\n", bad: "not [SSH_Agent]"},
		{text: "[ssh_agent]\nforward = true\nKeys = [\"" + fp + "\"]\n", bad: "[ssh_agent] has no setting `Keys`"},
		// A value of the wrong type turns forwarding off too.
		{text: "[ssh_agent]\nforward = \"yes\"\n", bad: "[ssh_agent] could not be read (line 2 (last key \"ssh_agent.forward\")"},
		// Elsewhere unknown keys are still ignored, those named like an SSH
		// agent setting other than keys too: misplaced, they leave
		// forwarding as it was.
		{text: "[attach]\nfrobnicate = 1\n", sessionSocket: true, agent: "/tmp/ssh-x/agent.1"},
		{text: "forward = true\n[remote]\nforward = true\n[agents.x]\ncommand = \"x\"\nagent = \"y\"\n", sessionSocket: true, agent: "/tmp/ssh-x/agent.1"},
	} {
		if err := os.WriteFile(path, []byte(tc.text), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("%q: %v", tc.text, err)
		}
		if tc.bad != "" {
			if !strings.Contains(cfg.SSHAgent.Problem, tc.bad) {
				t.Fatalf("%q: problem %q, want %q", tc.text, cfg.SSHAgent.Problem, tc.bad)
			}
			continue
		}
		a := cfg.SSHAgent
		if a.Problem != "" {
			t.Fatalf("%q: problem %q", tc.text, a.Problem)
		}
		if a.SessionSocketEnabled() != tc.sessionSocket || a.Forward != tc.forward || a.AgentSocket() != tc.agent || !slices.Equal(a.Keys, tc.keys) {
			t.Fatalf("%q: %+v (session socket %v, agent %q)", tc.text, a, a.SessionSocketEnabled(), a.AgentSocket())
		}
	}
	// [ssh_agent] that cannot be read leaves every setting at its default,
	// session_socket too, and the rest of the file stands.
	if err := os.WriteFile(path, []byte("default_agent = \"x\"\n[agents.x]\ncommand = \"x\"\n[ssh_agent]\nsession_socket = false\nforward = \"yes\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err := Load(path); err != nil || cfg.DefaultAgent != "x" || !cfg.SSHAgent.SessionSocketEnabled() || cfg.SSHAgent.Forward {
		t.Fatalf("unreadable [ssh_agent]: %+v, %v", cfg, err)
	}
	// forward = false turns forwarding off on purpose, whatever else is
	// wrong there; unset, it does not.
	for text, off := range map[string]bool{
		"[ssh_agent]\nforward = false\nkey = []\n": true,
		"[ssh_agent]\nkey = []\n":                  false,
		"[ssh_agent]\nforward = true\nkey = []\n":  false,
	} {
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if cfg, err := Load(path); err != nil || cfg.SSHAgent.Problem == "" || cfg.SSHAgent.ForwardOff() != off {
			t.Fatalf("%q: forward off %v, problem %q (%v), want off %v", text, cfg.SSHAgent.ForwardOff(), cfg.SSHAgent.Problem, err, off)
		}
	}
	// Without $SSH_AUTH_SOCK or an agent there is nothing to forward.
	t.Setenv("SSH_AUTH_SOCK", "")
	if got := (SSHAgentConfig{Forward: true}).AgentSocket(); got != "" {
		t.Fatalf("agent socket = %q", got)
	}
}

// The frame options of [attach]: alt scrollback and Submit and switch off,
// the wheel prediction on, unless set, in the built-in defaults and the
// written file alike.
func TestAttachFrameOptions(t *testing.T) {
	// Value: protects=the alt scrollback prototype and Submit and switch
	// stay off and the wheel prediction on for anyone who did not set them;
	// fails_when=a toml key is misspelt, WheelPredicted's nil default
	// flips, or the written template turns a prototype on; why_new=no test
	// read these keys; seam=none; no_claim=what the painter does with them
	// (screen tests).
	agentsOnPath(t)
	dir := t.TempDir()
	written := filepath.Join(dir, "written.toml")
	if err := WriteDefault(written); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, text         string
		alt, wheel, submit bool
	}{
		{"unset", "", false, true, false},
		{"written", "", false, true, false},
		{"alt", "[attach]\nalt_scrollback = true\n", true, true, false},
		{"no wheel", "[attach]\npredict_wheel = false\n", false, false, false},
		{"wheel", "[attach]\npredict_wheel = true\n", false, true, false},
		{"submit", "[attach]\nsubmit_and_switch = true\n", false, true, true},
	} {
		path := written
		if tc.name != "written" {
			path = filepath.Join(dir, "config.toml")
			if err := os.WriteFile(path, []byte(tc.text), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if a := cfg.Attach; a.AltScrollback != tc.alt || a.WheelPredicted() != tc.wheel || a.SubmitAndSwitch != tc.submit {
			t.Errorf("%s: alt %v wheel %v submit %v", tc.name, a.AltScrollback, a.WheelPredicted(), a.SubmitAndSwitch)
		}
	}
}
