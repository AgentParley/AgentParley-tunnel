package harness

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/agentparley/tunnel/internal/config"
)

// claudeMinVersion is the lowest Claude Code CLI version this daemon trusts to leave NO tool source when pinned
// with claudeArgs below. 2.1.207 is the OLDEST version verified by hand to carry --safe-mode and
// --strict-mcp-config; the floor started at 2.1.224 purely because that was the dev machine's version, and it
// wrongly refused a real 2.1.207 install. The capability probe below is the gate that proves every pinned flag
// works on the installed CLI — this floor only keeps out releases too old to have been looked at.
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

type claudeHarness struct {
	command      string
	path         string
	modelConfigs []config.HarnessModelConfig
}

func newClaudeHarness(harnessConfig config.HarnessConfig, discovered discoveredHarness) *claudeHarness {
	command := harnessConfig.Command
	if command == "" {
		command = discovered.Command
	}
	if command == "" {
		command = "claude"
	}
	return &claudeHarness{command: command, path: discovered.Path, modelConfigs: harnessConfig.Models}
}

func (h *claudeHarness) Detect(ctx context.Context) error {
	resolvedPath, err := exec.LookPath(h.command)
	if err != nil {
		if filepath.IsAbs(h.command) {
			return fmt.Errorf("%q no longer exists — it may have been moved or uninstalled; re-run 'agentparley register' to rediscover claude: %w", h.command, err)
		}
		return fmt.Errorf("claude CLI (%q) not found on PATH: %w", h.command, err)
	}

	versionCtx, cancel := context.WithTimeout(ctx, capabilityProbeTimeout)
	defer cancel()
	versionCmd := exec.CommandContext(versionCtx, resolvedPath, "--version")
	versionCmd.Env = envWithPath(h.path)
	versionOutput, err := versionCmd.Output()
	if err != nil {
		return fmt.Errorf("running %q --version: %w", h.command, err)
	}

	isVersionSupported, err := meetsFloor(string(versionOutput), claudeMinMajor, claudeMinMinor, claudeMinPatch)
	if err != nil {
		return fmt.Errorf("could not parse claude CLI version from %q: %w", strings.TrimSpace(string(versionOutput)), err)
	}
	if !isVersionSupported {
		return fmt.Errorf("claude CLI version %q is older than the minimum %d.%d.%d required to prove the non-agentic pin (--tools, --permission-mode dontAsk, --setting-sources, --strict-mcp-config) — upgrade the CLI before registering this harness",
			strings.TrimSpace(string(versionOutput)), claudeMinMajor, claudeMinMinor, claudeMinPatch)
	}

	return runCapabilityProbe(ctx, resolvedPath, claudeArgs(claudeDefaultModels[0].ID, "", ""), claudeProbePrompt, h.path, parseClaudeJSON)
}

func (h *claudeHarness) ListModels(ctx context.Context) ([]Model, error) {
	return resolveCLIModels(claudeDefaultModels, h.modelConfigs, Claude)
}

func (h *claudeHarness) Invoke(ctx context.Context, model, payload string) (InvokeOutcome, error) {
	decoded, err := parseClaudeCodexPayload(payload)
	if err != nil {
		return InvokeOutcome{}, err
	}

	systemPromptFile := ""
	if decoded.SystemPrompt != "" {
		promptDir, err := os.MkdirTemp("", "agentparley-harness-prompt-*")
		if err != nil {
			return InvokeOutcome{}, fmt.Errorf("creating a directory for the claude system prompt: %w", err)
		}
		defer os.RemoveAll(promptDir)

		systemPromptFile = filepath.Join(promptDir, "system-prompt.txt")
		if err := os.WriteFile(systemPromptFile, []byte(decoded.SystemPrompt), 0o600); err != nil {
			return InvokeOutcome{}, fmt.Errorf("writing the claude system prompt file: %w", err)
		}
	}

	return runCLI(ctx, h.command, claudeArgs(model, systemPromptFile, decoded.ReasoningEffort), decoded.Prompt, h.path)
}

// claudeArgs is the FIXED, daemon-owned argv for every claude invocation (Detect's probe and Invoke alike) —
// never built from wire input. --tools "" removes every built-in tool (an allowlist of none, so tools added in
// later CLI releases stay off — a name denylist goes stale); --permission-mode dontAsk denies anything that would
// still prompt, whatever the box owner's own default mode is; --setting-sources "" stops the owner's user/project/
// local settings (a bypassPermissions default, hooks, CLAUDE.md) from applying; --strict-mcp-config with no
// --mcp-config leaves no MCP servers; --safe-mode additionally switches off plugins, skills and hooks.
// systemPromptFile, when set, is read by the CLI so the prompt never appears in the process list.
//
// NEVER add --bare, despite Anthropic's docs recommending it for scripted use: it disables OAuth and keychain
// reads, so the subscription login this harness exists to spend stops working and the CLI demands an API key —
// and a user with an API key needs no tunnel at all. Those docs also say --bare will become the -p default in a
// future release, at which point Detect's probe starts failing "not signed in"; that is the fail-closed signal to
// pin the old behaviour explicitly, not to adopt --bare.
func claudeArgs(model, systemPromptFile, reasoningEffort string) []string {
	args := []string{
		"-p", "--output-format", "json",
		"--model", model,
		"--safe-mode",
		"--tools", "",
		"--permission-mode", "dontAsk",
		"--setting-sources", "",
		"--strict-mcp-config",
		"--no-session-persistence",
	}
	if systemPromptFile != "" {
		args = append(args, "--append-system-prompt-file", systemPromptFile)
	}
	if reasoningEffort != "" {
		args = append(args, "--effort", reasoningEffort)
	}
	return args
}
