package client

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	tunnelpb "github.com/agentparley/tunnel/internal/proto"
	"github.com/agentparley/tunnel/internal/wsproto"
)

// workspaceCallTimeout bounds one host-initiated workspace control call end to end (send up the tunnel, platform
// mints/commits, result back). The file bytes never ride this path, so this is a control-message deadline, not a
// transfer one.
const workspaceCallTimeout = 30 * time.Second

// workspaceBridge serves the local unix socket for the `agentparley ws` subcommand: it turns each JSON request into
// a WorkspaceCall up the CURRENT tunnel stream and correlates the WorkspaceResult that comes back. It lives for the
// daemon's whole lifetime (one listener across reconnects); the active send channel is swapped in and out as streams
// come and go, so a call while disconnected returns not_connected rather than hanging.
type workspaceBridge struct {
	socketPath string
	logger     *log.Logger

	mu      sync.Mutex
	send    chan<- *tunnelpb.AgentMessage // the active stream's send queue; nil when disconnected
	done    <-chan struct{}              // closed when the active stream ends
	pending map[string]chan *tunnelpb.WorkspaceResult
	nextID  uint64
}

func newWorkspaceBridge(socketPath string, logger *log.Logger) *workspaceBridge {
	return &workspaceBridge{
		socketPath: socketPath,
		logger:     logger,
		pending:    make(map[string]chan *tunnelpb.WorkspaceResult),
	}
}

// setStream binds the bridge to a freshly connected stream's send queue and done signal; clearStream unbinds it and
// fails every in-flight call (a closed result channel reads as a nil result, which callers map to not_connected).
func (b *workspaceBridge) setStream(send chan<- *tunnelpb.AgentMessage, done <-chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.send = send
	b.done = done
}

func (b *workspaceBridge) clearStream() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.send = nil
	b.done = nil
	for callID, resultChan := range b.pending {
		close(resultChan)
		delete(b.pending, callID)
	}
}

// deliver routes a WorkspaceResult from the receive loop to the waiting call. Unknown or timed-out call ids are
// dropped silently — the caller has already given up and removed its pending entry.
func (b *workspaceBridge) deliver(result *tunnelpb.WorkspaceResult) {
	b.mu.Lock()
	resultChan, ok := b.pending[result.GetCallId()]
	if ok {
		delete(b.pending, result.GetCallId())
	}
	b.mu.Unlock()
	if ok {
		resultChan <- result
	}
}

// call sends one WorkspaceCall up the current stream and waits for its result. Returns a not_connected error result
// when no stream is bound or it ends mid-call, and a timeout error result when the platform does not answer in time.
func (b *workspaceBridge) call(workspaceCall *tunnelpb.WorkspaceCall) *tunnelpb.WorkspaceResult {
	resultChan := make(chan *tunnelpb.WorkspaceResult, 1)

	b.mu.Lock()
	if b.send == nil {
		b.mu.Unlock()
		return errorResult("not_connected", "the AgentParley tunnel is not connected")
	}
	b.nextID++
	callID := strconv.FormatUint(b.nextID, 10)
	b.pending[callID] = resultChan
	send := b.send
	done := b.done
	b.mu.Unlock()

	workspaceCall.CallId = callID
	message := &tunnelpb.AgentMessage{Kind: &tunnelpb.AgentMessage_WorkspaceCall{WorkspaceCall: workspaceCall}}

	select {
	case send <- message:
	case <-done:
		b.forget(callID)
		return errorResult("not_connected", "the AgentParley tunnel is not connected")
	}

	select {
	case result := <-resultChan:
		if result == nil { // clearStream closed the channel — the stream ended mid-call
			return errorResult("not_connected", "the AgentParley tunnel is not connected")
		}
		return result
	case <-time.After(workspaceCallTimeout):
		b.forget(callID)
		return errorResult("timeout", "the workspace request timed out")
	}
}

func (b *workspaceBridge) forget(callID string) {
	b.mu.Lock()
	delete(b.pending, callID)
	b.mu.Unlock()
}

