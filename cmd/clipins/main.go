// Command clipins is the release pipeline's tool for the CLI pin list and for signing.
//
//	clipins check [-pins internal/managedcli/cli-pins.json] [-out candidate.json]
//	    looks up the newest Codex and Claude Code builds; when either is newer than the pin list it downloads the builds,
//	    computes their SHA-256, writes a candidate pin list (serial + 1) and prints changed=true. Otherwise it prints
//	    changed=false and writes nothing.
//
//	clipins sign <file>
//	    writes <file>.sig, an Ed25519 signature over the file's exact bytes by the key in RELEASE_SIGNING_KEY (base64).
//	    It signs the published CLI pin list and every box binary's checksum file; the daemon verifies both against the
//	    public key embedded in managedcli.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/agentparley/tunnel/internal/managedcli"
)

const (
	codexLatestReleaseURL = "https://api.github.com/repos/openai/codex/releases/latest"
	codexReleaseTagPrefix = "rust-v"
	codexDownloadBase     = "https://github.com/openai/codex/releases/download/"
	downloadTimeout       = 15 * time.Minute
)

// platforms maps the daemon's runtime.GOOS/GOARCH keys to the names each vendor uses in its release assets.
var platforms = []struct{ key, codex, claude string }{
	{"linux/amd64", "x86_64-unknown-linux-musl", "linux-x64"},
	{"linux/arm64", "aarch64-unknown-linux-musl", "linux-arm64"},
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "sign":
		if len(os.Args) != 3 {
			usage()
		}
		err = sign(os.Args[2])
	case "check":
		err = check(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "clipins:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: clipins check [-pins file] [-out file] | clipins sign <file>")
	os.Exit(2)
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

func check(args []string) error {
	flagSet := flag.NewFlagSet("check", flag.ExitOnError)
	pinsPath := flagSet.String("pins", "internal/managedcli/cli-pins.json", "the pin list to compare against")
	outputPath := flagSet.String("out", "candidate.json", "where to write the candidate pin list when something is newer")
	if err := flagSet.Parse(args); err != nil {
		return err
	}

	current, err := managedcli.ReadManifestFile(*pinsPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", *pinsPath, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*downloadTimeout)
	defer cancel()
	client := &http.Client{Timeout: downloadTimeout}

	candidate := managedcli.Manifest{Serial: current.Serial + 1, IssuedAt: time.Now().UTC().Truncate(time.Second), Harnesses: map[string]managedcli.Pin{}}
	changed := false
	for name, pin := range current.Harnesses {
		candidate.Harnesses[name] = pin
	}

	newestCodex, err := latestCodexVersion(ctx, client)
	if err != nil {
		return err
	}
	if isNewer(newestCodex, current.Version("codex")) {
		pin, err := codexPin(ctx, client, newestCodex)
		if err != nil {
			return err
		}
		candidate.Harnesses["codex"] = pin
		changed = true
	}

	base, err := claudeReleaseBase(current)
	if err != nil {
		return err
	}
	newestClaude, err := latestClaudeVersion(ctx, client, base)
	if err != nil {
		return err
	}
	if isNewer(newestClaude, current.Version("claude")) {
		pin, err := claudePin(ctx, client, base, newestClaude)
		if err != nil {
			return err
		}
		candidate.Harnesses["claude"] = pin
		changed = true
	}

	fmt.Printf("codex=%s\nclaude=%s\n", candidate.Version("codex"), candidate.Version("claude"))
	if !changed {
		fmt.Println("changed=false")
		return nil
	}
	encoded, err := json.MarshalIndent(candidate, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(*outputPath, append(encoded, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Println("changed=true")
	return nil
}

func get(ctx context.Context, client *http.Client, url string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if token := os.Getenv("GITHUB_TOKEN"); token != "" && strings.HasPrefix(url, "https://api.github.com/") {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("GET %s: status %d", url, response.StatusCode)
	}
	return response, nil
}

func latestCodexVersion(ctx context.Context, client *http.Client) (string, error) {
	response, err := get(ctx, client, codexLatestReleaseURL)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(response.Body).Decode(&release); err != nil {
		return "", fmt.Errorf("reading the latest Codex release: %w", err)
	}
	version := strings.TrimPrefix(release.TagName, codexReleaseTagPrefix)
	if version == release.TagName || version == "" {
		return "", fmt.Errorf("the latest Codex release tag %q does not look like %s<version>", release.TagName, codexReleaseTagPrefix)
	}
	return version, nil
}

// claudeReleaseBase is the official release base the current pin was taken from: the pin's own URL up to the version.
func claudeReleaseBase(current managedcli.Manifest) (string, error) {
	pin := current.Harnesses["claude"]
	for _, platform := range platforms {
		asset, ok := pin.Assets[platform.key]
		if !ok {
			continue
		}
		suffix := "/" + pin.Version + "/" + platform.claude + "/claude"
		if base, found := strings.CutSuffix(asset.URL, suffix); found {
			return base, nil
		}
	}
	return "", fmt.Errorf("the current Claude Code pin has no URL of the form <base>/<version>/<platform>/claude")
}

func latestClaudeVersion(ctx context.Context, client *http.Client, base string) (string, error) {
	response, err := get(ctx, client, base+"/latest")
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1024))
	if err != nil {
		return "", err
	}
	version := strings.TrimSpace(string(body))
	if _, ok := parseVersion(version); !ok {
		return "", fmt.Errorf("the latest Claude Code version %q is not a version number", version)
	}
	return version, nil
}

func codexPin(ctx context.Context, client *http.Client, version string) (managedcli.Pin, error) {
	pin := managedcli.Pin{Version: version, Assets: map[string]managedcli.Asset{}}
	for _, platform := range platforms {
		member := "codex-" + platform.codex
		url := codexDownloadBase + codexReleaseTagPrefix + version + "/" + member + ".tar.gz"
		checksum, err := sha256Of(ctx, client, url)
		if err != nil {
			return pin, err
		}
		pin.Assets[platform.key] = managedcli.Asset{URL: url, SHA256: checksum, ArchiveMember: member}
	}
	return pin, nil
}

// claudePin downloads each build, and requires its SHA-256 to match the one in the vendor's own release manifest.
func claudePin(ctx context.Context, client *http.Client, base, version string) (managedcli.Pin, error) {
	pin := managedcli.Pin{Version: version, Assets: map[string]managedcli.Asset{}}
	published, err := claudePublishedChecksums(ctx, client, base, version)
	if err != nil {
		return pin, err
	}
	for _, platform := range platforms {
		url := base + "/" + version + "/" + platform.claude + "/claude"
		checksum, err := sha256Of(ctx, client, url)
		if err != nil {
			return pin, err
		}
		if want := published[platform.claude]; want != "" && want != checksum {
			return pin, fmt.Errorf("%s: downloaded SHA-256 %s differs from the release manifest's %s", url, checksum, want)
		}
		pin.Assets[platform.key] = managedcli.Asset{URL: url, SHA256: checksum}
	}
	return pin, nil
}

func claudePublishedChecksums(ctx context.Context, client *http.Client, base, version string) (map[string]string, error) {
	response, err := get(ctx, client, base+"/"+version+"/manifest.json")
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	var manifest struct {
		Platforms map[string]struct {
			Checksum string `json:"checksum"`
		} `json:"platforms"`
	}
	if err := json.NewDecoder(response.Body).Decode(&manifest); err != nil {
		return nil, fmt.Errorf("reading the Claude Code release manifest: %w", err)
	}
	checksums := map[string]string{}
	for platform, entry := range manifest.Platforms {
		checksums[platform] = entry.Checksum
	}
	return checksums, nil
}

func sha256Of(ctx context.Context, client *http.Client, url string) (string, error) {
	response, err := get(ctx, client, url)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, response.Body); err != nil {
		return "", fmt.Errorf("downloading %s: %w", url, err)
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func parseVersion(version string) ([3]int, bool) {
	var parsed [3]int
	fields := strings.Split(version, ".")
	if len(fields) != 3 {
		return parsed, false
	}
	for index, field := range fields {
		number, err := strconv.Atoi(field)
		if err != nil || number < 0 {
			return parsed, false
		}
		parsed[index] = number
	}
	return parsed, true
}

// isNewer reports whether candidate is a higher x.y.z than current; a candidate that is not a plain version is never
// newer (a pre-release tag does not move the pin).
func isNewer(candidate, current string) bool {
	candidateParts, ok := parseVersion(candidate)
	if !ok {
		return false
	}
	currentParts, ok := parseVersion(current)
	if !ok {
		return true
	}
	for index := range candidateParts {
		if candidateParts[index] != currentParts[index] {
			return candidateParts[index] > currentParts[index]
		}
	}
	return false
}
