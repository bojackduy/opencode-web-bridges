// chatgpt-recover is the human-operable recovery CLI for the ChatGPT Web
// bridge. It wraps the same read-only-first endpoints the assistant tool
// uses, but runs from a terminal with an explicit session id — so it works
// even when the assistant session it belongs to is itself stuck behind an
// uncertain turn.
//
// Usage:
//
//		chatgpt-recover [-bridge chatgpt|gemini] [-addr ...] [-reason "..."] <inspect|recover|abandon> <session-id>
//
//	  - inspect (default): read-only state, counts and available actions.
//	  - recover: read-only adoption of the pending turn. Never submits.
//	  - abandon: clears ONLY the pending-uncertainty binding for the named
//	    session; transcript, thread, results and fingerprints are retained.
//	    Historical tools are never re-run.
//
// It never submits a turn, never resends an uncertain turn, and never
// touches any session other than the one named on the command line.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func usage() {
	fmt.Fprintf(os.Stderr, `usage: chatgpt-recover [-bridge chatgpt|gemini] [-addr ...] [-reason "..."] <inspect|recover|abandon> <session-id>

  bridges: chatgpt (default addr 127.0.0.1:8788), gemini (default addr 127.0.0.1:8789)
  gemini has no read-only adoption endpoint: recover reports that and
  names the real moves (a distinct request, or abandon).

  inspect  read-only state: what is unknown, what was kept, what you can do
  recover  read-only adoption of the pending turn; never submits anything
  abandon  clear the pending-uncertainty binding for the named session only;
           transcript, thread, results and fingerprints are retained

flags:
`)
	flag.PrintDefaults()
}

func refusedSession(id string) string {
	switch {
	case strings.TrimSpace(id) == "":
		return "empty session id; name the session explicitly"
	case strings.Contains(id, "*"):
		return fmt.Sprintf("wildcard %q refused; name the session explicitly", id)
	case strings.EqualFold(id, "all"), strings.EqualFold(id, "any"):
		return fmt.Sprintf("%q is not a session id; name the session explicitly", id)
	}
	return ""
}

func doJSON(client *http.Client, method, url, session string, body any) (int, map[string]any, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("X-Session-Id", session)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("adapter unreachable at %s: %w", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return resp.StatusCode, nil, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if resp.StatusCode >= 400 {
		msg := string(raw)
		if e, ok := out["error"].(map[string]any); ok {
			if m, ok := e["message"].(string); ok && m != "" {
				msg = m
			}
		}
		return resp.StatusCode, out, fmt.Errorf("status %d: %s", resp.StatusCode, msg)
	}
	return resp.StatusCode, out, nil
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func num(m map[string]any, key string) int {
	f, _ := m[key].(float64)
	return int(f)
}

func run() int {
	addr := flag.String("addr", "", "bridge adapter listen address (default depends on -bridge)")
	bridge := flag.String("bridge", "chatgpt", "bridge: chatgpt or gemini")
	reason := flag.String("reason", "", "reason recorded with abandon")
	flag.Usage = usage
	flag.Parse()
	args := flag.Args()
	if len(args) < 1 {
		usage()
		return 2
	}
	action, session := args[0], ""
	if len(args) > 1 {
		session = args[1]
	}
	if action != "inspect" && action != "recover" && action != "abandon" {
		fmt.Fprintf(os.Stderr, "chatgpt-recover: unknown action %q (want inspect|recover|abandon)\n", action)
		return 2
	}
	if bad := refusedSession(session); bad != "" {
		fmt.Fprintf(os.Stderr, "chatgpt-recover: %s\n", bad)
		return 2
	}
	if *bridge != "chatgpt" && *bridge != "gemini" {
		fmt.Fprintf(os.Stderr, "chatgpt-recover: unknown bridge %q (want chatgpt|gemini)\n", *bridge)
		return 2
	}
	if *addr == "" {
		if *bridge == "gemini" {
			*addr = "127.0.0.1:8789"
		} else {
			*addr = "127.0.0.1:8788"
		}
	}
	base := "http://" + strings.TrimPrefix(*addr, "http://")

	switch action {
	case "inspect":
		client := &http.Client{Timeout: 15 * time.Second}
		_, out, err := doJSON(client, http.MethodGet, base+"/v1/sessions/state", session, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "chatgpt-recover: %v\n", err)
			return 1
		}
		fmt.Printf("session: %s\nstate: %s\n", session, str(out, "state"))
		if s := str(out, "summary"); s != "" {
			fmt.Printf("summary: %s\n", s)
		}
		fmt.Printf("completed_messages: %d\npending_prompt_bytes: %d\noutstanding_tool_calls: %d\nthread_url_present: %v\ncan_recover: %v\ncan_resync: %v\n",
			num(out, "completed_messages"), num(out, "pending_prompt_bytes"),
			num(out, "outstanding_tool_calls"), out["thread_url_present"] == true,
			out["can_recover"] == true, out["can_resync"] == true)
		if acts, ok := out["actions"].([]any); ok && len(acts) > 0 {
			names := make([]string, 0, len(acts))
			for _, a := range acts {
				if s, ok := a.(string); ok {
					names = append(names, s)
				}
			}
			fmt.Printf("actions: %s\n", strings.Join(names, ", "))
		}
		return 0
	case "recover":
		if *bridge == "gemini" {
			fmt.Printf("session %s: the Gemini bridge has no read-only adoption endpoint.\n", session)
			fmt.Printf("moves: send a distinct request (the adapter reseeds a fresh thread and abandons the old one, never resending it), or run: chatgpt-recover -bridge gemini abandon %s\n", session)
			return 1
		}
		client := &http.Client{Timeout: 120 * time.Second}
		_, out, err := doJSON(client, http.MethodPost, base+"/v1/sessions/recover", session, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "chatgpt-recover: %v\nNothing was resent and no tools were re-run.\n", err)
			return 1
		}
		if b, ok := out["recovered"].(bool); ok && b {
			fmt.Printf("session %s: pending turn adopted read-only (cached_tool_calls=%v).\n", session, out["cached_tool_calls"])
			return 0
		}
		fmt.Printf("session %s: nothing could be verified; nothing was resent and no tools were re-run.\n", session)
		return 1
	case "abandon":
		client := &http.Client{Timeout: 20 * time.Second}
		_, out, err := doJSON(client, http.MethodPost, base+"/v1/sessions/abandon", session,
			map[string]any{"sessions": []string{session}, "reason": *reason})
		if err != nil {
			fmt.Fprintf(os.Stderr, "chatgpt-recover: %v\n", err)
			return 1
		}
		fmt.Printf("session %s: abandonment recorded.\n", session)
		if results, ok := out["results"].([]any); ok {
			for _, r := range results {
				if m, ok := r.(map[string]any); ok {
					fmt.Printf("  outcome=%v state_before=%v delivered_calls=%v recorded_results=%v outstanding_calls=%v abandoned_prompt_bytes=%v\n",
						m["outcome"], m["state_before"], m["delivered_calls"],
						m["recorded_results"], m["outstanding_calls"], m["abandoned_prompt_bytes"])
				}
			}
		}
		fmt.Printf("kept: completed transcript, thread URL, results and fingerprints. Historical tools will not be re-run.\n")
		return 0
	}
	return 2
}

func main() {
	os.Exit(run())
}
