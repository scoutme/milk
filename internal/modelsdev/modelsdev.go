// Package modelsdev fetches and caches the models.dev model catalog
// (https://models.dev/api.json), used as a best-effort fallback to auto-fill
// an agent's context window when context_window_tokens isn't explicitly
// configured. A model's context window essentially never changes once
// published, so a snapshot of the catalog (snapshot.json) is embedded at
// build time and consulted first — Lookup works fully offline, with zero
// startup latency, for every model that existed when the snapshot was last
// regenerated (see scripts/update-models-dev-snapshot.sh). The network
// fetch/disk cache below exists purely as a fallback for a miss against the
// embedded snapshot (a model released after the snapshot was cut); it never
// blocks a caller.
package modelsdev

import (
	"context"
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	cacheTTL     = 24 * time.Hour
	fetchTimeout = 10 * time.Second
)

// apiURL is a var (not const) so tests can point it at an httptest server.
var apiURL = "https://models.dev/api.json"

// Limit mirrors models.dev's per-model token limits.
type Limit struct {
	Context int `json:"context"`
	Output  int `json:"output"`
}

// Model is one entry in a provider's model map, keyed by model ID.
type Model struct {
	Limit Limit `json:"limit"`
}

// Provider is one entry in the top-level catalog, keyed by provider ID.
type Provider struct {
	Models map[string]Model `json:"models"`
}

// Catalog is the full models.dev response: provider ID -> Provider.
type Catalog map[string]Provider

//go:embed snapshot.json
var snapshotJSON []byte

// embedded is the build-time catalog baseline, parsed once at init. Never
// mutated after init — safe to read without locking.
var embedded Catalog

func init() {
	if err := json.Unmarshal(snapshotJSON, &embedded); err != nil {
		embedded = Catalog{}
	}
}

var (
	mu    sync.RWMutex
	cache Catalog
)

// Lookup returns the context window (tokens) for modelID: first against the
// embedded build-time snapshot, then against the live network-fetched
// catalog (if loaded). milk stores agent model names as bare strings (no
// provider prefix), so matching is case-insensitive against every
// provider's model IDs rather than a single namespaced key.
func Lookup(modelID string) (int, bool) {
	if modelID == "" {
		return 0, false
	}
	want := strings.ToLower(modelID)
	if window, ok := lookupIn(embedded, want); ok {
		return window, true
	}
	mu.RLock()
	defer mu.RUnlock()
	return lookupIn(cache, want)
}

func lookupIn(c Catalog, want string) (int, bool) {
	for _, p := range c {
		for id, m := range p.Models {
			if strings.ToLower(id) == want && m.Limit.Context > 0 {
				return m.Limit.Context, true
			}
		}
	}
	return 0, false
}

// EnsureLoaded loads the on-disk cache at cachePath synchronously (cheap —
// local file read) so Lookup has data as soon as possible, then refreshes
// from the network in the background if the cache is missing or older than
// cacheTTL. Safe to call multiple times. Never blocks on network I/O.
func EnsureLoaded(cachePath string) {
	fresh := loadFromDisk(cachePath)
	if fresh {
		if fi, err := os.Stat(cachePath); err == nil && time.Since(fi.ModTime()) < cacheTTL {
			return
		}
	}
	go refresh(cachePath)
}

func loadFromDisk(path string) bool {
	if path == "" {
		return false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var c Catalog
	if err := json.Unmarshal(b, &c); err != nil {
		return false
	}
	mu.Lock()
	cache = c
	mu.Unlock()
	return true
}

func refresh(cachePath string) {
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return
	}
	var c Catalog
	if err := json.Unmarshal(body, &c); err != nil {
		return
	}

	mu.Lock()
	cache = c
	mu.Unlock()

	if cachePath == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(cachePath), 0o700)
	_ = os.WriteFile(cachePath, body, 0o600)
}
