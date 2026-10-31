package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ReadFileArgs are the arguments of the read_file tool.
type ReadFileArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset,omitempty"` // 1-based first line to return
	Limit  int    `json:"limit,omitempty"`  // max lines to return
}

// ReadFile reads a file, optionally a window of it, mirroring the classic
// agent read_file contract.
func (t *Tools) ReadFile(args ReadFileArgs) (string, error) {
	if args.Path == "" {
		return "", fmt.Errorf("path is required")
	}
	if err := t.checkPath(args.Path); err != nil {
		return "", err
	}
	data, err := os.ReadFile(args.Path)
	if err != nil {
		return "", err
	}
	text := string(data)
	if strings.ContainsRune(text, '\x00') {
		return "", fmt.Errorf("%s appears to be a binary file; refusing to read as text", args.Path)
	}

	lines := strings.Split(text, "\n")
	total := len(lines)

	start := 0
	if args.Offset > 0 {
		start = args.Offset - 1
	}
	if start >= total {
		return "", fmt.Errorf("offset %d is past the end of %s (%d lines)", args.Offset, args.Path, total)
	}

	end := total
	if args.Limit > 0 && start+args.Limit < end {
		end = start + args.Limit
	}
	if t.MaxFileLines > 0 && end-start > t.MaxFileLines {
		end = start + t.MaxFileLines
	}

	window := lines[start:end]
	var b strings.Builder
	fmt.Fprintf(&b, "--- %s (%d lines total, showing %d-%d) ---\n", args.Path, total, start+1, end)
	for i, line := range window {
		fmt.Fprintf(&b, "%6d\t%s\n", start+i+1, line)
	}
	if end < total {
		fmt.Fprintf(&b, "--- truncated: %d more lines (use offset=%d to continue) ---\n", total-end, end+1)
	}
	return b.String(), nil
}

// SearchArgs are the arguments of the search_text tool.
type SearchArgs struct {
	Pattern string   `json:"pattern"`
	Root    string   `json:"root,omitempty"`
	Include []string `json:"include,omitempty"` // filename globs, e.g. ["*.go","*.ts"]
	Exclude []string `json:"exclude,omitempty"` // filename globs to skip
	MaxHits int      `json:"max_hits,omitempty"`
}

// SearchText recursively searches files under root (default ".") for a
// regular expression, returning matches with file/line context.
func (t *Tools) SearchText(args SearchArgs) (string, error) {
	if args.Pattern == "" {
		return "", fmt.Errorf("pattern is required")
	}
	re, err := regexp.Compile(args.Pattern)
	if err != nil {
		return "", fmt.Errorf("invalid pattern: %w", err)
	}
	root := args.Root
	if root == "" {
		root = "."
	}
	if err := t.checkPath(root); err != nil {
		return "", err
	}
	maxHits := args.MaxHits
	if maxHits <= 0 {
		maxHits = 50
	}

	var includeRe, excludeRe []*regexp.Regexp
	for _, g := range args.Include {
		if re, err := filepathGlobToRegexp(g); err == nil {
			includeRe = append(includeRe, re)
		}
	}
	for _, g := range args.Exclude {
		if re, err := filepathGlobToRegexp(g); err == nil {
			excludeRe = append(excludeRe, re)
		}
	}

	var b strings.Builder
	hits := 0
	checked := 0

	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		if len(includeRe) > 0 && !matchAny(includeRe, path) {
			return nil
		}
		if matchAny(excludeRe, path) {
			return nil
		}
		checked++
		if hits >= maxHits {
			return filepath.SkipAll
		}
		data, err := os.ReadFile(path)
		if err != nil || strings.ContainsRune(string(data), '\x00') {
			return nil
		}
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			if re.MatchString(line) {
				fmt.Fprintf(&b, "%s:%d: %s\n", path, i+1, strings.TrimRight(line, "\r"))
				hits++
				if hits >= maxHits {
					break
				}
			}
		}
		return nil
	})
	if walkErr != nil {
		return "", walkErr
	}
	fmt.Fprintf(&b, "--- %d match(es) in %d file(s); hit limit %d ---\n", hits, checked, maxHits)
	return b.String(), nil
}

