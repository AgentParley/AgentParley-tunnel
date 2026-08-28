package selfupdate

import (
	"crypto/ed25519"
	"encoding/base64"
)

// releasePublicKeyBase64 is filled in at the signing key ceremony (see release/sign -keygen) with the base64
// 32-byte Ed25519 public key. The placeholder below is deliberately NOT a valid 32-byte key — it must make every
// verification fail rather than silently pass, so a build that forgot the ceremony fails closed instead of
// trusting an unsigned release.
const releasePublicKeyBase64 = "REPLACE-AT-KEY-CEREMONY-this-placeholder-must-never-verify"

// releasePublicKey is decoded once. A malformed or wrong-length placeholder leaves it nil, which verifyRelease
// treats as "never verifies" rather than panicking.
var releasePublicKey ed25519.PublicKey

func init() {
	decoded, err := base64.StdEncoding.DecodeString(releasePublicKeyBase64)
	if err != nil || len(decoded) != ed25519.PublicKeySize {
		return
	}
	releasePublicKey = ed25519.PublicKey(decoded)
}

// verifyRelease reports whether signature is a valid Ed25519 signature of data under the baked-in release public
// key. A nil/wrong-length key (an un-ceremonied placeholder) always fails here — see releasePublicKeyBase64.
func verifyRelease(data, signature []byte) bool {
	if len(releasePublicKey) != ed25519.PublicKeySize {
		return false
	}
	return ed25519.Verify(releasePublicKey, data, signature)
}
