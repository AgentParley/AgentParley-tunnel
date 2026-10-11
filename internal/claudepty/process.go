// Package claudepty drives Claude Code the way a person would use it, in a terminal the daemon owns, without ever
// typing into that terminal. Everything goes through the AgentParley mod loaded into Claude Code (claudemod): the mod
// registers exactly the tools of the conversation, replaces the system prompt, reports every model step and tool call
// over a private unix socket, and takes its instructions (seed history, submit a prompt, one-shot completions) from
// the same socket. One Claude Code process is one warm conversation; a session implements cliround.Runner.
package claudepty

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/agentparley/tunnel/internal/claudemod"
	"github.com/agentparley/tunnel/internal/cliauth"
	"github.com/agentparley/tunnel/internal/cliround"
	"github.com/agentparley/tunnel/internal/managedcli"
)

const (
	startupWait   = 30 * time.Second
	stopGrace     = 3 * time.Second
	screenKeep    = 64 << 10
	screenColumns = 200
	screenRows    = 50

	// placeholderBuiltin is the one built-in --tools names. --tools "" would be tidier, but Claude Code 2.1.296 then
	// refuses every tool a plugin registers; a deny rule on the one named built-in leaves the model no built-in at all
	// (checked against the request the engine sends: only the mcp__agentparley__ tools are in it).
	placeholderBuiltin = "Read"
)

var modelPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._\-\[\]]{0,63}$`)

var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;?<>=]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[()][A-Za-z0-9]|\r`)

// Driver starts Claude Code processes and owns the shared one-shot process.
type Driver struct {
	manifest func() managedcli.Manifest
	logger   *log.Logger

	// baseURL and stubToken point processes at a local stand-in for the API with a made-up login. Only the replay
	// benchmark (`doctor --bench-claude-replay`) sets them, so it can run without a login or a model call.
	baseURL   string
	stubToken string
	// benchmark answers the step after the replayed window with a finished turn instead of a real model request.
	benchmark bool

	counter atomic.Int64

	utilityMutex sync.Mutex
	utility      *utility
}

func NewDriver(manifest func() managedcli.Manifest, logger *log.Logger) *Driver {
	return &Driver{manifest: manifest, logger: logger}
}

// Shutdown ends the one-shot process, if one was started.
func (d *Driver) Shutdown() {
	d.utilityMutex.Lock()
	running := d.utility
	d.utility = nil
	d.utilityMutex.Unlock()
	if running != nil {
		running.close()
	}
}

type processSpec struct {
	model  string
	effort string
	hello  hello
	sink   modSink
}

// ringBuffer keeps the tail of the terminal output for error messages.
type ringBuffer struct {
	mutex sync.Mutex
	data  []byte
}

func (r *ringBuffer) Write(chunk []byte) (int, error) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.data = append(r.data, chunk...)
	if len(r.data) > screenKeep {
		r.data = append([]byte(nil), r.data[len(r.data)-screenKeep:]...)
	}
	return len(chunk), nil
}

func (r *ringBuffer) tail(lines int) string {
	r.mutex.Lock()
	text := string(r.data)
	r.mutex.Unlock()
	text = ansiPattern.ReplaceAllString(text, "")
	var kept []string
	for _, line := range strings.Split(text, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			kept = append(kept, trimmed)
		}
	}
	if len(kept) > lines {
		kept = kept[len(kept)-lines:]
	}
	return strings.Join(kept, "\n")
}

// process is one Claude Code in its own terminal and process group, with the bridge the mod talks to.
type process struct {
	command *exec.Cmd
	ptmx    *os.File
	screen  *ringBuffer
	bridge  *bridge
	version string
	exited  chan struct{}

	closeOnce sync.Once
}

