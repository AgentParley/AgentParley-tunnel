package cliround

import "errors"

// The sentinel errors map one-to-one onto the OperationError codes dispatchInvokeHarness sends
// (AgentParley.Cloud.Domain/Ssh/SshEgressErrorCodes.cs).
var (
	ErrSuperseded     = errors.New("a newer round for this conversation has taken over")
	ErrBusy           = errors.New("the box is running too many chats on this model right now")
	ErrSignedOut      = errors.New("the CLI is signed out — run `agentparley register` again on this box")
	ErrUsageLimit     = errors.New("the CLI account has hit its usage limit")
	ErrUpdateRequired = errors.New("the CLI vendor requires a newer CLI build")
	ErrContextFull    = errors.New("the CLI's context window is full")

	ErrPayloadTooLarge = errors.New("the round request is too large")
)

// HarnessError is any other CLI failure, worded for the person reading the chat.
type HarnessError struct {
	Message string
}

func (e *HarnessError) Error() string {
	return e.Message
}
