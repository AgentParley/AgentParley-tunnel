// Package client drives the daemon's one long-lived job: refresh an access token, dial the SSH egress service,
// hold its Connect stream open, and answer the Operations the platform sends down it — reconnecting forever on a
// dropped connection, and exiting only on a terminal rejection (design doc: "network drops → jittered exponential
// backoff … forever. A terminal rejection … stops the loop and logs loudly").
package client

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/agentparley/tunnel/internal/config"
	"github.com/agentparley/tunnel/internal/credstore"
	"github.com/agentparley/tunnel/internal/policy"
	tunnelpb "github.com/agentparley/tunnel/internal/proto"
	"github.com/agentparley/tunnel/internal/sessions"
	"github.com/agentparley/tunnel/internal/shellrun"
	"github.com/agentparley/tunnel/internal/version"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
)

// RouteHeader is this leg's copy of Domain.Ssh.SshEgressRouteHeader.Name — the load-balancer hash key both the
// Runtime's operation calls and this connect stream MUST send identically, or the two land on different egress
// instances (design doc: "a gRPC request's path is not ours to choose").
const RouteHeader = "x-agentparley-route" // gRPC metadata keys are lower-cased on the wire regardless of case here

// Keepalive matches SshEgressLimits.KeepAliveSeconds/KeepAliveTimeoutSeconds exactly — a mismatch would leave a
// stateful NAT free to silently drop the idle flow with neither side noticing (design doc, "The wire protocol").
const (
	keepAliveInterval = 30 * time.Second
	keepAliveTimeout  = 10 * time.Second
)

const maxReconnectBackoffSeconds = 60

// defaultMaxConcurrentOperations bounds how many operations this daemon runs at once when config.yaml doesn't
// override it — a crew of agents (or a tool call racing a plugin's host.ssh.command callback) can otherwise fork
// an unbounded number of processes on hardware we don't own.
const defaultMaxConcurrentOperations = 8

const (
	refreshHTTPTimeout    = 15 * time.Second
	deregisterGracePeriod = 200 * time.Millisecond
	sendQueueDepth        = 32
	receivedQueueDepth    = 1
	sessionStateTTL       = 72 * time.Hour // SshEgressLimits.SessionStateTtlHours
)

type Daemon struct {
	apiBaseURL         string
	egressAddress      string
	allowInsecure      bool
	store              credstore.Store
	tunnelConfig       *config.Config
	policy             *policy.Policy
	ledger             *sessions.Ledger
	runAsUser          *shellrun.User
	logger             *log.Logger
	operationSemaphore chan struct{}
	workspaceSocketPath string
	workspaceBridge     *workspaceBridge
}

func New(apiBaseURL, egressAddress string, allowInsecure bool, store credstore.Store, tunnelConfig *config.Config, runAsUser *shellrun.User) *Daemon {
	maxConcurrentOperations := tunnelConfig.MaxConcurrentOperations
	if maxConcurrentOperations <= 0 {
		maxConcurrentOperations = defaultMaxConcurrentOperations
	}

	workspaceSocketPath := sessions.WorkspaceSocketPath(runAsUser.Home)

	return &Daemon{
		apiBaseURL:         apiBaseURL,
		egressAddress:      egressAddress,
		allowInsecure:      allowInsecure,
		store:              store,
		tunnelConfig:       tunnelConfig,
		policy:             policy.New(tunnelConfig),
		ledger:             sessions.NewLedger(),
		runAsUser:          runAsUser,
		logger:             log.Default(),
		operationSemaphore: make(chan struct{}, maxConcurrentOperations),
		workspaceSocketPath: workspaceSocketPath,
		workspaceBridge:     newWorkspaceBridge(workspaceSocketPath, log.Default()),
	}
}

