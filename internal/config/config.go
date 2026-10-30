// Package config reads config.toml, which the daemon and the agt CLI
// share. Unknown keys and tables are ignored, except around [ssh_agent]:
// there a mistake turns forwarding off instead (SSHAgentConfig.Problem).
package config

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"agtd.sh/internal/sshagent"
)

type Config struct {
	DefaultAgent string                 `toml:"default_agent"`
	Agents       map[string]AgentConfig `toml:"agents"`
	Remote       RemoteConfig           `toml:"remote"`
	Attach       AttachConfig           `toml:"attach"`
	Composer     ComposerConfig         `toml:"composer"`
	// SSHAgent is [ssh_agent], decoded on its own (file), so that a
	// mistake there turns forwarding off rather than failing the file.
	SSHAgent SSHAgentConfig `toml:"-"`

	// agentOrder lists the Agents keys in file order, which a map loses.
	agentOrder []string
}

// file is config.toml as Load decodes it: [ssh_agent] is kept aside, and
// decoded into Config.SSHAgent on its own.
type file struct {
	Config
	SSHAgent toml.Primitive `toml:"ssh_agent"`
}

// RemoteConfig enables remote access over QUIC. It is off unless Listen is
// set; even then only clients listed in the authorized-clients file can
// connect.
type RemoteConfig struct {
	// Listen is the UDP host:port to accept QUIC connections on. Prefer a
	// Tailscale or WireGuard address over a public one.
	Listen string `toml:"listen"`
}

// AttachConfig is how `agt attach` fills the terminal.
type AttachConfig struct {
	// ScrollbackRows caps the rows of a session's scrollback an attach
	// copies into the terminal, the most recent ones: -1 copies all of it,
	// 0 only the screen. Unset means DefaultAttachScrollbackRows. `agent
	// history` has the rest.
	ScrollbackRows *int `toml:"scrollback_rows"`
	// Mode is how the screen reaches the terminal: "auto" (screen frames
	// from a remote daemon, the raw output from the local one), "frames"
	// or "raw". Unset means auto. See ARCHITECTURE.md, "Screen frames".
	Mode string `toml:"mode"`
	// AltScrollback, under frames, keeps the terminal on the primary
	// screen while an agent runs on the alternate one and pushes the rows
	// that leave the top of its transcript into the terminal's own
	// scrollback. A prototype, off unless set: it applies to every
	// program on the alternate screen. See
	// docs/designs/alt-screen-scrollback.md.
	AltScrollback bool `toml:"alt_scrollback"`
	// PredictWheel, under frames, draws a wheel tick over the transcript
	// of an agent on the alternate screen at once, and lets the agent's
	// frames correct it, with or without AltScrollback. Unset means on.
	PredictWheel *bool `toml:"predict_wheel"`
	// PredictSelection, under frames, draws the selection an agent that
	// takes the mouse (Claude Code, pi in fullscreen) makes as the left
	// button is dragged at once, and lets the agent's frames correct it.
	// Unset means on.
	PredictSelection *bool `toml:"predict_selection"`
	// SubmitAndSwitch offers Submit and switch first in the Ctrl-Y
	// palette (so Ctrl-Y, Enter is it). Hidden unless set.
	SubmitAndSwitch bool `toml:"submit_and_switch"`
}

// WheelPredicted is PredictWheel with its default applied.
func (a AttachConfig) WheelPredicted() bool { return a.PredictWheel == nil || *a.PredictWheel }

// SelectionPredicted is PredictSelection with its default applied.
func (a AttachConfig) SelectionPredicted() bool {
	return a.PredictSelection == nil || *a.PredictSelection
}

// AttachMode is Mode with its default applied.
func (a AttachConfig) AttachMode() string {
	if a.Mode == "" {
		return "auto"
	}
	return a.Mode
}

// DefaultAttachScrollbackRows bounds what an attach sends: a session's full
// scrollback can be hundreds of KiB (see BENCHMARKS.md).
const DefaultAttachScrollbackRows = 1000

// AttachScrollbackRows is ScrollbackRows with its default applied: -1 for
// all of the scrollback, otherwise a number of rows.
func (a AttachConfig) AttachScrollbackRows() int {
	if a.ScrollbackRows == nil {
		return DefaultAttachScrollbackRows
	}
	return *a.ScrollbackRows
}

