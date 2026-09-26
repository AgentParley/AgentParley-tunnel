// Package harness is the daemon-side owner of local-model detection, model listing, and invocation for the four
// harnesses this box can expose: two agentic CLIs pinned into non-agentic, tool-free model endpoints (codex,
// claude), and two OpenAI-compatible HTTP servers (ollama, openai-compatible-local). Nothing here ever accepts a
// URL, binary path, flag, or argv fragment from the wire — every Harness resolves what to call solely from this
// box's own local config, its own built-in defaults, or what `register` already found and persisted on THIS box
// (see discovery.go) — which is the whole point of the tunnel design: a compromised platform can name a harness
// and a model, never how that harness is invoked. A model id IS wire-supplied (InvokeHarnessCall.model), so every
// Invoke that turns it into a flag VALUE (codex's -m) first requires a plain model-slug shape — never an arbitrary
// wire string reaching a subprocess argv unchecked.
package harness

import (
	"context"
	"fmt"

	"github.com/agentparley/tunnel/internal/config"
)

// These names are the wire vocabulary shared with AgentParley.Cloud.Domain's TunnelHarnessNames and the proto's
// InvokeHarnessCall.harness field — renaming any one of the three silently breaks dispatch.
const (
	Codex                 = "codex"
	Claude                = "claude"
	Ollama                = "ollama"
	OpenAICompatibleLocal = "openai-compatible-local"
)

// Model is one locally-available model this box offers. ContextWindowTokens is ALWAYS > 0 — every ListModels
// implementation fails rather than emit a model with an unknown window (a believed-200k window on a real-8k model
// would stop compaction from ever firing and brick the session once history outgrows the real window).
type Model struct {
	ID                  string
	Label               string
	ContextWindowTokens int
}

// InvokeOutcome is what Invoke hands back for the platform to interpret — this package never interprets it itself.
// StatusCode is the local HTTP status (server family) or process exit code (CLI family); Payload is the response
// body / stdout verbatim; ErrorOutput is stderr (CLI family) or empty (server family).
type InvokeOutcome struct {
	Payload     string
	StatusCode  int
	ErrorOutput string
}

// Harness is the daemon-side contract every one of the four kinds implements.
type Harness interface {
	// Detect reports whether this harness can run on this box right now — binary on PATH / server answers — and,
	// for the CLI family, additionally PROVES the non-agentic pin (built-ins disabled, MCP servers not loaded,
	// local settings unable to re-enable either) against an empirically-established minimum version floor. The
	// returned error message is user-facing: it is what `register` prints when it refuses a harness.
	Detect(ctx context.Context) error
	// ListModels lists this box's locally-available models. Every returned Model.ContextWindowTokens is > 0; a
	// model whose window cannot be determined is omitted from the list AND causes ListModels to fail with an error
	// naming the affected model and the exact config key to set — fail closed, never a 0 or a default.
	ListModels(ctx context.Context) ([]Model, error)
	// Invoke runs one completion against model with the given payload (opaque to this package — the caller, C#,
	// owns the request/response shape). ctx carries the operation deadline; every implementation cancels its local
	// HTTP call or subprocess when ctx is done, so a timed-out invoke is actually killed on the box.
	Invoke(ctx context.Context, model, payload string) (InvokeOutcome, error)
}

// Resolve returns the Harness implementation for harnessName, or an error naming why it can't run on this box — an
// unknown name, or missing REQUIRED config (openai-compatible-local's url has no built-in default). It is called
// per invoke (dispatch.go) as well as by `register`, so it re-reads the discovery file on every call — a small
// local file read, cheap enough to skip caching — merging whatever `register` found in under whatever config.yaml
// already says (config always wins; see withDiscoveredURL and the CLI constructors).
func Resolve(harnessName string, tunnelConfig *config.Config) (Harness, error) {
	harnessConfig := tunnelConfig.Harnesses[harnessName]
	discoveries, err := loadDiscoveries()
	if err != nil {
		return nil, fmt.Errorf("reading discovered harnesses: %w", err)
	}
	discovered := discoveries[harnessName]

	switch harnessName {
	case Codex:
		return newCodexHarness(harnessConfig, discovered), nil
	case Claude:
		return newClaudeHarness(harnessConfig, discovered), nil
	case Ollama:
		return newOllamaHarness(withDiscoveredURL(harnessConfig, discovered)), nil
	case OpenAICompatibleLocal:
		merged := withDiscoveredURL(harnessConfig, discovered)
		if merged.URL == "" {
			return nil, fmt.Errorf("harness %q requires harnesses.%s.url in config, or run `agentparley-tunnel register` to auto-discover a local server — there is no built-in default for a bespoke OpenAI-compatible server", harnessName, harnessName)
		}
		return newOpenAICompatibleLocalHarness(merged), nil
	default:
		return nil, fmt.Errorf("unknown harness %q — valid harnesses are %s, %s, %s, %s", harnessName, Codex, Claude, Ollama, OpenAICompatibleLocal)
	}
}

// withDiscoveredURL fills harnessConfig.URL from discovered only when config left it empty — explicit config
// always wins over anything `register` found on its own.
func withDiscoveredURL(harnessConfig config.HarnessConfig, discovered discoveredHarness) config.HarnessConfig {
	if harnessConfig.URL == "" {
		harnessConfig.URL = discovered.URL
	}
	return harnessConfig
}

// findModelOverride looks up a config override for modelID within a harness's Models list, or reports found=false.
func findModelOverride(models []config.HarnessModelConfig, modelID string) (config.HarnessModelConfig, bool) {
	for _, override := range models {
		if override.ID == modelID {
			return override, true
		}
	}
	return config.HarnessModelConfig{}, false
}
