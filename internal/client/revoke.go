package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/agentparley/tunnel/internal/credstore"
)

// Revoke authenticates exactly like refreshAccessToken (refresh token + Ed25519 possession proof) and calls
// POST /tunnel/revoke, the same server-side teardown a portal delete performs. Exported for main's logout command.
func Revoke(httpClient *http.Client, apiBaseURL string, credentials *credstore.Credentials) error {
	proof, err := buildPossessionProof(credentials)
	if err != nil {
		return err
	}

	requestBody, err := json.Marshal(revokeRequest{
		RefreshToken: proof.RefreshToken,
		Signature:    proof.Signature,
		UnixSeconds:  proof.UnixSeconds,
		Jti:          proof.Jti,
	})
	if err != nil {
		return err
	}

	httpResponse, err := httpClient.Post(apiBaseURL+"/tunnel/revoke", "application/json", bytes.NewReader(requestBody))
	if err != nil {
		return err
	}
	defer httpResponse.Body.Close()

	if httpResponse.StatusCode != http.StatusOK {
		return fmt.Errorf("revoking this install: unexpected status %d", httpResponse.StatusCode)
	}
	return nil
}

type revokeRequest struct {
	RefreshToken string `json:"refreshToken"`
	Signature    string `json:"signature"`
	UnixSeconds  int64  `json:"unixSeconds"`
	Jti          string `json:"jti"`
}
