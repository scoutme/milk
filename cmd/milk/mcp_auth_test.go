package main

import (
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/config"
)

func TestValidateMCPAuthArgs_EmptyName(t *testing.T) {
	cfg := config.Config{}
	idx, errMsg := validateMCPAuthArgs("", cfg)
	if idx != -1 {
		t.Errorf("expected idx=-1 for empty name, got %d", idx)
	}
	if !strings.Contains(errMsg, "usage") {
		t.Errorf("expected usage hint in errMsg, got %q", errMsg)
	}
}

func TestValidateMCPAuthArgs_WhitespaceName(t *testing.T) {
	cfg := config.Config{}
	idx, errMsg := validateMCPAuthArgs("   ", cfg)
	if idx != -1 {
		t.Errorf("expected idx=-1 for whitespace-only name, got %d", idx)
	}
	if !strings.Contains(errMsg, "usage") {
		t.Errorf("expected usage hint in errMsg for whitespace name, got %q", errMsg)
	}
}

func TestValidateMCPAuthArgs_UnknownServer(t *testing.T) {
	cfg := config.Config{
		MCPServers: []config.MCPServerConfig{
			{Name: "known-server", URL: "https://mcp.example.com"},
		},
	}
	idx, errMsg := validateMCPAuthArgs("nonexistent", cfg)
	if idx != -1 {
		t.Errorf("expected idx=-1 for unknown server, got %d", idx)
	}
	if !strings.Contains(errMsg, "not found") {
		t.Errorf("expected 'not found' in errMsg, got %q", errMsg)
	}
}

func TestValidateMCPAuthArgs_KnownServer(t *testing.T) {
	cfg := config.Config{
		MCPServers: []config.MCPServerConfig{
			{Name: "my-oauth-server", URL: "https://mcp.example.com", Auth: "oauth"},
		},
	}
	idx, errMsg := validateMCPAuthArgs("my-oauth-server", cfg)
	if errMsg != "" {
		t.Errorf("expected no error for known server, got %q", errMsg)
	}
	if idx != 0 {
		t.Errorf("expected idx=0 for first server, got %d", idx)
	}
}

func TestValidateMCPAuthArgs_CaseInsensitive(t *testing.T) {
	cfg := config.Config{
		MCPServers: []config.MCPServerConfig{
			{Name: "MyServer", URL: "https://mcp.example.com"},
		},
	}
	idx, errMsg := validateMCPAuthArgs("myserver", cfg)
	if errMsg != "" {
		t.Errorf("expected case-insensitive match, got errMsg=%q", errMsg)
	}
	if idx != 0 {
		t.Errorf("expected idx=0 for case-insensitive match, got %d", idx)
	}
}

func TestValidateMCPAuthArgs_MultipleServers(t *testing.T) {
	cfg := config.Config{
		MCPServers: []config.MCPServerConfig{
			{Name: "server-a", URL: "https://a.example.com"},
			{Name: "server-b", URL: "https://b.example.com"},
			{Name: "server-c", URL: "https://c.example.com"},
		},
	}
	idx, errMsg := validateMCPAuthArgs("server-b", cfg)
	if errMsg != "" {
		t.Errorf("expected no error for server-b, got %q", errMsg)
	}
	if idx != 1 {
		t.Errorf("expected idx=1 for server-b, got %d", idx)
	}
}
