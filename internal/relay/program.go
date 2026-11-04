package relay

import (
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/agentparley/tunnel/internal/config"
	"github.com/agentparley/tunnel/internal/shellrun"
)

const (
	stderrTailBytes    = 2048
	programWaitDelay   = 5 * time.Second
	programKillGrace   = 5 * time.Second
	strayPipeCloseWait = 2 * time.Second
	exitMessageWait    = 3 * time.Second
)

// Program is a started relay program. Reading gives its stdout, writing feeds its stdin.
type Program struct {
	cmd      *exec.Cmd
	stdin    *os.File
	stdout   *os.File
	pgid     int
	registry *PidFile
	tail     *tailBuffer

	exited  chan struct{}
	waitErr error

	closeOnce sync.Once
}

// StartProgram execs program.Command directly (never a shell) in its own process group.
func StartProgram(user *shellrun.User, program config.RelayProgram, registry *PidFile) (*Program, *Error) {
	stdinRead, stdinWrite, err := os.Pipe()
	if err != nil {
		return nil, &Error{CodeInternal, err.Error()}
	}
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		stdinRead.Close()
		stdinWrite.Close()
		return nil, &Error{CodeInternal, err.Error()}
	}
	closePipes := func() {
		for _, file := range []*os.File{stdinRead, stdinWrite, stdoutRead, stdoutWrite} {
			file.Close()
		}
	}

	tail := &tailBuffer{limit: stderrTailBytes}
	cmd := exec.Command(program.Command[0], program.Command[1:]...)
	cmd.Dir = user.Home
	cmd.Env = programEnvironment(user, program.Env)
	cmd.Stdin = stdinRead
	cmd.Stdout = stdoutWrite
	cmd.Stderr = tail
	cmd.WaitDelay = programWaitDelay
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}

	started := make(chan error, 1)
	exited := make(chan struct{})
	// Pdeathsig fires when the thread that forked the child exits, so that thread must outlive the process.
	go func() {
		runtime.LockOSThread()
		started <- cmd.Start()
		<-exited
	}()
	if err := <-started; err != nil {
		close(exited)
		closePipes()
		return nil, &Error{CodeProgramFailed, err.Error()}
	}
	stdinRead.Close()
	stdoutWrite.Close()

	running := &Program{cmd: cmd, stdin: stdinWrite, stdout: stdoutRead, pgid: cmd.Process.Pid, registry: registry, tail: tail, exited: exited}
	go func() {
		running.waitErr = cmd.Wait()
		close(exited)
		// A grandchild that kept stdout open must not keep the pipe alive after the program itself is gone.
		time.AfterFunc(strayPipeCloseWait, func() { stdoutRead.Close() })
	}()
	if registry != nil {
		if err := registry.Add(running.pgid); err != nil {
			running.Close()
			return nil, &Error{CodeInternal, "recording the program for cleanup: " + err.Error()}
		}
	}
	return running, nil
}

func programEnvironment(user *shellrun.User, extra map[string]string) []string {
	shell := user.Shell
	if shell == "" {
		shell = "/bin/sh"
	}
	merged := map[string]string{
		"HOME":    user.Home,
		"USER":    user.Username,
		"LOGNAME": user.Username,
		"SHELL":   shell,
		"PATH":    shellrun.DefaultPath,
	}
	for key, value := range extra {
		merged[key] = value
	}
	keys := make([]string, 0, len(merged))
	for key := range merged {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	environment := make([]string, 0, len(keys))
	for _, key := range keys {
		environment = append(environment, key+"="+merged[key])
	}
	return environment
}

func (p *Program) Read(buffer []byte) (int, error)  { return p.stdout.Read(buffer) }
func (p *Program) Write(buffer []byte) (int, error) { return p.stdin.Write(buffer) }

// Close stops the program: stdin closed, SIGTERM to the group, SIGKILL after 5 s if it has not exited.
func (p *Program) Close() error {
	p.closeOnce.Do(func() {
		p.stdin.Close()
		_ = syscall.Kill(-p.pgid, syscall.SIGTERM)
		select {
		case <-p.exited:
		case <-time.After(programKillGrace):
			_ = syscall.Kill(-p.pgid, syscall.SIGKILL)
			<-p.exited
		}
		p.stdout.Close()
		if p.registry != nil {
			p.registry.Remove(p.pgid)
		}
	})
	return nil
}

// ExitMessage describes how the program ended: "exit status N"; the stderr tail goes to the local log only.
func (p *Program) ExitMessage() string {
	select {
	case <-p.exited:
	case <-time.After(exitMessageWait):
		return "program closed its output"
	}
	status := "exit status 0"
	var exitError *exec.ExitError
	if errors.As(p.waitErr, &exitError) {
		status = exitError.ProcessState.String()
	} else if p.waitErr != nil {
		status = p.waitErr.Error()
	}
	if tail := p.tail.String(); tail != "" {
		log.Printf("relay program %d stderr tail: %s", p.pgid, tail)
	}
	return status
}

var _ io.ReadWriteCloser = (*Program)(nil)

type tailBuffer struct {
	mu    sync.Mutex
	limit int
	data  []byte
}

func (t *tailBuffer) Write(chunk []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.data = append(t.data, chunk...)
	if len(t.data) > t.limit {
		t.data = t.data[len(t.data)-t.limit:]
	}
	return len(chunk), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.data)
}
