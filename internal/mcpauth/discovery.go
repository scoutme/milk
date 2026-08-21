package mcpauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ProtectedResourceMetadata is the RFC 9728 metadata document served by an
// MCP server at /.well-known/oauth-protected-resource.
type ProtectedResourceMetadata struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
}

// AuthServerMetadata is the RFC 8414 metadata document served by an
// authorization server at /.well-known/oauth-authorization-server.
type AuthServerMetadata struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	RegistrationEndpoint  string `json:"registration_endpoint,omitempty"`
}

// originOf returns scheme://host[:port] for rawURL.
func originOf(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse url: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("url %q missing scheme/host", rawURL)
	}
	return u.Scheme + "://" + u.Host, nil
}

func fetchJSON(ctx context.Context, url string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d fetching %s", resp.StatusCode, url)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, dst)
}

// fetchProtectedResourceMetadata fetches RFC 9728 metadata from the MCP
// server's origin.
func fetchProtectedResourceMetadata(ctx context.Context, mcpServerURL string) (*ProtectedResourceMetadata, error) {
	origin, err := originOf(mcpServerURL)
	if err != nil {
		return nil, err
	}
	var meta ProtectedResourceMetadata
	if err := fetchJSON(ctx, origin+"/.well-known/oauth-protected-resource", &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

// fetchAuthServerMetadata fetches RFC 8414 metadata from issuerBase.
func fetchAuthServerMetadata(ctx context.Context, issuerBase string) (*AuthServerMetadata, error) {
	var meta AuthServerMetadata
	if err := fetchJSON(ctx, strings.TrimRight(issuerBase, "/")+"/.well-known/oauth-authorization-server", &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

// DiscoverAuthServer resolves the authorization server for an MCP server URL:
// it first tries RFC 9728 protected-resource metadata to find the declared
// authorization server(s), then fetches RFC 8414 metadata from the first one.
// If the protected-resource document is unavailable, it falls back to
// fetching authorization-server metadata directly from the MCP server's own
// origin (some servers skip the indirection and are the authorization server
// themselves).
func DiscoverAuthServer(ctx context.Context, mcpServerURL string) (*AuthServerMetadata, error) {
	if prm, err := fetchProtectedResourceMetadata(ctx, mcpServerURL); err == nil && len(prm.AuthorizationServers) > 0 {
		asMeta, err := fetchAuthServerMetadata(ctx, prm.AuthorizationServers[0])
		if err == nil {
			return asMeta, nil
		}
	}
	origin, err := originOf(mcpServerURL)
	if err != nil {
		return nil, err
	}
	asMeta, err := fetchAuthServerMetadata(ctx, origin)
	if err != nil {
		return nil, fmt.Errorf("discover authorization server: %w", err)
	}
	return asMeta, nil
}
