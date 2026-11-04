// Package config reads /etc/agentparley/config.yaml — the daemon's only configuration file. Nothing in this
// package touches state. Durable identity (credentials, machine id) lives under /var/lib/agentparley/,
// owned by internal/credstore. The session ledger and per-session shell state are deliberately NOT durable — they
// live under a tmpfs root (internal/sessions.stateRoot), so they are gone after a reboot, never on persistent disk.
package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultPath is where the packaged systemd unit points the daemon.
const DefaultPath = "/etc/agentparley/config.yaml"

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

	// UpdateServer and AutoUpdate govern the `update` subcommand's automatic path — systemd's ExecStartPre runs
	// `agentparley update --from-service` before every start. Unlike Enabled/RunAs, an absent value here IS a
	// safe default — updating should work out of the box.
	UpdateServer string `yaml:"update_server"`
	AutoUpdate   bool   `yaml:"auto_update"`

	// EnrolmentTokenFile is where a token handed to this box out of band (a rented computer's read-only disk) is
	// read to log in without a person — only when there is no usable saved login. Defaults to
	// DefaultEnrolmentTokenFile; AGENTPARLEY_ENROLMENT_TOKEN_FILE overrides it.
	EnrolmentTokenFile string `yaml:"enrolment_token_file"`

	// Harnesses configures per-harness connection details (URL/command) and model overrides, keyed by harness name
	// (internal/harness's Codex/Claude/Ollama/OpenAICompatibleLocal constants).
	Harnesses      map[string]HarnessConfig `yaml:"harnesses"`
	AllowHarnesses []string                 `yaml:"allow_harnesses"`
	DenyHarnesses  []string                 `yaml:"deny_harnesses"`

	Relay RelayConfig `yaml:"relay"`
}

// RelayConfig is what this box lets AgentParley pass a connection on to. Both lists are empty on every install until
// the box owner adds an entry (`sudo agentparley allow` / `program add`); Enabled=false stops relays too.
type RelayConfig struct {
	Allow         []string                `yaml:"allow"`
	Programs      map[string]RelayProgram `yaml:"programs"`
	MaxConcurrent int                     `yaml:"max_concurrent"` // 0 = 128
}

// RelayProgram is one named program a relay may start. Command[0] is an absolute path; it is exec'd directly, never
// through a shell. Env is merged over the base environment and may hold secrets, which is why config.yaml is 0640.
type RelayProgram struct {
	Command []string          `yaml:"command"`
	Env     map[string]string `yaml:"env"`
}

var (
	programNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	envKeyPattern      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// NormaliseAllowEntry validates a "host:port" allow entry and returns it in the canonical form the matcher compares:
// host lower-cased with IPv6 brackets removed, port without leading zeros.
func NormaliseAllowEntry(entry string) (host string, port int, err error) {
	host, portText, err := net.SplitHostPort(strings.TrimSpace(entry))
	if err != nil {
		return "", 0, fmt.Errorf("relay.allow entry %q must be host:port: %w", entry, err)
	}
	if host == "" {
		return "", 0, fmt.Errorf("relay.allow entry %q has no host", entry)
	}
	port, err = strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("relay.allow entry %q has an invalid port (1-65535)", entry)
	}
	return strings.ToLower(host), port, nil
}

