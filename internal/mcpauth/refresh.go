package mcpauth

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrNotAuthorized is returned when no token has been stored for a server
// (the user has never run /mcp auth for it).
var ErrNotAuthorized = errors.New("not authorized — run /mcp auth <server> first")

// ErrNoRefreshToken is returned when the stored token has expired (or is
// about to) and there is no refresh token to renew it with.
var ErrNoRefreshToken = errors.New("no refresh token available — run /mcp auth <server> to re-authorize")

// refreshSkew is how far ahead of expiry EnsureFresh proactively refreshes.
const refreshSkew = 60 * time.Second

// serverLocks serializes refreshes per server name so concurrent requests to
// the same MCP server don't race to hit the token endpoint simultaneously.
// The token store itself lives on disk (shared across processes); this lock
// only dedups within a single milk process.
var serverLocks sync.Map // map[string]*sync.Mutex

func lockFor(serverName string) *sync.Mutex {
	v, _ := serverLocks.LoadOrStore(serverName, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// EnsureFresh returns a usable token for serverName, proactively refreshing
// it if it's within refreshSkew of expiring. Returns ErrNotAuthorized if
// /mcp auth has never been run, or ErrNoRefreshToken (with the stale token,
// non-nil) if the token is expired/expiring and can't be renewed.
func EnsureFresh(ctx context.Context, serverName string) (*TokenSet, error) {
	mu := lockFor(serverName)
	mu.Lock()
	defer mu.Unlock()

	ts, err := LoadToken(serverName)
	if err != nil {
		return nil, err
	}
	if ts == nil {
		return nil, ErrNotAuthorized
	}
	if ts.ExpiresAt.IsZero() || time.Until(ts.ExpiresAt) > refreshSkew {
		return ts, nil
	}
	if ts.RefreshToken == "" {
		return ts, ErrNoRefreshToken
	}
	return doRefresh(ctx, serverName, ts)
}

// RefreshOnly forces a token refresh for serverName (used reactively after a
// 401 response), bypassing the expiry check.
func RefreshOnly(ctx context.Context, serverName string) (*TokenSet, error) {
	mu := lockFor(serverName)
	mu.Lock()
	defer mu.Unlock()

	ts, err := LoadToken(serverName)
	if err != nil {
		return nil, err
	}
	if ts == nil {
		return nil, ErrNotAuthorized
	}
	if ts.RefreshToken == "" {
		return nil, ErrNoRefreshToken
	}
	return doRefresh(ctx, serverName, ts)
}

// doRefresh performs the refresh_token grant and persists the result. Caller
// must hold the per-server lock.
func doRefresh(ctx context.Context, serverName string, ts *TokenSet) (*TokenSet, error) {
	if ts.TokenEndpoint == "" || ts.ClientID == "" {
		// Incomplete/stale record (e.g. from an older milk version) — the
		// only way forward is a fresh /mcp auth.
		return nil, ErrNotAuthorized
	}
	newTS, err := RefreshAccessToken(ctx, ts.TokenEndpoint, ts.RefreshToken, ts.ClientID, ts.ClientSecret)
	if err != nil {
		return nil, err
	}
	// Preserve discovery/client fields the refresh response doesn't repeat.
	newTS.Issuer = ts.Issuer
	newTS.AuthorizationEndpoint = ts.AuthorizationEndpoint
	newTS.TokenEndpoint = ts.TokenEndpoint
	newTS.RegistrationEndpoint = ts.RegistrationEndpoint
	newTS.ClientID = ts.ClientID
	newTS.ClientSecret = ts.ClientSecret
	if newTS.RefreshToken == "" {
		newTS.RefreshToken = ts.RefreshToken // some servers don't rotate refresh tokens
	}
	if err := SaveToken(serverName, newTS); err != nil {
		return nil, err
	}
	return newTS, nil
}
