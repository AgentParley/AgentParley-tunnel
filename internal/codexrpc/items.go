package codexrpc

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/agentparley/tunnel/internal/cliround"
)

const (
	// toolNamespace is the one dynamic-tool namespace every AgentParley tool lives in. It is also listed in
	// features.code_mode.direct_only_tool_namespaces (appserver.go), so the model calls these tools directly and never
	// from inside a code-mode cell.
	toolNamespace = "agentparley"

	// maxToolOutputBytes cuts one tool result before it reaches Codex; Claude uses the same cap.
	maxToolOutputBytes = 3584 << 10

	toolOutputCutMarker = "\n[Output cut by AgentParley: it was larger than 3.5 MiB.]"
)

func capToolOutput(text string) string {
	if len(text) <= maxToolOutputBytes {
		return text
	}
	cut := maxToolOutputBytes
	for cut > 0 && text[cut]&0xC0 == 0x80 {
		cut--
	}
	return text[:cut] + toolOutputCutMarker
}

func userInput(items []cliround.Item) []map[string]any {
	input := make([]map[string]any, 0, len(items))
	for _, item := range items {
		input = append(input, map[string]any{"type": "text", "text": item.Text, "text_elements": []any{}})
	}
	return input
}

func messageItem(role, contentType, text string) map[string]any {
	return map[string]any{
		"type":    "message",
		"role":    role,
		"content": []map[string]any{{"type": contentType, "text": text}},
	}
}

// callArguments returns the arguments of a historical call as a JSON object string; anything that is not an object is
// wrapped as {"value": "<raw>"}.
func callArguments(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "{}"
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) == nil {
		return trimmed
	}
	encoded, _ := json.Marshal(map[string]string{"value": trimmed})
	return string(encoded)
}

// historyItems turns history rows into the raw Responses API items thread/inject_items appends to a thread.
func historyItems(items []cliround.Item) []map[string]any {
	history := make([]map[string]any, 0, len(items))
	for _, item := range items {
		switch item.Role {
		case cliround.RoleUser:
			history = append(history, messageItem("user", "input_text", item.Text))
		case cliround.RoleAssistant:
			if item.Text != "" {
				history = append(history, messageItem("assistant", "output_text", item.Text))
			}
			for _, call := range item.Calls {
				history = append(history, map[string]any{
					"type":      "function_call",
					"call_id":   call.ID,
					"name":      call.Name,
					"namespace": toolNamespace,
					"arguments": callArguments(call.Arguments),
				})
			}
		case cliround.RoleTool:
			history = append(history, map[string]any{
				"type":    "function_call_output",
				"call_id": item.CallID,
				"output":  capToolOutput(item.Text),
			})
		}
	}
	return history
}

// splitTrailingUser splits items into the history to inject and the trailing run of user rows that open the turn.
func splitTrailingUser(items []cliround.Item) (history, trailing []cliround.Item) {
	cut := len(items)
	for cut > 0 && items[cut-1].Role == cliround.RoleUser {
		cut--
	}
	return items[:cut], items[cut:]
}

func hasToolStructure(items []cliround.Item) bool {
	for _, item := range items {
		if item.Role == cliround.RoleTool || len(item.Calls) > 0 {
			return true
		}
	}
	return false
}

// toolSpecs builds the dynamic-tool namespace for thread/start.
func toolSpecs(tools []cliround.Tool) ([]map[string]any, error) {
	specs := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		if tool.Name == "" || strings.ContainsAny(tool.Name, " \t\r\n.") {
			return nil, &cliround.InvalidRequestError{Message: fmt.Sprintf("tool name %q is not usable", tool.Name)}
		}
		var schema any = map[string]any{"type": "object", "properties": map[string]any{}}
		if len(tool.Parameters) > 0 && string(tool.Parameters) != "null" {
			schema = json.RawMessage(tool.Parameters)
		}
		specs = append(specs, map[string]any{
			"type":        "function",
			"name":        tool.Name,
			"description": tool.Description,
			"inputSchema": schema,
		})
	}
	return []map[string]any{{
		"type":        "namespace",
		"name":        toolNamespace,
		"description": "AgentParley tools.",
		"tools":       specs,
	}}, nil
}
