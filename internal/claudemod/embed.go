// Package claudemod carries the AgentParley mod that runs inside the daemon's private Claude Code: it registers the
// tools of the current round, replaces the system prompt, and relays every model step and tool call to the daemon over
// a private unix socket. It is Claude Code's own extension format, not an AgentParley plugin, and never runs in
// Platform's sandbox.
package claudemod

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/agentparley/tunnel/internal/credstore"
)

//go:embed files/.claude-plugin/plugin.json files/hooks/hooks.json files/hooks/register.ts
var modFiles embed.FS

var modPaths = []string{".claude-plugin/plugin.json", "hooks/hooks.json", "hooks/register.ts"}

// Install writes the mod into <state>/claude-mod/<hash of its contents>/ when that folder is not there yet, and
// returns the folder. Every build of the mod lives in its own folder, so a running session keeps the files it started
// with.
func Install() (string, error) {
	contents := make(map[string][]byte, len(modPaths))
	hasher := sha256.New()
	for _, relative := range modPaths {
		data, err := fs.ReadFile(modFiles, "files/"+relative)
		if err != nil {
			return "", err
		}
		contents[relative] = data
		hasher.Write([]byte(relative))
		hasher.Write(data)
	}
	directory := filepath.Join(credstore.StateDir(), "claude-mod", hex.EncodeToString(hasher.Sum(nil))[:12])
	if allPresent(directory, contents) {
		return directory, nil
	}
	for relative, data := range contents {
		path := filepath.Join(directory, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return "", fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return "", fmt.Errorf("writing %s: %w", path, err)
		}
	}
	return directory, nil
}

func allPresent(directory string, contents map[string][]byte) bool {
	for relative, want := range contents {
		have, err := os.ReadFile(filepath.Join(directory, relative))
		if err != nil || string(have) != string(want) {
			return false
		}
	}
	return true
}
