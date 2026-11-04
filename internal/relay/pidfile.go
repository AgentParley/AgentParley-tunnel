package relay

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// PidFile records the process group of every running relay program, so a daemon that died hard (and so could not
// stop them) kills them when it next starts. The start time guards against a recycled pid.
type PidFile struct{ dir string }

func NewPidFile(stateRoot string) *PidFile {
	return &PidFile{dir: filepath.Join(stateRoot, "relay-programs")}
}

func (f *PidFile) Add(pgid int) error {
	startTime, err := processStartTime(pgid)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(f.dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(f.dir, strconv.Itoa(pgid)), []byte(startTime), 0o600)
}

func (f *PidFile) Remove(pgid int) {
	_ = os.Remove(filepath.Join(f.dir, strconv.Itoa(pgid)))
}

// SweepAtStart kills the group of every listed program that is still the same process, then forgets all entries.
func (f *PidFile) SweepAtStart() {
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		path := filepath.Join(f.dir, entry.Name())
		pgid, err := strconv.Atoi(entry.Name())
		recorded, readErr := os.ReadFile(path)
		if err == nil && readErr == nil && pgid > 1 {
			if current, startErr := processStartTime(pgid); startErr == nil && current == strings.TrimSpace(string(recorded)) {
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			}
		}
		_ = os.Remove(path)
	}
}

func processStartTime(pid int) (string, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	return parseStartTime(string(raw))
}

// parseStartTime returns field 22 of /proc/<pid>/stat. The command name (field 2) may contain spaces and
// parentheses, so fields are counted from the last ")".
func parseStartTime(stat string) (string, error) {
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return "", fmt.Errorf("unexpected /proc stat format")
	}
	fields := strings.Fields(stat[end+1:])
	const startTimeIndex = 22 - 3
	if len(fields) <= startTimeIndex {
		return "", fmt.Errorf("unexpected /proc stat format")
	}
	return fields[startTimeIndex], nil
}
