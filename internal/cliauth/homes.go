// Package cliauth owns the daemon's private CLI logins. The daemon never reads or writes the owner's own ~/.claude
// or ~/.codex: each CLI runs from a folder under the daemon's state dir with a login made just for AgentParley.
package cliauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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

// CodexWorkDir is the only folder the Codex app-server is started in, on tmpfs so nothing it writes there survives a
// reboot.
func CodexWorkDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	return ensureDir(filepath.Join(sessions.StateRoot(home), "codex"))
}

func ensureDir(path string) string {
	_ = os.MkdirAll(path, privateDirMode)
	return path
}

func tokenPath() string {
	return filepath.Join(cliDir(), tokenFileName)
}

// SeedClaudeConfig writes the onboarding and trust state Claude Code would otherwise ask for on screen, keeping any
// other key Claude Code wrote itself.
func SeedClaudeConfig(pinnedVersion string) error {
	configPath := filepath.Join(ClaudeHome(), ".claude.json")
	settings := map[string]any{}
	data, err := os.ReadFile(configPath)
	if err == nil {
		if err := json.Unmarshal(data, &settings); err != nil {
			settings = map[string]any{}
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

	encoded, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath, encoded, privateFileMode)
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
