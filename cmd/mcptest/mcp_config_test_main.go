//go:build ignore

package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/scoutme/milk/internal/config"
)

type httpEntry struct {
	Type    string            `json:"type"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

func main() {
	cfg, err := config.Load()
	if err != nil {
		panic(err)
	}
	servers := cfg.EffectiveMCPServers("claude")
	fmt.Printf("MCP servers for 'claude': %d\n", len(servers))
	for _, s := range servers {
		fmt.Printf("  - %s: %s (auth=%q)\n", s.Name, s.URL, s.Auth)
	}

	entries := make(map[string]httpEntry, len(servers))
	for _, s := range servers {
		entry := httpEntry{Type: "http", URL: s.URL}
		if strings.ToLower(s.Auth) == "bearer" && s.APIKey != "" {
			entry.Headers = map[string]string{"Authorization": "Bearer " + s.APIKey}
		}
		entries[s.Name] = entry
	}
	payload := map[string]any{"mcpServers": entries}
	data, _ := json.MarshalIndent(payload, "", "  ")
	fmt.Printf("\n--mcp-config JSON:\n%s\n", data)
}
