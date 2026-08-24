package mcp

import (
	"context"
	"net/http"
	"testing"

	"github.com/scoutme/milk/internal/config"
)

func TestApplyAuth_Bearer(t *testing.T) {
	c := New(config.MCPServerConfig{Name: "s", URL: "https://example.com", Auth: "bearer", APIKey: "secret"})
	req, _ := http.NewRequest(http.MethodPost, "https://example.com", nil)
	c.applyAuth(context.Background(), req)
	if got := req.Header.Get("Authorization"); got != "Bearer secret" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer secret")
	}
}

func TestApplyAuth_TokenCmd_UsesCachedHeader(t *testing.T) {
	c := New(config.MCPServerConfig{Name: "s", URL: "https://example.com", Auth: "token_cmd", TokenCmd: "echo mytoken"})
	if err := c.resolveToken(context.Background()); err != nil {
		t.Fatalf("resolveToken: %v", err)
	}
	req, _ := http.NewRequest(http.MethodPost, "https://example.com", nil)
	c.applyAuth(context.Background(), req)
	if got := req.Header.Get("Authorization"); got != "Bearer mytoken" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer mytoken")
	}
}

func TestApplyAuth_None(t *testing.T) {
	c := New(config.MCPServerConfig{Name: "s", URL: "https://example.com"})
	req, _ := http.NewRequest(http.MethodPost, "https://example.com", nil)
	c.applyAuth(context.Background(), req)
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want empty", got)
	}
}
