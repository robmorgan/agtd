// Package config reads config.toml, which the daemon and the agt CLI
// share. Unknown keys and tables are ignored.
package config

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	DefaultAgent string                 `toml:"default_agent"`
	Agents       map[string]AgentConfig `toml:"agents"`
	Remote       RemoteConfig           `toml:"remote"`
	Attach       AttachConfig           `toml:"attach"`
	Composer     ComposerConfig         `toml:"composer"`

	// agentOrder lists the Agents keys in file order, which a map loses.
	agentOrder []string
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
	// SubmitAndSwitch offers Submit and switch first in the Ctrl-Y
	// palette (so Ctrl-Y, Enter is it). Hidden unless set.
	SubmitAndSwitch bool `toml:"submit_and_switch"`
}

// WheelPredicted is PredictWheel with its default applied.
func (a AttachConfig) WheelPredicted() bool { return a.PredictWheel == nil || *a.PredictWheel }

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

type AgentConfig struct {
	Command string   `toml:"command"`
	Args    []string `toml:"args"`
	// ModelFlag is passed before the model name when a session requests a
	// model. Empty disables it; unset means DefaultModelFlag.
	ModelFlag *string `toml:"model_flag"`
}

const DefaultModelFlag = "--model"

// preferredDefaultAgent is used when a config does not name default_agent
// and configures it (or configures no agents); otherwise the first
// configured agent is the default.
const preferredDefaultAgent = "claude"

// builtinAgents are the agents configured when there is no config.toml, in
// picker order. Each runs the command of the same name.
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
	cfg := &Config{}
	meta, err := toml.Decode(string(data), cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", path, err)
	}
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
