package cliround

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// SeedHeader opens every rendered record handed to a CLI as history.
const SeedHeader = "Earlier part of this conversation, kept as a record. The tool calls in it already ran. To use a tool now, call it; never write a tool call as text.\n\n"

const itemSeparator = "\n\n"

// RenderTranscript renders history rows as plain text, one paragraph block per row.
func RenderTranscript(items []Item) string {
	blocks := make([]string, 0, len(items))
	for _, item := range items {
		switch item.Role {
		case RoleUser:
			blocks = append(blocks, "User: "+item.Text)
		case RoleAssistant:
			if item.Text != "" || len(item.Calls) == 0 {
				blocks = append(blocks, "Assistant: "+item.Text)
			}
			for _, call := range item.Calls {
				arguments := string(call.Arguments)
				if arguments == "" {
					arguments = "{}"
				} else if !json.Valid(call.Arguments) {
					encoded, _ := json.Marshal(map[string]string{"value": arguments})
					arguments = string(encoded)
				}
				blocks = append(blocks, fmt.Sprintf("Assistant called %s with %s (call %s)", call.Name, arguments, call.ID))
			}
		case RoleTool:
			blocks = append(blocks, fmt.Sprintf("Result of call %s: %s", item.CallID, item.Text))
		}
	}
	return strings.Join(blocks, itemSeparator)
}

// Chunk splits text into pieces of at most maxBytes, on block boundaries where possible and on rune boundaries
// inside a block that is larger than maxBytes on its own.
func Chunk(text string, maxBytes int) []string {
	if text == "" {
		return nil
	}
	var chunks []string
	var current strings.Builder
	flush := func() {
		if current.Len() > 0 {
			chunks = append(chunks, current.String())
			current.Reset()
		}
	}
	for _, block := range strings.Split(text, itemSeparator) {
		if len(block) > maxBytes {
			flush()
			chunks = append(chunks, splitOnRunes(block, maxBytes)...)
			continue
		}
		if current.Len() > 0 && current.Len()+len(itemSeparator)+len(block) > maxBytes {
			flush()
		}
		if current.Len() > 0 {
			current.WriteString(itemSeparator)
		}
		current.WriteString(block)
	}
	flush()
	return chunks
}

func splitOnRunes(text string, maxBytes int) []string {
	var pieces []string
	for len(text) > maxBytes {
		cut := maxBytes
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		if cut == 0 {
			cut = maxBytes
		}
		pieces = append(pieces, text[:cut])
		text = text[cut:]
	}
	if text != "" {
		pieces = append(pieces, text)
	}
	return pieces
}
