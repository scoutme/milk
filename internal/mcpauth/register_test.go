package mcpauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRegisterClient_Success(t *testing.T) {
	var gotBody dcrRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"client_id": "abc123", "client_secret": ""}`))
	}))
	defer srv.Close()

	reg, err := RegisterClient(context.Background(), srv.URL, "http://127.0.0.1:1234/callback", []string{"read", "write"})
	if err != nil {
		t.Fatalf("RegisterClient: %v", err)
	}
	if reg.ClientID != "abc123" {
		t.Errorf("ClientID = %q", reg.ClientID)
	}
	if gotBody.TokenEndpointAuthMethod != "none" {
		t.Errorf("token_endpoint_auth_method = %q, want %q (public client + PKCE)", gotBody.TokenEndpointAuthMethod, "none")
	}
	if len(gotBody.RedirectURIs) != 1 || gotBody.RedirectURIs[0] != "http://127.0.0.1:1234/callback" {
		t.Errorf("redirect_uris = %v", gotBody.RedirectURIs)
	}
	if gotBody.Scope != "read write" {
		t.Errorf("scope = %q", gotBody.Scope)
	}
}

func TestRegisterClient_ErrorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error": "invalid_client_metadata", "error_description": "redirect_uris is required"}`))
	}))
	defer srv.Close()

	_, err := RegisterClient(context.Background(), srv.URL, "http://127.0.0.1:1234/callback", nil)
	if err == nil {
		t.Fatal("expected error for 400 response")
	}
}

func TestRegisterClient_MissingClientID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	_, err := RegisterClient(context.Background(), srv.URL, "http://127.0.0.1:1234/callback", nil)
	if err == nil {
		t.Fatal("expected error when response has no client_id")
	}
}
