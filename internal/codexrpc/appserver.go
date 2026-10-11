package codexrpc

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/agentparley/tunnel/internal/cliauth"
	"github.com/agentparley/tunnel/internal/managedcli"
	"github.com/agentparley/tunnel/internal/version"
)

const (
	initializeTimeout = 30 * time.Second
	stopGrace         = 3 * time.Second
	maxLoggedLine     = 500
)

// disabledFeatures switches off every native Codex tool. code_mode and code_mode_host stay on: the gpt-5.6 models
// are code-mode-only, and AgentParley's tools bypass code mode through direct_only_tool_namespaces instead.
var disabledFeatures = []string{
	"shell_tool", "unified_exec", "multi_agent", "goals", "view_image", "image_generation", "apps", "plugins",
	"browser_use", "computer_use", "tool_suggest", "skill_search", "memories",
}

func appServerArgs() []string {
	args := []string{
		"app-server", "--listen", "stdio://",
		"-c", "mcp_servers={}",
		"-c", "web_search=disabled",
		"-c", "include_apply_patch_tool=false",
		"-c", "notify=[]",
		"-c", "check_for_update_on_startup=false",
		"-c", "model_auto_compact_token_limit=9223372036854775807",
		"-c", `features.code_mode.direct_only_tool_namespaces=["` + toolNamespace + `"]`,
	}
	for _, feature := range disabledFeatures {
		args = append(args, "-c", "features."+feature+"=false")
	}
	return args
}

// server is one running `codex app-server` process and the JSON-RPC client talking to it.
type server struct {
	command *exec.Cmd
	client  *client
	exited  chan struct{}
}

func startServer(ctx context.Context, binary string, logger *log.Logger) (*server, error) {
	command := exec.Command(binary, appServerArgs()...)
	command.Dir = cliauth.CodexWorkDir()
	command.Env = append(withoutEnv(os.Environ(), "CODEX_HOME"), "CODEX_HOME="+cliauth.CodexHome())
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}

	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("starting codex app-server: %w", err)
	}

	running := &server{command: command, client: newClient(stdin, logger), exited: make(chan struct{})}
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			if len(line) > maxLoggedLine {
				line = line[:maxLoggedLine]
			}
			logger.Printf("codex app-server: %s", line)
		}
	}()
	go func() {
		running.client.readLoop(stdout)
		_ = command.Wait()
		close(running.exited)
		running.client.fail(fmt.Errorf("codex app-server exited"))
	}()

	initCtx, cancel := context.WithTimeout(ctx, initializeTimeout)
	defer cancel()
	err = running.client.call(initCtx, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "agentparley-tunnel", "version": version.Version},
		"capabilities": map[string]any{"experimentalApi": true},
	}, nil)
	if err == nil {
		err = running.client.notify("initialized")
	}
	if err != nil {
		running.stop()
		return nil, fmt.Errorf("starting codex app-server: %w", err)
	}
	return running, nil
}

func withoutEnv(environment []string, name string) []string {
	kept := make([]string, 0, len(environment))
	for _, entry := range environment {
		if !strings.HasPrefix(entry, name+"=") {
			kept = append(kept, entry)
		}
	}
	return kept
}

func (s *server) alive() bool {
	select {
	case <-s.exited:
		return false
	default:
		return true
	}
}

// stop ends the app-server and its whole process group.
func (s *server) stop() {
	if s.command.Process == nil {
		return
	}
	_ = syscall.Kill(-s.command.Process.Pid, syscall.SIGTERM)
	select {
	case <-s.exited:
	case <-time.After(stopGrace):
		_ = syscall.Kill(-s.command.Process.Pid, syscall.SIGKILL)
		<-s.exited
	}
}

// Driver owns the box's app-server and starts threads on it. The process starts on the first round and restarts on
// the first round after it exits.
type Driver struct {
	manifest func() managedcli.Manifest
	logger   *log.Logger

	mutex   sync.Mutex
	running *server
}

func NewDriver(manifest func() managedcli.Manifest, logger *log.Logger) *Driver {
	return &Driver{manifest: manifest, logger: logger}
}

var (
	sharedOnce   sync.Once
	sharedDriver *Driver
)

// Shared is the daemon's one driver; the first caller's manifest source and logger are kept.
func Shared(manifest func() managedcli.Manifest, logger *log.Logger) *Driver {
	sharedOnce.Do(func() { sharedDriver = NewDriver(manifest, logger) })
	return sharedDriver
}

// Shutdown stops the shared driver's app-server, if one was started.
func Shutdown() {
	if sharedDriver != nil {
		sharedDriver.Shutdown()
	}
}

func (d *Driver) Shutdown() {
	d.mutex.Lock()
	running := d.running
	d.running = nil
	d.mutex.Unlock()
	if running != nil {
		running.stop()
	}
}

func (d *Driver) serverFor(ctx context.Context) (*server, error) {
	d.mutex.Lock()
	defer d.mutex.Unlock()
	if d.running != nil && d.running.alive() {
		return d.running, nil
	}
	binary, _, err := managedcli.Ensure(ctx, d.manifest(), "codex")
	if err != nil {
		return nil, err
	}
	running, err := startServer(ctx, binary, d.logger)
	if err != nil {
		return nil, err
	}
	d.running = running
	return running, nil
}
