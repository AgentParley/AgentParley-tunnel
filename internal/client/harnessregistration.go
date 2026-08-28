package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/agentparley/tunnel/internal/credstore"
	"github.com/agentparley/tunnel/internal/harness"
)

// RegisterHarnessResult is what the platform hands back after materializing this box's detected models as
// ordinary Provider/LlmModel rows.
type RegisterHarnessResult struct {
	ProviderID   string `json:"providerId"`
	ProviderName string `json:"providerName"`
	ModelCount   int    `json:"modelCount"`
}

type harnessModelWire struct {
	ModelIdentifier     string `json:"modelIdentifier"`
	DisplayName         string `json:"displayName"`
	ContextWindowTokens int    `json:"contextWindowTokens"`
}

type registerHarnessRequest struct {
	RefreshToken string             `json:"refreshToken"`
	Signature    string             `json:"signature"`
	UnixSeconds  int64              `json:"unixSeconds"`
	Jti          string             `json:"jti"`
	Harness      string             `json:"harness"`
	Models       []harnessModelWire `json:"models"`
}

type unregisterHarnessRequest struct {
	RefreshToken string `json:"refreshToken"`
	Signature    string `json:"signature"`
	UnixSeconds  int64  `json:"unixSeconds"`
	Jti          string `json:"jti"`
	Harness      string `json:"harness"`
}

// RegisterHarness sends this box's detected harness + model list to the platform over the same possession-proof
// auth /tunnel/token uses. models is guaranteed by the caller (register's local Detect/ListModels) to carry a
// positive ContextWindowTokens for every entry — there is no 0-means-unknown mapping on the wire.
func RegisterHarness(httpClient *http.Client, apiBaseURL string, credentials *credstore.Credentials, harnessName string, models []harness.Model) (RegisterHarnessResult, error) {
	proof, err := buildPossessionProof(credentials)
	if err != nil {
		return RegisterHarnessResult{}, err
	}

	wireModels := make([]harnessModelWire, 0, len(models))
	for _, model := range models {
		wireModels = append(wireModels, harnessModelWire{
			ModelIdentifier:     model.ID,
			DisplayName:         model.Label,
			ContextWindowTokens: model.ContextWindowTokens,
		})
	}

	requestBody, err := json.Marshal(registerHarnessRequest{
		RefreshToken: proof.RefreshToken,
		Signature:    proof.Signature,
		UnixSeconds:  proof.UnixSeconds,
		Jti:          proof.Jti,
		Harness:      harnessName,
		Models:       wireModels,
	})
	if err != nil {
		return RegisterHarnessResult{}, err
	}

	httpResponse, err := httpClient.Post(apiBaseURL+"/tunnel/harnesses", "application/json", bytes.NewReader(requestBody))
	if err != nil {
		return RegisterHarnessResult{}, err
	}
	defer httpResponse.Body.Close()

	responseBody, err := io.ReadAll(httpResponse.Body)
	if err != nil {
		return RegisterHarnessResult{}, fmt.Errorf("reading the registration response: %w", err)
	}
	if httpResponse.StatusCode != http.StatusOK {
		return RegisterHarnessResult{}, fmt.Errorf("registering harness %q: unexpected status %d: %s", harnessName, httpResponse.StatusCode, truncate(string(responseBody), 500))
	}

	var result RegisterHarnessResult
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return RegisterHarnessResult{}, fmt.Errorf("decoding the registration response: %w", err)
	}
	return result, nil
}

// UnregisterHarness removes this box's provider for harnessName — the daemon's counterpart to `register`.
func UnregisterHarness(httpClient *http.Client, apiBaseURL string, credentials *credstore.Credentials, harnessName string) error {
	proof, err := buildPossessionProof(credentials)
	if err != nil {
		return err
	}

	requestBody, err := json.Marshal(unregisterHarnessRequest{
		RefreshToken: proof.RefreshToken,
		Signature:    proof.Signature,
		UnixSeconds:  proof.UnixSeconds,
		Jti:          proof.Jti,
		Harness:      harnessName,
	})
	if err != nil {
		return err
	}

	httpResponse, err := httpClient.Post(apiBaseURL+"/tunnel/harnesses/unregister", "application/json", bytes.NewReader(requestBody))
	if err != nil {
		return err
	}
	defer httpResponse.Body.Close()

	if httpResponse.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(httpResponse.Body)
		return fmt.Errorf("unregistering harness %q: unexpected status %d: %s", harnessName, httpResponse.StatusCode, truncate(string(responseBody), 500))
	}
	return nil
}

func truncate(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	return value[:maxBytes]
}
