package cliround

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
)

// Frame is everything a warm session was started with that cannot change inside it.
type Frame struct {
	Model, Effort, SystemHash, ToolsHash string
}

// Consumed is what a warm session has taken in, and the output it last produced and is waiting on Platform for.
type Consumed struct {
	Keys    []string
	Pending *Output
}

type Output struct {
	CallIDs    []string
	AnswerText string
	IsAnswer   bool
}

type Plan struct {
	Reason   RebuildReason
	Accept   []string
	Results  []Item
	NewInput []Item
}

// FrameOf derives the frame a request implies. Tools are hashed in canonical form so a re-ordered JSON object in a
// tool schema does not count as a changed tool set.
func FrameOf(model string, request *Request) Frame {
	system := ""
	if request.System != nil {
		system = *request.System
	}
	return Frame{
		Model:      model,
		Effort:     request.Effort,
		SystemHash: hashOf([]byte(system)),
		ToolsHash:  hashOf(canonicalTools(request.Tools)),
	}
}

func canonicalTools(tools []Tool) []byte {
	canonical := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		var parameters any
		if len(tool.Parameters) > 0 {
			if err := json.Unmarshal(tool.Parameters, &parameters); err != nil {
				parameters = string(tool.Parameters)
			}
		}
		canonical = append(canonical, map[string]any{"name": tool.Name, "description": tool.Description, "parameters": parameters})
	}
	encoded, _ := json.Marshal(canonical) // cannot fail: only strings and decoded JSON values go in
	return encoded
}

func hashOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Reconcile decides whether the warm session can take items as they are (Reason "none", with the pieces to feed it)
// or has to be rebuilt, and why. replyDirectives are the stage-direction names Platform strips from a stored reply.
func Reconcile(frame Frame, sessionFrame *Frame, consumed *Consumed, items []Item, replyDirectives []string) Plan {
	if sessionFrame == nil || consumed == nil {
		return Plan{Reason: RebuildNoSession}
	}
	if frame != *sessionFrame {
		return Plan{Reason: RebuildFrameChanged}
	}
	if len(items) < len(consumed.Keys) {
		return Plan{Reason: RebuildHistoryDiverged}
	}
	for index, key := range consumed.Keys {
		if key == "" || items[index].Key != key {
			return Plan{Reason: RebuildHistoryDiverged}
		}
	}

	remaining := items[len(consumed.Keys):]
	if consumed.Pending == nil {
		if !allUserItems(remaining) {
			return Plan{Reason: RebuildHistoryDiverged}
		}
		return Plan{Reason: RebuildNone, NewInput: remaining}
	}

	if consumed.Pending.IsAnswer {
		return reconcileAnswer(consumed.Pending, remaining, replyNormalizer(replyDirectives))
	}
	return reconcileCalls(consumed.Pending, remaining)
}

func reconcileAnswer(pending *Output, remaining []Item, normalize func(string) string) Plan {
	if len(remaining) == 0 {
		return Plan{Reason: RebuildReplyChanged}
	}
	accepted := remaining[0]
	if accepted.Role != RoleAssistant || len(accepted.Calls) > 0 ||
		normalize(accepted.Text) != normalize(pending.AnswerText) {
		return Plan{Reason: RebuildReplyChanged}
	}
	if accepted.Key == "" || !allUserItems(remaining[1:]) {
		return Plan{Reason: RebuildHistoryDiverged}
	}
	return Plan{Reason: RebuildNone, Accept: []string{accepted.Key}, NewInput: remaining[1:]}
}

func reconcileCalls(pending *Output, remaining []Item) Plan {
	if len(remaining) == 0 {
		return Plan{Reason: RebuildHistoryDiverged}
	}
	accepted := remaining[0]
	if accepted.Role != RoleAssistant || accepted.Key == "" || !sameCallIDs(accepted.Calls, pending.CallIDs) {
		return Plan{Reason: RebuildHistoryDiverged}
	}

	resultByCallID := map[string]Item{}
	cursor := 1
	for cursor < len(remaining) && remaining[cursor].Role == RoleTool {
		result := remaining[cursor]
		if _, duplicate := resultByCallID[result.CallID]; duplicate || !containsCallID(pending.CallIDs, result.CallID) {
			return Plan{Reason: RebuildHistoryDiverged}
		}
		resultByCallID[result.CallID] = result
		cursor++
	}
	if len(resultByCallID) != len(pending.CallIDs) || !allUserItems(remaining[cursor:]) {
		return Plan{Reason: RebuildHistoryDiverged}
	}

	results := make([]Item, 0, len(pending.CallIDs))
	for _, callID := range pending.CallIDs {
		results = append(results, resultByCallID[callID])
	}
	return Plan{Reason: RebuildNone, Accept: []string{accepted.Key}, Results: results, NewInput: remaining[cursor:]}
}

func allUserItems(items []Item) bool {
	for _, item := range items {
		if item.Role != RoleUser {
			return false
		}
	}
	return true
}

func sameCallIDs(calls []Call, callIDs []string) bool {
	if len(calls) != len(callIDs) {
		return false
	}
	for index, call := range calls {
		if call.ID != callIDs[index] {
			return false
		}
	}
	return true
}

func containsCallID(callIDs []string, callID string) bool {
	for _, candidate := range callIDs {
		if candidate == callID {
			return true
		}
	}
	return false
}

// replyNormalizer applies the same presentation clean-up Platform applies to a final answer before storing it
// (the request's reply directives, written [name: ...], stripped, then trim), so a stored reply that differs from the
// model's only by a stripped directive still counts as the model's own reply.
func replyNormalizer(directives []string) func(string) string {
	quoted := make([]string, 0, len(directives))
	for _, directive := range directives {
		if name := strings.TrimSpace(directive); name != "" {
			quoted = append(quoted, regexp.QuoteMeta(name))
		}
	}
	if len(quoted) == 0 {
		return strings.TrimSpace
	}
	pattern := regexp.MustCompile(`(?i)[ \t]?\[\s*(?:` + strings.Join(quoted, "|") + `)\s*:[^\[\]\r\n]{0,60}\]`)
	return func(text string) string {
		if strings.Contains(text, "[") {
			text = stripUnlessLink(pattern, text)
		}
		return strings.TrimSpace(text)
	}
}

func stripUnlessLink(pattern *regexp.Regexp, text string) string {
	var builder strings.Builder
	cursor := 0
	for _, match := range pattern.FindAllStringIndex(text, -1) {
		builder.WriteString(text[cursor:match[0]])
		if match[1] < len(text) && text[match[1]] == '(' {
			builder.WriteString(text[match[0]:match[1]])
		}
		cursor = match[1]
	}
	builder.WriteString(text[cursor:])
	return builder.String()
}
