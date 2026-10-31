// Package tools implements the local execution layer of the chatgpt-proxy
// agent: file read/search/patch, git, shell, process and system tools, all
// gated by a safety Policy.
package tools

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// toolResult mirrors the MCP tools/call result shape without importing mcp.
type toolResult struct {
	Content []toolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func textResult(s string) toolResult {
	return toolResult{Content: []toolContent{{Type: "text", Text: s}}}
}

func errorResult(s string) toolResult {
	return toolResult{Content: []toolContent{{Type: "text", Text: s}}, IsError: true}
}

// jsonArgs decodes raw JSON arguments into a typed struct.
func jsonArgs[T any](raw json.RawMessage) (T, error) {
	var args T
	if len(raw) == 0 {
		return args, nil
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return args, fmt.Errorf("invalid arguments: %w", err)
	}
	return args, nil
}

// Tools bundles every tool implementation plus the shared configuration
// (policy and workspace roots).
type Tools struct {
	// Policy gates destructive operations.
	Policy *Policy
	// WorkspaceRoots restricts file/shell access to these directories.
	// Empty means unrestricted.
	WorkspaceRoots []string
	// MaxFileLines caps how many lines read_file returns per call.
	MaxFileLines int
}

// NewTools builds a Tools instance with a default (deny-destructive) policy.
func NewTools(workspaceRoots []string) *Tools {
	return &Tools{Policy: NewDefaultPolicy(), WorkspaceRoots: workspaceRoots, MaxFileLines: 2000}
}

// checkPath verifies that a path stays inside the configured workspace roots
// (when roots are configured).
func (t *Tools) checkPath(path string) error {
	if len(t.WorkspaceRoots) == 0 {
		return nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	for _, root := range t.WorkspaceRoots {
		rAbs, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		if abs == rAbs || strings.HasPrefix(abs, rAbs+string(filepath.Separator)) {
			return nil
		}
	}
	return fmt.Errorf("path %s is outside the configured workspace roots", path)
}

// Call dispatches a tool call by name. It returns a JSON-marshaled ToolResult.
func (t *Tools) Call(name string, args json.RawMessage) ([]byte, error) {
	var result any
	var err error

	switch name {
	case "read_file":
		var a ReadFileArgs
		if a, err = jsonArgs[ReadFileArgs](args); err == nil {
			result, err = t.ReadFile(a)
		}
	case "search_text":
		var a SearchArgs
		if a, err = jsonArgs[SearchArgs](args); err == nil {
			result, err = t.SearchText(a)
		}
	case "apply_patch":
		var a ApplyPatchArgs
		if a, err = jsonArgs[ApplyPatchArgs](args); err == nil {
			result, err = t.ApplyPatch(a)
		}
	case "workspace_list":
		var a WorkspaceArgs
		if a, err = jsonArgs[WorkspaceArgs](args); err == nil {
			result, err = t.WorkspaceList(a)
		}
	case "git_status":
		var a GitArgs
		if a, err = jsonArgs[GitArgs](args); err == nil {
			result, err = t.GitStatus(a)
		}
	case "git_diff":
		var a GitArgs
		if a, err = jsonArgs[GitArgs](args); err == nil {
			result, err = t.GitDiff(a)
		}
	case "git_log":
		var a GitLogArgs
		if a, err = jsonArgs[GitLogArgs](args); err == nil {
			result, err = t.GitLog(a)
		}
	case "shell":
		var a ShellArgs
		if a, err = jsonArgs[ShellArgs](args); err == nil {
			var r ShellResult
			r, err = t.Shell(a)
			if err == nil {
				result = fmt.Sprintf("exit code: %d\nstdout:\n%s\nstderr:\n%s", r.ExitCode, r.Stdout, r.Stderr)
			}
		}
	case "process_list":
		result, err = t.ProcessList()
	case "process_start":
		var a ProcessArgs
		if a, err = jsonArgs[ProcessArgs](args); err == nil {
			result, err = t.ProcessStart(a)
		}
	case "process_stop":
		var a ProcessArgs
		if a, err = jsonArgs[ProcessArgs](args); err == nil {
			result, err = t.ProcessStop(a)
		}
	case "system_info":
		result, err = t.SystemInfo()
	default:
		return nil, fmt.Errorf("unknown tool: %s", name)
	}

	if err != nil {
		return json.Marshal(errorResult(err.Error()))
	}
	return json.Marshal(textResult(fmt.Sprint(result)))
}

// List returns the MCP tool definitions the agent exposes.
func (t *Tools) List() []ToolDef {
	return []ToolDef{
		{
			Name:        "read_file",
			Description: "Read a text file, optionally a line window (offset is 1-based). Use for code, config, logs. Binary files are rejected.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":   map[string]any{"type": "string", "description": "path to the file"},
					"offset": map[string]any{"type": "integer", "description": "1-based first line to return"},
					"limit":  map[string]any{"type": "integer", "description": "max lines to return"},
				},
				"required": []string{"path"},
			},
		},
		{
			Name:        "search_text",
			Description: "Recursively search files under a root for a regular expression, returning file:line matches.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern":  map[string]any{"type": "string", "description": "regular expression"},
					"root":     map[string]any{"type": "string", "description": "directory to search (default .)"},
					"include":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "filename globs to include"},
					"exclude":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "filename globs to exclude"},
					"max_hits": map[string]any{"type": "integer", "description": "max matches (default 50)"},
				},
				"required": []string{"pattern"},
			},
		},
		{
			Name:        "apply_patch",
			Description: "Modify files. Accepts either a list of exact find/replace edits or a unified diff. Deletions are blocked by policy unless allow_delete is enabled.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"edits": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"file":           map[string]any{"type": "string"},
								"old_string":     map[string]any{"type": "string"},
								"new_string":     map[string]any{"type": "string"},
								"allow_multiple": map[string]any{"type": "boolean"},
							},
							"required": []string{"file", "old_string", "new_string"},
						},
					},
					"diff": map[string]any{"type": "string", "description": "unified diff to apply via patch -p1"},
				},
			},
		},
		{
			Name:        "workspace_list",
			Description: "List the projects (subdirectories) available on this machine under the given root.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"root": map[string]any{"type": "string", "description": "directory to list (default .)"},
				},
			},
		},
		{
			Name:        "git_status",
			Description: "Show the working tree status of a git repository (git status --short).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"root": map[string]any{"type": "string", "description": "repository root (default cwd)"},
				},
			},
		},
		{
			Name:        "git_diff",
			Description: "Show unstaged changes of a git repository (git diff).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"root": map[string]any{"type": "string", "description": "repository root (default cwd)"},
				},
			},
		},
		{
			Name:        "git_log",
			Description: "Show recent commit history (git log --oneline).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"root": map[string]any{"type": "string", "description": "repository root (default cwd)"},
					"n":    map[string]any{"type": "integer", "description": "number of commits (default 20)"},
				},
			},
		},
		{
			Name:        "shell",
			Description: "Run a shell command on the machine and return stdout/stderr/exit code. Destructive commands are blocked by policy.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command":         map[string]any{"type": "string"},
					"cwd":             map[string]any{"type": "string"},
					"timeout_seconds": map[string]any{"type": "integer"},
				},
				"required": []string{"command"},
			},
		},
		{
			Name:        "process_list",
			Description: "List running processes.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			Name:        "process_start",
			Description: "Start a command detached from the agent and return its pid.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{"type": "string"},
				},
				"required": []string{"command"},
			},
		},
		{
			Name:        "process_stop",
			Description: "Terminate a process by pid.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pid": map[string]any{"type": "integer"},
				},
				"required": []string{"pid"},
			},
		},
		{
			Name:        "system_info",
			Description: "Report OS, hostname, CPU and memory information about this machine.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		},
	}
}

// ToolDef mirrors mcp.Tool without importing it, keeping tools decoupled.
type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}
