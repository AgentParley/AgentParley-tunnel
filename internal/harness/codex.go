package harness

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/agentparley/tunnel/internal/config"
)

// codexMinMajor/Minor/Patch is a fast pre-check, not the real gate — the empirical probe in Detect (which runs the
// exact pinned argv) is what actually proves this install accepts every pinned flag and is signed in, so anything
// where the pin doesn't work is refused regardless of version. Floor kept at 0.146.1 as a conservative floor; the
// pinned flag contract below was verified EMPIRICALLY against codex-cli 0.150.1 (a completion ran, and a prompted
// write was refused with "this session has read-only filesystem access" — no file created). The flags changed from
// earlier codex: `--ask-for-approval` was removed (exec is non-interactive, so approvals auto-decline on EOF stdin),
// `--skip-git-repo-check` is now required to run outside a git repo, and a ChatGPT-account codex rejects an explicit
// `-m`. `-c key=value` still overrides at highest precedence, which is what lets `-c mcp_servers={}` empty the box's
// MCP table. Re-verify the flag contract against a real codex before bumping the floor.
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

// One entry, no per-model IDs: codex authed with a ChatGPT account rejects an explicit `-m gpt-5`/`gpt-5-codex`
// ("not supported when using Codex with a ChatGPT account"), so the pinned argv passes NO -m and codex uses the
// model its account is entitled to. The console shows this single logical provider; the actual model is codex's
// own default. Context window is held conservative — overstating is the failure that bricks a session (see
// HarnessModelConfig.ContextWindowTokens); a box that needs the true window sets it in config.
var codexDefaultModels = []Model{
	{ID: "codex", Label: "Codex (ChatGPT account default)", ContextWindowTokens: 256_000},
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
		return fmt.Errorf("codex CLI version %q is older than the minimum %d.%d.%d required to prove the non-agentic pin (--sandbox read-only --skip-git-repo-check -c mcp_servers={}) — upgrade the CLI before registering this harness",
			strings.TrimSpace(string(versionOutput)), codexMinMajor, codexMinMinor, codexMinPatch)
	}

	args := codexArgs()
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

	return runCLI(ctx, h.command, codexArgs(), prompt)
}

// codexArgs is the FIXED, daemon-owned argv for every codex invocation (Detect's probe and Invoke alike) — never
// built from wire input. --sandbox read-only blocks writes/shell/network side effects (codex exec is non-interactive,
// so any approval prompt reads EOF and auto-declines — no --ask-for-approval flag exists in codex 0.150+);
// --skip-git-repo-check lets exec run outside a git repo (the box's home dir); NO -m is passed, because a
// ChatGPT-account codex rejects a named model and uses its own default;
// -c mcp_servers={} is a highest-precedence inline override (documented: CLI/-c overrides beat profile, project
// .codex/config.toml, user config, and system config) that empties the box's locally configured MCP server table
// regardless of what the box's own config says. Never add --dangerously-bypass-approvals-and-sandbox: it disables
// the read-only sandbox AND is codex's only documented way to auto-approve MCP tool calls non-interactively
// (`codex exec` closes stdin, so the approval prompt reads EOF and auto-declines otherwise) — passing it would
// silently undo this entire pin.
func codexArgs() []string {
	return []string{
		"exec", "--json",
		"--skip-git-repo-check",
		"--sandbox", "read-only",
		"-c", "mcp_servers={}",
	}
}
