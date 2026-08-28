// Package shellrun reproduces sshd's own exec contract: resolve the run-as user, set the handful of environment
// variables sshd itself would set, and run an ALREADY-WRAPPED command verbatim. It never re-wraps or edits the
// command string — SshLoginShellWrap, on the platform side, is the one place that knows the shell-text format.
package shellrun

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// User is the handful of /etc/passwd fields sshd itself would resolve before running a command — everything
// Run needs to set SHELL/HOME/USER/LOGNAME.
type User struct {
	Username string
	UID      int
	Home     string
	Shell    string
}

// ResolveUser reads /etc/passwd directly rather than os/user, because the standard library's cgo-less lookup on
// Linux does not return the login shell — and the shell is exactly the field this package exists to get right.
func ResolveUser(username string) (*User, error) {
	file, err := os.Open("/etc/passwd")
	if err != nil {
		return nil, fmt.Errorf("opening /etc/passwd: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) < 7 || fields[0] != username {
			continue
		}

		uid, err := strconv.Atoi(fields[2])
		if err != nil {
			return nil, fmt.Errorf("parsing uid for %s in /etc/passwd: %w", username, err)
		}
		return &User{Username: username, UID: uid, Home: fields[5], Shell: fields[6]}, nil
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading /etc/passwd: %w", err)
	}
	return nil, fmt.Errorf("run_as user %q not found in /etc/passwd", username)
}

// VerifyMatchesProcess enforces edge case 17: a run_as naming anyone other than the daemon's own uid is a
// misconfiguration the daemon refuses to start under, rather than silently running commands as the wrong identity
// (this daemon has no privilege to setuid — it is not root, and reproducing sshd's env-setting contract is not the
// same as reproducing its privilege separation).
func VerifyMatchesProcess(user *User) error {
	processUid := os.Getuid()
	if user.UID != processUid {
		return fmt.Errorf("config run_as=%q (uid %d) does not match the uid this daemon is running as (%d) — "+
			"the daemon has no privilege to switch users; run it as %q or change run_as", user.Username, user.UID, processUid, user.Username)
	}
	return nil
}
