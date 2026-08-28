package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/agentparley/tunnel/internal/config"
)

const defaultOllamaURL = "http://localhost:11434"

type ollamaHarness struct {
	url          string
	modelConfigs []config.HarnessModelConfig
}

func newOllamaHarness(harnessConfig config.HarnessConfig) *ollamaHarness {
	url := harnessConfig.URL
	if url == "" {
		url = defaultOllamaURL
	}
	return &ollamaHarness{url: strings.TrimSuffix(url, "/"), modelConfigs: harnessConfig.Models}
}

func (h *ollamaHarness) Detect(ctx context.Context) error {
	status, _, err := doGET(ctx, h.url+"/api/tags")
	if err != nil {
		return fmt.Errorf("ollama not reachable at %s: %w", h.url, err)
	}
	if status != 200 {
		return fmt.Errorf("ollama at %s returned HTTP %d for /api/tags", h.url, status)
	}
	return nil
}

type ollamaTagsResponse struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

type ollamaShowResponse struct {
	ModelInfo map[string]json.Number `json:"model_info"`
}

func (h *ollamaHarness) ListModels(ctx context.Context) ([]Model, error) {
	status, body, err := doGET(ctx, h.url+"/api/tags")
	if err != nil {
		return nil, fmt.Errorf("listing ollama models: %w", err)
	}
	if status != 200 {
		return nil, fmt.Errorf("ollama at %s returned HTTP %d for /api/tags", h.url, status)
	}

	var tags ollamaTagsResponse
	if err := json.Unmarshal(body, &tags); err != nil {
		return nil, fmt.Errorf("parsing ollama /api/tags response: %w", err)
	}

	detected := make([]Model, 0, len(tags.Models))
	for _, tag := range tags.Models {
		contextWindowTokens := h.contextLength(ctx, tag.Name)
		detected = append(detected, Model{ID: tag.Name, Label: tag.Name, ContextWindowTokens: contextWindowTokens})
	}

	models, missingWindows := mergeServerModelOverrides(detected, h.modelConfigs)
	if len(missingWindows) > 0 {
		return nil, fmt.Errorf("ollama did not report a context length for model(s) %s — set harnesses.ollama.models[].context_window_tokens for each",
			strings.Join(missingWindows, ", "))
	}
	return models, nil
}

// contextLength reads the model's context length from POST /api/show, returning 0 (not an error) when the server
// doesn't expose it — ListModels' merge step is what turns "0 and no config override" into the fail-closed error,
// since a config override might still supply the window for this exact model.
func (h *ollamaHarness) contextLength(ctx context.Context, modelName string) int {
	requestBody, err := json.Marshal(map[string]string{"name": modelName})
	if err != nil {
		return 0
	}
	status, body, err := doPOST(ctx, h.url+"/api/show", "application/json", string(requestBody))
	if err != nil || status != 200 {
		return 0
	}

	var showResponse ollamaShowResponse
	if err := json.Unmarshal(body, &showResponse); err != nil {
		return 0
	}
	for key, value := range showResponse.ModelInfo {
		if strings.HasSuffix(key, ".context_length") {
			if length, err := value.Int64(); err == nil && length > 0 {
				return int(length)
			}
		}
	}
	return 0
}

func (h *ollamaHarness) Invoke(ctx context.Context, model, payload string) (InvokeOutcome, error) {
	return invokeChatCompletions(ctx, h.url, payload)
}
