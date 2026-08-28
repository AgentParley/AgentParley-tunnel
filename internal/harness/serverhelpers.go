// serverhelpers.go holds the plumbing shared by the two HTTP-server-family harnesses (ollama,
// openai-compatible-local): a plain client honoring the operation's deadline, and the model-override merge rule
// config applies on top of whatever the server itself reports (unlike the CLI family's wholesale replace — see
// clihelpers.go).
package harness

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/agentparley/tunnel/internal/config"
)

var serverHTTPClient = &http.Client{}

func doGET(ctx context.Context, url string) (status int, body []byte, err error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	return doRequest(request)
}

func doPOST(ctx context.Context, url, contentType, requestBody string) (status int, body []byte, err error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBufferString(requestBody))
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Content-Type", contentType)
	return doRequest(request)
}

func doRequest(request *http.Request) (status int, body []byte, err error) {
	response, err := serverHTTPClient.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("reading response body: %w", err)
	}
	return response.StatusCode, responseBody, nil
}

// invokeChatCompletions is the invoke path shared by both server-family harnesses: the payload is opaque JSON the
// caller (C#) already built as an OpenAI chat.completions request, forwarded to the local server verbatim.
func invokeChatCompletions(ctx context.Context, baseURL, payload string) (InvokeOutcome, error) {
	status, body, err := doPOST(ctx, baseURL+"/v1/chat/completions", "application/json", payload)
	if err != nil {
		return InvokeOutcome{}, err
	}
	return InvokeOutcome{Payload: string(body), StatusCode: status}, nil
}

// mergeServerModelOverrides applies the server family's "override when present" rule: detected carries what the
// server itself reported (label/window may be zero-valued when the server didn't expose them); overrides fills in
// or replaces label/context per model id, matched by id. A detected model with neither a server-reported window
// nor a config override is dropped and its id collected into missingWindows, so the caller can fail closed naming
// exactly which models need `harnesses.<name>.models[].context_window_tokens` set.
func mergeServerModelOverrides(detected []Model, overrides []config.HarnessModelConfig) (models []Model, missingWindows []string) {
	for _, model := range detected {
		if override, found := findModelOverride(overrides, model.ID); found {
			if override.Label != "" {
				model.Label = override.Label
			}
			if override.ContextWindowTokens > 0 {
				model.ContextWindowTokens = override.ContextWindowTokens
			}
		}
		if model.Label == "" {
			model.Label = model.ID
		}
		if model.ContextWindowTokens <= 0 {
			missingWindows = append(missingWindows, model.ID)
			continue
		}
		models = append(models, model)
	}
	return models, missingWindows
}
