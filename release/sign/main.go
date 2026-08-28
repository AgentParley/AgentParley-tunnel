// Command sign is the release signing tool: it generates the Ed25519 keypair that backs the daemon's self-update
// verification, and signs release binaries with it. Stdlib only — this never ships to a customer box, but it
// shares the crypto/ed25519 primitives internal/selfupdate verifies against.
//
//	go run ./release/sign -keygen        generate a keypair, print private/public as base64
//	go run ./release/sign <file>...      sign each file, writing <file>.sha256 and <file>.sig next to it
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	if os.Args[1] == "-keygen" {
		runKeygen()
		return
	}

	if err := runSign(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "sign:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: sign -keygen | sign <file>...")
}

func runKeygen() {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sign: generating keypair:", err)
		os.Exit(1)
	}

	fmt.Println("private:", base64.StdEncoding.EncodeToString(privateKey))
	fmt.Println("public: ", base64.StdEncoding.EncodeToString(publicKey))
	fmt.Println()
	fmt.Println("Put the private key in the Woodpecker secret tunnel_release_signing_key AND an offline backup " +
		"(password manager) — losing both means every existing install must be manually re-installed to trust a " +
		"new key. Put the public key in internal/selfupdate/publickey.go.")
}

func runSign(paths []string) error {
	privateKey, err := loadSigningKey()
	if err != nil {
		return err
	}

	for _, path := range paths {
		if err := signFile(privateKey, path); err != nil {
			return fmt.Errorf("signing %s: %w", path, err)
		}
	}
	return nil
}

func loadSigningKey() (ed25519.PrivateKey, error) {
	encoded := os.Getenv("TUNNEL_RELEASE_SIGNING_KEY")
	if encoded == "" {
		return nil, fmt.Errorf("TUNNEL_RELEASE_SIGNING_KEY is empty or not set")
	}

	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("TUNNEL_RELEASE_SIGNING_KEY is not valid base64: %w", err)
	}
	if len(decoded) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("TUNNEL_RELEASE_SIGNING_KEY decodes to %d bytes, want %d", len(decoded), ed25519.PrivateKeySize)
	}
	return ed25519.PrivateKey(decoded), nil
}

func signFile(privateKey ed25519.PrivateKey, path string) error {
	fileBytes, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	checksum := sha256.Sum256(fileBytes)
	checksumLine := fmt.Sprintf("%s  %s\n", hex.EncodeToString(checksum[:]), filepath.Base(path))
	if err := os.WriteFile(path+".sha256", []byte(checksumLine), 0o644); err != nil {
		return fmt.Errorf("writing checksum sidecar: %w", err)
	}

	signature := ed25519.Sign(privateKey, fileBytes)
	signatureLine := base64.StdEncoding.EncodeToString(signature) + "\n"
	if err := os.WriteFile(path+".sig", []byte(signatureLine), 0o644); err != nil {
		return fmt.Errorf("writing signature sidecar: %w", err)
	}
	return nil
}
