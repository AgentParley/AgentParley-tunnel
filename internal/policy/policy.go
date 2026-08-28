// Package policy is the local owner's levers over what this box lets AgentParley do — checked before every
// operation, never after. It has no knowledge of accounts, sessions, or the wire protocol; it answers exactly one
// question per call: is this operation allowed right now.
package policy

import (
	"errors"
	"regexp"
	"strings"

	"github.com/agentparley/tunnel/internal/config"
)

// ErrDenied is returned for every policy refusal. The caller maps it to the wire's "policy_denied" code — the
// same string for every reason (disabled, read-only, command list), matching the plan's single denial code.
var ErrDenied = errors.New("policy_denied")

// Policy evaluates a live config.Config on every check, so `agentparley-tunnel start` picking up a fresh config
// only requires a config reload, not a policy rebuild.
type Policy struct {
	tunnelConfig *config.Config
}

func New(tunnelConfig *config.Config) *Policy {
	return &Policy{tunnelConfig: tunnelConfig}
}

// CheckRunCommand applies enabled, then deny-beats-allow command matching against policyMatchCommand — the RAW,
// UNWRAPPED command text the agent asked for (RunCommandCall.PolicyMatchCommand), never the base64-wrapped shell
// invocation the daemon actually executes. An empty AllowCommands list means every command is allowed (the same
// empty-means-all convention as the agent's SSH host allowlist).
func (policy *Policy) CheckRunCommand(policyMatchCommand string) error {
	if !policy.tunnelConfig.Enabled {
		return ErrDenied
	}
	if matchesAny(policy.tunnelConfig.DenyCommands, policyMatchCommand) {
		return ErrDenied
	}
	if len(policy.tunnelConfig.AllowCommands) > 0 && !matchesAny(policy.tunnelConfig.AllowCommands, policyMatchCommand) {
		return ErrDenied
	}
	return nil
}

func (policy *Policy) CheckRead() error {
	if !policy.tunnelConfig.Enabled {
		return ErrDenied
	}
	return nil
}

// CheckWrite covers both write_file and delete_file — read_only denies both, since a delete is a write to the
// directory entry.
func (policy *Policy) CheckWrite() error {
	if !policy.tunnelConfig.Enabled {
		return ErrDenied
	}
	if policy.tunnelConfig.ReadOnly {
		return ErrDenied
	}
	return nil
}

// CheckHarness applies enabled, then deny-beats-allow harness-name matching, the same idiom as CheckRunCommand. An
// empty AllowHarnesses list means every harness is allowed.
func (policy *Policy) CheckHarness(harness string) error {
	if !policy.tunnelConfig.Enabled {
		return ErrDenied
	}
	if matchesAny(policy.tunnelConfig.DenyHarnesses, harness) {
		return ErrDenied
	}
	if len(policy.tunnelConfig.AllowHarnesses) > 0 && !matchesAny(policy.tunnelConfig.AllowHarnesses, harness) {
		return ErrDenied
	}
	return nil
}

// matchesAny reports whether command matches any of patterns. A pattern is a glob over the FULL command string
// where "*" matches any run of characters, INCLUDING "/" — a command line has no path-segment structure to
// respect the way a filesystem path does, so `path.Match`'s segment-bounded "*" is the wrong tool here (it would
// never let "rm -rf /*" match "rm -rf /etc"). Everything else in the pattern is matched literally.
func matchesAny(patterns []string, command string) bool {
	for _, pattern := range patterns {
		if globToRegexp(pattern).MatchString(command) {
			return true
		}
	}
	return false
}

func globToRegexp(pattern string) *regexp.Regexp {
	parts := strings.Split(pattern, "*")
	for i, part := range parts {
		parts[i] = regexp.QuoteMeta(part)
	}
	return regexp.MustCompile("^" + strings.Join(parts, ".*") + "$")
}
