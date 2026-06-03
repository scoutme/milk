package obs

import (
	"context"
	"strings"
	"testing"
)

func TestAccumulateCacheTokens_Accumulates(t *testing.T) {
	ResetSessionTokens()
	AccumulateCacheTokens("model-x", "escalation", 1000, 200)
	AccumulateCacheTokens("model-x", "escalation", 500, 100)
	read, creation := SessionCacheByRole("escalation")
	if read != 1500 {
		t.Errorf("cache_read: got %d want 1500", read)
	}
	if creation != 300 {
		t.Errorf("cache_creation: got %d want 300", creation)
	}
}

func TestAccumulateCacheTokens_NoopOnZero(t *testing.T) {
	ResetSessionTokens()
	AccumulateCacheTokens("model-x", "escalation", 0, 0)
	read, creation := SessionCacheByRole("escalation")
	if read != 0 || creation != 0 {
		t.Errorf("expected zeros, got read=%d creation=%d", read, creation)
	}
}

func TestSessionCacheByRole_IsolatedByRole(t *testing.T) {
	ResetSessionTokens()
	AccumulateCacheTokens("model-x", "escalation", 1000, 200)
	AccumulateCacheTokens("model-y", "primary", 999, 999)
	read, creation := SessionCacheByRole("escalation")
	if read != 1000 || creation != 200 {
		t.Errorf("escalation cache should not include primary, got read=%d creation=%d", read, creation)
	}
}

func TestFormatTokenUsage_ShowsHitRate(t *testing.T) {
	entries := []SessionTokenEntry{
		{Model: "claude", Agent: "escalation", Prompt: 100, Completion: 20, CacheRead: 8000, CacheCreation: 1000},
	}
	out := FormatTokenUsage(context.TODO(), "/nonexistent", entries, 5)
	if !strings.Contains(out, "88.9%") {
		t.Errorf("expected hit rate 88.9%%, got:\n%s", out)
	}
	if !strings.Contains(out, "8000") {
		t.Errorf("expected cache_read 8000, got:\n%s", out)
	}
	if !strings.Contains(out, "1000") {
		t.Errorf("expected cache_creation 1000, got:\n%s", out)
	}
}

func TestFormatTokenUsage_NoCacheShowsDash(t *testing.T) {
	entries := []SessionTokenEntry{
		{Model: "qwen", Agent: "primary", Prompt: 500, Completion: 100},
	}
	out := FormatTokenUsage(context.TODO(), "/nonexistent", entries, 3)
	if !strings.Contains(out, "—") {
		t.Errorf("expected dash for no-cache row, got:\n%s", out)
	}
}
