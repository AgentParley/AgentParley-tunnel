package harness

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/agentparley/tunnel/internal/config"
)

// codexMinMajor/Minor/Patch is the lowest codex CLI version this daemon trusts to leave NO tool source when pinned
// with codexArgs below. Pinned at 0.146.1 — the current published @openai/codex release as of 2026-08-07 — because
// that is the version whose documented flag contract was verified from OpenAI's own docs: --sandbox read-only,
// --ask-for-approval never, and -c key=value overrides parsing their value as JSON with the HIGHEST-precedence
// ordering (CLI/-c beats profile, project .codex/config.toml, user config, and system config), which is what lets
// -c mcp_servers={} actually empty the box's locally configured MCP server table rather than merely being ignored.
// This is deliberately conservative, not a mechanically-derived floor: refusing a possibly-fine older install beats
// running an unpinned one. Re-verify this number (and the flag contract above) against OpenAI's current docs before
// ever bumping it — do not treat "the probe below passed" as sufficient evidence on its own.
const (
	codexMinMajor = 0
	codexMinMinor = 146
	codexMinPatch = 1
)

// codexProbePrompt is the trivial throwaway prompt Detect sends through the real pinned argv to empirically prove
// the installed codex CLI accepts every pinned flag AND is signed in — the version floor above proves the flags
// exist in this release (verified against OpenAI's docs), but not that this particular install is actually logged
// in, which registration should confirm before advertising its models to the console.
const codexProbePrompt = "reply with: ok"

var codexDefaultModels = []Model{
	{ID: "gpt-5-codex", Label: "GPT-5 Codex (via codex CLI)", ContextWindowTokens: 400_000},
	{ID: "gpt-5", Label: "GPT-5 (via codex CLI)", ContextWindowTokens: 400_000},
}

type codexHarness struct {
	command      string
	modelConfigs []config.HarnessModelConfig
}

func newCodexHarness(harnessConfig config.HarnessConfig) *codexHarness {
	command := harnessConfig.Command
	if command == "" {
		command = "codex"
	}
	return &codexHarness{command: command, modelConfigs: harnessConfig.Models}
}

// Detect runs the exact pinned Invoke argv (see codexArgs) against codexProbePrompt and requires it to succeed:
// the process must run, every flag must be accepted (an unknown-flag/parse error is a refusal), it must exit 0,
// and the `--json` output must parse as the JSON-lines event stream codex documents. Anything else refuses
// registration — fail closed, same as claude's floor, but proven empirically rather than by version number.
func (h *codexHarness) Detect(ctx context.Context) error {
	resolvedPath, err := exec.LookPath(h.command)
	if err != nil {
		return fmt.Errorf("codex CLI (%q) not found on PATH: %w", h.command, err)
	}

	versionCtx, cancel := context.WithTimeout(ctx, capabilityProbeTimeout)
	defer cancel()
	versionOutput, err := exec.CommandContext(versionCtx, resolvedPath, "--version").Output()
	if err != nil {
		return fmt.Errorf("running %q --version: %w", h.command, err)
	}

	isVersionSupported, err := meetsFloor(string(versionOutput), codexMinMajor, codexMinMinor, codexMinPatch)
	if err != nil {
		return fmt.Errorf("could not parse codex CLI version from %q: %w", strings.TrimSpace(string(versionOutput)), err)
	}
	if !isVersionSupported {
		return fmt.Errorf("codex CLI version %q is older than the minimum %d.%d.%d required to prove the non-agentic pin (--sandbox read-only --ask-for-approval never -c mcp_servers={}) — upgrade the CLI before registering this harness",
			strings.TrimSpace(string(versionOutput)), codexMinMajor, codexMinMinor, codexMinPatch)
	}

	args := codexArgs(codexDefaultModels[0].ID)
	return runCapabilityProbe(ctx, resolvedPath, args, codexProbePrompt, parseCodexJSONLines)
}

func (h *codexHarness) ListModels(ctx context.Context) ([]Model, error) {
	return resolveCLIModels(codexDefaultModels, h.modelConfigs, Codex)
}

func (h *codexHarness) Invoke(ctx context.Context, model, payload string) (InvokeOutcome, error) {
	prompt, _, err := parseClaudeCodexPayload(payload)
	if err != nil {
		return InvokeOutcome{}, err
	}

	return runCLI(ctx, h.command, codexArgs(model), prompt)
}

// codexArgs is the FIXED, daemon-owned argv for every codex invocation (Detect's probe and Invoke alike) — never
// built from wire input. --sandbox read-only + --ask-for-approval never block writes/shell/network side effects;
// -c mcp_servers={} is a highest-precedence inline override (documented: CLI/-c overrides beat profile, project
// .codex/config.toml, user config, and system config) that empties the box's locally configured MCP server table
// regardless of what the box's own config says. Never add --dangerously-bypass-approvals-and-sandbox: it disables
// the read-only sandbox AND is codex's only documented way to auto-approve MCP tool calls non-interactively
// (`codex exec` closes stdin, so the approval prompt reads EOF and auto-declines otherwise) — passing it would
// silently undo this entire pin.
func codexArgs(model string) []string {
	return []string{
		"exec", "--json",
		"-m", model,
		"--sandbox", "read-only",
		"--ask-for-approval", "never",
		"-c", "mcp_servers={}",
	}
}
