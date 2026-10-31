// Tool-wrapper protocol shared by every bridge.
//
// Contract: the model emits <opencode_bridge_call>{...}</opencode_bridge_call>
// wrappers; the adapter extracts, parses, validates them against JSON Schema
// and tool_choice, and returns native OpenAI tool_calls. Rendered args are
// never source; validation stays strict.
package bridgecore

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// bridgeCallRe matches <opencode_bridge_call>...</opencode_bridge_call>
// wrappers (non-greedy, dot-all). The optional backslash before each
// underscore accepts the Markdown-escaped delimiter variant chatgpt allows.
var bridgeCallRe = regexp.MustCompile(`(?s)<opencode\\?_bridge\\?_call>([\s\S]*?)</opencode\\?_bridge\\?_call>`)

// incompleteBridgeRe spots a dangling bridge-call tag.
var incompleteBridgeRe = regexp.MustCompile(`</?opencode\\?_bridge\\?_call`)

// BridgeWrappers extracts every wrapper payload from raw model text.
func BridgeWrappers(text string) []string {
	matches := bridgeCallRe.FindAllStringSubmatch(text, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		out = append(out, strings.TrimSpace(m[1]))
	}
	return out
}

// BridgeCall is one parsed wrapper invocation.
type BridgeCall struct {
	Name         string         `json:"name"`
	Arguments    map[string]any `json:"arguments"`
	RawArguments string         `json:"-"`
}

// ParseBridgeCall parses and minimally validates one wrapper payload.
func ParseBridgeCall(payload string) (*BridgeCall, error) {
	var raw struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	err := json.NewDecoder(strings.NewReader(payload)).Decode(&raw)
	if err != nil {
		if fixed := strings.TrimSpace(payload); strings.HasSuffix(fixed, `"}`) {
			fixed = strings.TrimSuffix(fixed, `"}`) + `}`
			err = json.NewDecoder(strings.NewReader(fixed)).Decode(&raw)
		}
	}
	// Canonical messages mirror the long-standing ChatGPT bridge strings
	// verbatim: downstream reason-code classifiers match on them, so any
	// rewording silently degrades rejection diagnostics on every bridge.
	if err != nil {
		return nil, fmt.Errorf("malformed tool JSON: %w", err)
	}
	var args map[string]any
	rawArgs := ""
	if len(raw.Arguments) == 0 {
		return nil, errors.New("tool arguments must be a JSON object")
	} else if err := json.Unmarshal(raw.Arguments, &args); err != nil || args == nil {
		return nil, errors.New("tool arguments must be a JSON object")
	} else {
		rawArgs = string(raw.Arguments)
	}
	return &BridgeCall{Name: raw.Name, Arguments: args, RawArguments: strings.TrimSpace(rawArgs)}, nil
}

// ValidateBridgeCall checks a parsed call against schemas and tool_choice.
func ValidateBridgeCall(call *BridgeCall, schemas map[string]*jsonschema.Schema, toolChoice string) error {
	sch, ok := schemas[call.Name]
	if !ok {
		return fmt.Errorf("tool name %q is not allowed by tools/tool_choice", call.Name)
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
		return fmt.Errorf("tool %s arguments: %w", call.Name, err)
	}
	switch toolChoice {
	case "", "auto", "required":
	case "none":
		return fmt.Errorf("tool name %q is not allowed by tools/tool_choice", call.Name)
	default:
		if call.Name != toolChoice {
			return fmt.Errorf("tool name %q is not allowed by tools/tool_choice", call.Name)
		}
	}
	return nil
}

// ExtractExecutableCalls returns validated calls or an error. No fence
// stripping: wrappers inside fences survive as text and fail the prose-mix
// check, so fenced examples are never accepted.
func ExtractExecutableCalls(text string, schemas map[string]*jsonschema.Schema, toolChoice string) ([]*BridgeCall, error) {
	wrappers := BridgeWrappers(text)
	if len(wrappers) == 0 {
		if incompleteBridgeRe.MatchString(text) {
			return nil, errors.New("incomplete opencode_bridge_call wrapper")
		}
		if toolChoice == "required" {
			return nil, fmt.Errorf("tool_choice required but answer contains no bridge wrapper")
		}
		return nil, nil
	}
	rest := strings.TrimSpace(bridgeCallRe.ReplaceAllString(text, ""))
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
		return nil, fmt.Errorf("tool name %q is not allowed by tools/tool_choice", calls[0].Name)
	}
	return calls, nil
}

// SplitCallsAndProse validates like ExtractExecutableCalls but preserves
// surrounding commentary instead of rejecting prose-mixed wrappers: it
// returns the calls plus the text with every wrapper excised. This is the
// long-standing ChatGPT-bridge behavior (its model habitually narrates
// around calls); the strict ExtractExecutableCalls stays the default for
// bridges whose models obey wrappers-only. Malformed, incomplete, fenced
// and disallowed shapes still error exactly as the strict path.
func SplitCallsAndProse(text string, schemas map[string]*jsonschema.Schema, toolChoice string) ([]*BridgeCall, string, error) {
	wrappers := BridgeWrappers(text)
	if len(wrappers) == 0 {
		if incompleteBridgeRe.MatchString(text) {
			return nil, "", errors.New("incomplete opencode_bridge_call wrapper")
		}
		if toolChoice == "required" {
			return nil, "", fmt.Errorf("tool_choice required but answer contains no bridge wrapper")
		}
		return nil, strings.TrimSpace(text), nil
	}
	prose := text
	var calls []*BridgeCall
	for _, m := range bridgeCallRe.FindAllStringSubmatch(text, -1) {
		if len(m) < 2 {
			continue
		}
		call, err := ParseBridgeCall(m[1])
		if err != nil {
			return nil, "", err
		}
		if err := ValidateBridgeCall(call, schemas, toolChoice); err != nil {
			return nil, "", err
		}
		calls = append(calls, call)
		prose = strings.Replace(prose, m[0], "", 1)
	}
	// A dangling opener left in the remainder is a botched tool attempt,
	// not commentary — same fail-closed rule as the strict path.
	if incompleteBridgeRe.MatchString(prose) {
		return nil, "", errors.New("incomplete opencode_bridge_call wrapper")
	}
	if toolChoice == "none" && len(calls) > 0 {
		return nil, "", fmt.Errorf("tool name %q is not allowed by tools/tool_choice", calls[0].Name)
	}
	return calls, strings.TrimSpace(prose), nil
}
