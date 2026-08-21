package mcpauth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/config"
)

// newFakeAuthServer returns a fully faked authorization server: metadata,
// dynamic client registration, and token exchange. tokenHandler lets each
// test customize the /token response (or leave it unreachable, for the
// timeout test).
func newFakeAuthServer(t *testing.T, tokenHandler http.HandlerFunc) *httptest.Server {
	t.Helper()
	var as *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"issuer": %q, "authorization_endpoint": %q, "token_endpoint": %q, "registration_endpoint": %q}`,
			as.URL, as.URL+"/authorize", as.URL+"/token", as.URL+"/register")
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"client_id": "dyn-client-1"}`))
	})
	if tokenHandler != nil {
		mux.HandleFunc("/token", tokenHandler)
	}
	as = httptest.NewServer(mux)
	return as
}

func newFakeMCPServer(t *testing.T, as *httptest.Server) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"resource": "mcp", "authorization_servers": [%q]}`, as.URL)
	})
	return httptest.NewServer(mux)
}

// fakeBrowserOpener simulates the browser: instead of visiting authURL and
// clicking through a consent screen, it parses the redirect_uri/state milk
// already embedded in authURL and hits the loopback callback directly with a
// fixed authorization code — exactly what a real browser would eventually
// deliver, without needing a real consent UI.
func fakeBrowserOpener(code string) func(authURL string) error {
	return func(authURL string) error {
		u, err := url.Parse(authURL)
		if err != nil {
			return err
		}
		q := u.Query()
		cb, err := url.Parse(q.Get("redirect_uri"))
		if err != nil {
			return err
		}
		cbq := cb.Query()
		cbq.Set("code", code)
		cbq.Set("state", q.Get("state"))
		cb.RawQuery = cbq.Encode()
		go func() { _, _ = http.Get(cb.String()) }() //nolint:errcheck
		return nil
	}
}

func TestAuthorize_FullFlow(t *testing.T) {
	withTempMilkHome(t)

	as := newFakeAuthServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token": "final-at", "refresh_token": "final-rt", "expires_in": 3600}`))
	})
	defer as.Close()
	mcpSrv := newFakeMCPServer(t, as)
	defer mcpSrv.Close()

	var gotURL string
	err := Authorize(context.Background(), config.MCPServerConfig{
		Name: "test-server",
		URL:  mcpSrv.URL + "/mcp",
	}, Options{
		OnAuthURL: func(u string) { gotURL = u },
		Opener:    fakeBrowserOpener("test-code"),
		Timeout:   5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if gotURL == "" {
		t.Error("OnAuthURL was never called")
	}

	ts, err := LoadToken("test-server")
	if err != nil {
		t.Fatalf("LoadToken: %v", err)
	}
	if ts == nil || ts.AccessToken != "final-at" || ts.RefreshToken != "final-rt" {
		t.Fatalf("got %+v", ts)
	}
	if ts.ClientID != "dyn-client-1" {
		t.Errorf("ClientID = %q, want the dynamically registered id", ts.ClientID)
	}
	if ts.TokenEndpoint == "" {
		t.Error("TokenEndpoint was not cached in the saved TokenSet")
	}
}

func TestAuthorize_StateMismatchRejected(t *testing.T) {
	withTempMilkHome(t)

	as := newFakeAuthServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("token endpoint should never be hit when state validation fails")
	})
	defer as.Close()
	mcpSrv := newFakeMCPServer(t, as)
	defer mcpSrv.Close()

	// Opener hits the callback with a deliberately wrong state.
	opener := func(authURL string) error {
		u, _ := url.Parse(authURL)
		cb, _ := url.Parse(u.Query().Get("redirect_uri"))
		cbq := cb.Query()
		cbq.Set("code", "test-code")
		cbq.Set("state", "wrong-state")
		cb.RawQuery = cbq.Encode()
		go func() { _, _ = http.Get(cb.String()) }() //nolint:errcheck
		return nil
	}

	err := Authorize(context.Background(), config.MCPServerConfig{
		Name: "test-server",
		URL:  mcpSrv.URL + "/mcp",
	}, Options{Opener: opener, Timeout: 5 * time.Second})
	if err == nil {
		t.Fatal("expected state-mismatch error")
	}
}

func TestAuthorize_TimesOut(t *testing.T) {
	withTempMilkHome(t)

	as := newFakeAuthServer(t, nil) // token endpoint unreachable — never called
	defer as.Close()
	mcpSrv := newFakeMCPServer(t, as)
	defer mcpSrv.Close()

	err := Authorize(context.Background(), config.MCPServerConfig{
		Name: "test-server",
		URL:  mcpSrv.URL + "/mcp",
	}, Options{
		Opener:  func(string) error { return nil }, // never actually calls back
		Timeout: 200 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected a timeout error")
	}
}

func TestAuthorize_ManualClientIDSkipsRegistration(t *testing.T) {
	withTempMilkHome(t)

	as := newFakeAuthServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token": "at", "expires_in": 3600}`))
	})
	defer as.Close()
	mcpSrv := newFakeMCPServer(t, as)
	defer mcpSrv.Close()

	err := Authorize(context.Background(), config.MCPServerConfig{
		Name:     "test-server",
		URL:      mcpSrv.URL + "/mcp",
		ClientID: "preconfigured-client",
	}, Options{
		Opener:  fakeBrowserOpener("code"),
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	ts, err := LoadToken("test-server")
	if err != nil {
		t.Fatalf("LoadToken: %v", err)
	}
	if ts.ClientID != "preconfigured-client" {
		t.Errorf("ClientID = %q, want the configured override preserved", ts.ClientID)
	}
}
