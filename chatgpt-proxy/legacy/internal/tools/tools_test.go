package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadFileWindow(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "data.txt")
	var lines []string
	for i := 1; i <= 10; i++ {
		lines = append(lines, fmt.Sprintf("line%d", i))
	}
	if err := os.WriteFile(f, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tl := NewTools(nil)

	out, err := tl.ReadFile(ReadFileArgs{Path: f})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "lines total") {
		t.Errorf("header missing: %s", out)
	}
	if !strings.Contains(out, "line10") {
		t.Errorf("last line missing: %s", out)
	}

	out, err = tl.ReadFile(ReadFileArgs{Path: f, Offset: 4, Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "showing 4-6") {
		t.Errorf("window header wrong: %s", out)
	}
	if !strings.Contains(out, "line4") || strings.Contains(out, "line7") {
		t.Errorf("window wrong: %s", out)
	}

	if _, err := tl.ReadFile(ReadFileArgs{Path: f, Offset: 99}); err == nil {
		t.Error("expected error for offset past EOF")
	}
}

func TestApplyPatchEdits(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "code.go")
	if err := os.WriteFile(f, []byte("package main\n\nfunc main() { println(\"old\") }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tl := NewTools(nil)

	out, err := tl.ApplyPatch(ApplyPatchArgs{Edits: []PatchEdit{
		{File: f, OldString: `println("old")`, NewString: `println("new")`},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1 replacement") {
		t.Errorf("unexpected result: %s", out)
	}
	data, _ := os.ReadFile(f)
	if !strings.Contains(string(data), `println("new")`) {
		t.Errorf("edit not applied: %s", data)
	}

	// Ambiguous edit without allow_multiple must fail and not modify.
	if err := os.WriteFile(f, []byte("dup\ndup\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = tl.ApplyPatch(ApplyPatchArgs{Edits: []PatchEdit{{File: f, OldString: "dup", NewString: "one"}}})
	if err == nil || !strings.Contains(err.Error(), "occurs 2 times") {
		t.Errorf("expected ambiguity error, got %v", err)
	}
	out, err = tl.ApplyPatch(ApplyPatchArgs{Edits: []PatchEdit{{File: f, OldString: "dup", NewString: "one", AllowMultiple: true}}})
	if err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(f)
	if strings.Contains(string(data), "dup") {
		t.Errorf("allow_multiple did not replace all: %s", data)
	}
}

func TestSearchText(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "a.go"), []byte("func foo() {}\n// foo here\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "sub", "b.ts"), []byte("const foo = 1\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "c.txt"), []byte("nothing\n"), 0o644)
	tl := NewTools(nil)

	out, err := tl.SearchText(SearchArgs{Pattern: "foo", Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a.go:1") || !strings.Contains(out, "a.go:2") || !strings.Contains(out, "b.ts:1") {
		t.Errorf("expected 3 matches, got:\n%s", out)
	}
	if strings.Contains(out, "c.txt") {
		t.Errorf("c.txt should not match:\n%s", out)
	}

	out, err = tl.SearchText(SearchArgs{Pattern: "foo", Root: dir, Include: []string{"*.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "b.ts") {
		t.Errorf("include filter failed:\n%s", out)
	}
}

func TestPolicy(t *testing.T) {
	p := NewDefaultPolicy()

	blocked := []string{
		"rm -rf /tmp/x",
		"rm -r somedir",
		"git push --force origin main",
		"git reset --hard HEAD~1",
		"shutdown now",
		"drop database app;",
	}
	for _, c := range blocked {
		if err := p.CheckShell(c); err == nil {
			t.Errorf("expected %q to be blocked", c)
		}
	}

	allowed := []string{
		"echo hello",
		"ls -la",
		"git status",
		"git add -A && git commit -m 'updates'",
		"rm file.txt", // single-file rm without -r is fine
	}
	for _, c := range allowed {
		if err := p.CheckShell(c); err != nil {
			t.Errorf("expected %q to be allowed, got %v", c, err)
		}
	}

	// AllowDelete turns the policy off.
	p2 := NewPolicy(nil, true)
	if err := p2.CheckShell("rm -rf /"); err != nil {
		t.Errorf("allow_delete should permit, got %v", err)
	}
}

func TestCallDispatch(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "x.txt")
	_ = os.WriteFile(f, []byte("hi\n"), 0o644)
	tl := NewTools(nil)

	args, _ := json.Marshal(map[string]any{"path": f})
	out, err := tl.Call("read_file", args)
	if err != nil {
		t.Fatal(err)
	}
	var tr toolResult
	if err := json.Unmarshal(out, &tr); err != nil {
		t.Fatal(err)
	}
	if tr.IsError || !strings.Contains(tr.Content[0].Text, "hi") {
		t.Errorf("bad result: %s", out)
	}

	// Unknown tool -> RPC-level error.
	if _, err = tl.Call("nope", nil); err == nil {
		t.Error("expected error for unknown tool")
	}
}

func TestWorkspaceRestriction(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	_ = os.WriteFile(outside, []byte("secret\n"), 0o644)

	inside := filepath.Join(dir, "ok.txt")
	_ = os.WriteFile(inside, []byte("ok\n"), 0o644)

	tl := NewTools([]string{dir})
	if _, err := tl.ReadFile(ReadFileArgs{Path: outside}); err == nil {
		t.Error("expected read outside workspace to fail")
	}
	if _, err := tl.ReadFile(ReadFileArgs{Path: inside}); err != nil {
		t.Errorf("read inside workspace should work: %v", err)
	}
}
