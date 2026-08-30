// Package sessions owns ONLY the ledger of which (session id, shell state generation) pairs this daemon has already
// served — never the
// shell state itself. The state directory (stateRoot()/sessions/{sessionId}-{generation}, holding cwd/env/history)
// is created and written entirely by the shell text SshLoginShellWrap emits; this package never reads or writes
// it, per the design doc's "No table for shell state" guardrail. Persisting the ledger to disk (rather than
// holding it only in memory, the way the egress service does for a direct host) is the entire durability
// difference between a tunnel host and a direct host: a daemon restart resumes, an egress restart reports fresh.
//
// The ledger itself lives under the SAME tmpfs root as the shell state (stateRoot(), not a persistent directory)
// so the two agree after a machine reboot: /dev/shm is cleared on restart, and if the ledger survived while the
// state it describes did not, every session would wrongly report "resumed" against state that no longer exists.
// A daemon PROCESS restart (not a reboot) still resumes, because tmpfs survives that — only the reboot clears it.
package sessions

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ledgerSubdir mirrors SshLoginShellWrap's own "sessions" subdirectory name, so the ledger and the state it
// describes always resolve under the identical root without duplicating the resolution logic's own constant.
const ledgerSubdir = "ledger"

// Ledger tracks served session ids as empty marker files (mtime = last-served time, used by the TTL sweep) and
// serializes commands within one session with a per-session mutex — restore-then-capture on the far side races if
// two commands for the same session overlap, and a plugin's host.ssh.command callback can overlap a tool call.
type Ledger struct {
	mutexesGuard sync.Mutex
	mutexes      map[int64]*sync.Mutex
}

func NewLedger() *Ledger {
	return &Ledger{mutexes: make(map[int64]*sync.Mutex)}
}

// Lock returns the per-session mutex for sessionId, creating it on first use. Callers must Unlock it when done.
func (ledger *Ledger) Lock(sessionID int64) *sync.Mutex {
	ledger.mutexesGuard.Lock()
	mutex, ok := ledger.mutexes[sessionID]
	if !ok {
		mutex = &sync.Mutex{}
		ledger.mutexes[sessionID] = mutex
	}
	ledger.mutexesGuard.Unlock()

	mutex.Lock()
	return mutex
}

// MarkServed records the (sessionId, shellStateGeneration) pair as served (updating its mtime if already recorded)
// and reports whether this is the FIRST time — the caller reports that as is_fresh_shell. Keying the marker on the
// generation, not the session id alone, is what makes a session reset (Session.ShellStateGeneration bumped) report
// fresh: the old generation's marker simply stops matching, the same way its state directory stops matching. Call
// once per operation addressed to a session, holding the session's mutex from Lock. homeDir is only the last-resort
// fallback root — see stateRoot.
func (ledger *Ledger) MarkServed(homeDir string, sessionID int64, shellStateGeneration int32) (wasFresh bool, err error) {
	ledgerDir := filepath.Join(stateRoot(homeDir), ledgerSubdir)
	if err := os.MkdirAll(ledgerDir, 0o700); err != nil {
		return false, fmt.Errorf("creating ledger dir: %w", err)
	}

	path := markerPath(ledgerDir, sessionID, shellStateGeneration)
	_, statErr := os.Stat(path)
	wasFresh = os.IsNotExist(statErr)

	now := time.Now()
	if wasFresh {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			return false, fmt.Errorf("writing session marker: %w", err)
		}
	} else if err := os.Chtimes(path, now, now); err != nil {
		return false, fmt.Errorf("touching session marker: %w", err)
	}
	return wasFresh, nil
}

// Sweep deletes marker files whose mtime is older than ttl, along with the ONE shell state directory
// (stateRoot(homeDir)/sessions/{sessionId}-{generation}) each marker names — one marker per (session, generation)
// pair now that the marker filename itself carries the generation, so no glob is needed: a retired generation's
// marker stops being touched the moment a reset moves the session onto a new generation, and ages out under the
// same TTL as any other marker. The per-session mutexes are deliberately kept: removing one a caller already holds
// a reference to lets the next caller build a second mutex for the same session, so two commands would run
// concurrently against one state directory.
func (ledger *Ledger) Sweep(homeDir string, ttl time.Duration) error {
	root := stateRoot(homeDir)
	ledgerDir := filepath.Join(root, ledgerSubdir)

	entries, err := os.ReadDir(ledgerDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}

	cutoff := time.Now().Add(-ttl)
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}
		canSweep, mutex := ledger.tryLockSession(entry.Name())
		if !canSweep {
			continue
		}
		_ = os.RemoveAll(filepath.Join(root, "sessions", entry.Name()))
		_ = os.Remove(filepath.Join(ledgerDir, entry.Name()))
		if mutex != nil {
			mutex.Unlock()
		}
	}
	return nil
}

