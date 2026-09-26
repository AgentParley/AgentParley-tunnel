// Package selfupdate is the shared engine behind `agentparley update`: fetch the published version pointer,
// compare it to the running binary, and if different, download, verify, and atomically swap in the new one. Two
// callers wrap Check with different failure contracts — main.runUpdate (a human's explicit call: every failure
// propagates and the command exits non-zero) and CheckFailOpen (the systemd unit's `ExecStartPre`, run via
// `agentparley update --from-service`: every failure is logged and swallowed, because a broken update check must
// never block the daemon from starting on the binary that's already there).
package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/agentparley/tunnel/internal/config"
	"github.com/agentparley/tunnel/internal/version"
)

// installedBinaryPath is a fixed constant, not derived from os.Executable() — a dev running a checkout build of
// update must never be able to clobber an arbitrary path found at runtime.
const installedBinaryPath = "/usr/local/bin/agentparley"

const partialBinaryPath = "/usr/local/bin/.agentparley.partial"

// pointerFetchTimeout bounds the tiny GET of the latest-version pointer.
const pointerFetchTimeout = 30 * time.Second

// binaryDownloadTimeout bounds the whole download+verify+swap. It is deliberately well under the systemd unit's
// TimeoutStartSec=420 so this subcommand always exits on its own terms rather than being killed mid-flight by
// systemd — a kill mid-rename could leave a corrupt binary in place, which would defeat the fail-open guarantee.
const binaryDownloadTimeout = 300 * time.Second

var semverPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// Result reports what Check found and did.
type Result struct {
	PreviousVersion string
	LatestVersion   string
	Updated         bool
}

// Check fetches updateServer's published version and, if it differs from the running binary — not just "newer": a
// deliberately older value is how rollback works — downloads, verifies, and atomically installs it. Every failure
// is returned to the caller; CheckFailOpen is the wrapper for a caller that instead needs to log and continue.
func Check(updateServer string) (Result, error) {
	httpClient := &http.Client{Timeout: pointerFetchTimeout}
	latestVersion, err := FetchLatestVersion(httpClient, updateServer)
	if err != nil {
		return Result{}, err
	}

	result := Result{PreviousVersion: version.Version, LatestVersion: latestVersion}
	if latestVersion == version.Version {
		return result, nil
	}

	if err := applyUpdate(updateServer, latestVersion); err != nil {
		return Result{}, err
	}
	result.Updated = true
	return result, nil
}

// CheckFailOpen is systemd's ExecStartPre contract (`agentparley update --from-service`, run as root just before
// every start): auto_update: false, or any failure anywhere in Check — network down, a 404, a bad checksum, disk
// full — is logged and swallowed rather than propagated, so a broken update check never stops the daemon from
// starting on the binary that's already there.
func CheckFailOpen(tunnelConfig *config.Config) {
	if !tunnelConfig.AutoUpdate {
		log.Println("update: auto-update disabled in config")
		return
	}

	result, err := Check(tunnelConfig.UpdateServer)
	if err != nil {
		log.Printf("update: %v", err)
		return
	}
	if result.Updated {
		log.Printf("update: installed %s -> %s", result.PreviousVersion, result.LatestVersion)
	} else {
		log.Printf("update: up to date (%s)", version.Version)
	}
}

// FetchLatestVersion GETs the plain-text version pointer and validates its shape — exported so `doctor`'s version
// check can reuse the exact same fetch-and-validate logic Check itself uses, rather than a second implementation.
func FetchLatestVersion(httpClient *http.Client, updateServer string) (string, error) {
	url := strings.TrimSuffix(updateServer, "/") + "/latest-version"

	response, err := httpClient.Get(url)
	if err != nil {
		return "", fmt.Errorf("fetching %s: %w", url, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s returned status %d", url, response.StatusCode)
	}

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", url, err)
	}

	latestVersion := strings.TrimSpace(string(body))
	if !semverPattern.MatchString(latestVersion) {
		return "", fmt.Errorf("%s did not return a valid version (got %q)", url, latestVersion)
	}
	return latestVersion, nil
}

// applyUpdate downloads, verifies, and atomically installs the release at latestVersion.
func applyUpdate(updateServer, latestVersion string) error {
	arch := runtime.GOARCH
	binaryURL := fmt.Sprintf("%s/releases/%s/agentparley-linux-%s", strings.TrimSuffix(updateServer, "/"), latestVersion, arch)

	_ = os.Remove(partialBinaryPath)

	ctx, cancel := context.WithTimeout(context.Background(), binaryDownloadTimeout)
	defer cancel()

	binaryBytes, err := downloadFile(ctx, binaryURL)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", binaryURL, err)
	}

	expectedChecksum, err := downloadChecksum(ctx, binaryURL+".sha256")
	if err != nil {
		return fmt.Errorf("downloading %s.sha256: %w", binaryURL, err)
	}

	actualChecksum := sha256.Sum256(binaryBytes)
	if hex.EncodeToString(actualChecksum[:]) != expectedChecksum {
		return fmt.Errorf("checksum mismatch for %s — discarding download", binaryURL)
	}

	if err := os.WriteFile(partialBinaryPath, binaryBytes, 0o755); err != nil {
		_ = os.Remove(partialBinaryPath)
		return fmt.Errorf("writing %s: %w", partialBinaryPath, err)
	}

	if err := os.Rename(partialBinaryPath, installedBinaryPath); err != nil {
		_ = os.Remove(partialBinaryPath)
		return fmt.Errorf("installing %s: %w", installedBinaryPath, err)
	}

	return nil
}

func downloadFile(ctx context.Context, url string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", response.StatusCode)
	}
	return io.ReadAll(response.Body)
}

// downloadChecksum extracts the hex digest from a sha256sum-format sidecar ("<hex>  <filename>").
func downloadChecksum(ctx context.Context, url string) (string, error) {
	body, err := downloadFile(ctx, url)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(body))
	if len(fields) == 0 {
		return "", fmt.Errorf("empty checksum file")
	}
	return fields[0], nil
}
