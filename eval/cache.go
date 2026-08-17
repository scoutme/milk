package eval

import "fmt"

// ComputeCacheMetrics derives caching efficiency metrics from a single turn's
// token usage. All rates are computed against the total input budget
// (CacheRead + CacheCreate + InputTokens). Division by zero returns zero rates.
func ComputeCacheMetrics(tokens TokenUsage) CacheMetrics {
	total := float64(tokens.CacheRead + tokens.CacheCreate + tokens.InputTokens)
	if total == 0 {
		return CacheMetrics{}
	}
	return CacheMetrics{
		CacheHitRate:    float64(tokens.CacheRead) / total,
		CacheCreateRate: float64(tokens.CacheCreate) / total,
		CachedTokens:    tokens.CacheRead + tokens.CacheCreate,
	}
}

// AggregateCacheMetrics sums TokenUsage across every result and computes
// cache metrics on the aggregate totals.
func AggregateCacheMetrics(results []RunResult) CacheMetrics {
	var total TokenUsage
	for _, r := range results {
		total.InputTokens += r.Tokens.InputTokens
		total.OutputTokens += r.Tokens.OutputTokens
		total.CacheCreate += r.Tokens.CacheCreate
		total.CacheRead += r.Tokens.CacheRead
	}
	return ComputeCacheMetrics(total)
}

// PerTurnCacheMetrics returns a CacheMetrics slice with one entry per result.
func PerTurnCacheMetrics(results []RunResult) []CacheMetrics {
	out := make([]CacheMetrics, len(results))
	for i, r := range results {
		out[i] = ComputeCacheMetrics(r.Tokens)
	}
	return out
}

// CostSavings estimates the dollar amount saved by cache reads compared to
// processing those tokens at the uncached price. Only CacheRead tokens
// contribute to savings (cache-creation tokens were paid at full price).
func CostSavings(metrics CacheMetrics, uncachedPricePerToken, cachedPricePerToken float64) float64 {
	return float64(metrics.CachedTokens) * (uncachedPricePerToken - cachedPricePerToken)
}

// AssertCacheExpectations validates multi-turn cache assertions defined in a
// CacheExpect block. Turn indices are 0-based internally; "after turn 1" means
// results[1:] (turn index >= 1).
//
// Checks:
//   - min_cache_hit_rate_after_turn1: aggregated hit rate of turns 2+ >= threshold
//   - no_cache_recreate_after_turn1: turns 2+ must have zero CacheCreate tokens
func AssertCacheExpectations(results []RunResult, expect CacheExpect) error {
	if len(results) < 2 {
		return nil // nothing to check when there is at most one turn
	}

	afterTurn1 := results[1:]

	// Check minimum cache hit rate for turns 2+.
	if expect.MinCacheHitRateAfterTurn1 > 0 {
		agg := AggregateCacheMetrics(afterTurn1)
		if agg.CacheHitRate < expect.MinCacheHitRateAfterTurn1 {
			return fmt.Errorf(
				"cache hit rate after turn 1 is %.4f, want >= %.4f",
				agg.CacheHitRate,
				expect.MinCacheHitRateAfterTurn1,
			)
		}
	}

	// Check no cache re-creation for turns 2+.
	if expect.NoCacheRecreateAfterTurn1 {
		for i, r := range afterTurn1 {
			if r.Tokens.CacheCreate > 0 {
				return fmt.Errorf(
					"turn %d (index %d) has %d cache_create tokens, want 0",
					i+2, i+1, r.Tokens.CacheCreate,
				)
			}
		}
	}

	return nil
}
