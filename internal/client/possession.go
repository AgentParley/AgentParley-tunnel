package client

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/agentparley/tunnel/internal/credstore"
)

// proofFields is the possession proof every daemon-facing endpoint (/tunnel/token, /tunnel/revoke, and now
// /tunnel/harnesses{,/remove}) authenticates with: the refresh token plus an Ed25519 signature over
// {sha256(refreshToken)}|{unixSeconds}|{jti}, proving the caller holds the install's pinned private key without
// ever putting that key on the wire.
type proofFields struct {
	RefreshToken string
	Signature    string
	UnixSeconds  int64
	Jti          string
}

// buildPossessionProof signs a fresh proof with credentials' pinned Ed25519 key. Extracted from what revoke.go and
// refresh.go each built inline, now shared by every possession-proof caller including the harness registration
// verbs.
func buildPossessionProof(credentials *credstore.Credentials) (proofFields, error) {
	privateKeySeed, err := base64.StdEncoding.DecodeString(credentials.PrivateKey)
	if err != nil {
		return proofFields{}, fmt.Errorf("decoding the stored install key: %w", err)
	}
	privateKey := ed25519.NewKeyFromSeed(privateKeySeed)

	refreshTokenHashBytes := sha256.Sum256([]byte(credentials.RefreshToken))
	refreshTokenHash := hex.EncodeToString(refreshTokenHashBytes[:])

	unixSeconds := time.Now().Unix()
	jti, err := randomHex(16)
	if err != nil {
		return proofFields{}, fmt.Errorf("generating a possession-proof jti: %w", err)
	}

	signedText := fmt.Sprintf("%s|%d|%s", refreshTokenHash, unixSeconds, jti)
	signature := ed25519.Sign(privateKey, []byte(signedText))

	return proofFields{
		RefreshToken: credentials.RefreshToken,
		Signature:    base64.StdEncoding.EncodeToString(signature),
		UnixSeconds:  unixSeconds,
		Jti:          jti,
	}, nil
}
