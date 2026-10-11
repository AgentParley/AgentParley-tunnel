package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/agentparley/tunnel/internal/cliauth"
	"github.com/agentparley/tunnel/internal/cliround"
	"github.com/agentparley/tunnel/internal/codexrpc"
	"github.com/agentparley/tunnel/internal/config"
	"github.com/agentparley/tunnel/internal/managedcli"
)

// codexLegacyModelID is the id that names no model: Codex uses whatever its account defaults to. Registrations made
// before `codex debug models` existed, and the fallback list below, use it.
const codexLegacyModelID = codexrpc.LegacyModelID

// codexDefaultModels is the fallback used only when `codex debug models` reports nothing usable. The context window
// is held conservative: overstating it is the failure that bricks a session (see HarnessModelConfig.ContextWindowTokens).
var codexDefaultModels = []Model{
	{ID: codexLegacyModelID, Label: "Codex (ChatGPT account default)", ContextWindowTokens: 256_000},
}

const (
	codexDebugModelsTimeout = 30 * time.Second
	codexProbeBudget        = 90 * time.Second
)

type codexHarness struct {
	modelConfigs []config.HarnessModelConfig
	sessions     *cliround.Manager
	driver       CLIDriver
	manifest     func() managedcli.Manifest
}

func newCodexHarness(harnessConfig config.HarnessConfig, cli CLIDeps) *codexHarness {
	return &codexHarness{modelConfigs: harnessConfig.Models, sessions: cli.Sessions, driver: cli.Drivers.For(Codex), manifest: manifestSource(cli.Pins)}
}

// Detect proves the pinned Codex build is installed, signed in with the daemon's own login, calls an AgentParley tool
// directly (never through a code-mode cell), and continues a thread rebuilt from injected history.
func (h *codexHarness) Detect(ctx context.Context) error {
	if !cliauth.HasCodexLogin() {
		return errors.New("Codex is not signed in for AgentParley — run `agentparley register codex`")
	}
	if _, _, err := managedcli.Ensure(ctx, h.manifest(), "codex"); err != nil {
		return err
	}
	models, err := h.ListModels(ctx)
	if err != nil {
		return err
	}

	probeCtx, cancel := context.WithTimeout(ctx, codexProbeBudget)
	defer cancel()
	driver := codexrpc.NewDriver(h.manifest, log.Default())
	defer driver.Shutdown()
	if err := probeDirectToolCall(probeCtx, driver, models[0].ID); err != nil {
		return fmt.Errorf("the direct tool-call probe failed: %w", err)
	}
	if err := probeRebuild(probeCtx, driver, models[0].ID); err != nil {
		return fmt.Errorf("the rebuild probe failed: %w", err)
	}
	return nil
}

var probeEchoTool = cliround.Tool{
	Name:        "probe_echo",
	Description: "Echo a number back.",
	Parameters:  json.RawMessage(`{"type":"object","properties":{"x":{"type":"number"}},"required":["x"],"additionalProperties":false}`),
}

func probeBudgets(ctx context.Context) cliround.Budgets {
	remaining := codexProbeBudget
	if deadline, ok := ctx.Deadline(); ok {
		remaining = time.Until(deadline)
	}
	return cliround.Budgets{Rebuild: remaining, Step: remaining}
}

func probeDirectToolCall(ctx context.Context, driver *codexrpc.Driver, model string) error {
	request := &cliround.Request{
		Effort: "low",
		Tools:  []cliround.Tool{probeEchoTool},
		Items:  []cliround.Item{{Key: "p1", Role: cliround.RoleUser, Text: "Call probe_echo with x=1, then reply ok."}},
	}
	runner, first, err := driver.Factory(ctx, model, request, probeBudgets(ctx))
	if err != nil {
		return err
	}
	defer runner.Close()
	if err := requireCleanRound(first); err != nil {
		return err
	}
	if first.Outcome != cliround.OutcomeToolCalls || len(first.Calls) != 1 || first.Calls[0].Name != "probe_echo" {
		return fmt.Errorf("expected one probe_echo call, got outcome %q with %d call(s)", first.Outcome, len(first.Calls))
	}
	if strings.HasPrefix(first.Calls[0].ID, "exec-") {
		return errors.New("the tool was called from a code-mode cell instead of directly")
	}

	plan := cliround.Plan{Results: []cliround.Item{{Role: cliround.RoleTool, CallID: first.Calls[0].ID, Text: `{"echo":1}`}}}
	second, err := runner.Extend(ctx, plan, request)
	if err != nil {
		return err
	}
	if err := requireCleanRound(second); err != nil {
		return err
	}
	if second.Outcome != cliround.OutcomeAnswer || strings.TrimSpace(second.Text) == "" {
		return fmt.Errorf("expected a final answer after the tool result, got outcome %q", second.Outcome)
	}
	return nil
}

