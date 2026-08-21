package mcpauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestExchangeCode_Success(t *testing.T) {
	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		gotForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token": "at1", "refresh_token": "rt1", "token_type": "Bearer", "expires_in": 3600}`))
	}))
	defer srv.Close()

	ts, err := ExchangeCode(context.Background(), srv.URL, "code1", "verifier1", "client1", "", "http://127.0.0.1:9/callback")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if ts.AccessToken != "at1" || ts.RefreshToken != "rt1" {
		t.Errorf("got %+v", ts)
	}
	if ts.ExpiresAt.Before(time.Now().Add(59 * time.Minute)) {
		t.Errorf("ExpiresAt too soon: %v", ts.ExpiresAt)
	}
	if gotForm.Get("grant_type") != "authorization_code" {
		t.Errorf("grant_type = %q", gotForm.Get("grant_type"))
	}
	if gotForm.Get("code_verifier") != "verifier1" {
		t.Errorf("code_verifier = %q", gotForm.Get("code_verifier"))
	}
}

func TestExchangeCode_ErrorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error": "invalid_grant", "error_description": "code expired"}`))
	}))
	defer srv.Close()

	_, err := ExchangeCode(context.Background(), srv.URL, "bad-code", "v", "c", "", "http://127.0.0.1:9/callback")
	if err == nil {
		t.Fatal("expected error for invalid_grant")
	}
}

func TestRefreshAccessToken_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		if r.PostForm.Get("grant_type") != "refresh_token" {
			t.Errorf("grant_type = %q", r.PostForm.Get("grant_type"))
		}
		if r.PostForm.Get("refresh_token") != "old-rt" {
			t.Errorf("refresh_token = %q", r.PostForm.Get("refresh_token"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token": "at2", "token_type": "Bearer", "expires_in": 1800}`))
	}))
	defer srv.Close()

	ts, err := RefreshAccessToken(context.Background(), srv.URL, "old-rt", "client1", "")
	if err != nil {
		t.Fatalf("RefreshAccessToken: %v", err)
	}
	if ts.AccessToken != "at2" {
		t.Errorf("AccessToken = %q", ts.AccessToken)
	}
	// Some servers don't return a new refresh_token — caller preserves the old one.
	if ts.RefreshToken != "" {
		t.Errorf("expected empty RefreshToken from this fixture, got %q", ts.RefreshToken)
	}
}