// ComposerConfig is the local echo of typing in `agt attach`: keys are drawn
// in the agent's own input field at once and sent as typed.
type ComposerConfig struct {
	// Enabled turns the local echo on (the default) or off, in which
	// case keys are sent as they are typed with no local drawing.
	Enabled *bool `toml:"enabled"`
	// FlushAfter is how long typed keys wait for company before they go
	// to the agent, as a duration ("50ms"); the local drawing never
	// waits. 0 sends every key at once. Unset means DefaultFlushAfter.
	FlushAfter string `toml:"flush_after"`
}

// DefaultFlushAfter batches keys typed within the window into one write,
// the way fast typing arrives, so the agent redraws per batch rather than
// per key.
const DefaultFlushAfter = 50 * time.Millisecond

// MaxFlushAfter bounds the batching; past it the agent would notice.
const MaxFlushAfter = 500 * time.Millisecond

// EchoEnabled is Enabled with its default applied.
func (c ComposerConfig) EchoEnabled() bool { return c.Enabled == nil || *c.Enabled }

// EchoFlushAfter is FlushAfter parsed, with its default applied.
func (c ComposerConfig) EchoFlushAfter() time.Duration {
	if c.FlushAfter == "" {
		return DefaultFlushAfter
	}
	d, err := time.ParseDuration(c.FlushAfter)
	if err != nil {
		return DefaultFlushAfter
	}
	return d
}

// SSHAgentConfig is SSH agent forwarding: a client lends its SSH agent to
// the sessions it drives (docs/designs/ssh-agent-forwarding.md). The daemon
// reads SessionSocket; agt reads the rest.
type SSHAgentConfig struct {
	// SessionSocket gives each new session its own agent socket, which the
	// agent's SSH_AUTH_SOCK names: it answers from the agent of the client
	// that drives the session, else from the agent the daemon was started
	// with. Unset means true; false leaves SSH_AUTH_SOCK as the daemon
	// inherited it, unless that is another session's agent socket, which is
	// dropped.
	SessionSocket *bool `toml:"session_socket"`
	// Forward makes every attachment agt opens forward this machine's
	// agent: attach, new, the inbox, the overlay's switch and every
	// reattach. Off by default.
	Forward bool `toml:"forward"`
	// Agent is the socket of the agent forwarded; empty means
	// $SSH_AUTH_SOCK. A leading ~/ is the home directory.
	Agent string `toml:"agent"`
	// Keys are the SHA256 fingerprints of the keys offered (as `ssh-add -l`
	// prints them); empty offers every key the agent holds.
	Keys []string `toml:"keys"`
	// Problem says why the settings above cannot be trusted, "" when they
	// can (sshAgentProblem): agt then forwards nothing, and says why, and
	// the settings are their defaults when [ssh_agent] could not be read at
	// all. Load sets it rather than fail, so a mistake here, a value of the
	// wrong type too, neither stops agtd from starting nor turns off agt's
	// other settings.
	Problem string `toml:"-"`
	// forwardSet records that the file sets forward.
	forwardSet bool
}

// ForwardOff reports that the file turns forwarding off (forward = false),
// so a Problem does not keep agt from forwarding anything it would have.
func (c SSHAgentConfig) ForwardOff() bool { return c.forwardSet && !c.Forward }

// sshAgentSettings are the settings [ssh_agent] takes.
var sshAgentSettings = []string{"session_socket", "forward", "agent", "keys"}

// decodeSSHAgent decodes [ssh_agent] into c, setting c.Problem for a
// mistake in it.
func decodeSSHAgent(meta toml.MetaData, table toml.Primitive, c *SSHAgentConfig) {
	if err := meta.PrimitiveDecode(table, c); err != nil {
		*c = SSHAgentConfig{Problem: "[ssh_agent] could not be read (" + strings.TrimPrefix(err.Error(), "toml: ") + ")"}
		return
	}
	c.forwardSet = meta.IsDefined("ssh_agent", "forward")
	c.Problem = sshAgentProblem(meta, c.Keys)
}

