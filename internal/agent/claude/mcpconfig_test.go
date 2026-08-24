package claude

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/scoutme/milk/internal/config"
)

func TestWriteMCPConfigFile_BearerHeaderForwarded(t *testing.T) {
	servers := []config.MCPServerConfig{
		{Name: "s1", URL: "https://example.com/mcp", Auth: "bearer", APIKey: "secret"},
	}
	path, cleanup, err := writeMCPConfigFile(context.Background(), servers)
	if err != nil {
		t.Fatalf("writeMCPConfigFile: %v", err)
	}
	defer cleanup()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var payload struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	entry, ok := payload.MCPServers["s1"]
	if !ok {
		t.Fatalf("missing entry for s1: %+v", payload.MCPServers)
	}
	if got := entry.Headers["Authorization"]; got != "Bearer secret" {
		t.Errorf("Authorization header = %q, want %q", got, "Bearer secret")
	}
}

func TestWriteMCPConfigFile_TokenCmdHeaderForwarded(t *testing.T) {
	servers := []config.MCPServerConfig{
		{Name: "s1", URL: "https://example.com/mcp", Auth: "token_cmd", TokenCmd: "echo mytoken"},
	}
	path, cleanup, err := writeMCPConfigFile(context.Background(), servers)
	if err != nil {
		t.Fatalf("writeMCPConfigFile: %v", err)
	}
	defer cleanup()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var payload struct {
		MCPServers map[string]struct {
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got := payload.MCPServers["s1"].Headers["Authorization"]; got != "Bearer mytoken" {
		t.Errorf("Authorization header = %q, want %q", got, "Bearer mytoken")
	}
}

func TestWriteMCPConfigFile_NoAuthNoHeaders(t *testing.T) {
	servers := []config.MCPServerConfig{
		{Name: "s1", URL: "https://example.com/mcp"},
	}
	path, cleanup, err := writeMCPConfigFile(context.Background(), servers)
	if err != nil {
		t.Fatalf("writeMCPConfigFile: %v", err)
	}
	defer cleanup()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var payload struct {
		MCPServers map[string]struct {
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(payload.MCPServers["s1"].Headers) != 0 {
		t.Errorf("expected no headers, got %+v", payload.MCPServers["s1"].Headers)
	}
}
