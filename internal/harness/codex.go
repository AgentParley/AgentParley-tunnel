package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/agentparley/tunnel/internal/config"
)

// codexMinMajor/Minor/Patch is a fast pre-check, not the real gate — the empirical probe in Detect (which runs the
// exact pinned argv) is what actually proves this install accepts every pinned flag and is signed in, so anything
// where the pin doesn't work is refused regardless of version. Floor kept at 0.146.1 as a conservative floor; the
// pinned flag contract below was verified EMPIRICALLY against codex-cli 0.150.1 (a completion ran, and a prompted
// write was refused with "this session has read-only filesystem access" — no file created). The flags changed from
// earlier codex: `--ask-for-approval` was removed (exec is non-interactive, so approvals auto-decline on EOF stdin),
// and `--skip-git-repo-check` is now required to run outside a git repo. `-c key=value` still overrides at highest
// precedence, which is what lets `-c mcp_servers={}` empty the box's MCP table. An earlier version of this comment
// claimed a ChatGPT-account codex rejects an explicit `-m` — re-verified against codex-cli 0.153.4 with a ChatGPT
// login: `-m gpt-5.6-sol` (a real account model slug) is accepted and runs fine, so that limitation no longer
// holds; Invoke now passes -m for every model except the legacy default id (codexLegacyModelID). Re-verify the
// flag contract against a real codex before bumping the floor.
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

// codexLegacyModelID is Invoke's no-flag special case: this id passes NO -m, so codex uses whatever model its
// account is entitled to. Kept for boxes registered before `codex debug models` existed (or a codex CLI too old to
// have it) — codexDefaultModels below is exactly this one entry, and listAccountModels falling back to it means an
// existing registration keeps invoking exactly the way it always has.
const codexLegacyModelID = "codex"

// codexDefaultModels is the fallback used only when `codex debug models` isn't available (a codex CLI too old to
// have it, verified present in 0.153.4) or reports nothing usable. Context window is held conservative —
// overstating is the failure that bricks a session (see HarnessModelConfig.ContextWindowTokens); a box that needs
// the true window sets it in config.
var codexDefaultModels = []Model{
	{ID: codexLegacyModelID, Label: "Codex (ChatGPT account default)", ContextWindowTokens: 256_000},
}

// codexDebugModelsTimeout bounds the `codex debug models` probe — a hung account lookup must not block register,
// or a later invoke's model validation, forever.
const codexDebugModelsTimeout = 30 * time.Second

type codexHarness struct {
	command      string
	path         string
	modelConfigs []config.HarnessModelConfig
}

func newCodexHarness(harnessConfig config.HarnessConfig, discovered discoveredHarness) *codexHarness {
	command := harnessConfig.Command
	if command == "" {
		command = discovered.Command
	}
	if command == "" {
		command = "codex"
	}
	return &codexHarness{command: command, path: discovered.Path, modelConfigs: harnessConfig.Models}
}

// Detect runs the exact pinned Invoke argv (see codexArgs) against codexProbePrompt and requires it to succeed:
// the process must run, every flag must be accepted (an unknown-flag/parse error is a refusal), it must exit 0,
// and the `--json` output must parse as the JSON-lines event stream codex documents. Anything else refuses
// registration — fail closed, same as claude's floor, but proven empirically rather than by version number.
func (h *codexHarness) Detect(ctx context.Context) error {
	resolvedPath, err := exec.LookPath(h.command)
	if err != nil {
		if filepath.IsAbs(h.command) {
			return fmt.Errorf("%q no longer exists — it may have been moved or uninstalled; re-run 'agentparley register' to rediscover codex: %w", h.command, err)
		}
		return fmt.Errorf("codex CLI (%q) not found on PATH: %w", h.command, err)
	}

	versionCtx, cancel := context.WithTimeout(ctx, capabilityProbeTimeout)
	defer cancel()
	versionCmd := exec.CommandContext(versionCtx, resolvedPath, "--version")
	versionCmd.Env = envWithPath(h.path)
	versionOutput, err := versionCmd.Output()
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

	return runCapabilityProbe(ctx, resolvedPath, codexArgs(codexLegacyModelID), codexProbePrompt, h.path, parseCodexJSONLines)
}

// ListModels returns the config override wholesale when one is set (unchanged behavior). Otherwise it asks the
// signed-in account for its real catalog via `codex debug models` and falls back to the single legacy entry only
// when that command isn't available or reports nothing — see listAccountModels.
func (h *codexHarness) ListModels(ctx context.Context) ([]Model, error) {
	if len(h.modelConfigs) > 0 {
		return resolveCLIModels(codexDefaultModels, h.modelConfigs, Codex)
	}

	models, err := h.listAccountModels(ctx)
	if err != nil || len(models) == 0 {
		return codexDefaultModels, nil
	}
	return models, nil
}