// CanonicalAllowEntry is NormaliseAllowEntry rendered back as text, brackets restored for IPv6.
func CanonicalAllowEntry(entry string) (string, error) {
	host, port, err := NormaliseAllowEntry(entry)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

// ValidateProgramName enforces the shape of a relay.programs key.
func ValidateProgramName(name string) error {
	if !programNamePattern.MatchString(name) {
		return fmt.Errorf("relay program name %q must match %s", name, programNamePattern)
	}
	return nil
}

// ValidateRelayProgram checks one program's command and env.
func ValidateRelayProgram(name string, program RelayProgram) error {
	if err := ValidateProgramName(name); err != nil {
		return err
	}
	if len(program.Command) == 0 || program.Command[0] == "" {
		return fmt.Errorf("relay.programs.%s.command is empty", name)
	}
	if !filepath.IsAbs(program.Command[0]) {
		return fmt.Errorf("relay.programs.%s.command[0] %q must be an absolute path", name, program.Command[0])
	}
	for key := range program.Env {
		if !envKeyPattern.MatchString(key) {
			return fmt.Errorf("relay.programs.%s.env key %q is not a valid environment variable name", name, key)
		}
	}
	return nil
}

// Validate checks the whole relay block; the error names the offending entry.
func (relay RelayConfig) Validate() error {
	for _, entry := range relay.Allow {
		if _, _, err := NormaliseAllowEntry(entry); err != nil {
			return err
		}
	}
	for name, program := range relay.Programs {
		if err := ValidateRelayProgram(name, program); err != nil {
			return err
		}
	}
	if relay.MaxConcurrent < 0 {
		return fmt.Errorf("relay.max_concurrent cannot be negative")
	}
	return nil
}

// HarnessConfig is one harness's local connection details. URL is used by the server family (ollama defaults to
// http://localhost:11434; openai-compatible-local has no default and is REQUIRED). The CLI family has no command
// key: the daemon runs its own pinned copy of Codex and Claude Code. MaxWarmSessions and WarmIdleMinutes size the CLI
// family's warm sessions (zero = the built-in default: 8 for claude, 32 for codex, 30 minutes idle). Models is the
// only way to supply a context window the box can't discover on its own — for the server family it OVERRIDES fields on a model the server already reported
// (an entry for an id the server didn't list is silently ignored, see mergeServerModelOverrides); for the CLI
// family it REPLACES the built-in model list wholesale (see managed.go).
type HarnessConfig struct {
	URL    string               `yaml:"url"`
	Models []HarnessModelConfig `yaml:"models"`

	MaxWarmSessions int `yaml:"max_warm_sessions"`
	WarmIdleMinutes int `yaml:"warm_idle_minutes"`
}

// HarnessModelConfig overrides or augments one model entry. ContextWindowTokens is the fail-closed escape hatch:
// when a server doesn't expose its context window (a bare OpenAI-compatible server with no max_model_len, or an
// Ollama /api/show without a context length), this is the only way to make that model registrable.
type HarnessModelConfig struct {
	ID                  string `yaml:"id"`
	Label               string `yaml:"label"`
	ContextWindowTokens int    `yaml:"context_window_tokens"`
}

// DefaultEnrolmentTokenFile is the token file location when config.yaml does not name one.
const DefaultEnrolmentTokenFile = "/run/agentparley/enrolment-token"

// DefaultUpdateServer is where install.sh points `update` when the customer didn't override it, and what `doctor`
// checks against when config.yaml can't be loaded at all.
const DefaultUpdateServer = "https://get.agentparley.ai"

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
		UpdateServer: DefaultUpdateServer,

		EnrolmentTokenFile: DefaultEnrolmentTokenFile,
	}
	if err := yaml.Unmarshal(data, &tunnelConfig); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	// AGENTPARLEY_HOST points a dev build at a local PlatformApi (e.g. https://192.168.0.3:4243) without
	// rewriting the installed config; it overrides only the API base URL — egress and update are separate hosts.
	if host := os.Getenv("AGENTPARLEY_HOST"); host != "" {
		tunnelConfig.Server.APIBaseURL = host
	}

	// The same names install.sh reads, so one image can be pointed at any environment from the unit's environment
	// without rewriting config.yaml. AGENTPARLEY_HOST (above) wins over AGENTPARLEY_API_SERVER.
	if apiServer := os.Getenv("AGENTPARLEY_API_SERVER"); apiServer != "" && os.Getenv("AGENTPARLEY_HOST") == "" {
		tunnelConfig.Server.APIBaseURL = apiServer
	}
	if egressServer := os.Getenv("AGENTPARLEY_EGRESS_SERVER"); egressServer != "" {
		tunnelConfig.Server.EgressAddress = egressServer
	}
	if tokenFile := os.Getenv("AGENTPARLEY_ENROLMENT_TOKEN_FILE"); tokenFile != "" {
		tunnelConfig.EnrolmentTokenFile = tokenFile
	}
	for harnessName, harnessConfig := range tunnelConfig.Harnesses {
		if harnessConfig.MaxWarmSessions < 0 || harnessConfig.WarmIdleMinutes < 0 {
			return nil, fmt.Errorf("parsing config %s: harnesses.%s max_warm_sessions and warm_idle_minutes cannot be negative", path, harnessName)
		}
	}
	if err := tunnelConfig.Relay.Validate(); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}
	return &tunnelConfig, nil
}