// sshAgentProblem is why the [ssh_agent] settings cannot be trusted, or "":
// a table spelled like [ssh_agent] but not as it is (the decoder matches
// tables and settings whatever their case, so [SSH_Agent] would replace
// [ssh_agent]), a setting [ssh_agent] does not take, spelled as it does,
// `keys` in any case outside it, or a key that is not a fingerprint. A
// misspelled or misplaced `keys` would otherwise offer every key the agent
// holds; the other settings misplaced only leave forwarding as it was.
func sshAgentProblem(meta toml.MetaData, keys []string) string {
	fold := strings.NewReplacer("-", "", "_", "")
	for _, key := range meta.Keys() {
		switch table := key[0]; {
		case table != "ssh_agent" && fold.Replace(strings.ToLower(table)) == "sshagent":
			return fmt.Sprintf("SSH agent settings go under [ssh_agent], not [%s]", table)
		case table == "ssh_agent" && len(key) > 1 && !slices.Contains(sshAgentSettings, key[1]):
			return fmt.Sprintf("[ssh_agent] has no setting `%s` (it takes %s)", key[1], strings.Join(sshAgentSettings, ", "))
		}
	}
	for _, key := range meta.Undecoded() {
		if name := key[len(key)-1]; strings.EqualFold(name, "keys") && key[0] != "ssh_agent" {
			where := "at the top of the file"
			if len(key) > 1 {
				where = "under [" + strings.Join(key[:len(key)-1], ".") + "]"
			}
			return fmt.Sprintf("`%s` is %s: SSH agent settings go under [ssh_agent]", name, where)
		}
	}
	for _, fp := range keys {
		if !sshagent.ValidFingerprint(fp) {
			return fmt.Sprintf("[ssh_agent] keys: `%s` is not a key fingerprint (SHA256:..., as `ssh-add -l` prints it)", fp)
		}
	}
	return ""
}

// SessionSocketEnabled is SessionSocket with its default applied.
func (c SSHAgentConfig) SessionSocketEnabled() bool {
	return c.SessionSocket == nil || *c.SessionSocket
}

// AgentSocket is the socket of the agent to forward, or "" when there is
// none: Agent, else $SSH_AUTH_SOCK.
func (c SSHAgentConfig) AgentSocket() string {
	path := c.Agent
	if path == "" {
		return os.Getenv("SSH_AUTH_SOCK")
	}
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return path
}

type AgentConfig struct {
	Command string   `toml:"command"`
	Args    []string `toml:"args"`
	// ModelFlag is passed before the model name when a session requests a
	// model. Empty disables it; unset means DefaultModelFlag.
	ModelFlag *string `toml:"model_flag"`
	// LoginShell starts the agent through the user's login shell ($SHELL,
	// interactive), so it gets the environment of the user's own shells,
	// rc files included, read afresh for each session. Unset means true;
	// false starts the command directly with the daemon's environment.
	LoginShell *bool `toml:"login_shell"`
}

// LoginShellEnabled is LoginShell with its default applied.
func (a AgentConfig) LoginShellEnabled() bool { return a.LoginShell == nil || *a.LoginShell }

const DefaultModelFlag = "--model"

// preferredDefaultAgent is used when a config does not name default_agent
// and configures it (or configures no agents); otherwise the first
// configured agent is the default.
const preferredDefaultAgent = "claude"

// builtinAgents are the agents configured when there is no config.toml, in
// the agent chooser's order. Each runs the command of the same name.
var builtinAgents = []string{"claude", "codex", "pi"}

// detectDefaultAgent is the first built-in agent whose command is on PATH,
// so a machine with only one of them installed starts it without
// configuration. With none installed it is preferredDefaultAgent.
func detectDefaultAgent() string {
	for _, name := range builtinAgents {
		if _, err := exec.LookPath(name); err == nil {
			return name
		}
	}
	return preferredDefaultAgent
}

// DefaultText is the config.toml written when none exists. It says the same
// as the built-in defaults, so creating the file changes nothing, and it
// fixes the default agent detected when it was written.
func DefaultText() string {
	return fmt.Sprintf(defaultTextFormat, detectDefaultAgent())
}

