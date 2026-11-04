// Package relay lets the daemon pass a connection on to an address or program the box owner has allowed, and
// nothing else. It never parses the bytes it carries.
package relay

import "fmt"

// Relay codes the daemon raises; the wire contract lives in Libs/Ssh's SshEgressErrorCodes.cs.
const (
	CodeRelayBusy           = "relay_busy"
	CodeRelayNotAllowed     = "relay_not_allowed"
	CodeRelayProgramUnknown = "relay_program_unknown"
	CodePolicyDenied        = "policy_denied"
	CodeConnectRefused      = "connect_refused"
	CodeConnectTimedOut     = "connect_timed_out"
	CodeConnectFailed       = "connect_failed"
	CodeProgramFailed       = "program_failed"
	CodeProgramExited       = "program_exited"
	CodeInvalidRequest      = "invalid_request"
	CodeInternal            = "internal"
)

// Error is a refusal or failure with the wire code the egress will see.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }
