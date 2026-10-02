package mcpauth

import (
	"context"
	"os/exec"
	"strings"

	"github.com/scoutme/milk/internal/config"
)

// ResolveHeader returns the Authorization header value for cfg's configured
// auth method ("bearer", "token_cmd", or "oauth"), or "" if none applies.
// This is the single auth-resolution path shared by every agent/provider that
// builds MCP request headers — internal/mcp.Client (local/primary agent) and
// claude-cli's --mcp-config generation (escalation agent) both call it, so a
// server only needs to be authorized once regardless of which agent uses it.
//
// "token_cmd" runs cfg.TokenCmd fresh on every call; callers that hold a
// long-lived connection (like mcp.Client) may want to cache the result
// themselves rather than re-exec per request.
//
// "oauth" is best-effort: an unauthorized or unrefreshable server returns
// ("", nil) rather than an error, so callers can send the request
// unauthenticated and let the target reject it, rather than failing the
// whole turn over a stale/missing token. When a token exists but renewal
// failed (expired refresh token, token endpoint down), the stale token is
// still returned — a token that might still validate beats none — with the
// reactive-refresh-on-401 path picking up if the target does reject it.
func ResolveHeader(ctx context.Context, cfg config.MCPServerConfig) (string, error) {
	switch strings.ToLower(cfg.Auth) {
	case "bearer":
		if cfg.APIKey == "" {
			return "", nil
		}
		return "Bearer " + cfg.APIKey, nil
	case "token_cmd":
		if cfg.TokenCmd == "" {
			return "", nil
		}
		out, err := exec.Command("sh", "-c", cfg.TokenCmd).Output() //nolint:gosec
		if err != nil {
			return "", err
		}
		return "Bearer " + strings.TrimSpace(string(out)), nil
	case "oauth":
		// A non-nil err here (refresh failed) with a non-nil token means the
		// renewal failed but a (possibly stale) token is still on record: use
		// it rather than dropping to an unauthenticated request.
		tok, _ := EnsureFresh(ctx, cfg.Name)
		if tok == nil {
			return "", nil
		}
		return "Bearer " + tok.AccessToken, nil
	default:
		return "", nil
	}
}