// tryLockSession parses the session id out of a "{sessionId}-{generation}" marker filename and takes that
// session's mutex without blocking, so a sweep never stalls behind — or tears state out from under — a command
// that is actually running. It LOOKS UP rather than get-or-creates: no mutex means no caller has ever locked this
// session in this process, so the sweep can proceed and must not leave one behind for a session it is about to
// delete.
func (ledger *Ledger) tryLockSession(markerName string) (canSweep bool, mutex *sync.Mutex) {
	sessionID, _, err := parseMarkerName(markerName)
	if err != nil {
		return true, nil // not a well-formed marker, so nothing can be holding it — sweep it rather than keep it forever
	}

	ledger.mutexesGuard.Lock()
	sessionMutex, ok := ledger.mutexes[sessionID]
	ledger.mutexesGuard.Unlock()
	if !ok {
		return true, nil
	}

	return sessionMutex.TryLock(), sessionMutex
}

func markerPath(ledgerDir string, sessionID int64, shellStateGeneration int32) string {
	return filepath.Join(ledgerDir, fmt.Sprintf("%d-%d", sessionID, shellStateGeneration))
}

func parseMarkerName(markerName string) (sessionID int64, shellStateGeneration int32, err error) {
	separatorIndex := strings.LastIndex(markerName, "-")
	if separatorIndex <= 0 || separatorIndex == len(markerName)-1 {
		return 0, 0, fmt.Errorf("malformed marker name %q", markerName)
	}
	sessionID, err = strconv.ParseInt(markerName[:separatorIndex], 10, 64)
	if err != nil {
		return 0, 0, err
	}
	generation, err := strconv.ParseInt(markerName[separatorIndex+1:], 10, 32)
	if err != nil {
		return 0, 0, err
	}
	return sessionID, int32(generation), nil
}

// stateRoot resolves the same tmpfs-first root SshLoginShellWrap.WrapSessionCommand computes inline in the shell
// text it emits — kept as one deliberate exception to "the wrap embeds the path, never passed in" (the design
// doc's Flow 3): the ledger has to independently agree on where state lives so it can clean it up, since it never
// sees the shell text itself. os.Getuid() is the daemon's own uid, not runAsUser's — shellrun.Run execs commands
// under this same process's identity (see shellrun.go), so this IS the uid the shell text's own `id -u` reports.
func stateRoot(homeDir string) string {
	uid := os.Getuid()
	shmCandidate := fmt.Sprintf("/dev/shm/agentparley-%d", uid)
	if isUsableRoot(shmCandidate) {
		return shmCandidate
	}

	if runtimeDir := os.Getenv("XDG_RUNTIME_DIR"); runtimeDir != "" {
		runtimeCandidate := filepath.Join(runtimeDir, "agentparley")
		if isUsableRoot(runtimeCandidate) {
			return runtimeCandidate
		}
	}

	return filepath.Join(homeDir, ".agentparley")
}

// WorkspaceSocketPath is the fixed unix socket the daemon serves for the `agentparley ws` bridge and injects as
// AGENTPARLEY_WS_SOCKET into every run-command's environment. It sits under the same tmpfs-first state root as the
// session shell state, so the daemon (serving it) and a `ws` child (or a human shell falling back to this path)
// agree on one location for the same uid, and it inherits that root's 0700 ownership.
func WorkspaceSocketPath(homeDir string) string {
	return filepath.Join(stateRoot(homeDir), "ws.sock")
}

// isUsableRoot creates path (0700) if needed and confirms this process can actually write into it — MkdirAll alone
// reports success for a directory that exists but isn't writable by this uid (e.g. another user's leftover
// /dev/shm entry), which would otherwise silently strand the ledger and state in different places.
func isUsableRoot(path string) bool {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return false
	}
	probe := filepath.Join(path, ".write-probe")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		return false
	}
	_ = os.Remove(probe)
	return true
}
