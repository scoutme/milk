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
	entries := []SessionTokenEntry{{Model: "claude", Agent: "escalation", Prompt: 100, Completion: 20, CacheRead: 8000, CacheCreation: 1000}}
	out := FormatTokenUsage(context.TODO(), "/nonexistent", entries, 5)
	// Hit rate is against TOTAL input (prompt + cacheRead + cacheCreation):
	// 8000 / (100+8000+1000) = 87.9%. Not 8000/(8000+1000)=88.9%, which ignores
	// the 100 genuinely fresh prompt tokens and overstates the hit rate.
	if !strings.Contains(out, "87.9%") {
		t.Errorf("expected hit rate 87.9%%, got:\n%s", out)
	}
	if !strings.Contains(out, "8000") {
		t.Errorf("expected cache_read 8000, got:\n%s", out)
	}
	if !strings.Contains(out, "1000") {
		t.Errorf("expected cache_creation 1000, got:\n%s", out)
	}
}

func TestFormatTokenUsage_NoCacheShowsDash(t *testing.T) {
	entries := []SessionTokenEntry{{Model: "qwen", Agent: "primary", Prompt: 500, Completion: 100}}
	out := FormatTokenUsage(context.TODO(), "/nonexistent", entries, 3)
	if !strings.Contains(out, "—") {
		t.Errorf("expected dash for no-cache row, got:\n%s", out)
	}
}

func TestFormatTokenUsage_SubagentWorkflowRows(t *testing.T) {
	entries := []SessionTokenEntry{
		{Model: "claude-3-opus", Agent: "escalation", Prompt: 1000, Completion: 500, CacheRead: 800, CacheCreation: 200},
		{Model: "claude-3-opus", Agent: "escalation:subagent", Prompt: 300, Completion: 100, CacheRead: 200, CacheCreation: 50},
		{Model: "claude-3-opus", Agent: "escalation:workflow", Prompt: 150, Completion: 50, CacheRead: 80, CacheCreation: 20},
	}
	out := FormatTokenUsage(context.TODO(), "/nonexistent", entries, 5)
	if !strings.Contains(out, "escalation:subagent") {
		t.Errorf("expected escalation:subagent row, got:\n%s", out)
	}
	if !strings.Contains(out, "escalation:workflow") {
		t.Errorf("expected escalation:workflow row, got:\n%s", out)
	}
	if !strings.Contains(out, "escalation ") {
		t.Errorf("expected main escalation row, got:\n%s", out)
	}
	// Total should include all: prompt=1450, completion=650, cache_read=1080, cache_creation=270
	if !strings.Contains(out, "1450") {
		t.Errorf("expected total prompt 1450, got:\n%s", out)
	}
}

func TestAccumulateCacheTokens_SubagentWorkflowRoles(t *testing.T) {
	ResetSessionTokens()
	AccumulateCacheTokens("model-x", "escalation", 1000, 200)
	AccumulateCacheTokens("model-x", "escalation:subagent", 500, 100)
	AccumulateCacheTokens("model-x", "escalation:workflow", 250, 50)

	read, creation := SessionCacheByRole("escalation")
	if read != 1000 || creation != 200 {
		t.Errorf("escalation cache: read=%d creation=%d, want 1000/200", read, creation)
	}
	read, creation = SessionCacheByRole("escalation:subagent")
	if read != 500 || creation != 100 {
		t.Errorf("escalation:subagent cache: read=%d creation=%d, want 500/100", read, creation)
	}
	read, creation = SessionCacheByRole("escalation:workflow")
	if read != 250 || creation != 50 {
		t.Errorf("escalation:workflow cache: read=%d creation=%d, want 250/50", read, creation)
	}
}

func TestSessionTokensByRole_SubagentPrefix(t *testing.T) {
	ResetSessionTokens()
	accumulateSessionTokens("m", "escalation", 100, 20)
	accumulateSessionTokens("m", "escalation:subagent", 50, 10)
	accumulateSessionTokens("m", "escalation:workflow", 25, 5)

	p, c := SessionTokensByRole("escalation")
	if p != 100 || c != 20 {
		t.Errorf("escalation: p=%d c=%d, want 100/20", p, c)
	}
	p, c = SessionTokensByRole("escalation:subagent")
	if p != 50 || c != 10 {
		t.Errorf("escalation:subagent: p=%d c=%d, want 50/10", p, c)
	}
	p, c = SessionTokensByRole("escalation:workflow")
	if p != 25 || c != 5 {
		t.Errorf("escalation:workflow: p=%d c=%d, want 25/5", p, c)
	}
}
