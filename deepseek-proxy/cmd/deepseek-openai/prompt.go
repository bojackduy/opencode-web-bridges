package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// chatMessage is the canonical client message. content accepts a string, null,
// or a list of text parts; anything else is rejected rather than silently
// dropped.
type chatMessage struct {
	Role       string     `json:"role"`
	Content    text       `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

type text string

func (t *text) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*t = ""
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*t = text(s)
		return nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(b, &parts); err != nil {
		return fmt.Errorf("content must be a string or a list of text parts")
	}
	var sb strings.Builder
	for _, p := range parts {
		if p.Type != "text" {
			return fmt.Errorf("content part %q is not supported by the DeepSeek web bridge", p.Type)
		}
		sb.WriteString(p.Text)
	}
	*t = text(sb.String())
	return nil
}

func (t text) MarshalJSON() ([]byte, error) { return json.Marshal(string(t)) }

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function callFunction `json:"function"`
}

type callFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type toolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Parameters  any    `json:"parameters"`
	} `json:"function"`
}

// canonicalMessages returns a copy with the differences a client cannot avoid
// normalised: null content, missing tool_call type, and JSON argument
// formatting. Everything else is compared exactly.
func canonicalMessages(in []chatMessage) []chatMessage {
	out := make([]chatMessage, len(in))
	for i, m := range in {
		c := chatMessage{
			Role:       strings.TrimSpace(m.Role),
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
		}
		for _, tc := range m.ToolCalls {
			typ := tc.Type
			if typ == "" {
				typ = "function"
			}
			c.ToolCalls = append(c.ToolCalls, toolCall{
				ID:       tc.ID,
				Type:     typ,
				Function: callFunction{Name: tc.Function.Name, Arguments: canonicalJSON(tc.Function.Arguments)},
			})
		}
		out[i] = c
	}
	return out
}

// canonicalJSON compacts and key-sorts a JSON object so the same arguments
// compare equal regardless of whitespace or key order.
func canonicalJSON(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return raw
	}
	b, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return string(b)
}

// prefix reports whether a starts with b, message for message.
func prefix(a, b []chatMessage) bool {
	if len(b) > len(a) {
		return false
	}
	for i := range b {
		if !sameMessage(a[i], b[i]) {
			return false
		}
	}
	return true
}

func sameMessage(a, b chatMessage) bool {
	if a.Role != b.Role || a.Content != b.Content || a.ToolCallID != b.ToolCallID || a.Name != b.Name {
		return false
	}
	if len(a.ToolCalls) != len(b.ToolCalls) {
		return false
	}
	for i := range a.ToolCalls {
		if a.ToolCalls[i] != b.ToolCalls[i] {
			return false
		}
	}
	return true
}

func equalMessages(a, b []chatMessage) bool {
	return len(a) == len(b) && prefix(a, b)
}

// maxReseedPromptBytes bounds a fresh remote prompt. Live completion turns
// with multi-hundred-KB reseeds fail validation (http_422); history is
// trimmed oldest-first, the newest turn is never dropped.
const maxReseedPromptBytes = 90000

// renderReseed renders a complete prompt for a fresh remote session.
func renderReseed(msgs []chatMessage, tools []toolDef, choice any) string {
	instr := strings.TrimSpace(instructions(msgs) + "\n\n" + toolPrompt(tools, choice))
	history, current := splitLastTurn(msgs)
	// Trim oldest history first until the prompt fits. The newest turn
	// (current) is authoritative and is never dropped.
	for {
		var sb strings.Builder
		if instr != "" {
			sb.WriteString("[Instructions]:\n")
			sb.WriteString(instr)
			sb.WriteString("\n\n")
		}
		if len(history) > 0 {
			sb.WriteString(historyIntro)
			sb.WriteString(renderMessages(history))
			sb.WriteString(historyEnd)
			sb.WriteString("\n")
		}
		if len(current) > 0 {
			sb.WriteString(turnIntro)
			sb.WriteString(renderMessages(current))
			sb.WriteString("\n")
		}
		sb.WriteString(answerNudge(tools, choice))
		if sb.Len() <= maxReseedPromptBytes || len(history) == 0 {
			return strings.TrimSpace(sb.String())
		}
		history = history[1:]
	}
}

// renderAppend renders only the messages the client added since the last
// committed turn. The remote thread already holds the earlier ones.
func renderAppend(msgs []chatMessage, tools []toolDef, choice any) string {
	var sb strings.Builder
	if len(tools) > 0 {
		sb.WriteString(toolReminder(tools))
	}
	sb.WriteString(turnIntro)
	sb.WriteString(renderMessages(msgs))
	sb.WriteString("\n")
	sb.WriteString(answerNudge(tools, choice))
	return strings.TrimSpace(sb.String())
}

func instructions(msgs []chatMessage) string {
	var parts []string
	for _, m := range msgs {
		if m.Role == "system" && strings.TrimSpace(string(m.Content)) != "" {
			parts = append(parts, strings.TrimSpace(string(m.Content)))
		}
	}
	return strings.Join(parts, "\n\n")
}

// splitLastTurn separates everything before the final non-system message from
// that message and any trailing ones.
func splitLastTurn(msgs []chatMessage) (history, current []chatMessage) {
	last := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "system" {
			last = i
			break
		}
	}
	if last < 0 {
		return nil, nil
	}
	return msgs[:last], msgs[last:]
}

func renderMessages(msgs []chatMessage) string {
	var sb strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case "system":
			continue
		case "user":
			sb.WriteString("User: ")
			sb.WriteString(string(m.Content))
			sb.WriteString("\n\n")
		case "assistant":
			if len(m.ToolCalls) > 0 {
				for _, tc := range m.ToolCalls {
					sb.WriteString("Assistant: TOOL_CALL: ")
					sb.WriteString(tc.Function.Name)
					sb.WriteString("\narguments: ")
					sb.WriteString(tc.Function.Arguments)
					sb.WriteString("\n\n")
				}
				if s := strings.TrimSpace(string(m.Content)); s != "" {
					sb.WriteString("Assistant: ")
					sb.WriteString(s)
					sb.WriteString("\n\n")
				}
				continue
			}
			if s := strings.TrimSpace(string(m.Content)); s != "" {
				sb.WriteString("Assistant: ")
				sb.WriteString(s)
				sb.WriteString("\n\n")
			}
		case "tool":
			sb.WriteString("[Tool Result")
			if m.Name != "" {
				sb.WriteString(": ")
				sb.WriteString(m.Name)
			}
			sb.WriteString("]:\n")
			sb.WriteString(string(m.Content))
			sb.WriteString("\n\n")
		}
	}
	return sb.String()
}

func answerNudge(tools []toolDef, choice any) string {
	return "Now produce the next assistant reply for the external client. Tool choice: " + toolChoiceText(choice) + ". A required/named tool means output the opencode_bridge_call JSON wrapper now, as TEXT for external execution.\n"
}

// toolNames returns the declared function names, sorted for stable logging.
func toolNames(tools []toolDef) []string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Function.Name)
	}
	sort.Strings(names)
	return names
}
