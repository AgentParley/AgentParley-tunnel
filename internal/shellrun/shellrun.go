package shellrun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
	"unicode/utf8"
)

// Result mirrors the wire's RunCommandResult (minus correlation/is_fresh_shell, which the caller attaches).
type Result struct {
	ExitCode   int
	Stdout     []byte
	Stderr     []byte
	Truncated  bool
	DurationMs int
	TimedOut   bool
}

const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// Run execs the already-wrapped command verbatim through the resolved user's login shell — `shell -c command` is
// exactly what sshd does, and the wrap text (see SshLoginShellWrap) assumes it is being handed to precisely that.
// Timeout enforcement kills the whole process group, not just the direct child, because the wrap text backgrounds
// nothing itself but the agent's OWN command might (a stray `sleep 600 &`), and a lone SIGKILL to the shell would
// orphan it.
func Run(ctx context.Context, user *User, command string, timeoutSeconds, maxOutputBytes int, sessionID int64, workspaceSocketPath string) (*Result, error) {
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
	defer cancel()

	shell := user.Shell
	if shell == "" {
		shell = "/bin/sh"
	}

	cmd := exec.Command(shell, "-c", command)
	cmd.Dir = user.Home
	cmd.Env = []string{
		"SHELL=" + shell,
		"HOME=" + user.Home,
		"USER=" + user.Username,
		"LOGNAME=" + user.Username,
		"PATH=" + defaultPath,
	}
	// The `agentparley ws` bridge reads these to reach the local daemon socket and default to this session's agent;
	// a nohup'd background job inherits them, so ws keeps working after the turn ends. session id 0 = no session
	// context (never injected). Neither is a bearer secret: the socket's 0600 perms are the local trust boundary.
	if workspaceSocketPath != "" {
		cmd.Env = append(cmd.Env, "AGENTPARLEY_WS_SOCKET="+workspaceSocketPath)
	}
	if sessionID != 0 {
		cmd.Env = append(cmd.Env, fmt.Sprintf("AGENTPARLEY_SESSION_ID=%d", sessionID))
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout := newLimitedWriter(maxOutputBytes)
	stderr := newLimitedWriter(maxOutputBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	var waitErr error
	var timedOut bool
	select {
	case waitErr = <-waitDone:
	case <-runCtx.Done():
		timedOut = true
		// Negative pid targets the whole process group created by Setpgid above.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		waitErr = <-waitDone
	}

	return &Result{
		ExitCode:   exitCodeFrom(waitErr),
		Stdout:     stdout.Bytes(),
		Stderr:     stderr.Bytes(),
		Truncated:  stdout.truncated || stderr.truncated,
		DurationMs: int(time.Since(start).Milliseconds()),
		TimedOut:   timedOut,
	}, nil
}

func exitCodeFrom(waitErr error) int {
	if waitErr == nil {
		return 0
	}
	var exitError *exec.ExitError
	if errors.As(waitErr, &exitError) {
		return exitError.ExitCode()
	}
	return -1
}

// limitedWriter caps captured output at limit bytes and, once truncated, trims back to a UTF-8 rune boundary so a
// multi-byte character is never split across the cut — the same guarantee EgressSshSession gives the direct-SSH
// path (Edge case 9: "truncated on the far side at a UTF-8 boundary").
type limitedWriter struct {
	limit     int
	buf       bytes.Buffer
	truncated bool
}

func newLimitedWriter(limit int) *limitedWriter {
	return &limitedWriter{limit: limit}
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if w.truncated {
		return len(p), nil
	}

	remaining := w.limit - w.buf.Len()
	if remaining <= 0 {
		w.truncated = true
		return len(p), nil
	}
	if len(p) <= remaining {
		w.buf.Write(p)
		return len(p), nil
	}
	w.buf.Write(p[:remaining])
	w.truncated = true
	return len(p), nil
}

func (w *limitedWriter) Bytes() []byte {
	if !w.truncated {
		return w.buf.Bytes()
	}
	return truncateToRuneBoundary(w.buf.Bytes())
}

// truncateToRuneBoundary drops a final rune that was cut mid-encoding by the byte-limit above. Content the
// command itself emitted as invalid UTF-8 (arbitrary binary output) is out of scope — there is no boundary to
// respect in bytes that were never valid text.
func truncateToRuneBoundary(b []byte) []byte {
	if utf8.Valid(b) {
		return b
	}
	for i := len(b) - 1; i >= 0 && i > len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			return b[:i]
		}
	}
	return b
}