func matchAny(res []*regexp.Regexp, s string) bool {
	for _, re := range res {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// filepathGlobToRegexp converts a glob like "*.go" or "**/*.test.ts" into a
// regexp anchored on the full path.
func filepathGlobToRegexp(glob string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for _, r := range glob {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		case '.', '(', ')', '+', '[', ']', '{', '}', '^', '$', '|', '\\':
			b.WriteString(`\` + string(r))
		default:
			b.WriteRune(r)
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// PatchEdit is a single find/replace edit inside one file.
type PatchEdit struct {
	File          string `json:"file"`
	OldString     string `json:"old_string"`
	NewString     string `json:"new_string"`
	AllowMultiple bool   `json:"allow_multiple,omitempty"`
}

// ApplyPatchArgs are the arguments of the apply_patch tool. Either Edits
// (find/replace pairs) or Diff (a unified diff) may be supplied.
type ApplyPatchArgs struct {
	Edits []PatchEdit `json:"edits,omitempty"`
	Diff  string      `json:"diff,omitempty"`
}

// ApplyPatch applies a set of edits or a unified diff to files on disk.
func (t *Tools) ApplyPatch(args ApplyPatchArgs) (string, error) {
	var results []string
	if len(args.Edits) > 0 {
		for _, e := range args.Edits {
			n, err := t.applyEdit(e)
			if err != nil {
				return "", err
			}
			results = append(results, fmt.Sprintf("patched %s (%d replacement(s))", e.File, n))
		}
	}
	if args.Diff != "" {
		if err := t.applyUnifiedDiff(args.Diff); err != nil {
			return "", err
		}
		results = append(results, "applied unified diff")
	}
	if len(results) == 0 {
		return "", fmt.Errorf("nothing to apply: provide either edits or diff")
	}
	return strings.Join(results, "\n"), nil
}

func (t *Tools) applyEdit(e PatchEdit) (int, error) {
	if e.File == "" {
		return 0, fmt.Errorf("edit.file is required")
	}
	if e.OldString == "" {
		return 0, fmt.Errorf("edit.old_string is required")
	}
	if err := t.checkPath(e.File); err != nil {
		return 0, err
	}
	data, err := os.ReadFile(e.File)
	if err != nil {
		return 0, err
	}
	content := string(data)
	n := strings.Count(content, e.OldString)
	if n == 0 {
		return 0, fmt.Errorf("old_string not found in %s", e.File)
	}
	if n > 1 && !e.AllowMultiple {
		return 0, fmt.Errorf("old_string occurs %d times in %s; set allow_multiple=true to replace all", n, e.File)
	}
	updated := strings.ReplaceAll(content, e.OldString, e.NewString)
	if err := os.WriteFile(e.File, []byte(updated), 0o644); err != nil {
		return 0, err
	}
	return n, nil
}

// applyUnifiedDiff applies a unified diff by delegating to the system
// `patch` command (portable and battle-tested).
func (t *Tools) applyUnifiedDiff(diff string) error {
	trimmed := strings.TrimSpace(diff)
	if !strings.HasPrefix(trimmed, "---") && !strings.HasPrefix(trimmed, "diff ") {
		return fmt.Errorf("diff does not look like a unified diff (must start with --- or diff )")
	}
	// `patch` can delete files; deletions are gated by policy.
	if strings.Contains(diff, "deleted file mode") && !t.Policy.AllowDelete {
		return fmt.Errorf("diff contains file deletions, blocked by policy; set allow_delete=true to enable")
	}
	stdin := diff
	out, err := runCapture("patch", &stdin, "-p1", "--batch", "--forward")
	if err != nil {
		return fmt.Errorf("patch failed: %v\n%s", err, out)
	}
	return nil
}

// WorkspaceArgs are the arguments of the workspace_list tool.
type WorkspaceArgs struct {
	Root string `json:"root,omitempty"`
}

// WorkspaceList lists the immediate subdirectories of the given root
// (default: the first configured workspace root, else "."), which
// represents the projects available on this machine.
func (t *Tools) WorkspaceList(args WorkspaceArgs) (string, error) {
	root := args.Root
	if root == "" {
		if len(t.WorkspaceRoots) > 0 {
			root = t.WorkspaceRoots[0]
		} else {
			root = "."
		}
	}
	if err := t.checkPath(root); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Strings(dirs)
	if len(dirs) == 0 {
		return "no subdirectories found", nil
	}
	return strings.Join(dirs, "\n"), nil
}