// codexDebugModelsResponse is `codex debug models`' JSON shape (verified against codex-cli 0.153.4). A "hide"
// visibility model is internal/unlisted and never offered — only "list" models are real, selectable account models.
type codexDebugModelsResponse struct {
	Models []struct {
		Slug          string `json:"slug"`
		DisplayName   string `json:"display_name"`
		Visibility    string `json:"visibility"`
		ContextWindow int    `json:"context_window"`
	} `json:"models"`
}

// listAccountModels runs `codex debug models` against this box's signed-in ChatGPT account. A model with no
// reported (or zero) context window is skipped, never emitted with a guessed value — this package never emits an
// unknown window (see harness.go's Model doc); a codex CLI too old to have `debug models` (or one that errors for
// any other reason) is reported as an error, which ListModels turns into the legacy single-entry fallback.
func (h *codexHarness) listAccountModels(ctx context.Context) ([]Model, error) {
	probeCtx, cancel := context.WithTimeout(ctx, codexDebugModelsTimeout)
	defer cancel()

	subprocess := exec.CommandContext(probeCtx, h.command, "debug", "models")
	subprocess.Env = envWithPath(h.path)
	output, err := subprocess.Output()
	if err != nil {
		return nil, fmt.Errorf("running %q debug models: %w", h.command, err)
	}

	var response codexDebugModelsResponse
	if err := json.Unmarshal(output, &response); err != nil {
		return nil, fmt.Errorf("parsing %q debug models output: %w", h.command, err)
	}

	models := make([]Model, 0, len(response.Models))
	for _, entry := range response.Models {
		if entry.Visibility != "list" || entry.ContextWindow <= 0 {
			continue
		}
		label := entry.DisplayName
		if label == "" {
			label = entry.Slug
		}
		models = append(models, Model{ID: entry.Slug, Label: label, ContextWindowTokens: entry.ContextWindow})
	}
	return models, nil
}

var codexModelSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

func (h *codexHarness) Invoke(ctx context.Context, model, payload string) (InvokeOutcome, error) {
	prompt, _, outputSchema, err := parseClaudeCodexPayload(payload)
	if err != nil {
		return InvokeOutcome{}, err
	}

	// model is wire-supplied and becomes -m's value: a plain model slug is the only shape allowed through
	// (harness.go's package doc — never a path/URL/flag from the wire); an unknown-but-well-formed slug is codex's
	// own error to report.
	if model != codexLegacyModelID && !codexModelSlug.MatchString(model) {
		return InvokeOutcome{}, fmt.Errorf("model %q is not a valid codex model id", model)
	}

	args := codexArgs(model)
	// output_schema forces codex's reply into the platform's tool-call envelope via OpenAI structured output — the
	// reliable replacement for the fenced-JSON-in-prose convention, which a frontier model still fought under the
	// non-agentic pin. The schema lives on the C# side (single source of truth); the daemon only materializes it to
	// a file because --output-schema takes a path.
	if outputSchema != "" {
		schemaPath, cleanup, err := writeOutputSchemaFile(outputSchema)
		if err != nil {
			return InvokeOutcome{}, err
		}
		defer cleanup()
		args = append(args, "--output-schema", schemaPath)
	}

	return runCLI(ctx, h.command, args, prompt, h.path)
}

func writeOutputSchemaFile(schema string) (path string, cleanup func(), err error) {
	file, err := os.CreateTemp("", "codex-output-schema-*.json")
	if err != nil {
		return "", nil, fmt.Errorf("creating codex output-schema temp file: %w", err)
	}
	if _, err := file.WriteString(schema); err != nil {
		file.Close()
		os.Remove(file.Name())
		return "", nil, fmt.Errorf("writing codex output-schema: %w", err)
	}
	file.Close()
	return file.Name(), func() { os.Remove(file.Name()) }, nil
}

// codexArgs is the FIXED, daemon-owned argv for every codex invocation (Detect's probe and Invoke alike) — never
// built from wire input beyond model, which Invoke has already checked is a plain model slug. --sandbox read-only blocks writes/shell/network side effects (codex
// exec is non-interactive, so any approval prompt reads EOF and auto-declines — no --ask-for-approval flag exists
// in codex 0.150+); --skip-git-repo-check lets exec run outside a git repo (the box's home dir); -m is omitted
// only for codexLegacyModelID (a ChatGPT account's own default) and passed explicitly for every other model
// (verified accepted against codex-cli 0.153.4); -c mcp_servers={} is a highest-precedence inline override
// (documented: CLI/-c overrides beat profile, project .codex/config.toml, user config, and system config) that
// empties the box's locally configured MCP server table regardless of what the box's own config says. Never add
// --dangerously-bypass-approvals-and-sandbox: it disables the read-only sandbox AND is codex's only documented way
// to auto-approve MCP tool calls non-interactively (`codex exec` closes stdin, so the approval prompt reads EOF
// and auto-declines otherwise) — passing it would silently undo this entire pin.
func codexArgs(model string) []string {
	args := []string{
		"exec", "--json",
		"--skip-git-repo-check",
		"--sandbox", "read-only",
		"-c", "mcp_servers={}",
	}
	if model != codexLegacyModelID {
		args = append(args, "-m", model)
	}
	return args
}
