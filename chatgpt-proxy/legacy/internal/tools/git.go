package tools

import (
	"fmt"
	"strings"
)

// GitArgs are the arguments of the git_status and git_diff tools.
type GitArgs struct {
	Root string `json:"root,omitempty"`
}

// GitLogArgs are the arguments of the git_log tool.
type GitLogArgs struct {
	Root string `json:"root,omitempty"`
	N    int    `json:"n,omitempty"`
}

func (t *Tools) gitRun(root, command string) (string, error) {
	if root != "" {
		if err := t.checkPath(root); err != nil {
			return "", err
		}
	}
	res, err := t.Shell(ShellArgs{Command: command, Cwd: root, Timeout: 30})
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = strings.TrimSpace(res.Stdout)
		}
		return "", fmt.Errorf("git failed (exit %d): %s", res.ExitCode, msg)
	}
	return strings.TrimSpace(res.Stdout), nil
}

// GitStatus shows the working tree status.
func (t *Tools) GitStatus(args GitArgs) (string, error) {
	return t.gitRun(args.Root, "git status --short")
}

// GitDiff shows unstaged changes.
func (t *Tools) GitDiff(args GitArgs) (string, error) {
	return t.gitRun(args.Root, "git diff")
}

// GitLog shows recent commit history.
func (t *Tools) GitLog(args GitLogArgs) (string, error) {
	n := args.N
	if n <= 0 {
		n = 20
	}
	if n > 200 {
		n = 200
	}
	return t.gitRun(args.Root, fmt.Sprintf("git log --oneline -n %d", n))
}
