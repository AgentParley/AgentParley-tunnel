package harness

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/agentparley/tunnel/internal/config"
)

// claudeMinVersion is the lowest Claude Code CLI version this daemon trusts to leave NO tool source when pinned
// with claudeNonAgenticArgs below. 2.1.207 is the OLDEST version verified by hand to carry all three flags; the
// floor started at 2.1.224 purely because that was the dev machine's version, and it wrongly refused a real 2.1.207
// install. Raise it only against a version whose flags were actually checked — the capability probe below is the
// gate that proves the flags work, this floor only keeps out releases too old to have been looked at.
// Confirmed by hand against `claude --help` on 2.1.224 and 2.1.207: --safe-mode disables
// CLAUDE.md/skills/plugins/hooks/MCP servers/custom commands/output styles wholesale (the single strongest lever —
// it is what kills the MCP tool source, which a built-in denylist alone cannot enumerate); --strict-mcp-config
// additionally restricts MCP servers to --mcp-config (which we never pass, so the effective set is empty even if a
// later release narrows --safe-mode's scope); --disallowedTools then closes the enumerable built-ins. Belt and
// braces on purpose — each flag alone would be a single point of failure across CLI versions.
const (
	claudeMinMajor = 2
	claudeMinMinor = 1
	claudeMinPatch = 207
)

// claudeProbePrompt is the trivial throwaway prompt Detect sends through the real pinned argv (see claudeArgs) to
// empirically prove the installed claude CLI accepts every pinned flag AND is signed in — the version floor above
// proves the flags exist in this release (hand-checked against 2.1.224's --help), but not that this particular
// install is actually logged in, which registration should confirm before advertising its models to the console.
const claudeProbePrompt = "reply with: ok"

var claudeDefaultModels = []Model{
	{ID: "opus", Label: "Claude Opus (via claude CLI)", ContextWindowTokens: 200_000},
	{ID: "sonnet", Label: "Claude Sonnet (via claude CLI)", ContextWindowTokens: 200_000},
	{ID: "haiku", Label: "Claude Haiku (via claude CLI)", ContextWindowTokens: 200_000},
}

// claudeBuiltInTools is the enumerable built-in tool set --disallowedTools closes. It is the second layer of the
// pin, not the first — --safe-mode/--strict-mcp-config are what actually remove the MCP tool source; this list
// only covers what a name-based denylist CAN cover.
var claudeBuiltInTools = []string{
	"Bash", "Edit", "MultiEdit", "Write", "NotebookEdit", "WebFetch", "WebSearch",
	"Read", "Glob", "Grep", "LS", "Task", "TodoWrite", "ExitPlanMode", "BashOutput", "KillShell",
}

type claudeHarness struct {
	command      string
	modelConfigs []config.HarnessModelConfig
}

func newClaudeHarness(harnessConfig config.HarnessConfig) *claudeHarness {
	command := harnessConfig.Command
	if command == "" {
		command = "claude"
	}
	return &claudeHarness{command: command, modelConfigs: harnessConfig.Models}
}

func (h *claudeHarness) Detect(ctx context.Context) error {
	resolvedPath, err := exec.LookPath(h.command)
	if err != nil {
		return fmt.Errorf("claude CLI (%q) not found on PATH: %w", h.command, err)
	}

	versionCtx, cancel := context.WithTimeout(ctx, capabilityProbeTimeout)
	defer cancel()
	versionOutput, err := exec.CommandContext(versionCtx, resolvedPath, "--version").Output()
	if err != nil {
		return fmt.Errorf("running %q --version: %w", h.command, err)
	}

	isVersionSupported, err := meetsFloor(string(versionOutput), claudeMinMajor, claudeMinMinor, claudeMinPatch)
	if err != nil {
		return fmt.Errorf("could not parse claude CLI version from %q: %w", strings.TrimSpace(string(versionOutput)), err)
	}
	if !isVersionSupported {
		return fmt.Errorf("claude CLI version %q is older than the minimum %d.%d.%d required to prove the non-agentic pin (--safe-mode --strict-mcp-config --disallowedTools) — upgrade the CLI before registering this harness",
			strings.TrimSpace(string(versionOutput)), claudeMinMajor, claudeMinMinor, claudeMinPatch)
	}

	return runCapabilityProbe(ctx, resolvedPath, claudeArgs(claudeDefaultModels[0].ID, ""), claudeProbePrompt, parseClaudeJSON)
}

func (h *claudeHarness) ListModels(ctx context.Context) ([]Model, error) {
	return resolveCLIModels(claudeDefaultModels, h.modelConfigs, Claude)
}

func (h *claudeHarness) Invoke(ctx context.Context, model, payload string) (InvokeOutcome, error) {
	prompt, systemPrompt, _, err := parseClaudeCodexPayload(payload)
	if err != nil {
		return InvokeOutcome{}, err
	}

	return runCLI(ctx, h.command, claudeArgs(model, systemPrompt), prompt)
}

// claudeArgs is the FIXED, daemon-owned argv for every claude invocation (Detect's probe and Invoke alike) —
// never built from wire input. --safe-mode disables CLAUDE.md/skills/plugins/hooks/MCP servers/custom
// commands/output styles wholesale (the lever that kills the MCP tool source); --strict-mcp-config additionally
// restricts MCP servers to --mcp-config, which we never pass; --disallowedTools then closes the enumerable
// built-ins as a second layer.
//
// NEVER add --bare, despite Anthropic's docs recommending it for scripted use: it disables OAuth and keychain
// reads, so the subscription login this harness exists to spend stops working and the CLI demands an API key —
// and a user with an API key needs no tunnel at all. Those docs also say --bare will become the -p default in a
// future release, at which point Detect's probe starts failing "not signed in"; that is the fail-closed signal to
// pin the old behaviour explicitly, not to adopt --bare.
func claudeArgs(model, systemPrompt string) []string {
	args := []string{
		"-p", "--output-format", "json",
		"--model", model,
		"--safe-mode",
		"--strict-mcp-config",
		"--disallowedTools", strings.Join(claudeBuiltInTools, ","),
	}
	if systemPrompt != "" {
		args = append(args, "--append-system-prompt", systemPrompt)
	}
	return args
}