func probeRebuild(ctx context.Context, driver *codexrpc.Driver, model string) error {
	request := &cliround.Request{
		Effort: "low",
		Tools:  []cliround.Tool{probeEchoTool},
		Items: []cliround.Item{
			{Key: "p1", Role: cliround.RoleUser, Text: "Call probe_echo with x=7, then reply with the echoed number only."},
			{Key: "p2", Role: cliround.RoleAssistant, Calls: []cliround.Call{{ID: "history-1", Name: "probe_echo", Arguments: json.RawMessage(`{"x":7}`)}}},
			{Key: "p3", Role: cliround.RoleTool, CallID: "history-1", Text: `{"echo":7}`},
		},
	}
	runner, result, err := driver.Factory(ctx, model, request, probeBudgets(ctx))
	if err != nil {
		return err
	}
	defer runner.Close()
	if err := requireCleanRound(result); err != nil {
		return err
	}
	if result.Outcome != cliround.OutcomeAnswer || strings.TrimSpace(result.Text) == "" {
		return fmt.Errorf("expected a final answer from the rebuilt thread, got outcome %q", result.Outcome)
	}
	return nil
}

// requireCleanRound fails a probe round in which Codex reached for anything that is not an AgentParley tool.
func requireCleanRound(result *cliround.Result) error {
	for _, event := range result.Events {
		if event.Kind == cliround.EventBlockedTool || event.Kind == cliround.EventInternalStep {
			return fmt.Errorf("Codex used a native tool: %s (%s)", event.Tool, event.Detail)
		}
	}
	return nil
}

// ListModels returns the config override wholesale when one is set. Otherwise it asks the signed-in account for its
// real catalog via `codex debug models` and falls back to the single legacy entry when that reports nothing.
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

// codexDebugModelsResponse is `codex debug models`' JSON shape. A "hide" visibility model is internal and never
// offered.
type codexDebugModelsResponse struct {
	Models []struct {
		Slug          string `json:"slug"`
		DisplayName   string `json:"display_name"`
		Visibility    string `json:"visibility"`
		ContextWindow int    `json:"context_window"`
	} `json:"models"`
}

func (h *codexHarness) listAccountModels(ctx context.Context) ([]Model, error) {
	probeCtx, cancel := context.WithTimeout(ctx, codexDebugModelsTimeout)
	defer cancel()

	binary, _, err := managedcli.Ensure(probeCtx, h.manifest(), "codex")
	if err != nil {
		return nil, err
	}
	output, err := cliauth.CodexCommand(probeCtx, binary, "debug", "models").Output()
	if err != nil {
		return nil, fmt.Errorf("running codex debug models: %w", err)
	}

	var response codexDebugModelsResponse
	if err := json.Unmarshal(output, &response); err != nil {
		return nil, fmt.Errorf("parsing codex debug models output: %w", err)
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

// Invoke runs one round: the request's history is reconciled with the conversation's warm thread, which is extended
// or rebuilt, and the model's next step comes back as the round result.
func (h *codexHarness) Invoke(ctx context.Context, model, payload string) (InvokeOutcome, error) {
	request, err := cliround.ParseRequest(payload)
	if err != nil {
		return InvokeOutcome{}, err
	}
	if model != codexLegacyModelID && !codexModelSlug.MatchString(model) {
		return InvokeOutcome{}, &cliround.InvalidRequestError{Message: fmt.Sprintf("model %q is not a valid codex model id", model)}
	}
	return runCLIRound(ctx, h.sessions, h.driver, Codex, model, request)
}
