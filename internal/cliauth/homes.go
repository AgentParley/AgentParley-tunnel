// Package cliauth owns the daemon's private CLI logins. The daemon never reads or writes the owner's own ~/.claude
// or ~/.codex: each CLI runs from a folder under the daemon's state dir with a login made just for AgentParley.
package cliauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"

	"github.com/agentparley/tunnel/internal/atomicfile"
	"github.com/agentparley/tunnel/internal/cliround"
	"github.com/agentparley/tunnel/internal/credstore"
	"github.com/agentparley/tunnel/internal/sessions"
)

const (
	privateDirMode  = 0o700
	privateFileMode = 0o600
	tokenFileName   = "claude-oauth-token"
)

func cliDir() string {
	return filepath.Join(credstore.StateDir(), "cli")
}

// ClaudeHome is the CLAUDE_CONFIG_DIR every Claude Code process of the daemon runs with.
func ClaudeHome() string {
	return ensureDir(filepath.Join(cliDir(), "claude-home"))
}

// CodexHome is the CODEX_HOME every Codex process of the daemon runs with.
func CodexHome() string {
	return ensureDir(filepath.Join(cliDir(), "codex-home"))
}

// ClaudeWorkDir is the only folder Claude Code is started in, on tmpfs so nothing it writes there survives a reboot.
func ClaudeWorkDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	return ensureDir(filepath.Join(sessions.StateRoot(home), "claude", "cwd"))
}

// ClaudeSocketDir holds the private unix sockets the Claude Code mod talks to, on tmpfs.
func ClaudeSocketDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	return ensureDir(filepath.Join(sessions.StateRoot(home), "claude"))
}

// CodexWorkDir is the only folder the Codex app-server is started in, on tmpfs so nothing it writes there survives a
// reboot.
func CodexWorkDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	return ensureDir(filepath.Join(sessions.StateRoot(home), "codex"))
}

// CodexCommand is the one way the daemon launches its private Codex: in the private work folder, with an explicit
// CODEX_HOME that replaces any the daemon inherited from its own environment.
func CodexCommand(ctx context.Context, binary string, args ...string) *exec.Cmd {
	command := exec.CommandContext(ctx, binary, args...)
	command.Dir = CodexWorkDir()
	command.Env = append(withoutEnv(os.Environ(), "CODEX_HOME"), "CODEX_HOME="+CodexHome())
	return command
}

func withoutEnv(environment []string, name string) []string {
	kept := make([]string, 0, len(environment))
	for _, entry := range environment {
		if !strings.HasPrefix(entry, name+"=") {
			kept = append(kept, entry)
		}
	}
	return kept
}

func ensureDir(path string) string {
	if err := os.MkdirAll(path, privateDirMode); err != nil {
		log.Printf("cliauth: creating %s: %v", path, err)
	}
	return path
}

func tokenPath() string {
	return filepath.Join(cliDir(), tokenFileName)
}

// seedMutex keeps concurrent starts from interleaving their read-modify-write of the shared .claude.json.
var seedMutex sync.Mutex

// SeedClaudeConfig writes the onboarding and trust state Claude Code would otherwise ask for on screen, keeping any
// other key Claude Code wrote itself.
func SeedClaudeConfig(pinnedVersion string) error {
	seedMutex.Lock()
	defer seedMutex.Unlock()
	configPath := filepath.Join(ClaudeHome(), ".claude.json")
	settings := map[string]any{}
	original := map[string]any{}
	data, err := os.ReadFile(configPath)
	if err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			settings = map[string]any{}
		} else {
			_ = json.Unmarshal(data, &original)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	settings["hasCompletedOnboarding"] = true
	settings["lastOnboardingVersion"] = pinnedVersion
	settings["theme"] = "dark"
	settings["autoCompactEnabled"] = false

	projects, ok := settings["projects"].(map[string]any)
	if !ok {
		projects = map[string]any{}
	}
	workDir := ClaudeWorkDir()
	project, ok := projects[workDir].(map[string]any)
	if !ok {
		project = map[string]any{}
	}
	project["hasTrustDialogAccepted"] = true
	projects[workDir] = project
	settings["projects"] = projects

	if err == nil && reflect.DeepEqual(original, settings) {
		return nil
	}
	encoded, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(configPath, encoded)
}

// ClaudeToken returns the long-lived Claude Code login token stored at register time.
func ClaudeToken() (string, error) {
	data, err := os.ReadFile(tokenPath())
	if errors.Is(err, os.ErrNotExist) {
		return "", cliround.ErrSignedOut
	}
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", cliround.ErrSignedOut
	}
	return token, nil
}

func HasClaudeLogin() bool {
	_, err := ClaudeToken()
	return err == nil
}

func HasCodexLogin() bool {
	_, err := os.Stat(filepath.Join(CodexHome(), "auth.json"))
	return err == nil
}

// Logout deletes the named CLI's token or private home — the inverse of the login.
func Logout(name string) error {
	switch name {
	case "claude":
		if err := os.Remove(tokenPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return os.RemoveAll(filepath.Join(cliDir(), "claude-home"))
	case "codex":
		return os.RemoveAll(filepath.Join(cliDir(), "codex-home"))
	default:
		return fmt.Errorf("%s has no daemon login", name)
	}
}
