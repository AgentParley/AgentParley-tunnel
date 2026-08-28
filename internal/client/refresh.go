package client

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/agentparley/tunnel/internal/credstore"
)

// terminalRefreshError marks a refresh rejection the daemon must NOT retry — a revoked install, an unknown token,
// or a key that no longer matches the pin (design doc edge case 3: "Never silently re-pin; the user must delete
// and re-enrol"). Distinguished from a transient network failure so Run's reconnect loop knows to stop.
type terminalRefreshError struct {
	detail string
}

func (e *terminalRefreshError) Error() string {
	return "tunnel refresh rejected: " + e.detail
}

// refreshAccessToken signs the possession proof with the pinned Ed25519 key and exchanges the refresh token for a
// fresh, routing-key-scoped access token. Called once per connect attempt — see client.go's connectOnce.
func refreshAccessToken(httpClient *http.Client, apiBaseURL string, creds *credstore.Credentials) (accessToken string, err error) {
	privateKeySeed, err := base64.StdEncoding.DecodeString(creds.PrivateKey)
	if err != nil {
		return "", fmt.Errorf("decoding the stored install key: %w", err)
	}
	privateKey := ed25519.NewKeyFromSeed(privateKeySeed)

	refreshTokenHashBytes := sha256.Sum256([]byte(creds.RefreshToken))
	refreshTokenHash := hex.EncodeToString(refreshTokenHashBytes[:])

	unixSeconds := time.Now().Unix()
	jti, err := randomHex(16)
	if err != nil {
		return "", fmt.Errorf("generating a possession-proof jti: %w", err)
	}

	signedText := fmt.Sprintf("%s|%d|%s", refreshTokenHash, unixSeconds, jti)
	signature := ed25519.Sign(privateKey, []byte(signedText))

	requestBody, err := json.Marshal(refreshRequest{
		RefreshToken: creds.RefreshToken,
		Signature:    base64.StdEncoding.EncodeToString(signature),
		UnixSeconds:  unixSeconds,
		Jti:          jti,
	})
	if err != nil {
		return "", err
	}

	httpResponse, err := httpClient.Post(apiBaseURL+"/tunnel/token", "application/json", bytes.NewReader(requestBody))
	if err != nil {
		return "", err
	}
	defer httpResponse.Body.Close()

	if httpResponse.StatusCode == http.StatusUnauthorized {
		// The control plane deliberately returns a bare 401 with no body for every refresh rejection — one opaque
		// failure, so a stolen refresh token and a forged signature look identical on the wire.
		return "", &terminalRefreshError{detail: "unknown/revoked refresh token, or an invalid possession proof"}
	}
	if httpResponse.StatusCode != http.StatusOK {
		return "", fmt.Errorf("refreshing the access token: unexpected status %d", httpResponse.StatusCode)
	}

	var response refreshResponse
	if err := json.NewDecoder(httpResponse.Body).Decode(&response); err != nil {
		return "", fmt.Errorf("decoding the refresh response: %w", err)
	}
	return response.AccessToken, nil
}

func randomHex(numBytes int) (string, error) {
	b := make([]byte, numBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

type refreshRequest struct {
	RefreshToken string `json:"refreshToken"`
	Signature    string `json:"signature"`
	UnixSeconds  int64  `json:"unixSeconds"`
	Jti          string `json:"jti"`
}

type refreshResponse struct {
	AccessToken string `json:"accessToken"`
	RoutingKey  string `json:"routingKey"`
	SSHHostID   string `json:"sshHostId"`
	ExpiresAt   string `json:"expiresAt"`
}
