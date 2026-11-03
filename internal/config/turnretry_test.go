package config

// Issue #391: limits.turn_retries / limits.turn_retry_backoff_max_secs
// resolution. The defaults' equality with workflow's constants is pinned
// from workflow's side (TestRetryPolicy_DefaultsMatchConfig there — this
// package must not import workflow).

import (
	"testing"
	"time"
)

func TestTurnRetries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limits *AgentLimits
		want   int
	}{
		{"nil limits", nil, 2},
		{"empty limits", &AgentLimits{}, 2},
		{"unset field", &AgentLimits{TurnRetries: nil}, 2},
		{"zero = built-in default", &AgentLimits{TurnRetries: intPtr(0)}, 2},
		{"negative = disabled", &AgentLimits{TurnRetries: intPtr(-1)}, 0},
		{"positive = exact", &AgentLimits{TurnRetries: intPtr(5)}, 5},
		{"one = single retry", &AgentLimits{TurnRetries: intPtr(1)}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := TurnRetries(tc.limits); got != tc.want {
				t.Errorf("TurnRetries = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestTurnRetryBackoffMax(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limits *AgentLimits
		want   time.Duration
	}{
		{"nil limits", nil, 30 * time.Second},
		{"unset field", &AgentLimits{TurnRetryBackoffMaxSecs: nil}, 30 * time.Second},
		{"zero = built-in default", &AgentLimits{TurnRetryBackoffMaxSecs: intPtr(0)}, 30 * time.Second},
		{"non-positive = default (no no-backoff mode)", &AgentLimits{TurnRetryBackoffMaxSecs: intPtr(-1)}, 30 * time.Second},
		{"positive = exact seconds", &AgentLimits{TurnRetryBackoffMaxSecs: intPtr(7)}, 7 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := TurnRetryBackoffMax(tc.limits); got != tc.want {
				t.Errorf("TurnRetryBackoffMax = %v, want %v", got, tc.want)
			}
		})
	}
}
