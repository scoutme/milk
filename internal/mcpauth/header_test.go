package mcpauth

import (
	"context"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/config"
)

func TestResolveHeader_Bearer(t *testing.T) {
	cfg := config.MCPServerConfig{Name: "s", Auth: "bearer", APIKey: "secret"}
	got, err := ResolveHeader(context.Background(), cfg)
	if err != nil {
		t.Fatalf("ResolveHeader: %v", err)
	}
	if got != "Bearer secret" {
		t.Errorf("got %q, want %q", got, "Bearer secret")
	}
}

func TestResolveHeader_Bearer_NoAPIKey(t *testing.T) {
	cfg := config.MCPServerConfig{Name: "s", Auth: "bearer"}
	got, err := ResolveHeader(context.Background(), cfg)
	if err != nil {
		t.Fatalf("ResolveHeader: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestResolveHeader_TokenCmd(t *testing.T) {
	cfg := config.MCPServerConfig{Name: "s", Auth: "token_cmd", TokenCmd: "echo mytoken"}
	got, err := ResolveHeader(context.Background(), cfg)
	if err != nil {
		t.Fatalf("ResolveHeader: %v", err)
	}
	if got != "Bearer mytoken" {
		t.Errorf("got %q, want %q", got, "Bearer mytoken")
	}
}

func TestResolveHeader_TokenCmd_Failure(t *testing.T) {
	cfg := config.MCPServerConfig{Name: "s", Auth: "token_cmd", TokenCmd: "exit 1"}
	if _, err := ResolveHeader(context.Background(), cfg); err == nil {
		t.Error("expected error from failing token_cmd, got nil")
	}
}

func TestResolveHeader_TokenCmd_Empty(t *testing.T) {
	cfg := config.MCPServerConfig{Name: "s", Auth: "token_cmd"}
	got, err := ResolveHeader(context.Background(), cfg)
	if err != nil {
		t.Fatalf("ResolveHeader: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestResolveHeader_OAuth_Authorized(t *testing.T) {
	withTempMilkHome(t)
	if err := SaveToken("s", &TokenSet{
		AccessToken: "at123",
		ExpiresAt:   time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}
	cfg := config.MCPServerConfig{Name: "s", Auth: "oauth"}
	got, err := ResolveHeader(context.Background(), cfg)
	if err != nil {
		t.Fatalf("ResolveHeader: %v", err)
	}
	if got != "Bearer at123" {
		t.Errorf("got %q, want %q", got, "Bearer at123")
	}
}

func TestResolveHeader_OAuth_NotAuthorized(t *testing.T) {
	withTempMilkHome(t)
	cfg := config.MCPServerConfig{Name: "s", Auth: "oauth"}
	got, err := ResolveHeader(context.Background(), cfg)
	if err != nil {
		t.Fatalf("ResolveHeader: expected best-effort nil error, got %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty (not yet authorized)", got)
	}
}

// TestResolveHeader_OAuth_StaleTokenUsedWhenRefreshFails pins #161's change:
// EnsureFresh returns a non-nil stale token alongside ErrNoRefreshToken when
// a token exists but has expired with no refresh token to renew it — the
// stale access token is still returned ("might still validate beats none"),
// not dropped to an unauthenticated request. The reactive-refresh-on-401
// path (internal/mcp.Client.doHTTP) picks up if the target does reject it.
func TestResolveHeader_OAuth_StaleTokenUsedWhenRefreshFails(t *testing.T) {
	withTempMilkHome(t)
	if err := SaveToken("s", &TokenSet{
		AccessToken: "stale-at",
		ExpiresAt:   time.Now().Add(-time.Hour), // already expired
		// RefreshToken deliberately empty: EnsureFresh can't renew it.
	}); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}
	cfg := config.MCPServerConfig{Name: "s", Auth: "oauth"}
	got, err := ResolveHeader(context.Background(), cfg)
	if err != nil {
		t.Fatalf("ResolveHeader: expected best-effort nil error, got %v", err)
	}
	if got != "Bearer stale-at" {
		t.Errorf("got %q, want the stale token still used: %q", got, "Bearer stale-at")
	}
}

func TestResolveHeader_NoneOrUnknown(t *testing.T) {
	for _, auth := range []string{"", "none", "bogus"} {
		cfg := config.MCPServerConfig{Name: "s", Auth: auth}
		got, err := ResolveHeader(context.Background(), cfg)
		if err != nil {
			t.Fatalf("ResolveHeader(auth=%q): %v", auth, err)
		}
		if got != "" {
			t.Errorf("ResolveHeader(auth=%q) = %q, want empty", auth, got)
		}
	}
}