// serveSocket listens on the unix socket for the daemon's whole lifetime, accepting one JSON request per connection.
func (b *workspaceBridge) serveSocket(ctx context.Context) {
	if err := os.MkdirAll(filepath.Dir(b.socketPath), 0o700); err != nil {
		b.logger.Printf("workspace bridge: cannot create socket directory: %v", err)
		return
	}
	_ = os.Remove(b.socketPath) // a stale socket from a previous run would block Listen
	listener, err := net.Listen("unix", b.socketPath)
	if err != nil {
		b.logger.Printf("workspace bridge: cannot listen on %s: %v", b.socketPath, err)
		return
	}
	if err := os.Chmod(b.socketPath, 0o600); err != nil {
		b.logger.Printf("workspace bridge: cannot chmod socket: %v", err)
	}
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return // the daemon is shutting down; the deferred Close above tripped Accept
			}
			continue
		}
		go b.handleConn(conn)
	}
}

func (b *workspaceBridge) handleConn(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(workspaceCallTimeout + 5*time.Second))

	var req wsproto.Request
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		_ = json.NewEncoder(conn).Encode(wsproto.Response{ErrorCode: "internal", ErrorMessage: "malformed request"})
		return
	}

	workspaceCall, err := buildWorkspaceCall(req)
	if err != nil {
		_ = json.NewEncoder(conn).Encode(wsproto.Response{ErrorCode: "internal", ErrorMessage: err.Error()})
		return
	}

	_ = json.NewEncoder(conn).Encode(toResponse(b.call(workspaceCall)))
}

func buildWorkspaceCall(req wsproto.Request) (*tunnelpb.WorkspaceCall, error) {
	workspaceCall := &tunnelpb.WorkspaceCall{SessionId: req.SessionID, AgentSelector: req.AgentSelector}
	switch req.Op {
	case "read":
		workspaceCall.Op = &tunnelpb.WorkspaceCall_ReadOpen{ReadOpen: &tunnelpb.WorkspaceReadOpen{Path: req.Path}}
	case "writeBegin":
		workspaceCall.Op = &tunnelpb.WorkspaceCall_WriteBegin{WriteBegin: &tunnelpb.WorkspaceWriteBegin{Path: req.Path, SizeBytes: req.SizeBytes}}
	case "writeCommit":
		workspaceCall.Op = &tunnelpb.WorkspaceCall_WriteCommit{WriteCommit: &tunnelpb.WorkspaceWriteCommit{Path: req.Path, UploadToken: req.UploadToken, ContentHash: req.ContentHash}}
	default:
		return nil, fmt.Errorf("unknown workspace op %q", req.Op)
	}
	return workspaceCall, nil
}

func toResponse(result *tunnelpb.WorkspaceResult) wsproto.Response {
	switch {
	case result.GetReadTarget() != nil:
		return wsproto.Response{OK: true, URL: result.GetReadTarget().GetUrl()}
	case result.GetWriteTarget() != nil:
		target := result.GetWriteTarget()
		return wsproto.Response{OK: true, URL: target.GetUrl(), Fields: target.GetFields(), UploadToken: target.GetUploadToken()}
	case result.GetCommitted() != nil:
		committed := result.GetCommitted()
		return wsproto.Response{OK: true, DisplayPath: committed.GetDisplayPath(), SizeBytes: committed.GetSizeBytes()}
	default:
		callError := result.GetError()
		if callError == nil {
			return wsproto.Response{ErrorCode: "internal", ErrorMessage: "empty workspace result"}
		}
		return wsproto.Response{ErrorCode: callError.GetCode(), ErrorMessage: callError.GetMessage()}
	}
}

func errorResult(code, message string) *tunnelpb.WorkspaceResult {
	return &tunnelpb.WorkspaceResult{Outcome: &tunnelpb.WorkspaceResult_Error{Error: &tunnelpb.WorkspaceCallError{Code: code, Message: message}}}
}
