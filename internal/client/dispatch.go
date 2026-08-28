package client

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agentparley/tunnel/internal/files"
	"github.com/agentparley/tunnel/internal/harness"
	tunnelpb "github.com/agentparley/tunnel/internal/proto"
	"github.com/agentparley/tunnel/internal/shellrun"
)

// These mirror AgentParley.Cloud.Domain/Ssh/SshLimits.cs's own defaults — kept here as named constants (not scattered
// literals) so the two sides' relationship is visible at the call site, even though nothing enforces they stay equal.
const (
	defaultCommandTimeoutSeconds = 60
	defaultMaxOutputBytes        = 200_000
	defaultMaxListEntries        = 1_000
	defaultMaxReadWriteBytes     = 2_000_000
)

// These mirror AgentParley.Cloud.Domain/Ssh/SshEgressErrorCodes.cs — the C# side pattern-matches these exact
// strings, so a rename here without a matching rename there is a silent behavior change, not a compile error.
const (
	errorCodePolicyDenied       = "policy_denied"
	errorCodeInternal           = "internal"
	errorCodeTimeout            = "timeout"
	errorCodeTooLarge           = "too_large"
	errorCodeInvalidRequest     = "invalid_request"
	errorCodeHarnessUnavailable = "harness_unavailable"
)

// Mirrors AgentParley.Cloud.Domain/Ssh/SshEgressLimits.cs's HarnessInvokeTimeoutSeconds.
const defaultHarnessInvokeTimeoutSeconds = 600

// operationTimeoutSeconds mirrors the per-kind default each dispatchXxx below applies to its own
// context.WithTimeout, so handleOperation's queue-wait deadline (before dispatch ever runs) matches the deadline
// the operation will get once it runs — a queued invoke_harness must wait up to 600s for a slot, not 60s.
func operationTimeoutSeconds(operation *tunnelpb.Operation) int {
	if timeoutSeconds := int(operation.GetTimeoutSeconds()); timeoutSeconds > 0 {
		return timeoutSeconds
	}
	if _, ok := operation.GetCall().(*tunnelpb.Operation_InvokeHarness); ok {
		return defaultHarnessInvokeTimeoutSeconds
	}
	return defaultCommandTimeoutSeconds
}

// dispatch runs one Operation against this box and returns the AgentMessage to send back — always a Result or an
// Error, correlated by the operation's own correlation_id. It never returns a Go error itself: every failure this
// package can produce has an OperationError code, and an unexpected panic is out of scope for a single command.
func (d *Daemon) dispatch(ctx context.Context, operation *tunnelpb.Operation) *tunnelpb.AgentMessage {
	correlationID := operation.GetCorrelationId()

	if err := d.checkPolicy(operation); err != nil {
		return errorMessage(correlationID, errorCodePolicyDenied, err.Error())
	}

	// invoke_harness carries no shell session (session_id is always 0) — it must be dispatched before the
	// session-id guard and the session ledger below, or a completion would either be rejected as an invalid
	// session-free request or spuriously mark session 0 as served.
	if invokeHarnessCall, ok := operation.GetCall().(*tunnelpb.Operation_InvokeHarness); ok {
		return d.dispatchInvokeHarness(ctx, correlationID, operation, invokeHarnessCall.InvokeHarness)
	}

	sessionID := operation.GetSessionId()
	if sessionID == 0 {
		// No genuinely session-free caller exists on the agent path today (proto comment) — a zero here means an
		// upstream bug, not a deliberate omission, and must not silently report a non-fresh shell for state that
		// was never established.
		return errorMessage(correlationID, errorCodeInvalidRequest, "session id is required")
	}

	isFreshShell, unlockSession := d.markSessionServed(sessionID, operation.GetShellStateGeneration())
	defer unlockSession()

	switch call := operation.GetCall().(type) {
	case *tunnelpb.Operation_RunCommand:
		return d.dispatchRunCommand(ctx, correlationID, isFreshShell, operation, call.RunCommand)
	case *tunnelpb.Operation_ListDirectory:
		return d.dispatchListDirectory(correlationID, isFreshShell, call.ListDirectory)
	case *tunnelpb.Operation_ReadFile:
		return d.dispatchReadFile(correlationID, isFreshShell, call.ReadFile)
	case *tunnelpb.Operation_WriteFile:
		return d.dispatchWriteFile(correlationID, isFreshShell, call.WriteFile)
	case *tunnelpb.Operation_DeleteFile:
		return d.dispatchDeleteFile(correlationID, isFreshShell, call.DeleteFile)
	default:
		return errorMessage(correlationID, errorCodeInternal, "unknown operation kind")
	}
}

