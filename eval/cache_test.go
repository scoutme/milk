package eval

import (
	"math"
	"testing"
)

const epsilon = 1e-9

func closeEnough(a, b float64) bool {
	return math.Abs(a-b) < epsilon
}

// ---------------------------------------------------------------------------
// ComputeCacheMetrics
// ---------------------------------------------------------------------------

func TestComputeCacheMetrics_KnownInputs(t *testing.T) {
	tokens := TokenUsage{
		InputTokens:  1000,
		OutputTokens: 500,
		CacheCreate:  200,
		CacheRead:    3000,
	}
	m := ComputeCacheMetrics(tokens)

	// total = 3000 + 200 + 1000 = 4200
	wantHitRate := 3000.0 / 4200.0
	wantCreateRate := 200.0 / 4200.0

	if !closeEnough(m.CacheHitRate, wantHitRate) {
		t.Errorf("CacheHitRate = %f, want %f", m.CacheHitRate, wantHitRate)
	}
	if !closeEnough(m.CacheCreateRate, wantCreateRate) {
		t.Errorf("CacheCreateRate = %f, want %f", m.CacheCreateRate, wantCreateRate)
	}
	if m.CachedTokens != 3200 {
		t.Errorf("CachedTokens = %d, want 3200", m.CachedTokens)
	}
}

func TestComputeCacheMetrics_DivisionByZero(t *testing.T) {
	m := ComputeCacheMetrics(TokenUsage{})
	if m.CacheHitRate != 0 || m.CacheCreateRate != 0 || m.CachedTokens != 0 {
		t.Errorf("expected all-zero metrics for empty token usage, got %+v", m)
	}
}

func TestComputeCacheMetrics_AllCacheRead(t *testing.T) {
	m := ComputeCacheMetrics(TokenUsage{
		InputTokens: 0,
		CacheCreate: 0,
		CacheRead:   5000,
	})
	if !closeEnough(m.CacheHitRate, 1.0) {
		t.Errorf("CacheHitRate = %f, want 1.0", m.CacheHitRate)
	}
	if !closeEnough(m.CacheCreateRate, 0.0) {
		t.Errorf("CacheCreateRate = %f, want 0.0", m.CacheCreateRate)
	}
}

// ---------------------------------------------------------------------------
// AggregateCacheMetrics
// ---------------------------------------------------------------------------

func TestAggregateCacheMetrics_MultipleResults(t *testing.T) {
	results := []RunResult{
		{Tokens: TokenUsage{InputTokens: 100, CacheRead: 400, CacheCreate: 50}},
		{Tokens: TokenUsage{InputTokens: 200, CacheRead: 800, CacheCreate: 0}},
		{Tokens: TokenUsage{InputTokens: 150, CacheRead: 600, CacheCreate: 50}},
	}
	m := AggregateCacheMetrics(results)

	// totals: InputTokens=450, CacheRead=1800, CacheCreate=100, total=2350
	wantHitRate := 1800.0 / 2350.0
	wantCreateRate := 100.0 / 2350.0

	if !closeEnough(m.CacheHitRate, wantHitRate) {
		t.Errorf("CacheHitRate = %f, want %f", m.CacheHitRate, wantHitRate)
	}
	if !closeEnough(m.CacheCreateRate, wantCreateRate) {
		t.Errorf("CacheCreateRate = %f, want %f", m.CacheCreateRate, wantCreateRate)
	}
	if m.CachedTokens != 1900 {
		t.Errorf("CachedTokens = %d, want 1900", m.CachedTokens)
	}
}

func TestAggregateCacheMetrics_EmptyResults(t *testing.T) {
	m := AggregateCacheMetrics(nil)
	if m.CacheHitRate != 0 || m.CacheCreateRate != 0 || m.CachedTokens != 0 {
		t.Errorf("expected all-zero metrics for nil results, got %+v", m)
	}
}

// ---------------------------------------------------------------------------
// PerTurnCacheMetrics
// ---------------------------------------------------------------------------

