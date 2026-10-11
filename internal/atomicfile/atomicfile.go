// Package atomicfile replaces a file so a concurrent reader sees the old content or the new, never a partial write.
package atomicfile

import (
	"os"
	"path/filepath"
)

// Write replaces path with data, mode 0600.
func Write(path string, data []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".atomic-*.tmp")
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
