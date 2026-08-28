package credstore

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

const (
	systemStateDir   = "/var/lib/agentparley-tunnel"
	userStateDirName = ".agentparley"
	stateDirEnvVar   = "AGENTPARLEY_TUNNEL_STATE_DIR"
	dirMode          = 0o700
	fileMode         = 0o600
)

var resolveStateDirOnce sync.Once
var resolvedStateDir string

type linuxStore struct{}

// New returns the Linux credential store: a single JSON file at 0600 inside a 0700 directory.
func New() Store {
	return linuxStore{}
}

func (linuxStore) Load() (*Credentials, error) {
	data, err := os.ReadFile(credentialsPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var credentials Credentials
	if err := json.Unmarshal(data, &credentials); err != nil {
		return nil, err
	}
	return &credentials, nil
}

// Save writes atomically — temp file in the same directory, then rename — so a crash or power loss between writes
// never leaves a half-written credentials file that Load would then fail to parse.
func (linuxStore) Save(credentials *Credentials) error {
	if err := os.MkdirAll(StateDir(), dirMode); err != nil {
		return err
	}

	data, err := json.Marshal(credentials)
	if err != nil {
		return err
	}

	tempFile, err := os.CreateTemp(StateDir(), ".credentials-*.tmp")
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
	if err := os.Chmod(tempPath, fileMode); err != nil {
		return err
	}
	return os.Rename(tempPath, credentialsPath())
}

func (linuxStore) Delete() error {
	err := os.Remove(credentialsPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// StateDir is the shared root other packages persist under (credentials, the machine-id fallback), resolved in one
// place so every writer agrees. Root keeps the FHS location the packaged systemd unit installs; anyone else gets
// ~/.agentparley, because "register the box you already own" cannot require sudo to enrol a laptop — and a user who
// cannot write /var/lib would otherwise fail at `login` with a bare permission error. The env var overrides both,
// for containers and tests that want an explicit path.
func StateDir() string {
	resolveStateDirOnce.Do(func() {
		if override := os.Getenv(stateDirEnvVar); override != "" {
			resolvedStateDir = override
			return
		}
		if os.Geteuid() == 0 {
			resolvedStateDir = systemStateDir
			return
		}
		home, err := os.UserHomeDir()
		if err != nil {
			resolvedStateDir = systemStateDir
			return
		}
		resolvedStateDir = filepath.Join(home, userStateDirName)
	})
	return resolvedStateDir
}

func credentialsPath() string {
	return filepath.Join(StateDir(), "credentials.json")
}