func TestPerTurnCacheMetrics_ReturnsPerTurn(t *testing.T) {
	results := []RunResult{
		{Tokens: TokenUsage{InputTokens: 100, CacheRead: 0, CacheCreate: 0}},
		{Tokens: TokenUsage{InputTokens: 50, CacheRead: 450, CacheCreate: 0}},
	}
	metrics := PerTurnCacheMetrics(results)
	if len(metrics) != 2 {
		t.Fatalf("len = %d, want 2", len(metrics))
	}
	// Turn 0: no cache
	if !closeEnough(metrics[0].CacheHitRate, 0) {
		t.Errorf("turn 0 hit rate = %f, want 0", metrics[0].CacheHitRate)
	}
	// Turn 1: 450/(450+50) = 0.9
	if !closeEnough(metrics[1].CacheHitRate, 0.9) {
		t.Errorf("turn 1 hit rate = %f, want 0.9", metrics[1].CacheHitRate)
	}
}

// ---------------------------------------------------------------------------
// CostSavings
// ---------------------------------------------------------------------------

func TestCostSavings(t *testing.T) {
	m := CacheMetrics{CachedTokens: 1000}
	savings := CostSavings(m, 0.000015, 0.0000015)
	want := 1000.0 * (0.000015 - 0.0000015)
	if !closeEnough(savings, want) {
		t.Errorf("CostSavings = %f, want %f", savings, want)
	}
}

// ---------------------------------------------------------------------------
// AssertCacheExpectations
// ---------------------------------------------------------------------------

func makeResults(turns int, setup func(i int, t *TokenUsage)) []RunResult {
	results := make([]RunResult, turns)
	for i := range results {
		setup(i, &results[i].Tokens)
	}
	return results
}

func TestAssertCacheExpectations_Pass(t *testing.T) {
	results := makeResults(3, func(i int, tok *TokenUsage) {
		if i == 0 {
			// Turn 1: cold start, full cache creation
			tok.InputTokens = 100
			tok.CacheCreate = 400
			tok.CacheRead = 0
		} else {
			// Turn 2+: warm cache, high hit rate, no re-creation
			tok.InputTokens = 10
			tok.CacheCreate = 0
			tok.CacheRead = 490
		}
	})
	expect := CacheExpect{
		MinCacheHitRateAfterTurn1: 0.8,
		NoCacheRecreateAfterTurn1: true,
	}
	if err := AssertCacheExpectations(results, expect); err != nil {
		t.Errorf("expected pass, got error: %v", err)
	}
}

func TestAssertCacheExpectations_FailLowHitRate(t *testing.T) {
	results := makeResults(3, func(i int, tok *TokenUsage) {
		if i == 0 {
			tok.InputTokens = 100
			tok.CacheCreate = 400
		} else {
			// Low cache hit rate: 100/(100+400) = 0.2
			tok.InputTokens = 400
			tok.CacheCreate = 0
			tok.CacheRead = 100
		}
	})
	expect := CacheExpect{
		MinCacheHitRateAfterTurn1: 0.8,
	}
	err := AssertCacheExpectations(results, expect)
	if err == nil {
		t.Fatal("expected error for low hit rate, got nil")
	}
	if got := err.Error(); !containsSubstring(got, "cache hit rate") {
		t.Errorf("error %q does not mention cache hit rate", got)
	}
}

func TestAssertCacheExpectations_FailCacheRecreate(t *testing.T) {
	results := makeResults(3, func(i int, tok *TokenUsage) {
		if i == 0 {
			tok.InputTokens = 100
			tok.CacheCreate = 400
		} else {
			tok.InputTokens = 10
			tok.CacheRead = 490
			tok.CacheCreate = 50 // unexpected re-creation
		}
	})
	expect := CacheExpect{
		NoCacheRecreateAfterTurn1: true,
	}
	err := AssertCacheExpectations(results, expect)
	if err == nil {
		t.Fatal("expected error for cache re-creation, got nil")
	}
	if got := err.Error(); !containsSubstring(got, "cache_create") {
		t.Errorf("error %q does not mention cache_create", got)
	}
}

func TestAssertCacheExpectations_SingleTurnNoop(t *testing.T) {
	results := []RunResult{
		{Tokens: TokenUsage{InputTokens: 100, CacheCreate: 500}},
	}
	expect := CacheExpect{
		MinCacheHitRateAfterTurn1: 0.9,
		NoCacheRecreateAfterTurn1: true,
	}
	// Should not error with only one turn — nothing to check "after turn 1".
	if err := AssertCacheExpectations(results, expect); err != nil {
		t.Errorf("expected nil for single turn, got: %v", err)
	}
}

func containsSubstring(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsAt(s, sub))
}

func containsAt(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
