// Command relay runs the public gateway that MCP clients (e.g. ChatGPT Web)
// connect to. Local agents connect to it outbound via /ws/agent.
//
// Environment variables:
//
//	RELAY_ADDR        listen address (default ":8080")
//	AGENT_TOKEN       required token for agents connecting to /ws/agent
//	CLIENT_TOKEN      optional Bearer token required on /mcp (empty = open)
//	AGENT_TIMEOUT     max seconds to wait for an agent reply (default 300)
package main

import (
	"log"
	"os"
	"strconv"
	"time"

	"chatgpt-proxy/legacy/internal/relay"
)

func main() {
	timeout := 300
	if v := os.Getenv("AGENT_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			timeout = n
		}
	}
	cfg := relay.Config{
		Addr:         getenv("RELAY_ADDR", ":8080"),
		AgentToken:   os.Getenv("AGENT_TOKEN"),
		ClientToken:  os.Getenv("CLIENT_TOKEN"),
		AgentTimeout: time.Duration(timeout) * time.Second,
	}
	if cfg.AgentToken == "" {
		log.Fatal("AGENT_TOKEN is required: set a secret the local agent will use to authenticate")
	}
	s := relay.New(cfg)
	log.Fatal(s.ListenAndServe())
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
