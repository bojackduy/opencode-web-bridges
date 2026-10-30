// Tool-wrapper protocol for the OpenAI-compatible adapter.
//
// Contract REUSED byte-for-byte from chatgpt-proxy so the plugin treats both
// engines identically. Meta receives the external client's function schemas
// and a text protocol:
//
//	<opencode_bridge_call>
//	{"name":"get_weather","arguments":{"city":"Hanoi"}}
//	</opencode_bridge_call>
//
// The distinct wrapper is prompt-emulated, not a native Meta tool. The
// adapter checks the function name and JSON-object arguments against JSON
// Schema and tool_choice, and returns native OpenAI tool_calls. OpenCode
// executes the tools, then supplies results on the next provider turn.
// Unknown names, bad JSON, schema violations and missing required calls
// produce an OpenAI-shaped error, never executable calls. Tool wrappers mixed
// with prose or Markdown fences are rejected. Ordinary Markdown code examples
// are not executable bridge requests.
package meta

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// bridgeCallRe matches <opencode_bridge_call>...</opencode_bridge_call>
// wrappers (non-greedy, dot-all emulated with [\s\S]).
var bridgeCallRe = regexp.MustCompile(`(?s)<opencode_bridge_call>[\s\S]*?</opencode_bridge_call>`)

// incompleteBridgeRe spots a dangling bridge-call tag — an opener that
// never became a valid wrapper (broken JSON, mismatched close tag).
var incompleteBridgeRe = regexp.MustCompile(`</?opencode_bridge_call`)

// BridgeWrappers extracts every <opencode_bridge_call>...</opencode_bridge_call>
// payload from raw model text. It returns the raw JSON strings in order.
func BridgeWrappers(text string) []string {
	matches := bridgeCallRe.FindAllStringSubmatch(text, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		inner := m[0]
		inner = strings.TrimPrefix(inner, "<opencode_bridge_call>")
		inner = strings.TrimSuffix(inner, "</opencode_bridge_call>")
		out = append(out, strings.TrimSpace(inner))
	}
	return out
}

// fenceRe detects Markdown code fences — wrappers inside fences are ordinary
// code examples, never executable bridge requests.
var fenceRe = regexp.MustCompile("(?s)```.*?```")

// StripFences removes fenced code blocks so wrapper scanning ignores examples.
func StripFences(text string) string {
	return fenceRe.ReplaceAllString(text, "")
}

// BridgeCall is one parsed wrapper invocation.
type BridgeCall struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// ParseBridgeCall parses and minimally validates one wrapper payload:
// it must be a JSON object with a string name and object arguments.
func ParseBridgeCall(payload string) (*BridgeCall, error) {
	var raw struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	err := json.NewDecoder(strings.NewReader(payload)).Decode(&raw)
	if err != nil {
		// The model sometimes closes the arguments object with an extra quote
		// (`..."arguments":{...}"}`), which the decoder reports as a quote after
		// an object key:value pair. Recover that exact shape once; any other
		// malformed payload stays invalid and is never executed.
		if fixed := strings.TrimSpace(payload); strings.HasSuffix(fixed, `"}`) {
			fixed = strings.TrimSuffix(fixed, `"}`) + `}`
			err = json.NewDecoder(strings.NewReader(fixed)).Decode(&raw)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("bridge wrapper is not valid JSON: %w", err)
	}
	if strings.TrimSpace(raw.Name) == "" {
		return nil, fmt.Errorf("bridge wrapper missing function name")
	}
	var args map[string]any
	if len(raw.Arguments) == 0 {
		args = map[string]any{}
	} else if err := json.Unmarshal(raw.Arguments, &args); err != nil {
		return nil, fmt.Errorf("bridge wrapper arguments must be a JSON object: %w", err)
	}
	return &BridgeCall{Name: raw.Name, Arguments: args}, nil
}

// ValidateBridgeCall checks a parsed call against the known tool schemas
// (JSON Schema per function name) and the effective tool choice.
// toolChoice is "none", "auto", "required", or a forced function name.
func ValidateBridgeCall(call *BridgeCall, schemas map[string]*jsonschema.Schema, toolChoice string) error {
	sch, ok := schemas[call.Name]
	if !ok {
		return fmt.Errorf("unknown function %q", call.Name)
	}
	rawArgs, err := json.Marshal(call.Arguments)
	if err != nil {
		return fmt.Errorf("cannot re-encode arguments: %w", err)
	}
	var v any
	if err := json.Unmarshal(rawArgs, &v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	if err := sch.Validate(v); err != nil {
		return fmt.Errorf("arguments for %q violate schema: %w", call.Name, err)
	}
	switch toolChoice {
	case "", "auto", "required":
		// Any valid call is acceptable.
	case "none":
		return fmt.Errorf("tool_choice none but model emitted a tool call")
	default:
		if call.Name != toolChoice {
			return fmt.Errorf("tool_choice forces %q but model called %q", toolChoice, call.Name)
		}
	}
	return nil
}

// ExtractExecutableCalls returns the validated bridge calls in raw model text,
// or an error describing why the text carries no executable call. Pure:
// prose-mixed wrappers, fenced wrappers, and non-wrapper text all fail
// closed (nil calls + descriptive error for the adapter's 422 path).
func ExtractExecutableCalls(text string, schemas map[string]*jsonschema.Schema, toolChoice string) ([]*BridgeCall, error) {
	scannable := StripFences(text)
	wrappers := BridgeWrappers(scannable)
	if len(wrappers) == 0 {
		// A dangling opener with no valid wrapper is a botched tool
		// attempt (broken JSON, mismatched close tag), not prose: fail
		// closed instead of delivering the junk as assistant text.
		if incompleteBridgeRe.MatchString(scannable) {
			return nil, fmt.Errorf("incomplete opencode_bridge_call wrapper: opening tag without a complete valid wrapper")
		}
		if toolChoice == "required" {
			return nil, fmt.Errorf("tool_choice required but answer contains no bridge wrapper")
		}
		return nil, nil
	}
	// Wrappers mixed with prose are rejected: the remaining text after
	// removing wrappers must be empty.
	rest := strings.TrimSpace(bridgeCallRe.ReplaceAllString(scannable, ""))
	if rest != "" {
		return nil, fmt.Errorf("bridge wrapper mixed with prose is rejected")
	}
	var calls []*BridgeCall
	for _, w := range wrappers {
		call, err := ParseBridgeCall(w)
		if err != nil {
			return nil, err
		}
		if err := ValidateBridgeCall(call, schemas, toolChoice); err != nil {
			return nil, err
		}
		calls = append(calls, call)
	}
	if toolChoice == "none" && len(calls) > 0 {
		return nil, fmt.Errorf("tool_choice none but model emitted tool calls")
	}
	return calls, nil
}
