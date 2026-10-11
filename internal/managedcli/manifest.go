// Package managedcli owns the CLI binaries (Codex, Claude Code) the daemon runs: which version is pinned, where it
// comes from, and the verified copy on this box. The pin list is embedded in the daemon as a floor and refreshed
// from a signed copy the release pipeline publishes, so a new CLI build reaches boxes without a new daemon.
package managedcli

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"runtime"
	"time"
)

//go:embed cli-pins.json
var embeddedPins []byte

type Asset struct {
	URL           string `json:"url"`
	SHA256        string `json:"sha256"`
	ArchiveMember string `json:"archiveMember"`
}

type Pin struct {
	Version string           `json:"version"`
	Assets  map[string]Asset `json:"assets"`
}

type Manifest struct {
	Serial    int64          `json:"serial"`
	IssuedAt  time.Time      `json:"issuedAt"`
	Harnesses map[string]Pin `json:"harnesses"`
}

// Embedded returns the pin list compiled into this binary. It carries no signature: it is part of the binary.
func Embedded() Manifest {
	manifest, err := parseManifest(embeddedPins)
	if err != nil {
		panic(fmt.Sprintf("embedded cli-pins.json is invalid: %v", err))
	}
	return manifest
}

// Verify parses body only after sig (base64) proves the release key signed exactly these bytes.
func Verify(body, sig []byte) (Manifest, error) {
	if err := VerifySignature(body, sig); err != nil {
		return Manifest{}, err
	}
	return parseManifest(body)
}

func parseManifest(body []byte) (Manifest, error) {
	var manifest Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("parsing pin list: %w", err)
	}
	if manifest.Serial <= 0 || len(manifest.Harnesses) == 0 {
		return Manifest{}, fmt.Errorf("pin list has no serial or no harnesses")
	}
	return manifest, nil
}

func platformKey() string {
	return runtime.GOOS + "/" + runtime.GOARCH
}

// Version returns the pinned version of name, or "" when the manifest does not pin it.
func (m Manifest) Version(name string) string {
	return m.Harnesses[name].Version
}
