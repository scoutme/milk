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
	// 300 read + 100 creation → 400 total, 75% hit rate.
	sess.AddTokensFull("test-model", "primary", 1000, 20, 300, 100)

	m := &model{
		width: 120,
		st: &interactiveState{
			sess: sess,
			cfg:  config.Config{},
		},
	}

	bar := stripANSI(m.headerBar())
	if !strings.Contains(bar, "cache:75%") {
		t.Errorf("want headerBar to contain %q, got %q", "cache:75%", bar)
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
