package managedcli

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/agentparley/tunnel/internal/config"
)

const (
	refreshInterval  = 6 * time.Hour
	fetchTimeout     = 30 * time.Second
	maxManifestBytes = 1 << 20
	warnEvery        = time.Hour
	pinsFileName     = "pins.json"
	pinsSigFileName  = "pins.json.sig"
)

type Refresher struct {
	stateDir     string
	updateServer string
	httpClient   *http.Client
	logger       *log.Logger
	nudge        chan struct{}

	mutex    sync.Mutex
	current  Manifest
	lastWarn time.Time
}

// NewRefresher starts from the embedded pins, upgraded to the persisted signed copy when that verifies and is newer.
func NewRefresher(stateDir string, logger *log.Logger) *Refresher {
	refresher := &Refresher{
		stateDir:     stateDir,
		updateServer: config.DefaultUpdateServer,
		httpClient:   &http.Client{Timeout: fetchTimeout},
		logger:       logger,
		nudge:        make(chan struct{}, 1),
		current:      Embedded(),
	}
	refresher.loadPersisted()
	return refresher
}

// SetUpdateServer points the refresher at the configured update host instead of the default.
func (r *Refresher) SetUpdateServer(updateServer string) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if updateServer != "" {
		r.updateServer = updateServer
	}
}

func (r *Refresher) Current() Manifest {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return r.current
}

// Run refreshes at start, then every six hours or when RefreshNow nudges it.
func (r *Refresher) Run(ctx context.Context) {
	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()
	r.refresh(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.refresh(ctx)
		case <-r.nudge:
			r.refresh(ctx)
		}
	}
}

// RefreshNow asks Run for an immediate refresh and never blocks.
func (r *Refresher) RefreshNow() {
	select {
	case r.nudge <- struct{}{}:
	default:
	}
}

func (r *Refresher) pinsPath() string {
	return filepath.Join(r.stateDir, "cli", pinsFileName)
}

func (r *Refresher) pinsSigPath() string {
	return filepath.Join(r.stateDir, "cli", pinsSigFileName)
}

func (r *Refresher) loadPersisted() {
	body, err := os.ReadFile(r.pinsPath())
	if err != nil {
		return
	}
	sig, err := os.ReadFile(r.pinsSigPath())
	if err != nil {
		return
	}
	manifest, err := Verify(body, sig)
	if err != nil {
		r.logger.Printf("cli pins: ignoring the saved pin list: %v", err)
		return
	}
	if manifest.Serial > r.current.Serial {
		r.current = manifest
	}
}

func (r *Refresher) refresh(ctx context.Context) {
	r.mutex.Lock()
	base := strings.TrimSuffix(r.updateServer, "/") + "/tunnel/cli-pins.json"
	r.mutex.Unlock()

	body, err := r.fetch(ctx, base)
	if err != nil {
		r.warnOncePerHour("cli pins: fetching the pin list failed: %v", err)
		return
	}
	sig, err := r.fetch(ctx, base+".sig")
	if err != nil {
		r.warnOncePerHour("cli pins: fetching the pin list signature failed: %v", err)
		return
	}
	manifest, err := Verify(body, sig)
	if err != nil {
		r.logger.Printf("cli pins: ignoring a fetched pin list: %v", err)
		return
	}

	r.mutex.Lock()
	defer r.mutex.Unlock()
	if manifest.Serial <= r.current.Serial {
		return
	}
	if err := os.MkdirAll(filepath.Dir(r.pinsPath()), 0o700); err != nil {
		r.logger.Printf("cli pins: cannot save the pin list: %v", err)
	} else if err := writeFileAtomic(r.pinsPath(), body); err != nil {
		r.logger.Printf("cli pins: cannot save the pin list: %v", err)
	} else if err := writeFileAtomic(r.pinsSigPath(), sig); err != nil {
		r.logger.Printf("cli pins: cannot save the pin list signature: %v", err)
	}
	r.current = manifest
	r.logger.Printf("cli pins: now on pin list serial %d", manifest.Serial)
}

func (r *Refresher) fetch(ctx context.Context, url string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	response, err := r.httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned status %d", url, response.StatusCode)
	}
	return io.ReadAll(io.LimitReader(response.Body, maxManifestBytes))
}

func (r *Refresher) warnOncePerHour(format string, args ...any) {
	r.mutex.Lock()
	due := time.Since(r.lastWarn) >= warnEvery
	if due {
		r.lastWarn = time.Now()
	}
	r.mutex.Unlock()
	if due {
		r.logger.Printf(format, args...)
	}
}

func writeFileAtomic(path string, data []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".pins-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tempPath, 0o600); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}
