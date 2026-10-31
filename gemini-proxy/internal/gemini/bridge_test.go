package gemini

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bojackduy/bridgecore"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func testSchemas(t *testing.T) map[string]*jsonschema.Schema {
	t.Helper()
	raw := `{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}`
	var schema any
	if err := json.Unmarshal([]byte(raw), &schema); err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("https://proxy.local/tool.json", schema); err != nil {
		t.Fatal(err)
	}
	sch, err := compiler.Compile("https://proxy.local/tool.json")
	if err != nil {
		t.Fatal(err)
	}
	return map[string]*jsonschema.Schema{"get_weather": sch}
}

func TestParseBridgeCallOK(t *testing.T) {
	call, err := ParseBridgeCall(`{"name":"get_weather","arguments":{"city":"Hanoi"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if call.Name != "get_weather" || call.Arguments["city"] != "Hanoi" {
		t.Fatalf("unexpected call: %+v", call)
	}
}

func TestParseBridgeCallStrayQuoteAfterValue(t *testing.T) {
	// Real model output: an extra quote closes the arguments object. The error
	// text was `invalid character '"' after object key:value pair`.
	call, err := ParseBridgeCall(`{"name":"get_weather","arguments":{"city":"Hanoi"}"}`)
	if err != nil {
		t.Fatal(err)
	}
	if call.Name != "get_weather" || call.Arguments["city"] != "Hanoi" {
		t.Fatalf("unexpected call: %+v", call)
	}
}

func TestParseBridgeCallStillRejectsOtherMalformedJSON(t *testing.T) {
	if _, err := ParseBridgeCall(`{"name":"get_weather" "arguments":{"city":"Hanoi"}}`); err == nil {
		t.Fatal("missing comma must stay invalid")
	}
}

func TestParseBridgeCallBadJSON(t *testing.T) {
	if _, err := ParseBridgeCall(`{not json`); err == nil {
		t.Fatal("expected error for bad JSON")
	}
}

func TestParseBridgeCallNonObjectArgs(t *testing.T) {
	if _, err := ParseBridgeCall(`{"name":"get_weather","arguments":[1,2]}`); err == nil {
		t.Fatal("expected error for array arguments")
	}
}

func TestExtractCleanWrapper(t *testing.T) {
	text := "<opencode_bridge_call>\n" + `{"name":"get_weather","arguments":{"city":"Hanoi"}}` + "\n</opencode_bridge_call>"
	calls, err := bridgecore.ExtractExecutableCalls(text, testSchemas(t), "auto")
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Name != "get_weather" {
		t.Fatalf("unexpected calls: %+v", calls)
	}
}

func TestExtractUnknownNameRejected(t *testing.T) {
	text := `<opencode_bridge_call>{"name":"nope","arguments":{}}</opencode_bridge_call>`
	if _, err := bridgecore.ExtractExecutableCalls(text, testSchemas(t), "auto"); err == nil {
		t.Fatal("expected unknown-name rejection")
	}
}

func TestExtractSchemaViolationRejected(t *testing.T) {
	text := `<opencode_bridge_call>{"name":"get_weather","arguments":{}}</opencode_bridge_call>`
	if _, err := bridgecore.ExtractExecutableCalls(text, testSchemas(t), "auto"); err == nil {
		t.Fatal("expected schema-violation rejection (missing city)")
	}
}

func TestExtractProseMixedRejected(t *testing.T) {
	text := "Here you go:\n<opencode_bridge_call>" + `{"name":"get_weather","arguments":{"city":"Hanoi"}}` + "</opencode_bridge_call>"
	if _, err := bridgecore.ExtractExecutableCalls(text, testSchemas(t), "auto"); err == nil {
		t.Fatal("expected prose-mixed rejection")
	}
}

func TestExtractFencedWrapperIgnored(t *testing.T) {
	text := "```json\n<opencode_bridge_call>" + `{"name":"get_weather","arguments":{"city":"Hanoi"}}` + "</opencode_bridge_call>\n```"
	if _, err := bridgecore.ExtractExecutableCalls(text, testSchemas(t), "auto"); err == nil {
		t.Fatal("fenced wrapper must be rejected, not ignored")
	}
}

func TestExtractToolChoiceNone(t *testing.T) {
	text := `<opencode_bridge_call>{"name":"get_weather","arguments":{"city":"Hanoi"}}</opencode_bridge_call>`
	if _, err := bridgecore.ExtractExecutableCalls(text, testSchemas(t), "none"); err == nil {
		t.Fatal("expected tool_choice=none rejection")
	}
}

func TestExtractToolChoiceForcedMismatch(t *testing.T) {
	compiler := jsonschema.NewCompiler()
	var schema any
	if err := json.Unmarshal([]byte(`{"type":"object"}`), &schema); err != nil {
		t.Fatal(err)
	}
	if err := compiler.AddResource("https://proxy.local/tool.json", schema); err != nil {
		t.Fatal(err)
	}
	sch, err := compiler.Compile("https://proxy.local/tool.json")
	if err != nil {
		t.Fatal(err)
	}
	schemas := map[string]*jsonschema.Schema{"a": sch, "b": sch}
	text := `<opencode_bridge_call>{"name":"a","arguments":{}}</opencode_bridge_call>`
	if _, err := bridgecore.ExtractExecutableCalls(text, schemas, "b"); err == nil {
		t.Fatal("expected forced-choice mismatch rejection")
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	call := &BridgeCall{Name: "x", Arguments: map[string]any{"k": "v"}}
	b, err := json.Marshal(call)
	if err != nil {
		t.Fatal(err)
	}
	var back BridgeCall
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
}

func TestExtractMismatchedWrapperRejected(t *testing.T) {
	// Live hallucination: opener, broken JSON (unescaped quotes), wrong
	// close tag. Must fail closed, never deliver the junk as text.
	text := "<opencode_bridge_call>" + `{"name":"execute","arguments":{"code":"return tools.opencode"skill" ? 1 : 2"}}` + "</opencode_bridge_link>"
	if _, err := bridgecore.ExtractExecutableCalls(text, testSchemas(t), "auto"); err == nil {
		t.Fatal("expected incomplete-wrapper rejection")
	} else if !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("wrong rejection: %v", err)
	}
}

func TestExtractPlainProseStillPasses(t *testing.T) {
	calls, err := bridgecore.ExtractExecutableCalls("just a plain answer, no tags", testSchemas(t), "auto")
	if err != nil || len(calls) != 0 {
		t.Fatalf("plain prose must pass through: calls=%v err=%v", calls, err)
	}
}
