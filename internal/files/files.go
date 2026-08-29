// Package files performs the four file operations directly over os — list/read/write/delete on this box's own
// filesystem, as the daemon's own OS user. Path traversal is deliberately NOT filtered (design doc edge case 18):
// the agent is authorised to act on this box as this user, and the OS permission model is the boundary, exactly as
// it is over real SSH.
package files

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Error codes mirror SshOperationException.Code exactly — see tunnel.proto's OperationError comment.
const (
	CodeFileNotFound     = "file_not_found"
	CodePermissionDenied = "permission_denied"
	CodeIsDirectory      = "is_directory"
	CodeTooLarge         = "too_large"
	CodeInternal         = "internal"
)

// OperationError carries a wire code alongside a human message, so the dispatcher can pass it straight through to
// the OperationError proto message without re-classifying a bare error.
type OperationError struct {
	Code    string
	Message string
}

func (e *OperationError) Error() string {
	return e.Message
}

func newError(code string, err error) *OperationError {
	return &OperationError{Code: code, Message: err.Error()}
}

// Entry mirrors the wire's DirectoryEntry.
type Entry struct {
	Name         string
	IsDirectory  bool
	SizeBytes    int64
	ModifiedUnix int64
}

// Resolve turns an agent-supplied path into an absolute one the way a real SSH/SFTP session would: a bare "~" or a
// leading "~/" expands to the run-as user's home; a relative path is taken FROM that home (matching sshd's default
// working directory — and the Direct-SSH egress, whose SFTP client starts in home — so the two host kinds agree);
// an absolute path is used as given. The result is cleaned. os operations here don't expand "~" or root relatives
// at home on their own (that is shell behaviour), which is why run_command — run through a login shell in home —
// accepts "~/x" while the file ops did not until this resolved the path first. Traversal is intentionally not fenced
// (see the package comment): the OS permission model is the boundary, exactly as over real SSH.
func Resolve(rawPath, homeDir string) string {
	switch {
	case rawPath == "~":
		return homeDir
	case strings.HasPrefix(rawPath, "~/"):
		return filepath.Join(homeDir, rawPath[len("~/"):])
	case filepath.IsAbs(rawPath):
		return filepath.Clean(rawPath)
	default:
		return filepath.Join(homeDir, rawPath)
	}
}

// List returns up to maxEntries entries of path, sorted by name for stable output across calls. IsTruncated is set
// when the directory holds more entries than the cap.
func List(path string, maxEntries int) (entries []Entry, isTruncated bool, err error) {
	directoryEntries, readErr := os.ReadDir(path)
	if readErr != nil {
		return nil, false, classifyReadError(readErr)
	}

	sort.Slice(directoryEntries, func(i, j int) bool { return directoryEntries[i].Name() < directoryEntries[j].Name() })

	truncated := len(directoryEntries) > maxEntries
	if truncated {
		directoryEntries = directoryEntries[:maxEntries]
	}

	result := make([]Entry, 0, len(directoryEntries))
	for _, directoryEntry := range directoryEntries {
		info, infoErr := directoryEntry.Info()
		if infoErr != nil {
			continue // vanished between ReadDir and Info (a race with the box's own workload) — skip, not fatal
		}
		result = append(result, Entry{
			Name:         directoryEntry.Name(),
			IsDirectory:  directoryEntry.IsDir(),
			SizeBytes:    info.Size(),
			ModifiedUnix: info.ModTime().UnixMilli(),
		})
	}
	return result, truncated, nil
}

// Read returns the file's content, or IsTooLarge with the real size if it exceeds maxBytes — never reading the
// oversized content into memory first.
func Read(path string, maxBytes int64) (content []byte, isTooLarge bool, sizeBytes int64, err error) {
	info, statErr := os.Stat(path)
	if statErr != nil {
		return nil, false, 0, classifyReadError(statErr)
	}
	if info.IsDir() {
		return nil, false, 0, &OperationError{Code: CodeIsDirectory, Message: path + " is a directory"}
	}
	if info.Size() > maxBytes {
		return nil, true, info.Size(), nil
	}

	data, readErr := os.ReadFile(path)
	if readErr != nil {
		return nil, false, 0, classifyReadError(readErr)
	}
	return data, false, info.Size(), nil
}

// Write creates or overwrites path with content. The MaxWriteFileBytes cap is enforced by the caller BEFORE
// dispatch (edge case 7), matching EgressSshSession — this function trusts the size it's handed.
func Write(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return classifyWriteError(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return classifyWriteError(err)
	}
	return nil
}

func Delete(path string) error {
	info, statErr := os.Stat(path)
	if statErr != nil {
		return classifyReadError(statErr)
	}
	if info.IsDir() {
		return &OperationError{Code: CodeIsDirectory, Message: path + " is a directory"}
	}
	if err := os.Remove(path); err != nil {
		return classifyWriteError(err)
	}
	return nil
}

func classifyReadError(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return &OperationError{Code: CodeFileNotFound, Message: err.Error()}
	}
	if errors.Is(err, os.ErrPermission) {
		return &OperationError{Code: CodePermissionDenied, Message: err.Error()}
	}
	return newError(CodeInternal, err)
}

func classifyWriteError(err error) error {
	if errors.Is(err, os.ErrPermission) {
		return &OperationError{Code: CodePermissionDenied, Message: err.Error()}
	}
	if errors.Is(err, os.ErrNotExist) {
		return &OperationError{Code: CodeFileNotFound, Message: err.Error()}
	}
	return newError(CodeInternal, err)
}
