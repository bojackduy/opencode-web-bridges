package bridgecore

import (
	"encoding/json"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func testSchema(t *testing.T) map[string]*jsonschema.Schema {
	t.Helper()
	compiler := jsonschema.NewCompiler()
	b := []byte(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`)
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if err := compiler.AddResource("s", v); err != nil {
		t.Fatal(err)
	}
	sch, err := compiler.Compile("s")
	if err != nil {
		t.Fatal(err)
	}
	return map[string]*jsonschema.Schema{"get_weather": sch}
}

func TestProtocolPureWrapper(t *testing.T) {
	schemas := testSchema(t)
	text := `<opencode_bridge_call>{"name":"get_weather","arguments":{"city":"Hanoi"}}</opencode_bridge_call>`
	calls, err := ExtractExecutableCalls(text, schemas, "auto")
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Name != "get_weather" {
		t.Fatalf("unexpected calls: %+v", calls)
	}
}

func TestProtocolProseNoWrapper(t *testing.T) {
	schemas := testSchema(t)
	calls, err := ExtractExecutableCalls("just prose", schemas, "auto")
	if err != nil {
		t.Fatal(err)
	}
	if calls != nil {
		t.Fatalf("expected nil calls, got %+v", calls)
	}
}

func TestProtocolIncompleteOpener(t *testing.T) {
	schemas := testSchema(t)
	_, err := ExtractExecutableCalls(`<opencode_bridge_call>{"name":"get_weather"`, schemas, "auto")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestProtocolFencedWrapperRejected(t *testing.T) {
	schemas := testSchema(t)
	text := "```\n<opencode_bridge_call>{\"name\":\"get_weather\",\"arguments\":{\"city\":\"Hanoi\"}}</opencode_bridge_call>\n```"
	_, err := ExtractExecutableCalls(text, schemas, "auto")
	if err == nil {
		t.Fatal("expected error for fenced wrapper")
	}
}

func TestProtocolProseMixedRejected(t *testing.T) {
	schemas := testSchema(t)
	text := `hi <opencode_bridge_call>{"name":"get_weather","arguments":{"city":"Hanoi"}}</opencode_bridge_call>`
	_, err := ExtractExecutableCalls(text, schemas, "auto")
	if err == nil {
		t.Fatal("expected error for prose-mixed")
	}
}

func TestProtocolRequiredNoWrapper(t *testing.T) {
	schemas := testSchema(t)
	_, err := ExtractExecutableCalls("no wrapper", schemas, "required")
	if err == nil {
		t.Fatal("expected error")
	}
}
