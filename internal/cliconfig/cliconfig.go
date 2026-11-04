// Package cliconfig is where the command-line sign-in lives: one JSON file in the user's own config folder.
package cliconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agentparley/tunnel/internal/atomicfile"
)

const DefaultServer = "https://services.agentparley.ai/platform"

// SignIn is what `auth login` saves. APIKey is a secret: the file is 0600 in a 0700 folder.
type SignIn struct {
	Server    string    `json:"server"`
	APIKey    string    `json:"apiKey"`
	KeyID     string    `json:"keyId"`
	KeyName   string    `json:"keyName"`
	KeyPrefix string    `json:"keyPrefix"`
	ExpiresAt time.Time `json:"expiresAt"`
	ManageURL string    `json:"manageUrl"`
}

var ErrNotSignedIn = errors.New("not signed in — run 'agentparley auth login'")

func Path() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("finding this user's config folder: %w (set AGENTPARLEY_API_KEY to use a key without saving it)", err)
	}
	return filepath.Join(dir, "agentparley", "cli.json"), nil
}

// Load returns (nil, nil) when nothing is saved.
func Load() (*SignIn, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var signIn SignIn
	if err := json.Unmarshal(data, &signIn); err != nil || signIn.APIKey == "" {
		return nil, fmt.Errorf("%s is not a valid sign-in file; run 'agentparley auth login' to replace it", path)
	}
	return &signIn, nil
}

func Save(signIn *SignIn) error {
	path, err := Path()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("securing %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(signIn, "", "  ")
	if err != nil {
		return err
	}
	// On Windows the mode bits barely apply; %AppData% is already private to the user.
	if err := atomicfile.Write(path, data); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// Delete treats a missing file as success.
func Delete() error {
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing %s: %w", path, err)
	}
	return nil
}

// Active is the credential every API command uses: AGENTPARLEY_API_KEY wins over the saved sign-in.
type Active struct {
	Server, APIKey  string
	FromEnvironment bool
	Saved           *SignIn
}

// Resolve returns ErrNotSignedIn when there is neither a key in the environment nor a saved sign-in.
func Resolve() (*Active, error) {
	saved, loadErr := Load()
	environmentKey := os.Getenv("AGENTPARLEY_API_KEY")
	if loadErr != nil && environmentKey == "" {
		return nil, loadErr
	}

	active := &Active{Saved: saved}
	if environmentKey != "" {
		active.APIKey = environmentKey
		active.FromEnvironment = true
	} else if saved != nil {
		active.APIKey = saved.APIKey
	} else {
		return nil, ErrNotSignedIn
	}

	active.Server = Server(saved)
	return active, nil
}

// Server is the base URL: AGENTPARLEY_API_SERVER, else the saved sign-in's, else the default.
func Server(saved *SignIn) string {
	server := os.Getenv("AGENTPARLEY_API_SERVER")
	if server == "" && saved != nil {
		server = saved.Server
	}
	if server == "" {
		server = DefaultServer
	}
	return strings.TrimSuffix(server, "/")
}