const defaultTextFormat = `# The agent "agt new" starts without --agent: the first of claude, codex
# and pi that was installed when this file was written.
default_agent = %q

# Each agent is started through your login shell ($SHELL -l -i), so it sees
# what your profile and rc files set, as in an ssh login. login_shell = false
# in an agent's table starts its command directly with agtd's environment.
[agents.claude]
command = "claude"
args = []
model_flag = "--model"

[agents.codex]
command = "codex"
args = []
model_flag = "--model"

[agents.pi]
command = "pi"
args = []
model_flag = "--model"

# How much of a session's scrollback "agt attach" copies into the
# terminal: the most recent rows (-1 for all of it, 0 for the screen only).
# "agt history" has the rest.
[attach]
scrollback_rows = 1000
# How the screen reaches the terminal: "auto" (screen frames from a remote
# daemon, the raw output from the local one), "frames" or "raw".
mode = "auto"
# Under frames, keep the terminal on the primary screen while an agent runs
# on the alternate one (Claude Code, Codex), and push the rows that leave
# the top of its transcript into this terminal's own scrollback. A
# prototype: it applies to every program on the alternate screen.
alt_scrollback = false
# Under frames, the wheel over the transcript of an agent on the alternate
# screen scrolls it at once, and the agent's own screen corrects the guess
# when it arrives.
#predict_wheel = true
# Under frames, dragging the mouse over an agent that takes it (Claude
# Code, pi in fullscreen) draws its selection at once, and the agent's own
# screen corrects the guess when it arrives.
#predict_selection = true
# Offer Submit and switch in the Ctrl-Y palette, first (so Ctrl-Y, Enter):
# Enter goes to the agent and you move to the session that needs attention.
#submit_and_switch = false

# Typing in "agt attach" is drawn in the agent's own input field at once
# and sent as typed, batched for at most flush_after ("0" sends every key
# on its own). enabled = false sends keys as they are typed with no local
# drawing.
[composer]
enabled = true
flush_after = "50ms"

# SSH agent forwarding. Each new session gets its own SSH_AUTH_SOCK
# (session_socket), which answers from the agent of the client driving the
# session, else from the agent agtd was started with. forward = true lends
# this machine's agent (agent = "path", else $SSH_AUTH_SOCK) to every
# session "agt attach" drives; keys = ["SHA256:..."] limits the keys it
# offers to those listed.
[ssh_agent]
session_socket = true
forward = false
`

func defaultConfig() *Config {
	cfg := &Config{
		DefaultAgent: detectDefaultAgent(),
		Agents:       map[string]AgentConfig{},
		agentOrder:   slices.Clone(builtinAgents),
	}
	for _, name := range builtinAgents {
		cfg.Agents[name] = AgentConfig{Command: name}
	}
	return cfg
}

// Load reads path, falling back to the built-in defaults when the file does
// not exist.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return defaultConfig(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", path, err)
	}
	var f file
	meta, err := toml.Decode(string(data), &f)
	if err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", path, err)
	}
	cfg := &f.Config
	decodeSSHAgent(meta, f.SSHAgent, &cfg.SSHAgent)
	for _, key := range meta.Keys() {
		if len(key) == 2 && key[0] == "agents" {
			cfg.agentOrder = append(cfg.agentOrder, key[1])
		}
	}
	if cfg.DefaultAgent == "" {
		cfg.DefaultAgent = preferredDefaultAgent
		if _, ok := cfg.Agents[preferredDefaultAgent]; !ok && len(cfg.agentOrder) > 0 {
			cfg.DefaultAgent = cfg.agentOrder[0]
		}
	}
	switch cfg.Attach.Mode {
	case "", "auto", "frames", "raw":
	default:
		return nil, fmt.Errorf("[attach] mode must be \"auto\", \"frames\" or \"raw\" in %s", path)
	}
	if n := cfg.Attach.ScrollbackRows; n != nil && *n < -1 {
		return nil, fmt.Errorf("[attach] scrollback_rows must be -1 (all), 0 or more in %s", path)
	}
	if f := cfg.Composer.FlushAfter; f != "" {
		d, err := time.ParseDuration(f)
		if err != nil || d < 0 || d > MaxFlushAfter {
			return nil, fmt.Errorf("[composer] flush_after must be a duration between 0 and %s in %s", MaxFlushAfter, path)
		}
	}
	if len(cfg.Agents) > 0 {
		if _, ok := cfg.Agents[cfg.DefaultAgent]; !ok {
			return nil, fmt.Errorf("default_agent `%s` is not configured under [agents] in %s", cfg.DefaultAgent, path)
		}
	}
	return cfg, nil
}

// WriteDefault creates path with DefaultText, private to the user like
// everything under the runtime root. An existing file is left alone.
func WriteDefault(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to write %s: %w", path, err)
	}
	if _, err := f.WriteString(DefaultText()); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("failed to write %s: %w", path, err)
	}
	return f.Close()
}

// AgentNames lists the configured agents in file order.
func (c *Config) AgentNames() []string {
	return append([]string(nil), c.agentOrder...)
}

// RequireAgent returns the named agent, or an error naming the config file.
func (c *Config) RequireAgent(name, path string) (AgentConfig, error) {
	agent, ok := c.Agents[name]
	if !ok {
		return AgentConfig{}, fmt.Errorf("agent `%s` is not configured in %s", name, path)
	}
	return agent, nil
}

// Flag returns the flag that introduces a model name, or "" for none.
func (a AgentConfig) Flag() string {
	if a.ModelFlag == nil {
		return DefaultModelFlag
	}
	return *a.ModelFlag
}
