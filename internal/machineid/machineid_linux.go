package machineid

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/agentparley/tunnel/internal/credstore"
)

const systemMachineIDPath = "/etc/machine-id"

// The fallback lives beside the credentials, wherever credstore resolved those to — root gets /var/lib, a normal
// user gets ~/.agentparley. Resolving it independently here would split the identity across two directories the
// moment the daemon runs as a user rather than a service.
func fallbackPath() string { return filepath.Join(credstore.StateDir(), "machine-id") }

// readRawID prefers the kernel-maintained /etc/machine-id (present on every systemd distribution) and falls back
// to a UUID this daemon generates once and persists — covering containers and minimal images that never write
// /etc/machine-id.
func readRawID() (string, error) {
	if id, err := readTrimmed(systemMachineIDPath); err == nil && id != "" {
		return id, nil
	}
	return readOrCreateFallback()
}

func readOrCreateFallback() (string, error) {
	if id, err := readTrimmed(fallbackPath()); err == nil && id != "" {
		return id, nil
	}

	id, err := newUUID()
	if err != nil {
		return "", fmt.Errorf("generating fallback machine id: %w", err)
	}
	if err := os.MkdirAll(credstore.StateDir(), 0o700); err != nil {
		return "", fmt.Errorf("creating %s: %w", credstore.StateDir(), err)
	}
	if err := os.WriteFile(fallbackPath(), []byte(id+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("writing %s: %w", fallbackPath(), err)
	}
	return id, nil
}

func readTrimmed(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// newUUID generates a random (v4) UUID without pulling in an external dependency for one call site.
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", errors.New("reading random bytes for machine id: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10

	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
