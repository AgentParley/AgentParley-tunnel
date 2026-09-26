// discovery.go lets `register` (main.go) fill in what a harness needs to run WITHOUT an operator hand-editing
// root-owned config.yaml — a codex/claude CLI installed under a login shell's own PATH (nvm, ~/.npm-global) that
// the daemon's own minimal systemd/sudo PATH never sees, or a local OpenAI-compatible server sitting on one of a
// few well-known ports. Everything this file finds is written to a JSON file next to credstore's own state (never
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
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentparley/tunnel/internal/config"
	"github.com/agentparley/tunnel/internal/credstore"
	"github.com/agentparley/tunnel/internal/shellrun"
)

const discoveryFileName = "harnesses.json"

// discoveredHarness is one harness's auto-discovered connection detail, keyed by harness name in the persisted
// file. Command/Path serve the CLI family: an absolute binary path and the PATH it needs again at invoke time (see
// clihelpers.go's envWithPath) — Path is recorded whenever discovery ran at all, even when config.yaml pins an
// explicit Command, because a #!/usr/bin/env node launcher needs node on PATH regardless of which path found it.
// URL serves the server family.
type discoveredHarness struct {
	Command string `json:"command,omitempty"`
	Path    string `json:"path,omitempty"`
	URL     string `json:"url,omitempty"`
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
		return discoverCLI(Codex, "codex", tunnelConfig, runAsUser)
	case Claude:
		return discoverCLI(Claude, "claude", tunnelConfig, runAsUser)
	case Ollama:
		return true, "", nil // always has a default url — Detect's own reachability check is the presence signal
	case OpenAICompatibleLocal:
		return discoverOpenAICompatibleLocal(tunnelConfig)
	default:
		return false, "", fmt.Errorf("unknown harness %q", harnessName)
	}
}

// discoverCLI resolves an absolute command and the PATH it needs at invoke time for one CLI-family harness, and
// persists both — even when config.yaml already pins an explicit command, because the PATH is still needed and
// config.yaml has no key of its own for it (a real failure this fixed: register run from an interactive terminal
// found codex fine via plain PATH, but the systemd-run daemon's own PATH never saw it, because only the command
// was being remembered, not the environment that made it resolve). Preference order: the run-as user's OWN login
// shell (the real environment an nvm/npm-global install lives in, and the only thing sudo/systemd's stripped-down
// PATH can't see) tried first and best-effort — any reason it doesn't pan out falls through rather than failing
// outright — then this process's own PATH, which is at least SOMETHING even when it isn't the richest answer.
func discoverCLI(harnessName, defaultCommand string, tunnelConfig *config.Config, runAsUser *shellrun.User) (found bool, hint string, err error) {
	explicitCommand := tunnelConfig.Harnesses[harnessName].Command
	lookupName := defaultCommand
	if explicitCommand != "" {
		lookupName = explicitCommand
	}

	if os.Getuid() == runAsUser.UID {
		if command, path, _ := discoverViaLoginShell(runAsUser, lookupName); command != "" {
			return true, "", saveDiscovery(harnessName, discoveredHarness{Command: command, Path: path})
		}
	}

	if resolvedPath, lookErr := exec.LookPath(lookupName); lookErr == nil {
		return true, "", saveDiscovery(harnessName, discoveredHarness{Command: resolvedPath, Path: os.Getenv("PATH")})
	}

	if explicitCommand != "" {
		return false, "", fmt.Errorf("configured command %q was not found on %s's login shell or this process's own PATH", explicitCommand, runAsUser.Username)
	}
	// Discovered paths must belong to the user the daemon actually runs harnesses as — asking a DIFFERENT user's
	// login shell would hand the daemon a path it has no business trusting. main.go's loadConfigAndCredentials
	// already refuses register entirely on a run_as mismatch before Discover is ever reached, so this only ever
	// fires as a second line of defense.
	if os.Getuid() != runAsUser.UID {
		return false, fmt.Sprintf("not on this process's PATH — re-run as: sudo -u %s agentparley register", runAsUser.Username), nil
	}
	return false, "", nil
}

const loginShellDiscoveryTimeout = 10 * time.Second

const (
	cmdMarker  = "__AP_CMD__"
	pathMarker = "__AP_PATH__"
)

// discoverViaLoginShell asks the run-as user's OWN login shell where bin lives — `-l -i` loads the same rc files
// an interactive terminal would, so `command -v` resolves exactly what the user would get by typing the command
// themselves (an nvm `.zshrc`/`.bashrc` PATH edit included). The marker prefixes let the answer survive whatever
// rc-file noise (a banner, fastfetch, a stray echo) prints ahead of it — only lines with the exact prefix are
// read. Stdin is left nil (the null device, per os/exec), so an rc file that waits on input can't hang the probe;
// the context timeout is the second, belt-and-braces guard against that. Returns command == "" (not an error) when
// the shell simply doesn't have bin — an error is reserved for the discovery mechanism itself failing (the shell
// timing out or exiting non-zero), which is best-effort from the caller's side, not fatal to discovery overall.
func discoverViaLoginShell(user *shellrun.User, bin string) (command, path string, err error) {
	if user.Shell == "" {
		return "", "", nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), loginShellDiscoveryTimeout)
	defer cancel()

	script := fmt.Sprintf(`printf '\n%s%%s\n' "$(command -v %s)"; printf '%s%%s\n' "$PATH"`, cmdMarker, bin, pathMarker)
	subprocess := exec.CommandContext(ctx, user.Shell, "-l", "-i", "-c", script)

	output, runErr := subprocess.Output()
	if ctx.Err() != nil {
		return "", "", fmt.Errorf("%s -l -i timed out after %s", user.Shell, loginShellDiscoveryTimeout)
	}
	if runErr != nil {
		return "", "", fmt.Errorf("%s -l -i -c failed: %w", user.Shell, runErr)
	}

	command = parseMarkerLine(string(output), cmdMarker)
	path = parseMarkerLine(string(output), pathMarker)
	if command == "" {
		return "", "", nil
	}
	if !filepath.IsAbs(command) {
		return "", "", fmt.Errorf("login shell reported a non-absolute path %q for %s", command, bin)
	}
	info, statErr := os.Stat(command)
	if statErr != nil {
		return "", "", fmt.Errorf("login shell reported %q for %s, but it does not exist: %w", command, bin, statErr)
	}
	if info.IsDir() || info.Mode()&0o111 == 0 {
		return "", "", fmt.Errorf("login shell reported %q for %s, but it is not an executable file", command, bin)
	}
	return command, path, nil
}

// parseMarkerLine returns the value following marker on whichever line of output carries it, trimmed, or "" if no
// line does. Output commonly holds unrelated rc-file lines before and after the markers.
func parseMarkerLine(output, marker string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		if value, ok := strings.CutPrefix(line, marker); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
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
