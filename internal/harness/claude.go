package harness

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/agentparley/tunnel/internal/claudepty"
	"github.com/agentparley/tunnel/internal/cliauth"
	"github.com/agentparley/tunnel/internal/cliround"
	"github.com/agentparley/tunnel/internal/config"
	"github.com/agentparley/tunnel/internal/managedcli"
)

// claudeDefaultModels are the aliases Claude Code resolves to its current models; the context window is held
// conservative (see HarnessModelConfig.ContextWindowTokens).
var claudeDefaultModels = []Model{
	{ID: "opus", Label: "Claude Opus (via claude CLI)", ContextWindowTokens: 200_000},
	{ID: "sonnet", Label: "Claude Sonnet (via claude CLI)", ContextWindowTokens: 200_000},
	{ID: "haiku", Label: "Claude Haiku (via claude CLI)", ContextWindowTokens: 200_000},
}

const (
	claudeProbeBudget = 120 * time.Second
	claudeProbeWord   = "PAPAYA"
)

type claudeHarness struct {
	modelConfigs []config.HarnessModelConfig
	sessions     *cliround.Manager
	driver       CLIDriver
	manifest     func() managedcli.Manifest
}

func newClaudeHarness(harnessConfig config.HarnessConfig, cli CLIDeps) *claudeHarness {
	return &claudeHarness{modelConfigs: harnessConfig.Models, sessions: cli.Sessions, driver: cli.Drivers.For(Claude), manifest: manifestSource(cli.Pins)}
}

// Detect proves the pinned Claude Code build is installed, signed in with the daemon's own login, shows no startup
// screen, registers and calls an AgentParley tool, and reads a history row seeded into it.
func (h *claudeHarness) Detect(ctx context.Context) error {
	if !cliauth.HasClaudeLogin() {
		return errors.New("Claude Code is not signed in for AgentParley — run `agentparley register claude`")
	}
	if _, _, err := managedcli.Ensure(ctx, h.manifest(), "claude"); err != nil {
		return err
	}

	probeCtx, cancel := context.WithTimeout(ctx, claudeProbeBudget)
	defer cancel()
	driver := claudepty.NewDriver(h.manifest, log.Default())
	defer driver.Shutdown()
	if err := probeToolCallAndSeed(probeCtx, driver, claudeDefaultModels[0].ID); err != nil {
		return fmt.Errorf("the tool-call probe failed: %w", err)
	}
	return nil
}

func probeToolCallAndSeed(ctx context.Context, driver *claudepty.Driver, model string) error {
	request := &cliround.Request{
		Effort: "low",
		Tools:  []cliround.Tool{probeEchoTool},
		Items: []cliround.Item{
			{Key: "p1", Role: cliround.RoleUser, Text: "The probe word is " + claudeProbeWord + "."},
			{Key: "p2", Role: cliround.RoleAssistant, Text: "Noted."},
			{Key: "p3", Role: cliround.RoleUser, Text: "Call probe_echo with x=1, then reply with the probe word."},
		},
	}
	runner, first, err := driver.Factory(ctx, model, request, probeBudgets(ctx))
	if err != nil {
		return err
	}
	defer runner.Close()
	if err := requireNoNativeUse(first); err != nil {
		return err
	}
	if first.Outcome != cliround.OutcomeToolCalls || len(first.Calls) != 1 || first.Calls[0].Name != "probe_echo" {
		return fmt.Errorf("expected one probe_echo call, got outcome %q with %d call(s)", first.Outcome, len(first.Calls))
	}

	plan := cliround.Plan{Results: []cliround.Item{{Role: cliround.RoleTool, CallID: first.Calls[0].ID, Text: `{"echo":1}`}}}
	second, err := runner.Extend(ctx, plan, request)
	if err != nil {
		return err
	}
	if err := requireNoNativeUse(second); err != nil {
		return err
	}
	if second.Outcome != cliround.OutcomeAnswer || !strings.Contains(strings.ToUpper(second.Text), claudeProbeWord) {
		return fmt.Errorf("expected an answer containing the seeded probe word, got outcome %q: %q", second.Outcome, second.Text)
	}
	return nil
}

// requireNoNativeUse fails a probe round in which Claude Code reached for anything that is not an AgentParley tool.
func requireNoNativeUse(result *cliround.Result) error {
	for _, event := range result.Events {
		if event.Kind == cliround.EventBlockedTool || event.Kind == cliround.EventInternalStep {
			return fmt.Errorf("Claude Code used a native tool: %s (%s)", event.Tool, event.Detail)
		}
	}
	return nil
}

func (h *claudeHarness) ListModels(ctx context.Context) ([]Model, error) {
	return resolveCLIModels(claudeDefaultModels, h.modelConfigs, Claude)
}

var claudeModelID = regexp.MustCompile(`^[a-z0-9][a-z0-9._\-\[\]]{0,63}$`)

// Invoke runs one round: the request's history is reconciled with the conversation's warm Claude Code, which is
// extended or rebuilt, and the model's next step comes back as the round result.
func (h *claudeHarness) Invoke(ctx context.Context, model, payload string) (InvokeOutcome, error) {
	request, err := cliround.ParseRequest(payload)
	if err != nil {
		return InvokeOutcome{}, err
	}
	if !claudeModelID.MatchString(model) {
		return InvokeOutcome{}, &cliround.InvalidRequestError{Message: fmt.Sprintf("model %q is not a valid claude model id", model)}
	}
	return runCLIRound(ctx, h.sessions, h.driver, Claude, model, request)
}
