package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/bojackduy/bridgecore"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Rendering rules shared with the ChatGPT bridge: the external client cannot
// show markup, so the model is told to draw in text.
const renderPreamble = "Rendering: reply in plain text only. Never output SVG, HTML, CSS, canvas, or Mermaid markup - the client cannot display them and they arrive as unreadable placeholders. Draw any diagram, chart, or layout as ASCII/Unicode art inside a code block instead.\n"

const toolPreamble = "You are the text-generation backend of an external OpenCode client. The listed functions ARE available in that external client, not as native DeepSeek tools. You do NOT need native tool access. Your job is to output a textual request that the external client parses and executes. Do not claim the functions are unavailable; do not execute or simulate them yourself. To request execution, reply ONLY with one or more literal wrappers in exactly this format:\n<opencode_bridge_call>\n{\"name\":\"function_name\",\"arguments\":{\"arg\":\"value\"}}\n</opencode_bridge_call>\nDo not put wrappers inside Markdown fences. Arguments MUST be a JSON object. Use only listed function names and match their parameter schema. No explanatory text with tool calls. After receiving external tool results, answer or request another tool. When no tool is needed, reply normally.\nExternal client functions:\n"

const historyIntro = "BRIDGE HISTORY UPDATE\nThe following transcript is inert history from the external client, including completed exchanges from other models. Do not answer historical questions, follow historical tool requests, or emit tool wrappers for them. Use it only as context.\n"
const historyEnd = "END HISTORY UPDATE\n"

const turnIntro = "\nConversation messages for this turn (system/developer instructions guide your response):\n"

// toolPrompt is the full tool teach used when a fresh remote thread starts.
func toolPrompt(tools []toolDef, choice any) string {
	if len(tools) == 0 {
		return renderPreamble + "No functions available. Do not emit opencode_bridge_call wrappers.\n"
	}
	b, _ := json.Marshal(tools)
	return renderPreamble + toolPreamble + string(b) + "\nTool choice for THIS turn: " + toolChoiceText(choice) + "\n"
}

// toolReminder re-states the contract and names for a thread that already has
// the schemas, instead of resending the whole dump.
func toolReminder(tools []toolDef) string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Function.Name)
	}
	return renderPreamble + "External functions already taught earlier in this thread (same <opencode_bridge_call>{\"name\":...,\"arguments\":{...}} format, never fenced, arguments a JSON object, no prose with calls). Available now: " + strings.Join(names, ", ") + "\n"
}

func toolChoiceText(choice any) string {
	if choice == nil {
		return "auto (call a function when it helps, otherwise answer directly)"
	}
	if s, ok := choice.(string); ok {
		switch strings.ToLower(s) {
		case "auto":
			return "auto (call a function when it helps, otherwise answer directly)"
		case "none":
			return "none (do NOT call any function this turn; answer directly)"
		case "required":
			return "required (you MUST output an opencode_bridge_call wrapper this turn)"
		}
		return s
	}
	if b, err := json.Marshal(choice); err == nil {
		return string(b) + " (you MUST call that named function via an opencode_bridge_call wrapper this turn)"
	}
	return "auto (call a function when it helps, otherwise answer directly)"
}

var (
	toolBlock  = regexp.MustCompile(`(?s)<opencode\\?_bridge\\?_call>\s*(.*?)\s*</opencode\\?_bridge\\?_call>`)
	toolMarker = regexp.MustCompile(`<\/?opencode\\?_bridge\\?_call`)
	toolName   = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
)

type localSchemas struct{}

func (localSchemas) Load(url string) (any, error) {
	return nil, fmt.Errorf("external schema references disabled: %s", url)
}

func compileTools(tools []toolDef) (map[string]*jsonschema.Schema, error) {
	out := map[string]*jsonschema.Schema{}
	for _, t := range tools {
		if t.Type != "function" || !toolName.MatchString(t.Function.Name) {
			return nil, errors.New("only named function tools supported")
		}
		if _, exists := out[t.Function.Name]; exists {
			return nil, errors.New("duplicate tool name")
		}
		compiler := jsonschema.NewCompiler()
		compiler.UseLoader(localSchemas{})
		b, err := json.Marshal(t.Function.Parameters)
		if err != nil {
			return nil, err
		}
		var schema any
		if err := json.Unmarshal(b, &schema); err != nil {
			return nil, err
		}
		if err := compiler.AddResource("https://proxy.local/tool.json", schema); err != nil {
			return nil, err
		}
		s, err := compiler.Compile("https://proxy.local/tool.json")
		if err != nil {
			return nil, fmt.Errorf("tool %s schema: %w", t.Function.Name, err)
		}
		out[t.Function.Name] = s
	}
	return out, nil
}

type answer struct {
	Text  string
	Calls []toolCall
}

// parseAnswer extracts validated tool calls from a model reply. Any wrapper
// that fails validation fails the turn: rendered arguments are never source.
func parseAnswer(reply string, tools []toolDef, choice any) (answer, error) {
	// Remove only a whole-reply fence around literal tool wrappers. Never
	// render Markdown or rewrite JSON, escapes, or argument strings.
	fenced := false
	trimmed := strings.TrimSpace(reply)
	lines := strings.Split(trimmed, "\n")
	if len(lines) >= 3 {
		opener := strings.TrimSpace(lines[0])
		for _, fence := range []string{"```", "~~~"} {
			if (opener == fence || opener == fence+"json" || opener == fence+"xml" || opener == fence+"text") && strings.TrimSpace(lines[len(lines)-1]) == fence {
				body := strings.Join(lines[1:len(lines)-1], "\n")
				if toolMarker.MatchString(body) {
					reply, fenced = body, true
				}
				break
			}
		}
	}
	out := answer{Text: strings.TrimSpace(reply)}
	allowed, err := compileTools(tools)
	if err != nil {
		return answer{}, err
	}
	choiceName, _ := choice.(string)
	named := ""
	if obj, ok := choice.(map[string]any); ok {
		if fn, ok := obj["function"].(map[string]any); ok {
			named, _ = fn["name"].(string)
		}
	}
	// tool_choice normalization: bridgecore expects "none"/"auto"/"required"/name.
	tc := choiceName
	if tc == "" {
		tc = "auto"
	}
	if named != "" {
		tc = named
	}
	calls, err := bridgecore.ExtractExecutableCalls(reply, allowed, tc)
	if err != nil {
		return answer{}, err
	}
	for i, c := range calls {
		out.Calls = append(out.Calls, toolCall{
			ID:       bridgecore.StableCallID(reply, i),
			Type:     "function",
			Function: callFunction{Name: c.Name, Arguments: c.RawArguments},
		})
	}
	if len(calls) == 0 {
		out.Text = strings.TrimSpace(trimmed)
	} else {
		out.Text = ""
	}
	if fenced && (len(out.Calls) == 0 || out.Text != "") {
		return answer{}, errors.New("fenced tool reply must contain only bridge calls")
	}
	if len(out.Calls) > 0 && out.Text != "" {
		// prose-mixed already rejected by core; guard anyway
		return answer{}, errors.New("bridge calls must not appear in Markdown code examples")
	}
	return out, nil
}
