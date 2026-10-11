// Package cliround is the daemon side of a CLI tunnel round: one model step for Codex or Claude Code, asked by
// Platform, answered with either the model's reply or the tool calls it wants run. The package owns the round
// contract, the rule that decides whether a warm CLI session can take the next round as-is or must be rebuilt, and
// the manager that keeps one warm session per conversation. It knows nothing about how a particular CLI is driven;
// each CLI supplies a Runner and a Factory for that.
package cliround

import (
	"encoding/json"
	"fmt"
)

// MaxPayloadBytes is the largest round request the daemon will parse.
const MaxPayloadBytes = 12 << 20

type RebuildReason string

const (
	RebuildNone            RebuildReason = "none"
	RebuildNoSession       RebuildReason = "no_session"
	RebuildFrameChanged    RebuildReason = "frame_changed"
	RebuildHistoryDiverged RebuildReason = "history_diverged"
	RebuildReplyChanged    RebuildReason = "reply_changed"
	RebuildDirty           RebuildReason = "dirty"
	RebuildContextFull     RebuildReason = "context_full"
)

const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

const (
	OutcomeToolCalls = "tool_calls"
	OutcomeAnswer    = "answer"
	OutcomeLength    = "length"
	OutcomeRefused   = "refused"
)

const (
	EventBlockedTool      = "blocked_tool"
	EventInternalStep     = "internal_step"
	EventRecoveredError   = "recovered_error"
	EventContextFull      = "context_full"
	EventOffRoundActivity = "off_round_activity"
)

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type Call struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Item is one history row. A null or empty Key means Platform could not name the row, which makes the session that
// consumed it unreusable on the next round.
type Item struct {
	Key    string `json:"key,omitempty"`
	Role   string `json:"role"`
	Text   string `json:"text"`
	Calls  []Call `json:"calls,omitempty"`
	CallID string `json:"callId,omitempty"`
}

type Request struct {
	Conversation string  `json:"conversation"`
	Fence        *int64  `json:"fence"`
	Effort       string  `json:"effort"`
	System       *string `json:"system"`
	Tools        []Tool  `json:"tools"`
	Items        []Item  `json:"items"`

	// ReplyDirectives are the stage-direction names ([name: ...]) Platform strips from a stored final answer.
	ReplyDirectives []string `json:"replyDirectives"`
}

type Usage struct {
	Input       int64 `json:"input"`
	CachedInput int64 `json:"cachedInput"`
	CacheWrite  int64 `json:"cacheWrite"`
	Output      int64 `json:"output"`
	Requests    int64 `json:"requests"`
}

func (u *Usage) Add(other Usage) {
	u.Input += other.Input
	u.CachedInput += other.CachedInput
	u.CacheWrite += other.CacheWrite
	u.Output += other.Output
	u.Requests += other.Requests
}

func (u Usage) IsZero() bool {
	return u == Usage{}
}

type Event struct {
	Kind   string `json:"kind"`
	Tool   string `json:"tool,omitempty"`
	Detail string `json:"detail"`
}

type Result struct {
	Outcome       string        `json:"outcome"`
	Text          string        `json:"text"`
	Thinking      *string       `json:"thinking"`
	Calls         []Call        `json:"calls"`
	Usage         Usage         `json:"usage"`
	Events        []Event       `json:"events"`
	RebuildReason RebuildReason `json:"rebuildReason"`
	RebuildMs     int64         `json:"rebuildMs"`
}

// InvalidRequestError is a round request the daemon refuses to run; it maps to the invalid_request error code.
type InvalidRequestError struct {
	Message string
}

func (e *InvalidRequestError) Error() string {
	return e.Message
}

// ParseRequest decodes and validates one round request payload.
func ParseRequest(payload string) (*Request, error) {
	if len(payload) > MaxPayloadBytes {
		return nil, ErrPayloadTooLarge
	}
	var request Request
	if err := json.Unmarshal([]byte(payload), &request); err != nil {
		return nil, &InvalidRequestError{Message: fmt.Sprintf("the round request is not valid JSON: %v", err)}
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	return &request, nil
}

// Validate checks the shape of the request. The model id shape is the harness's check, not this one's.
func (r *Request) Validate() error {
	switch r.Effort {
	case "", "low", "medium", "high":
	default:
		return &InvalidRequestError{Message: fmt.Sprintf("unknown effort %q", r.Effort)}
	}
	if (r.Conversation != "") != (r.Fence != nil) {
		return &InvalidRequestError{Message: "conversation and fence must be given together"}
	}
	for index, item := range r.Items {
		switch item.Role {
		case RoleUser:
		case RoleAssistant:
			for _, call := range item.Calls {
				if call.ID == "" {
					return &InvalidRequestError{Message: fmt.Sprintf("item %d has a call without an id", index)}
				}
			}
		case RoleTool:
			if item.CallID == "" {
				return &InvalidRequestError{Message: fmt.Sprintf("item %d is a tool result without a callId", index)}
			}
		default:
			return &InvalidRequestError{Message: fmt.Sprintf("item %d has unknown role %q", index, item.Role)}
		}
	}
	return nil
}
