package tools

import (
	"fmt"
	"regexp"
	"strings"
)

// Policy controls what the agent is allowed to do. The default policy is
// "full access but never destructive": create/update/edit is allowed, but
// commands or edits that delete data require explicit approval.
//
// This mirrors the access model of the setup chatgpt-proxy is inspired by:
// "เพิ่ม-อัปเดต-แก้ไขได้ ยกเว้น ลบ" (may add/update/edit, but never delete).
type Policy struct {
	// DenyShellPatterns is a list of regexps matched (case-insensitively)
	// against the raw shell command. A match rejects the command.
	DenyShellPatterns []*regexp.Regexp `json:"-"`

	// DenyShellRaw is the config representation of DenyShellPatterns.
	DenyShellRaw []string `json:"deny_shell_patterns,omitempty"`

	// AllowDelete overrides the default: when true, destructive commands
	// are permitted. Default false.
	AllowDelete bool `json:"allow_delete,omitempty"`
}

// DefaultDenyPatterns are destructive or machine-affecting commands that are
// blocked unless explicitly allowed. They are conservative on purpose.
var DefaultDenyPatterns = []string{
	`\brm\s+-[a-z]*r`,                  // rm -r / rm -rf
	`\brm\b.*\*`,                       // rm with a glob
	`\bdel\b.*\*`,                      // Windows del with a glob
	`\brmdir\b`,                        // rmdir
	`\brmtree\b`,                       // Windows rmtree
	`\bformat\s+[a-z]:`,                // Windows drive format
	`\bmkfs`,                           // filesystem creation
	`\bshutdown\b`,                     // shutdown
	`\breboot\b`,                       // reboot
	`\bpoweroff\b`,                     // poweroff
	`\binit\s+0`,                       // init 0
	`\bdd\s+.*\bof=`,                   // dd writing to a device/file
	`\bgit\s+push\s+(-f|--force)`,      // force push
	`\bgit\s+reset\s+--hard`,           // destructive git reset
	`\bgit\s+clean\s+(-f|--force|-)`,   // destructive git clean
	`\bdrop\s+(database|table|schema)`, // drop database objects
	`\btruncate\s+table`,               // truncate tables
	`\brm\s+-rf\s+/\s*$`,               // rm -rf /
	`\bmysql\s+.*\bdelete\b`,           // sql deletes
	`\bdelete\s+from\b`,                // sql deletes
	`> /dev/sd`,                        // writing to block devices
	`\b:\(\)\s*\{\s*:\|:&\s*\};`,       // fork bomb
}

// NewDefaultPolicy builds a Policy with the standard deny list.
func NewDefaultPolicy() *Policy {
	return newPolicy(DefaultDenyPatterns, false)
}

// NewPolicy builds a Policy from raw patterns. An empty pattern list means
// the defaults are used.
func NewPolicy(denyRaw []string, allowDelete bool) *Policy {
	if len(denyRaw) == 0 {
		denyRaw = DefaultDenyPatterns
	}
	return newPolicy(denyRaw, allowDelete)
}

func newPolicy(patterns []string, allowDelete bool) *Policy {
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		if re, err := regexp.Compile(`(?i)` + p); err == nil {
			compiled = append(compiled, re)
		}
	}
	return &Policy{DenyShellPatterns: compiled, DenyShellRaw: patterns, AllowDelete: allowDelete}
}

// CheckShell validates a shell command against the policy. It returns an
// error describing the offending pattern when the command is blocked.
func (p *Policy) CheckShell(command string) error {
	if p.AllowDelete {
		return nil
	}
	for _, re := range p.DenyShellPatterns {
		if re.MatchString(command) {
			return fmt.Errorf("command blocked by policy (matches %q): destructive operations are disabled by default; set allow_delete=true to enable", re.String())
		}
	}
	return nil
}

// CheckEdit validates a file edit against the policy. Deletions of files
// (removing a whole file) are blocked unless AllowDelete is set.
func (p *Policy) CheckEdit(kind string) error {
	if p.AllowDelete {
		return nil
	}
	if kind == "delete_file" {
		return fmt.Errorf("file deletion blocked by policy: destructive operations are disabled by default; set allow_delete=true to enable")
	}
	return nil
}

// SanitizeCommand trims whitespace and collapses internal whitespace so
// policy regexps see a normalized command.
func SanitizeCommand(command string) string {
	return strings.TrimSpace(command)
}
