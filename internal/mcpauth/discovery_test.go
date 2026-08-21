package mcpauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDiscoverAuthServer_ViaProtectedResource(t *testing.T) {
	as := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/oauth-authorization-server" {
			t.Errorf("unexpected AS request path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"issuer": "https://as.example.com",
			"authorization_endpoint": "https://as.example.com/authorize",
			"token_endpoint": "https://as.example.com/token",
			"registration_endpoint": "https://as.example.com/register"
		}`))
	}))
	defer as.Close()

	mcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/oauth-protected-resource" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"resource": "https://mcp.example.com", "authorization_servers": ["` + as.URL + `"]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mcpSrv.Close()

	meta, err := DiscoverAuthServer(context.Background(), mcpSrv.URL+"/mcp")
	if err != nil {
		t.Fatalf("DiscoverAuthServer: %v", err)
	}
	if meta.AuthorizationEndpoint != "https://as.example.com/authorize" {
		t.Errorf("AuthorizationEndpoint = %q", meta.AuthorizationEndpoint)
	}
	if meta.TokenEndpoint != "https://as.example.com/token" {
		t.Errorf("TokenEndpoint = %q", meta.TokenEndpoint)
	}
	if meta.RegistrationEndpoint != "https://as.example.com/register" {
		t.Errorf("RegistrationEndpoint = %q", meta.RegistrationEndpoint)
	}
}

func TestDiscoverAuthServer_FallbackToOwnOrigin(t *testing.T) {
	// No protected-resource document at all — the MCP server IS the AS.
	mcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-protected-resource":
			w.WriteHeader(http.StatusNotFound)
		case "/.well-known/oauth-authorization-server":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{
				"issuer": "self",
				"authorization_endpoint": "/authorize",
				"token_endpoint": "/token"
			}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mcpSrv.Close()

	meta, err := DiscoverAuthServer(context.Background(), mcpSrv.URL+"/mcp")
	if err != nil {
		t.Fatalf("DiscoverAuthServer: %v", err)
	}
	if meta.TokenEndpoint != "/token" {
		t.Errorf("TokenEndpoint = %q", meta.TokenEndpoint)
	}
}

func TestDiscoverAuthServer_NoneAvailable(t *testing.T) {
	mcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mcpSrv.Close()

	if _, err := DiscoverAuthServer(context.Background(), mcpSrv.URL+"/mcp"); err == nil {
		t.Error("expected an error when no metadata is available anywhere")
	}
}
