package local

// Regression tests for background-job transport isolation. cloneForBackground
// used to share the foreground agent's *http.Client (and therefore its
// connection pool) with every spawned background job — which, combined with
// background jobs running concurrently with the foreground agent and with
// each other, turned out to trigger HTTP/2 stream resets (INTERNAL_ERROR)
// against a real provider. NewFromConfig now builds a second, independent
// transport chain (backgroundClient) that cloneForBackground uses instead.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/scoutme/milk/internal/config"
)

func TestNewFromConfig_BackgroundClientIsIndependentTransport(t *testing.T) {
	a := NewFromConfig(config.AgentConfig{Name: "n", URL: "http://example.invalid", Model: "m"})

	if a.client == nil || a.backgroundClient == nil {
		t.Fatal("expected both client and backgroundClient to be set")
	}
	if a.client == a.backgroundClient {
		t.Fatal("client and backgroundClient must not be the same *http.Client")
	}
	if a.client.Transport == a.backgroundClient.Transport {
		t.Fatal("client and backgroundClient must not share the same RoundTripper/connection pool")
	}
}

func TestNewFromConfig_BackgroundClientIsIndependentTransport_Bedrock(t *testing.T) {
	a := NewFromConfig(config.AgentConfig{
		Name: "n", URL: "http://example.invalid", Model: "m", Provider: "bedrock",
		AWSKeyID: "k", AWSSecret: "s", AWSRegion: "us-east-1",
	})

	if a.client.Transport == a.backgroundClient.Transport {
		t.Fatal("bedrock client and backgroundClient must not share the same sigv4Transport instance")
	}
	if _, ok := a.backgroundClient.Transport.(*sigv4Transport); !ok {
		t.Fatalf("expected backgroundClient.Transport to be *sigv4Transport, got %T", a.backgroundClient.Transport)
	}
}

func TestCloneForBackground_UsesBackgroundClient(t *testing.T) {
	a := NewFromConfig(config.AgentConfig{Name: "n", URL: "http://example.invalid", Model: "m"})

	clone := a.cloneForBackground()
	if clone.client != a.backgroundClient {
		t.Error("cloneForBackground must use a.backgroundClient, not a.client")
	}
	if clone.client == a.client {
		t.Error("cloneForBackground must not fall back to the foreground client when backgroundClient is set")
	}
}

func TestCloneForBackground_FallsBackToClientWithoutBackgroundClient(t *testing.T) {
	// Bare New() (used by tests and the plain local-http constructor) has no
	// backgroundClient — cloneForBackground must fall back to a.client rather
	// than leaving clone.client nil (which would panic on first use).
	a := New("http://example.invalid", "m")

	clone := a.cloneForBackground()
	if clone.client != a.client {
		t.Error("expected cloneForBackground to fall back to a.client when backgroundClient is nil")
	}
}

func TestNewFromConfig_BackgroundClientPreservesAPIKeyAuth(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
	}))
	defer srv.Close()

	a := NewFromConfig(config.AgentConfig{Name: "n", URL: srv.URL, Model: "m", APIKey: "sekret"})

	req, _ := http.NewRequest("GET", srv.URL, nil)
	if _, err := a.backgroundClient.Do(req); err != nil {
		t.Fatalf("backgroundClient.Do: %v", err)
	}
	if gotAuth != "Bearer sekret" {
		t.Errorf("expected backgroundClient to carry the api_key auth header, got %q", gotAuth)
	}
}
