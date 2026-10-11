package managedcli

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
)

// releasePublicKey is the public half of RELEASE_SIGNING_KEY: the one Ed25519 key that signs the published pin list
// and every box binary's checksum file (selfupdate). The private half lives only in the release CI's secrets.
const releasePublicKey = "hfCxgYekG//jMmbrYqmnbezoBnimlDxjRSfzZHzFdpc="

// VerifySignature checks that signatureBase64 is a valid Ed25519 signature by the release key over exactly body.
func VerifySignature(body, signatureBase64 []byte) error {
	publicKey, err := base64.StdEncoding.DecodeString(releasePublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("the embedded release public key is malformed")
	}
	signature, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(signatureBase64)))
	if err != nil {
		return fmt.Errorf("signature is not valid base64: %w", err)
	}
	if len(signature) != ed25519.SignatureSize || !ed25519.Verify(publicKey, body, signature) {
		return fmt.Errorf("signature does not match")
	}
	return nil
}
