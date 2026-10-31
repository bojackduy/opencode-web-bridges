package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// ShellArgs are the arguments of the shell tool.
type ShellArgs struct {
	Command string `json:"command"`
	Cwd     string `json:"cwd,omitempty"`
	Timeout int    `json:"timeout_seconds,omitempty"` // default 60
}

// ShellResult is the structured outcome of a shell run.
type ShellResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// Shell runs a command through the platform shell with policy enforcement.
func (t *Tools) Shell(args ShellArgs) (ShellResult, error) {
	cmd := SanitizeCommand(args.Command)
	if cmd == "" {
		return ShellResult{}, fmt.Errorf("command is required")
	}
	if err := t.Policy.CheckShell(cmd); err != nil {
		return ShellResult{}, err
	}
	if args.Cwd != "" {
		if err := t.checkPath(args.Cwd); err != nil {
			return ShellResult{}, err
		}
	}

	timeout := args.Timeout
	if timeout <= 0 {
		timeout = 60
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()

	var sh, flag string
	if runtime.GOOS == "windows" {
		sh, flag = "cmd", "/C"
	} else {
		sh, flag = "sh", "-c"
	}
	execCmd := exec.CommandContext(ctx, sh, flag, cmd)
	execCmd.Dir = args.Cwd
	var stdout, stderr bytes.Buffer
	execCmd.Stdout = &stdout
	execCmd.Stderr = &stderr

	err := execCmd.Run()
	res := ShellResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			res.ExitCode = -1
			res.Stderr = strings.TrimSpace(res.Stderr) + "\n[tool] command timed out after " + strconv.Itoa(timeout) + "s"
			return res, nil
		}
		if ee, ok := err.(*exec.ExitError); ok {
			res.ExitCode = ee.ExitCode()
		} else {
			res.ExitCode = -1
			res.Stderr = err.Error()
		}
	}
	return res, nil
}

// ProcessArgs are the arguments of the process tools.
type ProcessArgs struct {
	PID     int    `json:"pid"`
	Command string `json:"command"`
}

// ProcessList lists running processes (pid, command, args).
func (t *Tools) ProcessList() (string, error) {
	res, err := t.Shell(ShellArgs{Command: processListCmd(), Timeout: 15})
	if err != nil {
		return "", err
	}
	return res.Stdout, nil
}

// ProcessStart launches a command detached from the agent and reports its pid.
func (t *Tools) ProcessStart(args ProcessArgs) (string, error) {
	cmd := SanitizeCommand(args.Command)
	if cmd == "" {
		return "", fmt.Errorf("command is required")
	}
	if err := t.Policy.CheckShell(cmd); err != nil {
		return "", err
	}
	var sh, flag string
	if runtime.GOOS == "windows" {
		sh, flag = "cmd", "/C"
	} else {
		sh, flag = "sh", "-c"
	}
	execCmd := exec.Command(sh, flag, cmd)
	execCmd.Stdout = os.Stdout
	execCmd.Stderr = os.Stderr
	if err := execCmd.Start(); err != nil {
		return "", err
	}
	// Detach: don't wait for it; the process outlives the call.
	_ = execCmd.Process.Release()
	return fmt.Sprintf("started pid %d: %s", execCmd.Process.Pid, cmd), nil
}

// ProcessStop terminates a process by pid.
func (t *Tools) ProcessStop(args ProcessArgs) (string, error) {
	if args.PID <= 0 {
		return "", fmt.Errorf("pid is required")
	}
	proc, err := os.FindProcess(args.PID)
	if err != nil {
		return "", fmt.Errorf("process %d not found: %w", args.PID, err)
	}
	if err := proc.Kill(); err != nil {
		return "", fmt.Errorf("failed to kill pid %d: %w", args.PID, err)
	}
	return fmt.Sprintf("terminated pid %d", args.PID), nil
}

func processListCmd() string {
	switch runtime.GOOS {
	case "windows":
		return "tasklist"
	case "darwin":
		return "ps -axo pid,ppid,comm,args"
	default:
		return "ps -eo pid,ppid,comm,args"
	}
}

// execOutput runs an external command and returns its trimmed stdout.
func execOutput(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

// runCapture runs an external command with stdin from a string and returns
// combined output. Used by apply_patch to drive the `patch` binary.
func runCapture(name string, stdin *string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	if stdin != nil {
		cmd.Stdin = strings.NewReader(*stdin)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}
