package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Lane identity must come from the client, not from content or tool counts.
// Main retains the legacy session key; auxiliary journals cannot alter it.
func completionSessionID(r *http.Request) (string, string, error) {
	id := sessionID(r)
	lane := r.Header.Get("X-OpenCode-Request-Lane")
	switch lane {
	case "", "main":
		return id, "main", nil
	case "title", "summary", "compaction", "auxiliary":
		if id == "" {
			return "", lane, nil
		}
		return fmt.Sprintf("aux:%x", sha256.Sum256([]byte(id+"\x00"+lane))), lane, nil
	default:
		return "", "", errors.New("unknown X-OpenCode-Request-Lane")
	}
}

// titlePromptPrefix is the exact first-user-message prefix the installed
// OpenCode host uses for title generation (session/prompt.ts ensureTitle).
// The host reuses the main session ID without a lane header, so an untagged
// title request would otherwise prefix-mismatch at index 0 and rotate the
// main working thread — observed live on ses_ec74b8ec4ffekz0UslWJu0xkCT.
const titlePromptPrefix = "Generate a title for this conversation:\n"

// isolateUntaggedTitle reroutes an untagged same-session title request to the
// title-lane journal. Every condition must hold: established main thread to
// protect, no tools, first message a user message with the exact host title
// prefix. Genuine main turns start with a system message, so the prefix test
// cannot catch them; near-misses, multipart content, and tool-carrying
// requests stay on main.
func isolateUntaggedTitle(mainID string, req completionsRequest, main *session) (string, bool) {
	if mainID == "" || main == nil || main.URL == "" || len(req.Tools) != 0 || len(req.Messages) == 0 {
		return "", false
	}
	first := req.Messages[0]
	if first.Role != "user" {
		return "", false
	}
	text, ok := first.Content.(string)
	if !ok || !strings.HasPrefix(text, titlePromptPrefix) {
		return "", false
	}
	return fmt.Sprintf("aux:%x", sha256.Sum256([]byte(mainID+"\x00"+"title"))), true
}
