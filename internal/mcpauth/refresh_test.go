package mcpauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestEnsureFresh_NotAuthorized(t *testing.T) {
	withTempMilkHome(t)
	if _, err := EnsureFresh(context.Background(), "never-authed"); !errors.Is(err, ErrNotAuthorized) {
		t.Errorf("EnsureFresh err = %v, want ErrNotAuthorized", err)
	}
}

func TestEnsureFresh_StillFresh(t *testing.T) {
	withTempMilkHome(t)
	want := &TokenSet{AccessToken: "at", ExpiresAt: time.Now().Add(time.Hour)}
	if err := SaveToken("srv", want); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}
	got, err := EnsureFresh(context.Background(), "srv")
	if err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	if got.AccessToken != "at" {
		t.Errorf("AccessToken = %q, want unchanged %q", got.AccessToken, "at")
	}
}

func TestEnsureFresh_RefreshesNearExpiry(t *testing.T) {
	withTempMilkHome(t)
	var refreshCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token": "at-new", "refresh_token": "rt-new", "expires_in": 3600}`))
	}))
	defer srv.Close()

	if err := SaveToken("srv", &TokenSet{
		AccessToken:   "at-old",
		RefreshToken:  "rt-old",
		ExpiresAt:     time.Now().Add(10 * time.Second), // within refreshSkew
		TokenEndpoint: srv.URL,
		ClientID:      "client1",
	}); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}

	got, err := EnsureFresh(context.Background(), "srv")
	if err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	if got.AccessToken != "at-new" {
		t.Errorf("AccessToken = %q, want refreshed value", got.AccessToken)
	}
	if refreshCalls != 1 {
		t.Errorf("refreshCalls = %d, want 1", refreshCalls)
	}

	// Persisted to disk too.
	persisted, err := LoadToken("srv")
	if err != nil {
		t.Fatalf("LoadToken: %v", err)
	}
	if persisted.AccessToken != "at-new" {
		t.Errorf("persisted AccessToken = %q", persisted.AccessToken)
	}
	// Discovery/client fields carried over from the pre-refresh record.
	if persisted.TokenEndpoint != srv.URL || persisted.ClientID != "client1" {
		t.Errorf("persisted lost cached discovery/client fields: %+v", persisted)
	}
}

func TestEnsureFresh_ExpiredNoRefreshToken(t *testing.T) {
	withTempMilkHome(t)
	if err := SaveToken("srv", &TokenSet{
		AccessToken: "at-old",
		ExpiresAt:   time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}
	ts, err := EnsureFresh(context.Background(), "srv")
	if !errors.Is(err, ErrNoRefreshToken) {
		t.Errorf("err = %v, want ErrNoRefreshToken", err)
	}
	if ts == nil || ts.AccessToken != "at-old" {
		t.Errorf("expected stale token returned alongside the error, got %+v", ts)
	}
}

func TestRefreshOnly_PreservesUnrotatedRefreshToken(t *testing.T) {
	withTempMilkHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token": "at-new"}`)) // no refresh_token in response
	}))
	defer srv.Close()

	if err := SaveToken("srv", &TokenSet{
		AccessToken:   "at-old",
		RefreshToken:  "rt-keep",
		TokenEndpoint: srv.URL,
		ClientID:      "client1",
	}); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}

	got, err := RefreshOnly(context.Background(), "srv")
	if err != nil {
		t.Fatalf("RefreshOnly: %v", err)
	}
	if got.RefreshToken != "rt-keep" {
		t.Errorf("RefreshToken = %q, want preserved %q", got.RefreshToken, "rt-keep")
	}
}

func TestRefreshOnly_NoRefreshToken(t *testing.T) {
	withTempMilkHome(t)
	if err := SaveToken("srv", &TokenSet{AccessToken: "at"}); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}
	if _, err := RefreshOnly(context.Background(), "srv"); !errors.Is(err, ErrNoRefreshToken) {
		t.Errorf("err = %v, want ErrNoRefreshToken", err)
	}
}

func TestRefreshOnly_StaleRecordMissingEndpoint(t *testing.T) {
	withTempMilkHome(t)
	if err := SaveToken("srv", &TokenSet{AccessToken: "at", RefreshToken: "rt"}); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}
	if _, err := RefreshOnly(context.Background(), "srv"); !errors.Is(err, ErrNotAuthorized) {
		t.Errorf("err = %v, want ErrNotAuthorized for a record with no cached token_endpoint/client_id", err)
	}
}