func (d *Daemon) checkPolicy(operation *tunnelpb.Operation) error {
	switch call := operation.GetCall().(type) {
	case *tunnelpb.Operation_RunCommand:
		return d.policy.CheckRunCommand(call.RunCommand.GetPolicyMatchCommand())
	case *tunnelpb.Operation_ListDirectory:
		return d.policy.CheckRead()
	case *tunnelpb.Operation_ReadFile:
		return d.policy.CheckRead()
	case *tunnelpb.Operation_WriteFile:
		return d.policy.CheckWrite()
	case *tunnelpb.Operation_DeleteFile:
		return d.policy.CheckWrite()
	case *tunnelpb.Operation_InvokeHarness:
		return d.policy.CheckHarness(call.InvokeHarness.GetHarness())
	default:
		return nil
	}
}

// markSessionServed reports whether this is the first operation this daemon has served for the (session,
// generation) pair (the caller guarantees sessionID != 0), and returns an unlock func the caller must hold for the
// remainder of the WHOLE operation (not just this call) — restore-then-capture on the far side races if two
// commands for one session overlap. The lock itself is keyed on sessionID alone (not the generation), so a command
// against the pre-reset generation and one against the post-reset generation still serialize against each other.
func (d *Daemon) markSessionServed(sessionID int64, shellStateGeneration int32) (isFreshShell bool, unlock func()) {
	mutex := d.ledger.Lock(sessionID)

	isFreshShell, err := d.ledger.MarkServed(d.runAsUser.Home, sessionID, shellStateGeneration)
	if err != nil {
		d.logger.Printf("session ledger write failed for session %d: %v — treating as fresh", sessionID, err)
		return true, mutex.Unlock
	}
	return isFreshShell, mutex.Unlock
}

func (d *Daemon) dispatchRunCommand(ctx context.Context, correlationID string, isFreshShell bool, operation *tunnelpb.Operation, call *tunnelpb.RunCommandCall) *tunnelpb.AgentMessage {
	timeoutSeconds := int(operation.GetTimeoutSeconds())
	if timeoutSeconds <= 0 {
		timeoutSeconds = defaultCommandTimeoutSeconds
	}
	maxOutputBytes := int(call.GetMaxOutputBytes())
	if maxOutputBytes <= 0 {
		maxOutputBytes = defaultMaxOutputBytes
	}

	result, err := shellrun.Run(ctx, d.runAsUser, call.GetCommand(), timeoutSeconds, maxOutputBytes)
	if err != nil {
		return errorMessage(correlationID, errorCodeInternal, fmt.Sprintf("running command: %v", err))
	}
	if result.TimedOut {
		return errorMessage(correlationID, errorCodeTimeout, fmt.Sprintf("the command did not finish within %ds", timeoutSeconds))
	}

	return &tunnelpb.AgentMessage{Kind: &tunnelpb.AgentMessage_Result{Result: &tunnelpb.OperationResult{
		CorrelationId: correlationID,
		IsFreshShell:  isFreshShell,
		Result: &tunnelpb.OperationResult_RunCommand{RunCommand: &tunnelpb.RunCommandResult{
			ExitCode:    int32(result.ExitCode),
			Output:      string(result.Stdout),
			ErrorOutput: string(result.Stderr),
			IsTruncated: result.Truncated,
			DurationMs:  int32(result.DurationMs),
		}},
	}}}
}

func (d *Daemon) dispatchListDirectory(correlationID string, isFreshShell bool, call *tunnelpb.ListDirectoryCall) *tunnelpb.AgentMessage {
	maxEntries := int(call.GetMaxEntries())
	if maxEntries <= 0 {
		maxEntries = defaultMaxListEntries
	}

	entries, isTruncated, err := files.List(call.GetPath(), maxEntries)
	if err != nil {
		return errorMessageFrom(correlationID, err)
	}

	wireEntries := make([]*tunnelpb.DirectoryEntry, 0, len(entries))
	for _, entry := range entries {
		wireEntries = append(wireEntries, &tunnelpb.DirectoryEntry{
			Name:           entry.Name,
			IsDirectory:    entry.IsDirectory,
			SizeBytes:      entry.SizeBytes,
			ModifiedUnixMs: entry.ModifiedUnix,
		})
	}

	return &tunnelpb.AgentMessage{Kind: &tunnelpb.AgentMessage_Result{Result: &tunnelpb.OperationResult{
		CorrelationId: correlationID,
		IsFreshShell:  isFreshShell,
		Result: &tunnelpb.OperationResult_ListDirectory{ListDirectory: &tunnelpb.ListDirectoryResult{
			Path:        call.GetPath(),
			Entries:     wireEntries,
			IsTruncated: isTruncated,
		}},
	}}}
}

