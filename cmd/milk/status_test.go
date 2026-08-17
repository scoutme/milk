package main

// TestHeaderBar_ShowsCachePercentage verifies that once sess.Tokens carries
// nonzero cache-read/cache-creation counts — the same TokenUsage fields
// dispatch.go's runPrimary/runEscalation feed via AddTokensFull — headerBar's
// role-agnostic sum over sess.Tokens renders a "cache:NN%" segment. This is
// the end-to-end display check requested by this sprint's acceptance
// criteria: cache values reaching onTokens/AddTokensFull must actually show
// up in the status bar / memory panel, not just land correctly in the struct.

import (
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/session"
)

func TestHeaderBar_ShowsCachePercentage(t *testing.T) {
	sess := &session.Session{ID: "abcdef1234"}
	// Hit rate is cacheRead / (prompt + cacheRead + cacheCreation), i.e. against
	// TOTAL input tokens, not just cacheRead+cacheCreation: 300 / (1000+300+100)
	// = 21.4% -> 21%. (Not 300/(300+100)=75%, which ignores the 1000 genuinely
	// fresh prompt tokens and would overstate the hit rate.)
	sess.AddTokensFull("test-model", "primary", 1000, 20, 300, 100)

	m := &model{
		width: 120,
		st: &interactiveState{
			sess: sess,
			cfg:  config.Config{},
		},
	}

	bar := stripANSI(m.headerBar())
	if !strings.Contains(bar, "cache:21%") {
		t.Errorf("want headerBar to contain %q, got %q", "cache:21%", bar)
	}
}

// TestHeaderBar_NoCacheSegmentWhenZero verifies the cache segment is omitted
// entirely when no cache tokens have been recorded, so the display doesn't
// show a misleading "cache:0%" for agents/providers that never report cache
// stats (the common case today).
func TestHeaderBar_NoCacheSegmentWhenZero(t *testing.T) {
	sess := &session.Session{ID: "abcdef1234"}
	sess.AddTokensFull("test-model", "primary", 1000, 20, 0, 0)

	m := &model{
		width: 120,
		st: &interactiveState{
			sess: sess,
			cfg:  config.Config{},
		},
	}

	bar := stripANSI(m.headerBar())
	if strings.Contains(bar, "cache:") {
		t.Errorf("want no cache segment when cache tokens are zero, got %q", bar)
	}
}

// TestHeaderBar_SubagentWorkflowTokens verifies that subagent/workflow tokens
// stored under "escalation:subagent" and "escalation:workflow" role strings are
// included in the header bar's token totals.
func TestHeaderBar_SubagentWorkflowTokens(t *testing.T) {
	sess := &session.Session{ID: "test1234"}
	sess.AddTokensFull("test-model", "escalation", 1000, 200, 500, 100)
	sess.AddTokensFull("test-model", "escalation:subagent", 300, 50, 150, 30)
	sess.AddTokensFull("test-model", "escalation:workflow", 100, 20, 50, 10)

	m := &model{
		width: 120,
		st: &interactiveState{
			sess: sess,
			cfg:  config.Config{},
		},
	}

	bar := stripANSI(m.headerBar())
	// Total prompt tokens: 1000 + 300 + 100 = 1400
	// Total completion tokens: 200 + 50 + 20 = 270
	// Cache read: 500 + 150 + 50 = 700
	// Cache creation: 100 + 30 + 10 = 140
	// Hit rate: 700 / (1400 + 700 + 140) = 31.25% -> 31%
	if !strings.Contains(bar, "cache:31%") {
		t.Errorf("want headerBar with cache:31%% (all roles combined), got %q", bar)
	}
}

// TestStatusTokens_CacheWithCreation verifies the status bar displays
// cache:read/creation(hit%) with non-zero creation tokens, proving the
// full format works when the provider reports cache creation.
func TestStatusTokens_CacheWithCreation(t *testing.T) {
	m := &model{
		width: 120,
		st: &interactiveState{
			sess: &session.Session{ID: "test1234"},
			cfg:  config.Config{},
		},
		busy: false,
		// Simulate a session where cache creation happened (e.g. first turn
		// with Bedrock explicit caching, or a fresh system prompt being written
		// to the prompt cache).
		escalationPrompt:       1000,
		escalationComp:         200,
		escalationCacheRead:    500,
		escalationCacheCreation: 100,
		lastTokenRole:          "escalation",
	}
	m.st.activeFallbackTarget = "escalation"

	bar := stripANSI(m.statusTokens())
	// cache:500/100(25%) — 500 read, 100 creation, hit rate = 500/(1000+500+100) = 31%
	// Actually: 500 / (1000 + 600) = 31.25% -> 31%
	if !strings.Contains(bar, "cache:") {
		t.Errorf("want cache segment, got %q", bar)
	}
	if !strings.Contains(bar, "/") {
		t.Errorf("want read/creation format with '/', got %q", bar)
	}
	if !strings.Contains(bar, "500") {
		t.Errorf("want cache read 500 in display, got %q", bar)
	}
	if !strings.Contains(bar, "100") {
		t.Errorf("want cache creation 100 in display, got %q", bar)
	}
}

// TestStatusTokens_CacheZeroCreation verifies the status bar displays
// cache:read(hit%) without the /creation part when creation is zero
// (the common case with Claude Code CLI which uses implicit caching).
func TestStatusTokens_CacheZeroCreation(t *testing.T) {
	m := &model{
		width: 120,
		st: &interactiveState{
			sess: &session.Session{ID: "test1234"},
			cfg:  config.Config{},
		},
		busy: false,
		// Claude Code CLI: system prompt cached on turn 1, subsequent turns
		// only read from cache. Creation is always 0.
		escalationPrompt:       2000,
		escalationComp:         300,
		escalationCacheRead:    800,
		escalationCacheCreation: 0,
		lastTokenRole:          "escalation",
	}
	m.st.activeFallbackTarget = "escalation"

	bar := stripANSI(m.statusTokens())
	// cache:800(28%) — no /0 since creation is zero
	if !strings.Contains(bar, "cache:") {
		t.Errorf("want cache segment, got %q", bar)
	}
	if strings.Contains(bar, "/") {
		t.Errorf("want no / when creation is zero, got %q", bar)
	}
	if !strings.Contains(bar, "800") {
		t.Errorf("want cache read 800 in display, got %q", bar)
	}
}
