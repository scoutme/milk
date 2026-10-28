package eval

import (
	"strings"
	"sync"
)

// ModelPricing is one model's token prices in US dollars per 1,000,000
// tokens. Zero means "free / unpriced" for that dimension: a model whose
// price is genuinely 0 (or unpublished — see CacheCreate on the built-ins)
// contributes nothing for those tokens instead of being guessed at.
type ModelPricing struct {
	Input       float64 `json:"input" yaml:"input"`
	Output      float64 `json:"output" yaml:"output"`
	CacheRead   float64 `json:"cache_read" yaml:"cache_read"`
	CacheCreate float64 `json:"cache_create" yaml:"cache_create"`
}

// builtinPricing seeds the pricing table with prices for the models this
// project benchmarks. Values are $/1M tokens from the models.dev catalog
// entry for provider "xiaomi" (the same catalog mimocode itself caches at
// ~/.cache/mimocode/models.json), fetched 2026-10-28.
//
// Cross-check: least-squares over mimo-code's own per-run cost reports in
// eval results (24 runs, in/out/cache-read) independently reproduced
// 0.4350 / 0.8703 / 0.0036 for mimo-v2.6-pro — matching the catalog to
// within rounding, so costs computed here and mimo's self-reported costs
// are directly comparable.
//
// CacheCreate is 0 for these models: their OpenAI-compatible endpoints
// never report cache-write tokens (observed across every recorded run, all
// zero), and the catalog publishes no write price for them. Override via
// an experiment's `pricing:` block if your path writes caches.
//
// Prices go stale — the table is a convenience, not a source of truth.
// `pricing:` in an experiment file overrides any entry; an unknown model
// always costs 0 rather than a fabricated number (see ComputeCost).
var builtinPricing = map[string]ModelPricing{
	"mimo-v2.6-flash":          {Input: 0.14, Output: 0.28, CacheRead: 0.0028},
	"mimo-v2.6-pro":            {Input: 0.435, Output: 0.87, CacheRead: 0.0036},
	"mimo-v2.6-pro-ultraspeed": {Input: 4.35, Output: 8.7, CacheRead: 0.036},
}

var (
	pricingMu    sync.RWMutex
	pricingTable = map[string]ModelPricing{}
)

func init() {
	for k, v := range builtinPricing {
		pricingTable[k] = v
	}
}

// SetPricing merges entries into the pricing table (experiment `pricing:`
// overrides win per model). Call it once before runs start; adapters price
// their results as they finish.
func SetPricing(entries map[string]ModelPricing) {
	if len(entries) == 0 {
		return
	}
	pricingMu.Lock()
	defer pricingMu.Unlock()
	for k, v := range entries {
		pricingTable[k] = v
	}
}

// ResetPricing restores the built-in table (test helper).
func ResetPricing() {
	pricingMu.Lock()
	defer pricingMu.Unlock()
	pricingTable = map[string]ModelPricing{}
	for k, v := range builtinPricing {
		pricingTable[k] = v
	}
}

// LookupPricing resolves a model's prices. Matching: exact key first, then
// a path-boundary match so qualified names resolve ("xiaomi/mimo-v2.6-pro"
// → "mimo-v2.6-pro") without false hits on longer names ("mimo-v2.6-pro-
// ultraspeed" does NOT match "mimo-v2.6-pro" — the boundary is "/" only).
// On ambiguity the longest key wins.
func LookupPricing(model string) (ModelPricing, bool) {
	if model == "" {
		return ModelPricing{}, false
	}
	pricingMu.RLock()
	defer pricingMu.RUnlock()
	if p, ok := pricingTable[model]; ok {
		return p, true
	}
	lower := strings.ToLower(model)
	best := ""
	for k := range pricingTable {
		kl := strings.ToLower(k)
		if lower == kl || strings.HasSuffix(lower, "/"+kl) || strings.HasPrefix(lower, kl+"/") {
			if len(k) > len(best) {
				best = k
			}
		}
	}
	if best != "" {
		return pricingTable[best], true
	}
	return ModelPricing{}, false
}

// ComputeCost prices one turn's token usage. Returns 0 for an unknown model
// — a missing price is never presented as a real dollar figure, and reports
// read $0.000 as "unpriced," same as before pricing existed.
func ComputeCost(model string, t TokenUsage) float64 {
	p, ok := LookupPricing(model)
	if !ok {
		return 0
	}
	return (float64(t.InputTokens)*p.Input +
		float64(t.OutputTokens)*p.Output +
		float64(t.CacheRead)*p.CacheRead +
		float64(t.CacheCreate)*p.CacheCreate) / 1e6
}
