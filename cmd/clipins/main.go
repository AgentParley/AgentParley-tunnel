// Command clipins is the release pipeline's signing tool: `clipins sign <file>` writes <file>.sig, an Ed25519
// signature over the file's exact bytes by the key in RELEASE_SIGNING_KEY. It signs the published CLI pin list and
// every box binary's checksum file; the daemon verifies both against the public key embedded in managedcli.
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

func main() {
	if len(os.Args) != 3 || os.Args[1] != "sign" {
		fmt.Fprintln(os.Stderr, "usage: clipins sign <file>")
		os.Exit(2)
	}
	if err := sign(os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, "clipins:", err)
		os.Exit(1)
	}
}

func sign(path string) error {
	encodedKey := strings.TrimSpace(os.Getenv("RELEASE_SIGNING_KEY"))
	if encodedKey == "" {
		return fmt.Errorf("RELEASE_SIGNING_KEY is not set")
	}
	privateKey, err := base64.StdEncoding.DecodeString(encodedKey)
	if err != nil || len(privateKey) != ed25519.PrivateKeySize {
		return fmt.Errorf("RELEASE_SIGNING_KEY is not a base64 Ed25519 private key")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	signature := ed25519.Sign(privateKey, body)
	return os.WriteFile(path+".sig", []byte(base64.StdEncoding.EncodeToString(signature)+"\n"), 0o644)
}