func (d *Driver) startProcess(ctx context.Context, spec processSpec) (*process, error) {
	if !modelPattern.MatchString(spec.model) {
		return nil, &cliround.InvalidRequestError{Message: fmt.Sprintf("model %q is not a valid claude model id", spec.model)}
	}
	binary, version, err := managedcli.Ensure(ctx, d.manifest(), "claude")
	if err != nil {
		return nil, err
	}
	modDirectory, err := claudemod.Install()
	if err != nil {
		return nil, err
	}
	token := d.stubToken
	if token == "" {
		token, err = cliauth.ClaudeToken()
		if err != nil {
			return nil, err
		}
	}
	if err := cliauth.SeedClaudeConfig(version); err != nil {
		return nil, fmt.Errorf("preparing the Claude Code configuration: %w", err)
	}

	socketPath := filepath.Join(cliauth.ClaudeSocketDir(), fmt.Sprintf("%d-%d.sock", os.Getpid(), d.counter.Add(1)))
	running, err := newBridge(socketPath, spec.hello, spec.sink)
	if err != nil {
		return nil, err
	}

	command := exec.Command(binary, claudeArgs(spec, modDirectory)...)
	command.Dir = cliauth.ClaudeWorkDir()
	command.Env = d.environment(token, socketPath)

	screen := &ringBuffer{}
	ptmx, err := pty.StartWithSize(command, &pty.Winsize{Rows: screenRows, Cols: screenColumns})
	if err != nil {
		running.close()
		return nil, fmt.Errorf("starting Claude Code: %w", err)
	}
	started := &process{command: command, ptmx: ptmx, screen: screen, bridge: running, version: version, exited: make(chan struct{})}
	go func() {
		buffer := make([]byte, 8192)
		for {
			count, readErr := ptmx.Read(buffer)
			if count > 0 {
				_, _ = screen.Write(buffer[:count])
			}
			if readErr != nil {
				return
			}
		}
	}()
	go func() {
		_ = command.Wait()
		close(started.exited)
	}()

	if err := started.awaitReady(ctx); err != nil {
		started.close()
		return nil, err
	}
	return started, nil
}

// claudeArgs is the fixed argv of every Claude Code the daemon starts. The wire only ever names a model and an effort.
func claudeArgs(spec processSpec, modDirectory string) []string {
	args := []string{"--model", spec.model}
	if spec.effort != "" {
		args = append(args, "--effort", spec.effort)
	}
	return append(args,
		"--tools", placeholderBuiltin,
		"--disallowedTools", placeholderBuiltin,
		"--strict-mcp-config",
		"--setting-sources", "",
		"--permission-mode", "dontAsk",
		"--plugin-dir", modDirectory,
	)
}

// environment is a minimal one: nothing of the daemon's own CLAUDE_* or ANTHROPIC_* variables reaches Claude Code.
func (d *Driver) environment(token, socketPath string) []string {
	environment := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + homeDirectory(),
		"LANG=" + valueOr(os.Getenv("LANG"), "C.UTF-8"),
		"TERM=xterm-256color",
		"CLAUDE_CONFIG_DIR=" + cliauth.ClaudeHome(),
		"CLAUDE_CODE_OAUTH_TOKEN=" + token,
		"AGENTPARLEY_BRIDGE=" + socketPath,
		"DISABLE_AUTOUPDATER=1",
		"DISABLE_AUTO_COMPACT=1",
		"MAX_MCP_OUTPUT_TOKENS=200000",
	}
	if d.baseURL != "" {
		environment = append(environment, "ANTHROPIC_BASE_URL="+d.baseURL)
	}
	return environment
}

func homeDirectory() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// awaitReady waits for the mod to say it registered the tools, and classifies what the terminal shows when it does not.
func (p *process) awaitReady(ctx context.Context) error {
	timer := time.NewTimer(startupWait)
	defer timer.Stop()
	select {
	case <-p.bridge.ready:
		if err := p.bridge.failed(); err != nil {
			return err
		}
		return nil
	case <-p.exited:
		return classifyScreen(p.screen.tail(12))
	case <-timer.C:
		return classifyScreen(p.screen.tail(12))
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// classifyScreen turns what Claude Code showed instead of starting into the error the person should see.
func classifyScreen(screen string) error {
	lowered := strings.ToLower(screen)
	switch {
	case strings.Contains(lowered, "update") && (strings.Contains(lowered, "required") || strings.Contains(lowered, "newer") || strings.Contains(lowered, "too old")),
		strings.Contains(lowered, "upgrade") && strings.Contains(lowered, "version"):
		return cliround.ErrUpdateRequired
	case strings.Contains(lowered, "invalid") && strings.Contains(lowered, "token"),
		strings.Contains(lowered, "not logged in"), strings.Contains(lowered, "please run /login"),
		strings.Contains(lowered, "log in"), strings.Contains(lowered, "sign in"):
		return cliround.ErrSignedOut
	}
	if screen == "" {
		return &cliround.HarnessError{Message: "Claude Code did not start (no output)"}
	}
	return &cliround.HarnessError{Message: "Claude Code did not start. Its screen showed:\n" + screen}
}

func (p *process) alive() bool {
	select {
	case <-p.exited:
		return false
	default:
		return true
	}
}

// close ends the process group and removes the socket.
func (p *process) close() {
	p.closeOnce.Do(func() {
		if p.command.Process != nil && p.alive() {
			_ = syscall.Kill(-p.command.Process.Pid, syscall.SIGTERM)
			select {
			case <-p.exited:
			case <-time.After(stopGrace):
				_ = syscall.Kill(-p.command.Process.Pid, syscall.SIGKILL)
				<-p.exited
			}
		}
		_ = p.ptmx.Close()
		p.bridge.close()
	})
}
