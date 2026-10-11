// discovery.go lets `register` (main.go) fill in what a harness needs to run WITHOUT an operator hand-editing
// root-owned config.yaml — whether the daemon's own CLI login exists (codex, claude), or a local OpenAI-compatible
// server sitting on one of a few well-known ports. Everything this file finds is written to a JSON file next to credstore's own state (never
// into config.yaml, and never touched by anything on the wire — see harness.go's package doc): Resolve reads it
// back on every call, merging it in under whatever config.yaml already says, so a fresh discovery reaches the
// running daemon on its very next invoke without a restart.
package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/agentparley/tunnel/internal/cliauth"
	"github.com/agentparley/tunnel/internal/config"
	"github.com/agentparley/tunnel/internal/credstore"
	"github.com/agentparley/tunnel/internal/shellrun"
)

const discoveryFileName = "harnesses.json"

// discoveredHarness is one harness's auto-discovered connection detail, keyed by harness name in the persisted
// file. URL serves the server family; a CLI harness records an empty entry, which only tells `doctor` that
// `register` has run for it.
type discoveredHarness struct {
	URL string `json:"url,omitempty"`
}

func discoveryFilePath() string {
	return filepath.Join(credstore.StateDir(), discoveryFileName)
}

// DiscoveredNames returns the harness names `register` has already discovered and persisted on this box, read-only
// — `doctor` uses this to know which harnesses to probe without re-running discovery (which writes to disk) itself.
func DiscoveredNames() ([]string, error) {
	discoveries, err := loadDiscoveries()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(discoveries))
	for name := range discoveries {
		names = append(names, name)
	}
	return names, nil
}

func loadDiscoveries() (map[string]discoveredHarness, error) {
	data, err := os.ReadFile(discoveryFilePath())
	if errors.Is(err, os.ErrNotExist) {
		return map[string]discoveredHarness{}, nil
	}
	if err != nil {
		return nil, err
	}

	discoveries := map[string]discoveredHarness{}
	if err := json.Unmarshal(data, &discoveries); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", discoveryFilePath(), err)
	}
	return discoveries, nil
}

// saveDiscovery records what register found for harnessName, written atomically (temp file + rename, mode 0600)
// so a crash mid-write can never leave the daemon's next Resolve reading a half-written file.
func saveDiscovery(harnessName string, discovered discoveredHarness) error {
	discoveries, err := loadDiscoveries()
	if err != nil {
		return err
	}
	discoveries[harnessName] = discovered

	data, err := json.MarshalIndent(discoveries, "", "  ")
	if err != nil {
		return err
	}

	stateDir := credstore.StateDir()
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	tempFile, err := os.CreateTemp(stateDir, ".harnesses-*.tmp")
	if err != nil {
		return err
	}
	tempPath := tempFile.Name()
	defer os.Remove(tempPath) // no-op once the rename below succeeds

	if _, err := tempFile.Write(data); err != nil {
		tempFile.Close()
		return err
	}
	if err := tempFile.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tempPath, 0o600); err != nil {
		return err
	}
	return os.Rename(tempPath, discoveryFilePath())
}

// Discover finds and persists whatever local connection detail harnessName needs beyond explicit config.yaml —
// called by `register` before Resolve, never from the invoke path (Resolve's own discovery-file read is all
// dispatch.go needs). found=false means the harness genuinely isn't installed/reachable anywhere this box knows to
// look, which register reports as "not found" rather than a failure — the ordinary case for most of the four
// harnesses on most boxes.
func Discover(harnessName string, tunnelConfig *config.Config, runAsUser *shellrun.User) (found bool, hint string, err error) {
	switch harnessName {
	case Codex:
		return discoverManagedCLI(Codex, cliauth.HasCodexLogin())
	case Claude:
		return discoverManagedCLI(Claude, cliauth.HasClaudeLogin())
	case Ollama:
		return true, "", nil // always has a default url — Detect's own reachability check is the presence signal
	case OpenAICompatibleLocal:
		return discoverOpenAICompatibleLocal(tunnelConfig)
	default:
		return false, "", fmt.Errorf("unknown harness %q", harnessName)
	}
}

// discoverManagedCLI reports a daemon-managed CLI as found once the daemon has its own login for it. Nothing about
// where the CLI lives is recorded: the daemon runs its own pinned copy. The empty entry only tells `doctor` that
// `register` has run for this harness.
func discoverManagedCLI(harnessName string, hasLogin bool) (found bool, hint string, err error) {
	if !hasLogin {
		return false, fmt.Sprintf("not signed in for AgentParley — run: agentparley register %s", harnessName), nil
	}
	return true, "", saveDiscovery(harnessName, discoveredHarness{})
}

// wellKnownOpenAICompatibleURLs is the fixed probe order openai-compatible-local's discovery tries when config
// gives no url — LM Studio, then vLLM, then llama.cpp; the first to answer /v1/models wins.
var wellKnownOpenAICompatibleURLs = []string{
	"http://localhost:1234", // LM Studio
	"http://localhost:8000", // vLLM
	"http://localhost:8080", // llama.cpp
}

const wellKnownProbeTimeout = 2 * time.Second

func discoverOpenAICompatibleLocal(tunnelConfig *config.Config) (found bool, hint string, err error) {
	if tunnelConfig.Harnesses[OpenAICompatibleLocal].URL != "" {
		return true, "", nil
	}

	for _, url := range wellKnownOpenAICompatibleURLs {
		if probeOpenAICompatibleModels(url) {
			return true, "", saveDiscovery(OpenAICompatibleLocal, discoveredHarness{URL: url})
		}
	}
	return false, "", nil
}

// probeOpenAICompatibleModels reports whether url answers GET /v1/models with a valid OpenAI-style model list —
// reusing openAICompatibleLocalModelsResponse's shape (see openaicompatiblelocal.go) so a server that returns 200
// with an unrelated body (a stray web server on that port) isn't mistaken for a match.
func probeOpenAICompatibleModels(url string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), wellKnownProbeTimeout)
	defer cancel()

	status, body, err := doGET(ctx, url+"/v1/models")
	if err != nil || status != 200 {
		return false
	}
	var listing openAICompatibleLocalModelsResponse
	return json.Unmarshal(body, &listing) == nil && len(listing.Data) > 0 && listing.Data[0].ID != ""
}
