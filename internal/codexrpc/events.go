package codexrpc

import (
	"encoding/json"
	"strings"

	"github.com/agentparley/tunnel/internal/cliround"
)

type threadItem struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Text      string          `json:"text"`
	Summary   json.RawMessage `json:"summary"`
	Status    string          `json:"status"`
	Namespace string          `json:"namespace"`
	Tool      string          `json:"tool"`
}

func (i threadItem) summaryText() string {
	var parts []string
	if json.Unmarshal(i.Summary, &parts) != nil {
		return ""
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

// nativeItems are Codex's own tools. With every one of them switched off they should never appear; when one does, the
// owner is told in the chat's work block.
var nativeItems = map[string]struct{ tool, detail string }{
	"commandExecution":    {"command", "Codex tried to run a shell command"},
	"fileChange":          {"file_change", "Codex tried to change a file"},
	"mcpToolCall":         {"mcp", "Codex tried to call an MCP tool"},
	"webSearch":           {"web_search", "Codex tried to search the web"},
	"imageGeneration":     {"image_generation", "Codex tried to generate an image"},
	"imageView":           {"view_image", "Codex tried to look at an image file"},
	"collabAgentToolCall": {"sub_agent", "Codex tried to start a helper agent"},
	"subAgentActivity":    {"sub_agent", "Codex tried to start a helper agent"},
}

func (t *Thread) onNotification(method string, params json.RawMessage) {
	t.mutex.Lock()
	defer t.mutex.Unlock()

	if method == "thread/tokenUsage/updated" {
		t.addUsageLocked(params)
		return
	}
	if t.closed {
		if method == "turn/completed" {
			t.turnID = ""
		}
		return
	}

	switch method {
	case "turn/started":
		var started struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		_ = json.Unmarshal(params, &started)
		t.turnID = started.Turn.ID
		if t.round == nil {
			t.offRoundLocked("turn started")
		}
	case "turn/completed":
		t.onTurnCompletedLocked(params)
	case "item/started":
		var started struct {
			Item threadItem `json:"item"`
		}
		_ = json.Unmarshal(params, &started)
		if t.round == nil && started.Item.Type != "userMessage" && started.Item.Type != "dynamicToolCall" {
			t.offRoundLocked("item " + started.Item.Type)
		}
	case "item/completed":
		var completed struct {
			Item threadItem `json:"item"`
		}
		_ = json.Unmarshal(params, &completed)
		t.onItemCompletedLocked(completed.Item)
	case "error":
		var failure struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
			WillRetry bool `json:"willRetry"`
		}
		_ = json.Unmarshal(params, &failure)
		if failure.WillRetry && t.round != nil {
			t.round.events = append(t.round.events, cliround.Event{Kind: cliround.EventRecoveredError, Detail: failure.Error.Message})
		}
	}
}

func (t *Thread) addUsageLocked(params json.RawMessage) {
	var updated struct {
		TokenUsage struct {
			Last struct {
				InputTokens           int64 `json:"inputTokens"`
				CachedInputTokens     int64 `json:"cachedInputTokens"`
				CacheWriteInputTokens int64 `json:"cacheWriteInputTokens"`
				OutputTokens          int64 `json:"outputTokens"`
			} `json:"last"`
		} `json:"tokenUsage"`
	}
	if json.Unmarshal(params, &updated) != nil {
		return
	}
	last := updated.TokenUsage.Last
	usage := cliround.Usage{
		Input:       last.InputTokens - last.CachedInputTokens,
		CachedInput: last.CachedInputTokens,
		CacheWrite:  last.CacheWriteInputTokens,
		Output:      last.OutputTokens,
		Requests:    1,
	}
	if t.round != nil {
		t.round.usage.Add(usage)
		return
	}
	t.carriedUsage.Add(usage)
}

func (t *Thread) onItemCompletedLocked(item threadItem) {
	if t.round == nil {
		if item.Type != "userMessage" && item.Type != "dynamicToolCall" {
			t.offRoundLocked("item " + item.Type)
		}
		return
	}
	current := t.round
	switch item.Type {
	case "agentMessage":
		if text := strings.TrimSpace(item.Text); text != "" {
			current.texts = append(current.texts, text)
		}
	case "reasoning":
		if summary := item.summaryText(); summary != "" {
			current.thinking = append(current.thinking, summary)
		}
	case "plan":
		current.events = append(current.events, cliround.Event{Kind: cliround.EventInternalStep, Tool: "update_plan", Detail: "Codex updated its own plan"})
	case "contextCompaction":
		current.events = append(current.events, cliround.Event{Kind: cliround.EventInternalStep, Tool: "context_compaction", Detail: "Codex compacted its context"})
	default:
		native, isNative := nativeItems[item.Type]
		if !isNative {
			return
		}
		if item.Status == "failed" || item.Status == "declined" {
			current.events = append(current.events, cliround.Event{Kind: cliround.EventBlockedTool, Tool: native.tool, Detail: native.detail})
			return
		}
		current.events = append(current.events, cliround.Event{Kind: cliround.EventInternalStep, Tool: native.tool, Detail: native.detail + " and it ran"})
		t.logger.Printf("codex ran a native tool despite the pin: %s", item.Type)
	}
}

func (t *Thread) onTurnCompletedLocked(params json.RawMessage) {
	t.turnID = ""
	var completed struct {
		Turn struct {
			Status string `json:"status"`
			Error  *struct {
				Message        string          `json:"message"`
				CodexErrorInfo json.RawMessage `json:"codexErrorInfo"`
			} `json:"error"`
		} `json:"turn"`
	}
	_ = json.Unmarshal(params, &completed)
	current := t.round
	if current == nil {
		t.offRoundLocked("turn completed")
		return
	}
	switch completed.Turn.Status {
	case "completed":
		t.finishLocked(current, cliround.OutcomeAnswer, nil)
	case "failed":
		message, info := "Codex failed the turn", json.RawMessage(nil)
		if completed.Turn.Error != nil {
			message, info = completed.Turn.Error.Message, completed.Turn.Error.CodexErrorInfo
		}
		if isRefusal(info) {
			current.texts = append(current.texts, message)
			t.finishLocked(current, cliround.OutcomeRefused, nil)
			return
		}
		t.finishLocked(current, "", mapTurnError(message, info))
	default:
		t.finishLocked(current, "", &cliround.HarnessError{Message: "Codex stopped the turn (" + completed.Turn.Status + ")"})
	}
}

// offRoundLocked records Codex doing something while no round is open: the session is no longer in step with
// AgentParley's history, so it is marked dirty and the next round rebuilds it.
func (t *Thread) offRoundLocked(what string) {
	t.state = cliround.StateDirty
	if t.offRoundLogged {
		return
	}
	t.offRoundLogged = true
	t.carriedEvents = append(t.carriedEvents, cliround.Event{Kind: cliround.EventOffRoundActivity, Detail: "Codex worked between steps"})
	t.logger.Printf("codex: activity between rounds on thread %s (%s); the session will be rebuilt", t.id, what)
}

func codexErrorKind(info json.RawMessage) (kind string, httpStatus int) {
	var name string
	if json.Unmarshal(info, &name) == nil {
		return name, 0
	}
	var variants map[string]struct {
		HTTPStatusCode *int `json:"httpStatusCode"`
	}
	if json.Unmarshal(info, &variants) != nil {
		return "", 0
	}
	for variant, detail := range variants {
		status := 0
		if detail.HTTPStatusCode != nil {
			status = *detail.HTTPStatusCode
		}
		return variant, status
	}
	return "", 0
}

func isRefusal(info json.RawMessage) bool {
	kind, _ := codexErrorKind(info)
	return kind == "cyberPolicy" || kind == "misalignmentPolicyViolation"
}

func mapTurnError(message string, info json.RawMessage) error {
	kind, httpStatus := codexErrorKind(info)
	lowered := strings.ToLower(message)
	switch {
	case kind == "usageLimitExceeded":
		return cliround.ErrUsageLimit
	case kind == "unauthorized":
		return cliround.ErrSignedOut
	case kind == "contextWindowExceeded":
		return cliround.ErrContextFull
	case httpStatus == 426, strings.Contains(lowered, "upgrade required"),
		strings.Contains(lowered, "update") && strings.Contains(lowered, "version"):
		return cliround.ErrUpdateRequired
	}
	return &cliround.HarnessError{Message: message}
}

func (t *Thread) onServerRequest(id json.RawMessage, method string, params json.RawMessage) {
	switch method {
	case "item/tool/call":
		t.onToolCall(id, params)
	case "item/commandExecution/requestApproval":
		t.declineNative(id, map[string]any{"decision": "decline"}, "command", "Codex asked to run a shell command")
	case "item/fileChange/requestApproval":
		t.declineNative(id, map[string]any{"decision": "decline"}, "file_change", "Codex asked to change a file")
	case "applyPatchApproval", "execCommandApproval":
		t.declineNative(id, map[string]any{"decision": "denied"}, "command", "Codex asked to run a command")
	case "item/permissions/requestApproval":
		t.declineNative(id, map[string]any{"permissions": map[string]any{}}, "permissions", "Codex asked for more permissions")
	case "item/tool/requestUserInput":
		t.declineNative(id, map[string]any{"answers": map[string]any{}}, "request_user_input", "Codex asked the user a question")
	case "mcpServer/elicitation/request":
		t.declineNative(id, map[string]any{"action": "decline"}, "mcp", "An MCP server asked Codex for input")
	default:
		t.client.respondError(id, -32000, "not supported by AgentParley")
	}
}

func (t *Thread) declineNative(id json.RawMessage, answer map[string]any, tool, detail string) {
	t.client.respond(id, answer)
	t.recordEvent(cliround.Event{Kind: cliround.EventBlockedTool, Tool: tool, Detail: detail})
}

func (t *Thread) recordEvent(event cliround.Event) {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	if t.round != nil {
		t.round.events = append(t.round.events, event)
		return
	}
	t.carriedEvents = append(t.carriedEvents, event)
}

func (t *Thread) onToolCall(id json.RawMessage, params json.RawMessage) {
	var call struct {
		CallID    string          `json:"callId"`
		Namespace string          `json:"namespace"`
		Tool      string          `json:"tool"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(params, &call) != nil || call.CallID == "" {
		t.client.respondError(id, -32602, "unreadable tool call")
		return
	}

	t.mutex.Lock()
	known := call.Namespace == toolNamespace && t.toolNames[call.Tool]
	if !known {
		event := cliround.Event{Kind: cliround.EventBlockedTool, Tool: call.Tool, Detail: "Codex tried to call a tool that is not an AgentParley tool"}
		if t.round != nil {
			t.round.events = append(t.round.events, event)
		} else {
			t.carriedEvents = append(t.carriedEvents, event)
		}
		t.mutex.Unlock()
		t.refuseToolCall(id)
		return
	}

	if t.closed || (t.round == nil && t.state != cliround.StateWaitingOnPlatform) {
		if !t.closed {
			t.offRoundLocked("tool call " + call.Tool)
		}
		t.mutex.Unlock()
		t.refuseToolCall(id)
		return
	}

	t.waiting = append(t.waiting, &waitingCall{rpcID: id, callID: call.CallID, tool: call.Tool, arguments: call.Arguments})
	if t.round != nil {
		t.armTimerLocked(t.round)
	}
	t.mutex.Unlock()
}

func (t *Thread) refuseToolCall(id json.RawMessage) {
	t.client.respond(id, map[string]any{
		"contentItems": []map[string]any{{"type": "inputText", "text": "Unknown tool."}},
		"success":      false,
	})
}
