// Package selfupdate implements the `self-update` subcommand systemd runs as root via ExecStartPre, just before
// every daemon start: check a plain-text version pointer, and if it differs from the running binary, download,
// verify, and atomically swap in the new one. Every failure path here logs and returns nil — an update failure
// must never become a startup failure (design doc: "network down or a bad download means it logs and starts the
// existing binary").
package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
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
// self-update must never be able to clobber an arbitrary path found at runtime.
const installedBinaryPath = "/usr/local/bin/agentparley-tunnel"

const partialBinaryPath = "/usr/local/bin/.agentparley-tunnel.partial"

// pointerFetchTimeout bounds the tiny GET of the latest-version pointer.
const pointerFetchTimeout = 30 * time.Second

// binaryDownloadTimeout bounds the whole download+verify+swap. It is deliberately well under the systemd unit's
// TimeoutStartSec=420 so this subcommand always exits on its own terms rather than being killed mid-flight by
// systemd — a kill mid-rename could leave a corrupt binary in place, which would defeat the fail-open guarantee.
const binaryDownloadTimeout = 300 * time.Second

var semverPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// Run checks tunnelConfig.UpdateServer for a newer (or, for rollback, an older) release than the running binary,
// and swaps it in if so. It always returns nil except for a programmer error — every expected failure (network,
// bad data, checksum/signature mismatch) is logged and swallowed so the caller can still start the daemon.
func Run(tunnelConfig *config.Config) error {
	if !tunnelConfig.AutoUpdate {
		log.Println("self-update: auto-update disabled in config")
		return nil
	}

	httpClient := &http.Client{Timeout: pointerFetchTimeout}
	latestVersion, ok := fetchLatestVersion(httpClient, tunnelConfig.UpdateServer)
	if !ok {
		return nil
	}

	if latestVersion == version.Version {
		log.Printf("self-update: up to date (%s)", version.Version)
		return nil
	}

	log.Printf("self-update: installed version %s differs from published %s, updating", version.Version, latestVersion)
	applyUpdate(tunnelConfig.UpdateServer, latestVersion)
	return nil
}

// fetchLatestVersion GETs the plain-text version pointer and validates its shape. Any failure — network, non-200,
// garbage body — is logged here and reported as "no update available" to the caller.
func fetchLatestVersion(httpClient *http.Client, updateServer string) (string, bool) {
	url := strings.TrimSuffix(updateServer, "/") + "/latest-version"

	response, err := httpClient.Get(url)
	if err != nil {
		log.Printf("self-update: fetching %s: %v", url, err)
		return "", false
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		log.Printf("self-update: %s returned status %d", url, response.StatusCode)
		return "", false
	}

	body, err := io.ReadAll(response.Body)
	if err != nil {
		log.Printf("self-update: reading %s: %v", url, err)
		return "", false
	}

	latestVersion := strings.TrimSpace(string(body))
	if !semverPattern.MatchString(latestVersion) {
		log.Printf("self-update: %s did not return a valid version (got %q)", url, latestVersion)
		return "", false
	}
	return latestVersion, true
}

// applyUpdate downloads, verifies, and atomically installs the release at latestVersion. Every failure logs and
// returns — never propagates an error, per Run's contract.
func applyUpdate(updateServer, latestVersion string) {
	arch := runtime.GOARCH
	binaryURL := fmt.Sprintf("%s/releases/%s/agentparley-tunnel-linux-%s", strings.TrimSuffix(updateServer, "/"), latestVersion, arch)

	_ = os.Remove(partialBinaryPath)

	ctx, cancel := context.WithTimeout(context.Background(), binaryDownloadTimeout)
	defer cancel()

	binaryBytes, err := downloadFile(ctx, binaryURL)
	if err != nil {
		log.Printf("self-update: downloading %s: %v", binaryURL, err)
		return
	}

	expectedChecksum, err := downloadChecksum(ctx, binaryURL+".sha256")
	if err != nil {
		log.Printf("self-update: downloading %s.sha256: %v", binaryURL, err)
		return
	}

	actualChecksum := sha256.Sum256(binaryBytes)
	if hex.EncodeToString(actualChecksum[:]) != expectedChecksum {
		log.Printf("self-update: checksum mismatch for %s — discarding download", binaryURL)
		return
	}

	signature, err := downloadSignature(ctx, binaryURL+".sig")
	if err != nil {
		log.Printf("self-update: downloading %s.sig: %v", binaryURL, err)
		return
	}

	if !verifyRelease(binaryBytes, signature) {
		log.Printf("self-update: SIGNATURE VERIFICATION FAILED for %s — this may indicate tampering, refusing to install", binaryURL)
		return
	}

	if err := os.WriteFile(partialBinaryPath, binaryBytes, 0o755); err != nil {
		log.Printf("self-update: writing %s: %v", partialBinaryPath, err)
		_ = os.Remove(partialBinaryPath)
		return
	}

	if err := os.Rename(partialBinaryPath, installedBinaryPath); err != nil {
		log.Printf("self-update: installing %s: %v", installedBinaryPath, err)
		_ = os.Remove(partialBinaryPath)
		return
	}

	log.Printf("self-update: installed %s -> %s", version.Version, latestVersion)
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

func downloadSignature(ctx context.Context, url string) ([]byte, error) {
	body, err := downloadFile(ctx, url)
	if err != nil {
		return nil, err
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(body)))
	if err != nil {
		return nil, fmt.Errorf("decoding signature: %w", err)
	}
	return decoded, nil
}
