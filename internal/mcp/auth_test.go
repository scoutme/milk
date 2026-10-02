package mcp

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/mcpauth"
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

// TestMaybeNotifyAuthRequired_FiresOncePerEpoch pins the #161 contract: the
// callback wired via WithOnAuthRequired fires exactly once per connected
// epoch when a call fails with an auth-required error, stays silent on
// further auth failures until the next successful Connect rearms it
// (Connect resets authNotified — see its doc comment), and never fires at
// all for an error that doesn't classify as "needs /mcp auth".
func TestMaybeNotifyAuthRequired_FiresOncePerEpoch(t *testing.T) {
	c := New(config.MCPServerConfig{Name: "s", URL: "https://example.com", Auth: "oauth"})
	var notified []string
	c.WithOnAuthRequired(func(name string) { notified = append(notified, name) })

	c.maybeNotifyAuthRequired(mcpauth.ErrNotAuthorized)
	c.maybeNotifyAuthRequired(mcpauth.ErrNotAuthorized) // same epoch: must stay silent
	if len(notified) != 1 || notified[0] != "s" {
		t.Fatalf("notified = %v, want exactly one call with %q", notified, "s")
	}

	c.maybeNotifyAuthRequired(errors.New("connection refused")) // not auth-required: must not notify
	if len(notified) != 1 {
		t.Errorf("non-auth error must not notify, got %v", notified)
	}

	// Rearm: this is exactly the field reset Connect performs on success.
	c.authNotified = false
	c.maybeNotifyAuthRequired(mcpauth.ErrNoRefreshToken)
	if len(notified) != 2 || notified[1] != "s" {
		t.Fatalf("notified after rearm = %v, want a second call with %q", notified, "s")
	}
}
