package codexrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/agentparley/tunnel/internal/atomicfile"
	"github.com/agentparley/tunnel/internal/cliauth"
	"github.com/agentparley/tunnel/internal/managedcli"
	"github.com/agentparley/tunnel/internal/version"
)

const (
	initializeTimeout = 30 * time.Second
	stopGrace         = 3 * time.Second
	maxLoggedLine     = 500
)

// disabledFeatures switches off every native Codex tool. code_mode stays on because the gpt-5.6 models are
// code-mode-only. The pinned build ships no code-mode host on purpose, so the model calls AgentParley's tools directly;
// direct_only_tool_namespaces is belt and braces. Codex's "Code Mode is unavailable" warning is therefore expected and
// only logged, never reported as a harness event.
var disabledFeatures = []string{
	"shell_tool", "unified_exec", "multi_agent", "goals", "view_image", "image_generation", "apps", "plugins",
	"browser_use", "computer_use", "tool_suggest", "skill_search", "memories",
}

func appServerArgs(modelCatalogPath string) []string {
	args := []string{
		"app-server", "--listen", "stdio://",
		"-c", "model_catalog_json=" + strconv.Quote(modelCatalogPath),
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

// writeModelCatalog writes the pinned build's own model catalog without multi_agent_version. The gpt-5.6 models carry
// "multi_agent_version":"v2", which turns Codex's sub-agent tools on whatever features.multi_agent says, and a sub-agent
// makes model calls of its own that Platform never sees as steps.
func writeModelCatalog(ctx context.Context, binary string) (string, error) {
	output, err := cliauth.CodexCommand(ctx, binary, "debug", "models").Output()
	if err != nil {
		return "", fmt.Errorf("reading the codex model catalog: %w", err)
	}
	var catalog struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(output, &catalog); err != nil || len(catalog.Models) == 0 {
		return "", fmt.Errorf("reading the codex model catalog: unexpected output")
	}
	for _, model := range catalog.Models {
		delete(model, "multi_agent_version")
	}
	data, err := json.Marshal(catalog)
	if err != nil {
		return "", err
	}
	path := filepath.Join(cliauth.CodexHome(), "agentparley-models-"+filepath.Base(filepath.Dir(binary))+".json")
	if err := atomicfile.Write(path, data); err != nil {
		return "", fmt.Errorf("writing the codex model catalog: %w", err)
	}
	return path, nil
}

func startServer(ctx context.Context, binary string, logger *log.Logger) (*server, error) {
	modelCatalogPath, err := writeModelCatalog(ctx, binary)
	if err != nil {
		return nil, err
	}
	command := cliauth.CodexCommand(context.Background(), binary, appServerArgs(modelCatalogPath)...)
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

// alive is false from the moment the client sees the app-server's output close, which is before the process has been
// waited for.
func (s *server) alive() bool {
	select {
	case <-s.exited:
		return false
	default:
		return !s.client.hasExited()
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
	if running := d.liveServer(); running != nil {
		return running, nil
	}
	binary, _, err := managedcli.Ensure(ctx, d.manifest(), "codex")
	if err != nil {
		return nil, err
	}

	d.mutex.Lock()
	defer d.mutex.Unlock()
	if d.running != nil && d.running.alive() {
		return d.running, nil
	}
	running, err := startServer(ctx, binary, d.logger)
	if err != nil {
		return nil, err
	}
	d.running = running
	return running, nil
}

func (d *Driver) liveServer() *server {
	d.mutex.Lock()
	defer d.mutex.Unlock()
	if d.running != nil && d.running.alive() {
		return d.running
	}
	return nil
}
