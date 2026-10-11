package cliauth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

var (
	claudeTokenPattern = regexp.MustCompile(`sk-ant-oat01-[A-Za-z0-9_-]+`)
	ansiPattern        = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[()][A-Za-z0-9]`)
)

// LoginClaude runs the pinned `claude setup-token` in a pty relayed to term and stores the token it prints.
func LoginClaude(ctx context.Context, binary string, term *os.File) error {
	command := exec.CommandContext(ctx, binary, "setup-token")
	command.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+ClaudeHome())
	command.Dir = ClaudeWorkDir()

	ptmx, err := pty.Start(command)
	if err != nil {
		return fmt.Errorf("starting claude setup-token: %w", err)
	}
	defer ptmx.Close()

	restore := makeRaw(term)
	defer restore()
	stopResize := followWindowSize(term, ptmx)
	defer stopResize()

	var captured strings.Builder
	var capturedMutex sync.Mutex
	relayDone := make(chan struct{})
	go func() {
		defer close(relayDone)
		buffer := make([]byte, 4096)
		for {
			count, readErr := ptmx.Read(buffer)
			if count > 0 {
				_, _ = term.Write(buffer[:count])
				capturedMutex.Lock()
				captured.Write(buffer[:count])
				capturedMutex.Unlock()
			}
			if readErr != nil {
				return
			}
		}
	}()
	go func() { _, _ = io.Copy(ptmx, term) }()

	waitErr := command.Wait()
	<-relayDone
	if waitErr != nil && ctx.Err() != nil {
		return ctx.Err()
	}

	capturedMutex.Lock()
	output := captured.String()
	capturedMutex.Unlock()
	columns := terminalColumns(term)
	token := findClaudeToken(output, columns)
	if token == "" {
		return errors.New("Claude Code did not print a login token")
	}
	if err := os.MkdirAll(cliDir(), privateDirMode); err != nil {
		return err
	}
	return os.WriteFile(tokenPath(), []byte(token), privateFileMode)
}

func findClaudeToken(output string, columns int) string {
	lines := strings.Split(strings.ReplaceAll(ansiPattern.ReplaceAllString(output, ""), "\r", ""), "\n")
	for index, line := range lines {
		match := claudeTokenPattern.FindString(line)
		if match != "" {
			return joinWrappedToken(lines, index, match, columns)
		}
	}
	return ""
}

func joinWrappedToken(lines []string, index int, match string, columns int) string {
	token := match
	line := lines[index]
	for next := index + 1; columns > 0 && next < len(lines) && strings.HasSuffix(line, token) && len(line) >= columns; next++ {
		continuation := strings.TrimRight(lines[next], " ")
		run := tokenRunPattern.FindString(continuation)
		if run == "" {
			break
		}
		token += run
		line = continuation
		if run != continuation {
			break
		}
	}
	return token
}

var tokenRunPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+`)

// LoginCodex runs the pinned `codex login --device-auth` with CODEX_HOME set, attached straight to the terminal.
func LoginCodex(ctx context.Context, binary string, term *os.File) error {
	command := exec.CommandContext(ctx, binary, "login", "--device-auth")
	command.Env = append(os.Environ(), "CODEX_HOME="+CodexHome())
	command.Stdin, command.Stdout, command.Stderr = term, term, term
	if err := command.Run(); err != nil {
		return fmt.Errorf("codex login failed: %w", err)
	}
	if _, err := os.Stat(filepath.Join(CodexHome(), "auth.json")); err != nil {
		return errors.New("codex login finished without saving a login")
	}
	return nil
}

func terminalColumns(term *os.File) int {
	size, err := pty.GetsizeFull(term)
	if err != nil {
		return 0
	}
	return int(size.Cols)
}

func followWindowSize(term, ptmx *os.File) func() {
	_ = pty.InheritSize(term, ptmx)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGWINCH)
	go func() {
		for range signals {
			_ = pty.InheritSize(term, ptmx)
		}
	}()
	return func() { signal.Stop(signals); close(signals) }
}

func makeRaw(term *os.File) func() {
	original, err := unix.IoctlGetTermios(int(term.Fd()), unix.TCGETS)
	if err != nil {
		return func() {}
	}
	raw := *original
	raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	raw.Oflag &^= unix.OPOST
	raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Cflag &^= unix.CSIZE | unix.PARENB
	raw.Cflag |= unix.CS8
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(int(term.Fd()), unix.TCSETS, &raw); err != nil {
		return func() {}
	}
	return func() { _ = unix.IoctlSetTermios(int(term.Fd()), unix.TCSETS, original) }
}
