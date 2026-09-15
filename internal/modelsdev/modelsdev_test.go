package modelsdev

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// resetState clears package-level state between tests and restores the real
// embedded snapshot afterward, since embedded/cache are package-level
// singletons — tests that swap them out must not leak into each other or
// into whatever else runs in this process.
func resetState(t *testing.T) {
	t.Helper()
	origEmbedded := embedded
	mu.Lock()
	cache = nil
	mu.Unlock()
	t.Cleanup(func() {
		embedded = origEmbedded
		mu.Lock()
		cache = nil
		mu.Unlock()
	})
}

func TestLookup_MatchesCaseInsensitiveAcrossProviders(t *testing.T) {
	resetState(t)
	embedded = Catalog{} // isolate from the real embedded snapshot
	mu.Lock()
	cache = Catalog{
		"anthropic": Provider{Models: map[string]Model{
			"test-fake-model": {Limit: Limit{Context: 200000, Output: 8192}},
		}},
	}
	mu.Unlock()

	got, ok := Lookup("Test-Fake-Model")
	if !ok || got != 200000 {
		t.Fatalf("Lookup() = %d, %v; want 200000, true", got, ok)
	}

	if _, ok := Lookup("does-not-exist"); ok {
		t.Fatal("Lookup() ok=true for an unknown model, want false")
	}
}

func TestLookup_EmptyOrZeroContextNeverMatches(t *testing.T) {
	resetState(t)
	embedded = Catalog{}
	mu.Lock()
	cache = Catalog{
		"local": Provider{Models: map[string]Model{
			"qwythos": {Limit: Limit{Context: 0}},
		}},
	}
	mu.Unlock()

	if _, ok := Lookup(""); ok {
		t.Fatal(`Lookup("") ok=true, want false`)
	}
	if _, ok := Lookup("qwythos"); ok {
		t.Fatal("Lookup() ok=true for a zero-context entry, want false")
	}
}

func TestLookup_EmbeddedSnapshotTakesPriorityOverLiveCache(t *testing.T) {
	resetState(t)
	embedded = Catalog{
		"anthropic": Provider{Models: map[string]Model{
			"shared-model": {Limit: Limit{Context: 111111}},
		}},
	}
	mu.Lock()
	cache = Catalog{
		"anthropic": Provider{Models: map[string]Model{
			"shared-model": {Limit: Limit{Context: 999999}},
		}},
	}
	mu.Unlock()

	got, ok := Lookup("shared-model")
	if !ok || got != 111111 {
		t.Fatalf("Lookup() = %d, %v; want the embedded snapshot's value 111111, true", got, ok)
	}
}

func TestLookup_FallsBackToLiveCacheOnEmbeddedMiss(t *testing.T) {
	resetState(t)
	embedded = Catalog{}
	mu.Lock()
	cache = Catalog{
		"local": Provider{Models: map[string]Model{
			"live-only-model": {Limit: Limit{Context: 32768}},
		}},
	}
	mu.Unlock()

	got, ok := Lookup("live-only-model")
	if !ok || got != 32768 {
		t.Fatalf("Lookup() = %d, %v; want 32768, true from the live-cache fallback", got, ok)
	}
}

// TestEmbeddedSnapshot_ParsesAndContainsModels guards against
// internal/modelsdev/snapshot.json going stale, corrupt, or empty. It
// deliberately doesn't assert specific context values — those legitimately
// change every time scripts/update-models-dev-snapshot.sh is re-run — just
// that the embedded baseline actually parses into something usable.
func TestEmbeddedSnapshot_ParsesAndContainsModels(t *testing.T) {
	if len(embedded) == 0 {
		t.Fatal("embedded snapshot parsed to zero providers — check internal/modelsdev/snapshot.json is valid and non-empty")
	}
	total := 0
	for _, p := range embedded {
		total += len(p.Models)
	}
	if total == 0 {
		t.Fatal("embedded snapshot has zero models across all providers")
	}
}

func TestEnsureLoaded_FetchesAndCachesToDisk(t *testing.T) {
	resetState(t)
	embedded = Catalog{} // isolate from the real snapshot possibly also having "test-model"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"testprov":{"models":{"test-model":{"limit":{"context":32768,"output":4096}}}}}`))
	}))
	defer srv.Close()

	origURL := apiURL
	setAPIURLForTest(srv.URL)
	defer setAPIURLForTest(origURL)

	dir := t.TempDir()
	cachePath := filepath.Join(dir, "models_dev.json")

	EnsureLoaded(cachePath)

	// refresh runs in a goroutine; poll briefly instead of a fixed sleep.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := Lookup("test-model"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for background refresh to populate the catalog")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if _, err := os.Stat(cachePath); err != nil {
		t.Errorf("want cache file written to disk, got: %v", err)
	}

	window, ok := Lookup("test-model")
	if !ok || window != 32768 {
		t.Fatalf("Lookup(\"test-model\") = %d, %v; want 32768, true", window, ok)
	}
}

var apiURLMu sync.Mutex

func setAPIURLForTest(url string) {
	apiURLMu.Lock()
	defer apiURLMu.Unlock()
	apiURL = url
}