// Run blocks until ctx is cancelled (a clean SIGTERM shutdown) or a terminal rejection ends the loop.
func (d *Daemon) Run(ctx context.Context) error {
	creds, err := d.store.Load()
	if err != nil {
		return fmt.Errorf("loading stored credentials: %w", err)
	}
	if creds == nil {
		return fmt.Errorf("not logged in — run 'agentparley-tunnel login' first")
	}

	go d.runLedgerSweep(ctx)
	go d.workspaceBridge.serveSocket(ctx)

	reconnectDelay := newBackoff(maxReconnectBackoffSeconds)
	for {
		if ctx.Err() != nil {
			return nil
		}

		connectErr := d.connectOnce(ctx, creds, reconnectDelay.reset)
		if connectErr == nil {
			reconnectDelay.reset()
			continue
		}
		if ctx.Err() != nil {
			return nil
		}

		var terminalErr *terminalRefreshError
		if errors.As(connectErr, &terminalErr) {
			d.logger.Printf("TERMINAL: %v — not retrying; delete and re-enrol this install", connectErr)
			return connectErr
		}

		wait := reconnectDelay.next()
		d.logger.Printf("connection ended (%v); reconnecting in %s", connectErr, wait.Round(time.Second))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

func (d *Daemon) connectOnce(ctx context.Context, creds *credstore.Credentials, onConnected func()) error {
	accessToken, err := refreshAccessToken(&http.Client{Timeout: refreshHTTPTimeout}, d.apiBaseURL, creds)
	if err != nil {
		return err
	}

	transportCredentials, err := dialCredentials(d.egressAddress, d.allowInsecure)
	if err != nil {
		return &terminalRefreshError{detail: err.Error()} // a bad --insecure target is a config mistake, not worth retrying
	}

	connection, err := grpc.NewClient(d.egressAddress,
		grpc.WithTransportCredentials(transportCredentials),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                keepAliveInterval,
			Timeout:             keepAliveTimeout,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return fmt.Errorf("dialing the egress service: %w", err)
	}
	defer connection.Close()

	streamCtx := metadata.AppendToOutgoingContext(ctx, RouteHeader, creds.RoutingKey, "authorization", "Bearer "+accessToken)
	tunnelClient := tunnelpb.NewTunnelServiceClient(connection)
	stream, err := tunnelClient.Connect(streamCtx)
	if err != nil {
		return fmt.Errorf("opening the connect stream: %w", err)
	}

	return d.serve(ctx, stream, creds, onConnected)
}

// serve sends Hello, then relays ServerMessages to dispatch and results back — everything after this point is the
// operation loop, until the stream ends (network drop, a Shutdown message, or ctx cancellation).
func (d *Daemon) serve(ctx context.Context, stream tunnelpb.TunnelService_ConnectClient, creds *credstore.Credentials, onConnected func()) error {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}

	if err := stream.Send(&tunnelpb.AgentMessage{Kind: &tunnelpb.AgentMessage_Hello{Hello: &tunnelpb.Hello{
		MachineId:       creds.MachineID,
		AgentVersion:    version.Version,
		RunAsUser:       d.runAsUser.Username,
		Hostname:        hostname,
		OperatingSystem: runtime.GOOS,
		Architecture:    runtime.GOARCH,
	}}}); err != nil {
		return fmt.Errorf("sending hello: %w", err)
	}

	sendQueue := make(chan *tunnelpb.AgentMessage, sendQueueDepth)
	sendErrors := make(chan error, 1)
	// done, not close(sendQueue), signals in-flight handleOperation goroutines that this stream is over — an
	// operation can still be running (a long command) after serve returns, and a send on a CLOSED channel panics
	// the whole process. done is safe to close from multiple places; sendQueue is never closed.
	done := make(chan struct{})
	defer close(done)
	// streamCtx is derived from the daemon's lifetime ctx but cancelled the moment THIS stream ends (network
	// drop, Shutdown, ctx cancellation) — not just when the result can no longer be delivered. Without this, an
	// in-flight invoke_harness (up to 600s: a local model completion or a CLI run) keeps running under the
	// daemon-lifetime ctx after its stream is gone, the daemon reconnects in about a second, and the platform's
	// retry lands a SECOND identical multi-minute job on the box while the first is still burning it. runCLI
	// already SIGKILLs on ctx cancellation and the server-family HTTP calls already honor ctx — only the wiring
	// from stream lifetime to operation ctx was missing.
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	go d.runWriter(stream, sendQueue, sendErrors, done)

	// Bind the workspace bridge to THIS stream so host-initiated `agentparley ws` calls ride it; clearStream on
	// return fails any in-flight bridge call with not_connected rather than letting it hang to its own timeout.
	d.workspaceBridge.setStream(sendQueue, done)
	defer d.workspaceBridge.clearStream()

	received := make(chan *tunnelpb.ServerMessage, receivedQueueDepth)
	recvErrors := make(chan error, 1)
	go func() {
		for {
			serverMessage, err := stream.Recv()
			if err != nil {
				recvErrors <- err
				return
			}
			received <- serverMessage
		}
	}()

	for {
		select {
		case <-ctx.Done():
			// Best-effort courtesy to the server — SIGTERM's "deregister, wait briefly for in-flight operations,
			// close cleanly" (design doc edge cases). The connection closing outright a moment later, once
			// connectOnce's deferred Close runs, is what actually ends the stream either way.
			sendQueue <- &tunnelpb.AgentMessage{Kind: &tunnelpb.AgentMessage_Deregister{Deregister: &tunnelpb.Deregister{}}}
			time.Sleep(deregisterGracePeriod)
			return nil

		case err := <-recvErrors:
			return fmt.Errorf("stream ended: %w", err)

		case sendErr := <-sendErrors:
			return sendErr

		case serverMessage := <-received:
			switch kind := serverMessage.GetKind().(type) {
			case *tunnelpb.ServerMessage_Connected:
				d.logger.Printf("connected: connectionId=%s", kind.Connected.GetConnectionId())
				onConnected() // the stream proved healthy — a backoff that never resets punishes every later blip
			case *tunnelpb.ServerMessage_Operation:
				go d.handleOperation(streamCtx, kind.Operation, sendQueue, done)
			case *tunnelpb.ServerMessage_Shutdown:
				return fmt.Errorf("server closed the connection: %s", kind.Shutdown.GetReason())
			case *tunnelpb.ServerMessage_WorkspaceResult:
				d.workspaceBridge.deliver(kind.WorkspaceResult)
			}
		}
	}
}

// handleOperation runs one operation and enqueues its result — dispatch itself never touches the stream, so
// concurrent operations (several turns addressing one host at once) never race on the single writer below.
// operationSemaphore bounds how many run at once (Step 14: "serve the operation loop with a bounded worker pool"),
// and every wait/send here selects on done so a stream that ended while this operation was still running can never
// block this goroutine forever or write to an abandoned queue.
func (d *Daemon) handleOperation(ctx context.Context, operation *tunnelpb.Operation, sendQueue chan<- *tunnelpb.AgentMessage, done <-chan struct{}) {
	timeoutSeconds := operationTimeoutSeconds(operation)
	// The queue wait counts against the operation's OWN deadline, derived here rather than once dispatch runs: a
	// slot that frees after the caller's own timeout has already elapsed must not then execute the side effect the
	// caller has already given up on (an SSH command re-issued while the stale queued one is still pending).
	operationCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
	defer cancel()

	select {
	case d.operationSemaphore <- struct{}{}:
	case <-operationCtx.Done():
		select {
		case sendQueue <- errorMessage(operation.GetCorrelationId(), errorCodeTimeout,
			fmt.Sprintf("the operation waited more than %ds for a free slot on this box", timeoutSeconds)):
		case <-done:
		}
		return
	case <-done:
		return
	}
	defer func() { <-d.operationSemaphore }()

	// A slot freeing in the same instant the deadline expires leaves both select cases ready, and Go picks one at
	// random — so re-check before dispatching, or that coin flip starts the command microseconds before killing it.
	if operationCtx.Err() != nil {
		select {
		case sendQueue <- errorMessage(operation.GetCorrelationId(), errorCodeTimeout,
			fmt.Sprintf("the operation waited more than %ds for a free slot on this box", timeoutSeconds)):
		case <-done:
		}
		return
	}

	message := d.dispatch(operationCtx, operation)

	select {
	case sendQueue <- message:
	case <-done:
	}
}

// runWriter is the ONLY goroutine that calls stream.Send — a gRPC client stream is not safe for concurrent writes,
// and several operations can be in flight for one connection at once (design doc, ConnectionRegistry's TunnelConnection).
// Selects on done rather than ranging over queue, since queue is never closed (see serve's own note on why).
func (d *Daemon) runWriter(stream tunnelpb.TunnelService_ConnectClient, queue <-chan *tunnelpb.AgentMessage, errs chan<- error, done <-chan struct{}) {
	for {
		select {
		case message := <-queue:
			if err := stream.Send(message); err != nil {
				select {
				case errs <- fmt.Errorf("writing to stream: %w", err):
				default:
				}
				return
			}
		case <-done:
			return
		}
	}
}

func (d *Daemon) runLedgerSweep(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := d.ledger.Sweep(d.runAsUser.Home, sessionStateTTL); err != nil {
				d.logger.Printf("session ledger sweep failed: %v", err)
			}
		}
	}
}
