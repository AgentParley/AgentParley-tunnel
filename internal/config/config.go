// Package config reads /etc/agentparley-tunnel/config.yaml — the daemon's only configuration file. Nothing in this
// package touches state. Durable identity (credentials, machine id) lives under /var/lib/agentparley-tunnel/,
// owned by internal/credstore. The session ledger and per-session shell state are deliberately NOT durable — they
// live under a tmpfs root (internal/sessions.stateRoot), so they are gone after a reboot, never on persistent disk.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// DefaultPath is where the packaged systemd unit points the daemon.
const DefaultPath = "/etc/agentparley-tunnel/config.yaml"

// Server names the two control-plane hosts the daemon talks to: the PlatformApi for login/enrol/refresh, and the
// SSH egress service for the long-lived Connect stream. They are deliberately separate — the design doc keeps the
// egress rarely-deployed and separately scaled from the PlatformApi.
type Server struct {
	APIBaseURL    string `yaml:"api"`
	EgressAddress string `yaml:"egress"`
}

// Config is the daemon's policy: who commands run as, and what they're allowed to do. Loaded fresh on every
// (re)connect attempt, so an operator can tighten it without a restart... except the daemon only reloads it at
// process start today; see main.go's start command.
type Config struct {
	Server        Server   `yaml:"server"`
	RunAs         string   `yaml:"run_as"`
	ReadOnly      bool     `yaml:"read_only"`
	AllowCommands []string `yaml:"allow_commands"`
	DenyCommands  []string `yaml:"deny_commands"`
	Enabled       bool     `yaml:"enabled"`

	// MaxConcurrentOperations bounds how many operations run at once on this box. Zero/unset falls back to a safe
	// built-in default (client.defaultMaxConcurrentOperations) — unlike Enabled/RunAs, an unset value here is a
	// reasonable default, not a misconfiguration.
	MaxConcurrentOperations int `yaml:"max_concurrent_operations"`

	// UpdateServer and AutoUpdate govern the `self-update` subcommand run by systemd's ExecStartPre. Unlike
	// Enabled/RunAs, an absent value here IS a safe default — self-update should work out of the box.
	UpdateServer string `yaml:"update_server"`
	AutoUpdate   bool   `yaml:"auto_update"`

	// Harnesses configures per-harness connection details (URL/command) and model overrides, keyed by harness name
	// (internal/harness's Codex/Claude/Ollama/OpenAICompatibleLocal constants).
	Harnesses      map[string]HarnessConfig `yaml:"harnesses"`
	AllowHarnesses []string                 `yaml:"allow_harnesses"`
	DenyHarnesses  []string                 `yaml:"deny_harnesses"`
}

// HarnessConfig is one harness's local connection details. URL is used by the server family (ollama defaults to
// http://localhost:11434; openai-compatible-local has no default and is REQUIRED). Command is used by the CLI
// family (defaults to "codex"/"claude", resolved via PATH). Models is the only way to supply a context window the
// box can't discover on its own — for the server family it OVERRIDES fields on a model the server already reported
// (an entry for an id the server didn't list is silently ignored, see mergeServerModelOverrides); for the CLI
// family it REPLACES the built-in model list wholesale (see clihelpers.go).
type HarnessConfig struct {
	URL     string               `yaml:"url"`
	Command string               `yaml:"command"`
	Models  []HarnessModelConfig `yaml:"models"`
}

// HarnessModelConfig overrides or augments one model entry. ContextWindowTokens is the fail-closed escape hatch:
// when a server doesn't expose its context window (a bare OpenAI-compatible server with no max_model_len, or an
// Ollama /api/show without a context length), this is the only way to make that model registrable.
type HarnessModelConfig struct {
	ID                  string `yaml:"id"`
	Label               string `yaml:"label"`
	ContextWindowTokens int    `yaml:"context_window_tokens"`
}

// defaultUpdateServer is where install.sh points self-update when the customer didn't override it.
const defaultUpdateServer = "https://tunnel-app.agentparley.ai"

// Load reads and parses the config file at path. There is no built-in default for Enabled/RunAs — an operator-less
// config is a misconfiguration, not a safe default, so a missing or malformed file is a hard error rather than a
// silent "commands allowed as root". UpdateServer/AutoUpdate DO get built-in defaults, pre-filled before unmarshal
// so an absent key keeps the default and an explicit `auto_update: false` still wins.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	tunnelConfig := Config{
		AutoUpdate:   true,
		UpdateServer: defaultUpdateServer,
	}
	if err := yaml.Unmarshal(data, &tunnelConfig); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	// AGENTPARLEY_HOST points a dev build at a local PlatformApi (e.g. https://192.168.0.3:4243) without
	// rewriting the installed config; it overrides only the API base URL — egress and update are separate hosts.
	if host := os.Getenv("AGENTPARLEY_HOST"); host != "" {
		tunnelConfig.Server.APIBaseURL = host
	}
	return &tunnelConfig, nil
}
