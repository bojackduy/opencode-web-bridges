// Command agent runs the local side of chatgpt-proxy: it opens an outbound
// WebSocket connection to the relay and executes MCP tool calls on this
// machine (file read/search/patch, git, shell, process, system info).
//
// Environment variables:
//
//	RELAY_URL     the relay's agent endpoint, e.g. wss://relay.example.com/ws/agent
//	AGENT_TOKEN   the token configured on the relay
//	AGENT_NAME    display name shown to MCP clients (default "local-agent")
//	WORKSPACE_ROOT  comma-separated directories the agent may touch (empty = unrestricted)
//	ALLOW_DELETE  "true" disables the destructive-command policy (default false)
package main

import (
	"log"
	"os"
	"strings"

	"chatgpt-proxy/legacy/internal/agent"
	"chatgpt-proxy/legacy/internal/tools"
)

func main() {
	relayURL := os.Getenv("RELAY_URL")
	if relayURL == "" {
		log.Fatal("RELAY_URL is required, e.g. wss://relay.example.com/ws/agent")
	}
	token := os.Getenv("AGENT_TOKEN")
	if token == "" {
		log.Fatal("AGENT_TOKEN is required")
	}
	name := getenv("AGENT_NAME", "local-agent")

	var roots []string
	for _, r := range strings.Split(os.Getenv("WORKSPACE_ROOT"), ",") {
		if r = strings.TrimSpace(r); r != "" {
			roots = append(roots, r)
		}
	}

	t := tools.NewTools(roots)
	if strings.EqualFold(os.Getenv("ALLOW_DELETE"), "true") {
		t.Policy.AllowDelete = true
		log.Printf("agent: ALLOW_DELETE=true — destructive commands are permitted")
	}

	a := &agent.Agent{
		RelayURL: relayURL,
		Token:    token,
		Name:     name,
		Tools:    t,
	}
	log.Printf("agent: starting, workspace roots=%v", roots)
	if err := a.Run(); err != nil {
		log.Fatal(err)
	}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
