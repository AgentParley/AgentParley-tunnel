package managedcli

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/agentparley/tunnel/internal/credstore"
)

const downloadTimeout = 5 * time.Minute

var installLocks sync.Map

// BinDir is the parent of every installed CLI version directory.
func BinDir() string {
	return filepath.Join(credstore.StateDir(), "cli", "bin")
}

func versionDir(name, version string) string {
	return filepath.Join(BinDir(), name+"-"+version)
}

// Ensure returns the path of the pinned build of name, downloading and verifying it first when this box does not
// have it yet. A second call for an installed version only checks the file exists.
func Ensure(ctx context.Context, m Manifest, name string) (binaryPath, version string, err error) {
	pin, ok := m.Harnesses[name]
	if !ok {
		return "", "", fmt.Errorf("the pin list has no build of %s", name)
	}
	asset, ok := pin.Assets[platformKey()]
	if !ok {
		return "", "", fmt.Errorf("this box's platform %s has no pinned %s build", platformKey(), name)
	}

	lock, _ := installLocks.LoadOrStore(name, &sync.Mutex{})
	mutex := lock.(*sync.Mutex)
	mutex.Lock()
	defer mutex.Unlock()

	directory := versionDir(name, pin.Version)
	binaryPath = filepath.Join(directory, name)
	if info, statErr := os.Stat(binaryPath); statErr == nil && info.Mode().IsRegular() {
		return binaryPath, pin.Version, nil
	}

	if err := os.MkdirAll(BinDir(), 0o700); err != nil {
		return "", "", fmt.Errorf("creating %s: %w", BinDir(), err)
	}
	staging, err := os.MkdirTemp(BinDir(), ".install-"+name+"-")
	if err != nil {
		return "", "", fmt.Errorf("creating a staging folder: %w", err)
	}
	defer os.RemoveAll(staging)

	downloadCtx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	if err := download(downloadCtx, asset, filepath.Join(staging, name)); err != nil {
		return "", "", fmt.Errorf("installing %s %s: %w", name, pin.Version, err)
	}

	_ = os.RemoveAll(directory)
	if err := os.Rename(staging, directory); err != nil {
		return "", "", fmt.Errorf("installing %s: %w", directory, err)
	}
	return binaryPath, pin.Version, nil
}

func download(ctx context.Context, asset Asset, destination string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.URL, nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", asset.URL, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading %s: status %d", asset.URL, response.StatusCode)
	}

	downloaded := destination + ".download"
	file, err := os.OpenFile(downloaded, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o700)
	if err != nil {
		return err
	}
	hasher := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(file, hasher), response.Body)
	closeErr := file.Close()
	if copyErr != nil {
		return fmt.Errorf("downloading %s: %w", asset.URL, copyErr)
	}
	if closeErr != nil {
		return closeErr
	}
	if hex.EncodeToString(hasher.Sum(nil)) != strings.ToLower(asset.SHA256) {
		return fmt.Errorf("checksum mismatch for %s — discarding the download", asset.URL)
	}

	if asset.ArchiveMember == "" {
		if err := os.Rename(downloaded, destination); err != nil {
			return err
		}
		return os.Chmod(destination, 0o755)
	}
	if err := extractMember(downloaded, asset.ArchiveMember, destination); err != nil {
		return err
	}
	return os.Remove(downloaded)
}

func extractMember(archivePath, member, destination string) error {
	archive, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer archive.Close()
	gzipReader, err := gzip.NewReader(archive)
	if err != nil {
		return fmt.Errorf("reading the archive: %w", err)
	}
	defer gzipReader.Close()

	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			return fmt.Errorf("the archive has no %s", member)
		}
		if err != nil {
			return fmt.Errorf("reading the archive: %w", err)
		}
		if header.Typeflag != tar.TypeReg || filepath.Base(header.Name) != member {
			continue
		}
		out, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, tarReader)
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	}
}

// PruneOldVersions deletes installed version directories that are not a current pin. Call it only at daemon start:
// a running session keeps its binary until the process exits, so a pin change never kills a warm session.
func PruneOldVersions(m Manifest) {
	entries, err := os.ReadDir(BinDir())
	if err != nil {
		return
	}
	pinned := map[string]bool{}
	for name, pin := range m.Harnesses {
		pinned[name+"-"+pin.Version] = true
	}
	for _, entry := range entries {
		if entry.IsDir() && !pinned[entry.Name()] {
			_ = os.RemoveAll(filepath.Join(BinDir(), entry.Name()))
		}
	}
}

// RemoveInstalled deletes every installed version of name — the inverse of Ensure, used by `unregister`.
func RemoveInstalled(name string) error {
	matches, err := filepath.Glob(filepath.Join(BinDir(), name+"-*"))
	if err != nil {
		return err
	}
	for _, match := range matches {
		if err := os.RemoveAll(match); err != nil {
			return err
		}
	}
	return nil
}