func (d *Daemon) dispatchReadFile(correlationID string, isFreshShell bool, call *tunnelpb.ReadFileCall) *tunnelpb.AgentMessage {
	maxBytes := call.GetMaxBytes()
	if maxBytes <= 0 {
		maxBytes = defaultMaxReadWriteBytes
	}

	content, isTooLarge, sizeBytes, err := files.Read(call.GetPath(), maxBytes)
	if err != nil {
		return errorMessageFrom(correlationID, err)
	}

	return &tunnelpb.AgentMessage{Kind: &tunnelpb.AgentMessage_Result{Result: &tunnelpb.OperationResult{
		CorrelationId: correlationID,
		IsFreshShell:  isFreshShell,
		Result: &tunnelpb.OperationResult_ReadFile{ReadFile: &tunnelpb.ReadFileResult{
			Path:       call.GetPath(),
			Content:    content,
			IsTooLarge: isTooLarge,
			SizeBytes:  sizeBytes,
		}},
	}}}
}

func (d *Daemon) dispatchWriteFile(correlationID string, isFreshShell bool, call *tunnelpb.WriteFileCall) *tunnelpb.AgentMessage {
	const maxWriteFileBytes = defaultMaxReadWriteBytes
	if len(call.GetContent()) > maxWriteFileBytes {
		return errorMessage(correlationID, errorCodeTooLarge, fmt.Sprintf("write exceeds the %d byte limit", maxWriteFileBytes))
	}

	if err := files.Write(call.GetPath(), call.GetContent()); err != nil {
		return errorMessageFrom(correlationID, err)
	}

	return &tunnelpb.AgentMessage{Kind: &tunnelpb.AgentMessage_Result{Result: &tunnelpb.OperationResult{
		CorrelationId: correlationID,
		IsFreshShell:  isFreshShell,
		Result:        &tunnelpb.OperationResult_WriteFile{WriteFile: &tunnelpb.WriteFileResult{}},
	}}}
}

func (d *Daemon) dispatchDeleteFile(correlationID string, isFreshShell bool, call *tunnelpb.DeleteFileCall) *tunnelpb.AgentMessage {
	if err := files.Delete(call.GetPath()); err != nil {
		return errorMessageFrom(correlationID, err)
	}

	return &tunnelpb.AgentMessage{Kind: &tunnelpb.AgentMessage_Result{Result: &tunnelpb.OperationResult{
		CorrelationId: correlationID,
		IsFreshShell:  isFreshShell,
		Result:        &tunnelpb.OperationResult_DeleteFile{DeleteFile: &tunnelpb.DeleteFileResult{}},
	}}}
}

// dispatchInvokeHarness runs one LLM completion against a locally-registered harness. It never touches the
// session ledger (see the call site's comment) and IsFreshShell stays false — a completion has no shell state to
// report freshness about.
func (d *Daemon) dispatchInvokeHarness(ctx context.Context, correlationID string, operation *tunnelpb.Operation, call *tunnelpb.InvokeHarnessCall) *tunnelpb.AgentMessage {
	timeoutSeconds := int(operation.GetTimeoutSeconds())
	if timeoutSeconds <= 0 {
		timeoutSeconds = defaultHarnessInvokeTimeoutSeconds
	}

	invokeCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
	defer cancel()

	resolvedHarness, err := harness.Resolve(call.GetHarness(), d.tunnelConfig)
	if err != nil {
		return errorMessage(correlationID, errorCodeHarnessUnavailable, err.Error())
	}

	outcome, err := resolvedHarness.Invoke(invokeCtx, call.GetModel(), call.GetPayload())
	if err != nil {
		if invokeCtx.Err() == context.DeadlineExceeded {
			return errorMessage(correlationID, errorCodeTimeout, fmt.Sprintf("the local model did not answer within %ds", timeoutSeconds))
		}
		return errorMessage(correlationID, errorCodeHarnessUnavailable, err.Error())
	}

	return &tunnelpb.AgentMessage{Kind: &tunnelpb.AgentMessage_Result{Result: &tunnelpb.OperationResult{
		CorrelationId: correlationID,
		IsFreshShell:  false,
		Result: &tunnelpb.OperationResult_InvokeHarness{InvokeHarness: &tunnelpb.InvokeHarnessResult{
			Payload:     outcome.Payload,
			StatusCode:  int32(outcome.StatusCode),
			ErrorOutput: outcome.ErrorOutput,
		}},
	}}}
}

func errorMessage(correlationID, code, message string) *tunnelpb.AgentMessage {
	return &tunnelpb.AgentMessage{Kind: &tunnelpb.AgentMessage_Error{Error: &tunnelpb.OperationError{
		CorrelationId: correlationID,
		Code:          code,
		Message:       message,
	}}}
}

func errorMessageFrom(correlationID string, err error) *tunnelpb.AgentMessage {
	var operationError *files.OperationError
	if errors.As(err, &operationError) {
		return errorMessage(correlationID, operationError.Code, operationError.Message)
	}
	return errorMessage(correlationID, errorCodeInternal, err.Error())
}
