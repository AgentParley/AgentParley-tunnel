package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/agentparley/tunnel/internal/config"
)

// openAICompatibleLocalHarness covers vLLM, llama.cpp, and LM Studio — one kind spanning servers that differ on whether
// they report a context window at all, which is why Resolve requires harnesses.openai-compatible-local.url
// (no built-in default) and ListModels leans hard on the config override for the window.
type openAICompatibleLocalHarness struct {
	url          string
	modelConfigs []config.HarnessModelConfig
}

// trimAPIVersionSuffix drops a trailing /v1 from the configured base URL. Every OpenAI-compatible client is
// configured with the version in the base ("http://host:8000/v1"), so users supply it here too — and this harness
// appends /v1/models and /v1/chat/completions itself, which turned that natural input into a 404 on /v1/v1/models.
func trimAPIVersionSuffix(url string) string {
	return strings.TrimSuffix(strings.TrimSuffix(url, "/"), "/v1")
}

func newOpenAICompatibleLocalHarness(harnessConfig config.HarnessConfig) *openAICompatibleLocalHarness {
	return &openAICompatibleLocalHarness{url: trimAPIVersionSuffix(harnessConfig.URL), modelConfigs: harnessConfig.Models}
}

func (h *openAICompatibleLocalHarness) Detect(ctx context.Context) error {
	status, _, err := doGET(ctx, h.url+"/v1/models")
	if err != nil {
		return fmt.Errorf("openai-compatible server not reachable at %s: %w", h.url, err)
	}
	if status != 200 {
		return fmt.Errorf("openai-compatible server at %s returned HTTP %d for /v1/models", h.url, status)
	}
	return nil
}

type openAICompatibleLocalModelsResponse struct {
	Data []struct {
		ID          string `json:"id"`
		MaxModelLen int    `json:"max_model_len"`
	} `json:"data"`
}

func (h *openAICompatibleLocalHarness) ListModels(ctx context.Context) ([]Model, error) {
	status, body, err := doGET(ctx, h.url+"/v1/models")
	if err != nil {
		return nil, fmt.Errorf("listing models at %s: %w", h.url, err)
	}
	if status != 200 {
		return nil, fmt.Errorf("openai-compatible server at %s returned HTTP %d for /v1/models", h.url, status)
	}

	var listing openAICompatibleLocalModelsResponse
	if err := json.Unmarshal(body, &listing); err != nil {
		return nil, fmt.Errorf("parsing /v1/models response: %w", err)
	}

	servedContextWindow := h.servedContextWindow(ctx)
	detected := make([]Model, 0, len(listing.Data))
	for _, entry := range listing.Data {
		contextWindow := entry.MaxModelLen
		if contextWindow <= 0 {
			contextWindow = servedContextWindow
		}
		detected = append(detected, Model{ID: entry.ID, Label: entry.ID, ContextWindowTokens: contextWindow})
	}

	models, missingWindows := mergeServerModelOverrides(detected, h.modelConfigs)
	if len(missingWindows) > 0 {
		return nil, fmt.Errorf("the server at %s did not report a context window for model(s) %s — set harnesses.openai-compatible-local.models[].context_window_tokens for each",
			h.url, strings.Join(missingWindows, ", "))
	}
	return models, nil
}

func (h *openAICompatibleLocalHarness) Invoke(ctx context.Context, model, payload string) (InvokeOutcome, error) {
	return invokeChatCompletions(ctx, h.url, payload)
}

// servedContextWindow reads llama.cpp's /props, whose default_generation_settings.n_ctx is the context the server
// is ACTUALLY serving. Deliberately not /v1/models' meta.n_ctx_train: that is the model's trained context and can
// be far larger than what this process serves (observed 262144 trained vs 131072 served on one box), and an
// overstated window stops compaction ever firing, which bricks a session permanently rather than degrading it.
// Any failure returns 0, which leaves the caller's fail-closed "set context_window_tokens" path intact.
func (h *openAICompatibleLocalHarness) servedContextWindow(ctx context.Context) int {
	status, body, err := doGET(ctx, h.url+"/props")
	if err != nil || status != 200 {
		return 0
	}
	var props struct {
		DefaultGenerationSettings struct {
			ContextTokens int `json:"n_ctx"`
		} `json:"default_generation_settings"`
	}
	if err := json.Unmarshal(body, &props); err != nil {
		return 0
	}
	return props.DefaultGenerationSettings.ContextTokens
}
