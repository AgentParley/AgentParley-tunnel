// clihelpers.go holds the plumbing shared by the two CLI-family harnesses (codex, claude): running the pinned
// subprocess, and resolving the model list config replaces wholesale rather than overrides field-by-field (unlike
// the server family, which merges per model — see serverhelpers.go).
package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/agentparley/tunnel/internal/config"
)

const maxCLIErrorOutputBytes = 64 * 1024

// capabilityProbeTimeout bounds Detect's probe subprocess. A probe that hangs (e.g. a CLI silently waiting on a
// network login flow) must fail closed rather than block `register` forever, so it gets its own short deadline
// distinct from whatever deadline the caller's ctx carries.
const capabilityProbeTimeout = 30 * time.Second

// cliPayload is the wire shape both CLI harnesses accept: {"prompt": "...", "systemPrompt": "..." | omitted}.
// Codex never sets systemPrompt — its system content is folded into prompt by the C# side.
type cliPayload struct {
	Prompt       string `json:"prompt"`
	SystemPrompt string `json:"systemPrompt"`
}

func parseClaudeCodexPayload(payload string) (prompt, systemPrompt string, err error) {
	var decoded cliPayload
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		return "", "", fmt.Errorf("invoke payload is not valid JSON: %w", err)
	}
	if decoded.Prompt == "" {
		return "", "", errors.New("invoke payload is missing a non-empty \"prompt\"")
	}
	return decoded.Prompt, decoded.SystemPrompt, nil
}

// resolveCLIModels applies the CLI family's "replaced wholesale" config rule: an empty overrides list means the
// built-in defaults; a non-empty list REPLACES them entirely (never merges), and every override's
// ContextWindowTokens must be > 0 — this package never emits an unknown window.
func resolveCLIModels(defaults []Model, overrides []config.HarnessModelConfig, harnessName string) ([]Model, error) {
	if len(overrides) == 0 {
		return defaults, nil
	}

	models := make([]Model, 0, len(overrides))
	for _, override := range overrides {
		if override.ContextWindowTokens <= 0 {
			return nil, fmt.Errorf("harnesses.%s.models[].context_window_tokens is required and must be > 0 for model %q", harnessName, override.ID)
		}
		label := override.Label
		if label == "" {
			label = override.ID
		}
		models = append(models, Model{ID: override.ID, Label: label, ContextWindowTokens: override.ContextWindowTokens})
	}
	return models, nil
}

// runCLI execs command with args, feeding prompt on stdin from a per-invoke temp working directory — the CLI is
// used strictly as a model endpoint, so it gets a throwaway directory rather than the daemon's own working
// directory or the run_as user's home. ctx carries the operation deadline: cancelling it kills the process, so a
// timed-out invoke does not keep running on the box after the caller has given up.
func runCLI(ctx context.Context, command string, args []string, prompt string) (InvokeOutcome, error) {
	workingDir, err := os.MkdirTemp("", "agentparley-tunnel-harness-*")
	if err != nil {
		return InvokeOutcome{}, fmt.Errorf("creating a working directory for %s: %w", command, err)
	}
	defer os.RemoveAll(workingDir)

	subprocess := exec.CommandContext(ctx, command, args...)
	subprocess.Dir = workingDir
	subprocess.Stdin = bytes.NewBufferString(prompt)
	subprocess.Cancel = func() error { return subprocess.Process.Signal(syscall.SIGKILL) }

	var stdout, stderr bytes.Buffer
	subprocess.Stdout = &stdout
	subprocess.Stderr = &stderr

	runErr := subprocess.Run()

	exitStatus := 0
	var exitError *exec.ExitError
	switch {
	case runErr == nil:
		exitStatus = 0
	case errors.As(runErr, &exitError):
		exitStatus = exitError.ExitCode()
	case ctx.Err() != nil:
		return InvokeOutcome{}, ctx.Err()
	default:
		return InvokeOutcome{}, fmt.Errorf("running %s: %w", command, runErr)
	}

	return InvokeOutcome{
		Payload:     stdout.String(),
		StatusCode:  exitStatus,
		ErrorOutput: truncate(stderr.String(), maxCLIErrorOutputBytes),
	}, nil
}

func truncate(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	return value[:maxBytes]
}

// runCapabilityProbe runs command with the harness's real pinned argv against a trivial throwaway prompt, and
// requires: the process runs, every flag is accepted (an unknown-flag/parse error surfaces as a non-zero exit or
// a run failure, either way a refusal), exit 0, and parseOutput accepts the stdout. This is what proves a CLI
// harness is actually usable — installed, flags accepted, signed in — rather than merely present on PATH or
// above some version number. It is capped at capabilityProbeTimeout so a hung CLI fails closed instead of
// blocking `register` indefinitely. Callers have already resolved command via exec.LookPath as part of their own
// version check, so this does not repeat that lookup.
func runCapabilityProbe(ctx context.Context, command string, args []string, prompt string, parseOutput func(stdout string) error) error {
	probeCtx, cancel := context.WithTimeout(ctx, capabilityProbeTimeout)
	defer cancel()

	outcome, err := runCLI(probeCtx, command, args, prompt)
	if err != nil {
		if probeCtx.Err() != nil {
			return fmt.Errorf("probing %q timed out after %s — cannot prove it is installed, accepts the pinned flags, and is signed in", command, capabilityProbeTimeout)
		}
		return fmt.Errorf("probing %q failed to run: %w", command, err)
	}
	if outcome.StatusCode != 0 {
		return fmt.Errorf("probing %q exited %d (stderr: %s) — the CLI must accept the pinned flags and be signed in before this harness can register",
			command, outcome.StatusCode, strings.TrimSpace(outcome.ErrorOutput))
	}
	if err := parseOutput(outcome.Payload); err != nil {
		return fmt.Errorf("probing %q exited 0 but its output did not match the expected format: %w", command, err)
	}
	return nil
}

// parseCodexJSONLines reports whether stdout is the JSON-lines event stream `codex exec --json` documents: every
// non-empty line a JSON object, at least one line present.
func parseCodexJSONLines(stdout string) error {
	sawEvent := false
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return fmt.Errorf("line %q is not a JSON object: %w", line, err)
		}
		sawEvent = true
	}
	if !sawEvent {
		return errors.New("no JSON-lines events in output")
	}
	return nil
}

// parseClaudeJSON reports whether stdout is the single JSON object `claude -p --output-format json` documents,
// and additionally rejects an error-shaped result (is_error true) — the surest sign the CLI ran but is not
// actually signed in, which a bare exit-0 check would miss.
func parseClaudeJSON(stdout string) error {
	var result struct {
		IsError bool   `json:"is_error"`
		Subtype string `json:"subtype"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		return fmt.Errorf("output is not a JSON object: %w", err)
	}
	if result.IsError {
		return fmt.Errorf("claude reported an error (subtype %q) — check that the CLI is signed in", result.Subtype)
	}
	return nil
}
