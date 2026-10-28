package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/loop"
)

func TestActiveAgent_ByName(t *testing.T) {
	cfg := Config{
		Agent: "second",
		Agents: []AgentConfig{
			{Name: "first", URL: "http://first", Model: "m1"},
			{Name: "second", URL: "http://second", Model: "m2", Provider: "bedrock"},
		},
	}
	got := cfg.ActiveAgent()
	if got.Name != "second" || got.URL != "http://second" || got.Provider != "bedrock" {
		t.Fatalf("expected second/bedrock, got %+v", got)
	}
}

func TestActiveAgent_FirstNonCLI(t *testing.T) {
	cfg := Config{
		Agents: []AgentConfig{
			{Name: "claude", Provider: "claude-cli"},
			{Name: "alpha", URL: "http://alpha", Model: "ma"},
			{Name: "beta", URL: "http://beta", Model: "mb"},
		},
	}
	got := cfg.ActiveAgent()
	if got.Name != "alpha" {
		t.Fatalf("expected alpha (first non-cli), got %q", got.Name)
	}
}

func TestActiveAgent_CaseInsensitive(t *testing.T) {
	cfg := Config{
		Agent: "HAIKU",
		Agents: []AgentConfig{
			{Name: "haiku", URL: "http://haiku", Model: "h"},
		},
	}
	got := cfg.ActiveAgent()
	if got.Name != "haiku" {
		t.Fatalf("expected haiku, got %q", got.Name)
	}
}

func TestActiveAgent_UnknownNameFallsToFirst(t *testing.T) {
	cfg := Config{
		Agent: "nonexistent",
		Agents: []AgentConfig{
			{Name: "only", URL: "http://only", Model: "m"},
		},
	}
	got := cfg.ActiveAgent()
	if got.Name != "only" {
		t.Fatalf("expected only, got %q", got.Name)
	}
}

func TestActiveAgent_EmptyConfigReturnsZero(t *testing.T) {
	cfg := Config{}
	got := cfg.ActiveAgent()
	if got.URL != "" || got.Model != "" {
		t.Fatalf("empty config should return zero AgentConfig, got %+v", got)
	}
}

func TestAgentByName_Found(t *testing.T) {
	cfg := Config{
		Agents: []AgentConfig{
			{Name: "first", URL: "http://first", Model: "m1"},
			{Name: "second", URL: "http://second", Model: "m2", Provider: "bedrock"},
		},
	}
	got, ok := cfg.AgentByName("SECOND")
	if !ok || got.URL != "http://second" || got.Provider != "bedrock" {
		t.Fatalf("expected second/bedrock, got %+v ok=%v", got, ok)
	}
}

func TestAgentByName_NotFound(t *testing.T) {
	cfg := Config{
		Agents: []AgentConfig{{Name: "only", URL: "http://only", Model: "m"}},
	}
	_, ok := cfg.AgentByName("missing")
	if ok {
		t.Fatal("expected ok=false for unknown agent name")
	}
}

func TestAgentByName_BuiltinClaudeCLI(t *testing.T) {
	cfg := Config{}
	got, ok := cfg.AgentByName("claude")
	if !ok || !got.IsCLI() {
		t.Fatalf("expected built-in claude-cli agent, got %+v ok=%v", got, ok)
	}
}

func TestEscalationAgentConfig_DefaultClaude(t *testing.T) {
	cfg := Config{}
	got := cfg.EscalationAgentConfig()
	if !got.IsCLI() || got.Name != "claude" {
		t.Fatalf("expected default claude-cli, got %+v", got)
	}
}

func TestEscalationAgentConfig_NamedLocal(t *testing.T) {
	cfg := Config{
		EscalationAgent: "haiku-aws",
		Agents: []AgentConfig{
			{Name: "haiku-aws", URL: "http://bedrock", Model: "arn:x", Provider: "bedrock"},
		},
	}
	got := cfg.EscalationAgentConfig()
	if got.Provider != "bedrock" || got.Name != "haiku-aws" {
		t.Fatalf("expected haiku-aws/bedrock, got %+v", got)
	}
}

func TestLocalMemoryResultMaxByteCount_Defaults(t *testing.T) {
	cfg := Config{}
	if got := cfg.LocalMemoryResultMaxByteCount(); got != 2048 {
		t.Errorf("expected default 2048, got %d", got)
	}
}

func TestLocalMemoryResultMaxByteCount_Explicit(t *testing.T) {
	cfg := Config{LocalMemoryResultMaxBytes: 500}
	if got := cfg.LocalMemoryResultMaxByteCount(); got != 500 {
		t.Errorf("expected 500, got %d", got)
	}
}

func TestLocalMemoryResultMaxByteCount_Disabled(t *testing.T) {
	cfg := Config{LocalMemoryResultMaxBytes: -1}
	if got := cfg.LocalMemoryResultMaxByteCount(); got != 0 {
		t.Errorf("expected 0 when disabled, got %d", got)
	}
}

func TestLocalMemoryReinjectionTurnThreshold_Defaults(t *testing.T) {
	cfg := Config{}
	if got := cfg.LocalMemoryReinjectionTurnThreshold(); got != 20 {
		t.Errorf("expected default 20, got %d", got)
	}
}

func TestLocalMemoryReinjectionTurnThreshold_Explicit(t *testing.T) {
	cfg := Config{LocalMemoryReinjectionTurns: 5}
	if got := cfg.LocalMemoryReinjectionTurnThreshold(); got != 5 {
		t.Errorf("expected 5, got %d", got)
	}
}

func TestLocalMemoryReinjectionTurnThreshold_Disabled(t *testing.T) {
	cfg := Config{LocalMemoryReinjectionTurns: -1}
	if got := cfg.LocalMemoryReinjectionTurnThreshold(); got != 0 {
		t.Errorf("expected 0 when disabled, got %d", got)
	}
}

func TestLocalMemoryReinjectionByteThreshold_Defaults(t *testing.T) {
	cfg := Config{}
	if got := cfg.LocalMemoryReinjectionByteThreshold(); got != 40000 {
		t.Errorf("expected default 40000, got %d", got)
	}
}

func TestLocalMemoryReinjectionByteThreshold_Disabled(t *testing.T) {
	cfg := Config{LocalMemoryReinjectionBytes: -1}
	if got := cfg.LocalMemoryReinjectionByteThreshold(); got != 0 {
		t.Errorf("expected 0 when disabled, got %d", got)
	}
}

// helpers for concise pointer literals in tests.
func intPtr(v int) *int    { return &v }
func boolPtr(v bool) *bool { return &v }

// --- Per-agent resolver tests ---

func TestAgentContextBudget_NilLimits(t *testing.T) {
	cfg := Config{ContextBudgetChars: 5000}
	ac := AgentConfig{}
	if got := cfg.AgentContextBudget(ac); got != 5000 {
		t.Errorf("expected global fallback 5000, got %d", got)
	}
}

func TestAgentContextBudget_Override(t *testing.T) {
	cfg := Config{ContextBudgetChars: 5000}
	ac := AgentConfig{Limits: &AgentLimits{ContextBudgetChars: intPtr(8000)}}
	if got := cfg.AgentContextBudget(ac); got != 8000 {
		t.Errorf("expected per-agent 8000, got %d", got)
	}
}

func TestAgentContextBudget_Zero_UsesDefault(t *testing.T) {
	cfg := Config{}
	ac := AgentConfig{Limits: &AgentLimits{ContextBudgetChars: intPtr(0)}}
	if got := cfg.AgentContextBudget(ac); got != 12000 {
		t.Errorf("expected built-in default 12000, got %d", got)
	}
}

func TestAgentContextBudget_Negative_Unlimited(t *testing.T) {
	cfg := Config{}
	ac := AgentConfig{Limits: &AgentLimits{ContextBudgetChars: intPtr(-1)}}
	if got := cfg.AgentContextBudget(ac); got != 0 {
		t.Errorf("expected 0 (unlimited), got %d", got)
	}
}

func TestAgentMessageBudget_NilLimits(t *testing.T) {
	cfg := Config{LocalContextBudgetChars: 6000}
	ac := AgentConfig{}
	if got := cfg.AgentMessageBudget(ac); got != 6000 {
		t.Errorf("expected global fallback 6000, got %d", got)
	}
}

func TestAgentMessageBudget_Override(t *testing.T) {
	cfg := Config{}
	ac := AgentConfig{Limits: &AgentLimits{MessageBudgetChars: intPtr(10000)}}
	if got := cfg.AgentMessageBudget(ac); got != 10000 {
		t.Errorf("expected 10000, got %d", got)
	}
}

func TestAgentMessageBudget_Zero_UsesDefault(t *testing.T) {
	cfg := Config{}
	ac := AgentConfig{Limits: &AgentLimits{MessageBudgetChars: intPtr(0)}}
	if got := cfg.AgentMessageBudget(ac); got != 24000 {
		t.Errorf("expected built-in default 24000, got %d", got)
	}
}

func TestAgentMessageBudget_Negative_Unlimited(t *testing.T) {
	cfg := Config{}
	ac := AgentConfig{Limits: &AgentLimits{MessageBudgetChars: intPtr(-1)}}
	if got := cfg.AgentMessageBudget(ac); got != 0 {
		t.Errorf("expected 0 (unlimited), got %d", got)
	}
}

func TestAgentMemoryReinjectionTurnThreshold_NilLimits_Local(t *testing.T) {
	cfg := Config{LocalMemoryReinjectionTurns: 7}
	ac := AgentConfig{}
	if got := cfg.AgentMemoryReinjectionTurnThreshold(ac, true); got != 7 {
		t.Errorf("expected local global fallback 7, got %d", got)
	}
}

func TestAgentMemoryReinjectionTurnThreshold_NilLimits_Escalation(t *testing.T) {
	cfg := Config{MemoryReinjectionTurns: 12}
	ac := AgentConfig{}
	if got := cfg.AgentMemoryReinjectionTurnThreshold(ac, false); got != 12 {
		t.Errorf("expected escalation global fallback 12, got %d", got)
	}
}

func TestAgentMemoryReinjectionTurnThreshold_Override(t *testing.T) {
	cfg := Config{}
	ac := AgentConfig{Limits: &AgentLimits{MemoryReinjectionTurns: intPtr(5)}}
	if got := cfg.AgentMemoryReinjectionTurnThreshold(ac, true); got != 5 {
		t.Errorf("expected 5, got %d", got)
	}
}

func TestAgentMemoryReinjectionTurnThreshold_Zero_UsesDefault(t *testing.T) {
	cfg := Config{}
	ac := AgentConfig{Limits: &AgentLimits{MemoryReinjectionTurns: intPtr(0)}}
	if got := cfg.AgentMemoryReinjectionTurnThreshold(ac, true); got != 20 {
		t.Errorf("expected built-in default 20, got %d", got)
	}
}

func TestAgentMemoryReinjectionTurnThreshold_Negative_Disabled(t *testing.T) {
	cfg := Config{}
	ac := AgentConfig{Limits: &AgentLimits{MemoryReinjectionTurns: intPtr(-1)}}
	if got := cfg.AgentMemoryReinjectionTurnThreshold(ac, true); got != 0 {
		t.Errorf("expected 0 (disabled), got %d", got)
	}
}

func TestAgentMemoryReinjectionByteThreshold_Override(t *testing.T) {
	cfg := Config{}
	ac := AgentConfig{Limits: &AgentLimits{MemoryReinjectionBytes: intPtr(20000)}}
	if got := cfg.AgentMemoryReinjectionByteThreshold(ac, true); got != 20000 {
		t.Errorf("expected 20000, got %d", got)
	}
}

func TestAgentMemoryReinjectionByteThreshold_Zero_UsesDefault(t *testing.T) {
	cfg := Config{}
	ac := AgentConfig{Limits: &AgentLimits{MemoryReinjectionBytes: intPtr(0)}}
	if got := cfg.AgentMemoryReinjectionByteThreshold(ac, false); got != 40000 {
		t.Errorf("expected built-in default 40000, got %d", got)
	}
}

func TestAgentMemoryReinjectionByteThreshold_Negative_Disabled(t *testing.T) {
	cfg := Config{}
	ac := AgentConfig{Limits: &AgentLimits{MemoryReinjectionBytes: intPtr(-1)}}
	if got := cfg.AgentMemoryReinjectionByteThreshold(ac, false); got != 0 {
		t.Errorf("expected 0 (disabled), got %d", got)
	}
}

func TestAgentMemoryResultMaxByteCount_NilLimits(t *testing.T) {
	cfg := Config{LocalMemoryResultMaxBytes: 1024}
	ac := AgentConfig{}
	if got := cfg.AgentMemoryResultMaxByteCount(ac); got != 1024 {
		t.Errorf("expected global fallback 1024, got %d", got)
	}
}

func TestAgentMemoryResultMaxByteCount_Override(t *testing.T) {
	cfg := Config{}
	ac := AgentConfig{Limits: &AgentLimits{MemoryResultMaxBytes: intPtr(512)}}
	if got := cfg.AgentMemoryResultMaxByteCount(ac); got != 512 {
		t.Errorf("expected 512, got %d", got)
	}
}

func TestAgentMemoryResultMaxByteCount_Zero_UsesDefault(t *testing.T) {
	cfg := Config{}
	ac := AgentConfig{Limits: &AgentLimits{MemoryResultMaxBytes: intPtr(0)}}
	if got := cfg.AgentMemoryResultMaxByteCount(ac); got != 2048 {
		t.Errorf("expected built-in default 2048, got %d", got)
	}
}

func TestAgentMemoryResultMaxByteCount_Negative_Unlimited(t *testing.T) {
	cfg := Config{}
	ac := AgentConfig{Limits: &AgentLimits{MemoryResultMaxBytes: intPtr(-1)}}
	if got := cfg.AgentMemoryResultMaxByteCount(ac); got != 0 {
		t.Errorf("expected 0 (unlimited), got %d", got)
	}
}

func TestAgentPayloadTrimCompactionThreshold_NilLimits(t *testing.T) {
	cfg := Config{}
	ac := AgentConfig{}
	if got := cfg.AgentPayloadTrimCompactionThreshold(ac); got != DefaultPayloadTrimCompactionThreshold {
		t.Errorf("expected default %d, got %d", DefaultPayloadTrimCompactionThreshold, got)
	}
}

func TestAgentPayloadTrimCompactionThreshold_Override(t *testing.T) {
	cfg := Config{}
	ac := AgentConfig{Limits: &AgentLimits{PayloadTrimCompactionThreshold: intPtr(5)}}
	if got := cfg.AgentPayloadTrimCompactionThreshold(ac); got != 5 {
		t.Errorf("expected 5, got %d", got)
	}
}

func TestAgentPayloadTrimCompactionThreshold_ZeroOrNegative_Disables(t *testing.T) {
	cfg := Config{}
	for _, v := range []int{0, -1} {
		ac := AgentConfig{Limits: &AgentLimits{PayloadTrimCompactionThreshold: intPtr(v)}}
		if got := cfg.AgentPayloadTrimCompactionThreshold(ac); got != v {
			t.Errorf("expected explicit %d to pass through unchanged, got %d", v, got)
		}
	}
}

func TestAgentEscalateAfterRecoveries_NilLimits(t *testing.T) {
	cfg := Config{}
	ac := AgentConfig{}
	if got := cfg.AgentEscalateAfterRecoveries(ac); got != DefaultEscalateAfterRecoveries {
		t.Errorf("expected default %d, got %d", DefaultEscalateAfterRecoveries, got)
	}
}

func TestAgentEscalateAfterRecoveries_Override(t *testing.T) {
	cfg := Config{}
	ac := AgentConfig{Limits: &AgentLimits{EscalateAfterRecoveries: intPtr(6)}}
	if got := cfg.AgentEscalateAfterRecoveries(ac); got != 6 {
		t.Errorf("expected 6, got %d", got)
	}
}

func TestAgentEscalateAfterRecoveries_ZeroOrNegative_Disables(t *testing.T) {
	cfg := Config{}
	for _, v := range []int{0, -1} {
		ac := AgentConfig{Limits: &AgentLimits{EscalateAfterRecoveries: intPtr(v)}}
		if got := cfg.AgentEscalateAfterRecoveries(ac); got != v {
			t.Errorf("expected explicit %d to pass through unchanged, got %d", v, got)
		}
	}
}

func TestAgentPerceptInjectMaxCount_NilLimits(t *testing.T) {
	cfg := Config{PerceptInjectMax: 10}
	ac := AgentConfig{}
	if got := cfg.AgentPerceptInjectMaxCount(ac); got != 10 {
		t.Errorf("expected global fallback 10, got %d", got)
	}
}

func TestAgentPerceptInjectMaxCount_Override(t *testing.T) {
	cfg := Config{}
	ac := AgentConfig{Limits: &AgentLimits{PerceptInjectMax: intPtr(3)}}
	if got := cfg.AgentPerceptInjectMaxCount(ac); got != 3 {
		t.Errorf("expected 3, got %d", got)
	}
}

func TestAgentPerceptInjectMaxCount_Zero_UsesDefault(t *testing.T) {
	cfg := Config{}
	ac := AgentConfig{Limits: &AgentLimits{PerceptInjectMax: intPtr(0)}}
	if got := cfg.AgentPerceptInjectMaxCount(ac); got != 25 {
		t.Errorf("expected built-in default 25, got %d", got)
	}
}

func TestAgentPerceptInjectMaxCount_Negative_Unlimited(t *testing.T) {
	cfg := Config{}
	ac := AgentConfig{Limits: &AgentLimits{PerceptInjectMax: intPtr(-1)}}
	if got := cfg.AgentPerceptInjectMaxCount(ac); got != 0 {
		t.Errorf("expected 0 (unlimited), got %d", got)
	}
}

func TestAgentPerceptInjectMaxByteCount_Override(t *testing.T) {
	cfg := Config{}
	ac := AgentConfig{Limits: &AgentLimits{PerceptInjectMaxBytes: intPtr(4096)}}
	if got := cfg.AgentPerceptInjectMaxByteCount(ac); got != 4096 {
		t.Errorf("expected 4096, got %d", got)
	}
}

func TestAgentPerceptInjectMaxByteCount_Zero_UsesDefault(t *testing.T) {
	cfg := Config{}
	ac := AgentConfig{Limits: &AgentLimits{PerceptInjectMaxBytes: intPtr(0)}}
	if got := cfg.AgentPerceptInjectMaxByteCount(ac); got != 2048 {
		t.Errorf("expected built-in default 2048, got %d", got)
	}
}

func TestAgentPerceptInjectMaxByteCount_Negative_Unlimited(t *testing.T) {
	cfg := Config{}
	ac := AgentConfig{Limits: &AgentLimits{PerceptInjectMaxBytes: intPtr(-1)}}
	if got := cfg.AgentPerceptInjectMaxByteCount(ac); got != 0 {
		t.Errorf("expected 0 (unlimited), got %d", got)
	}
}

func TestAgentPerceptRelevanceGateEnabled_NilLimits_DefaultTrue(t *testing.T) {
	cfg := Config{}
	ac := AgentConfig{}
	if got := cfg.AgentPerceptRelevanceGateEnabled(ac); !got {
		t.Error("expected default true when nil")
	}
}

func TestAgentPerceptRelevanceGateEnabled_Override_False(t *testing.T) {
	cfg := Config{}
	ac := AgentConfig{Limits: &AgentLimits{PerceptRelevanceGate: boolPtr(false)}}
	if got := cfg.AgentPerceptRelevanceGateEnabled(ac); got {
		t.Error("expected false from per-agent override")
	}
}

func TestAgentPerceptRelevanceGateEnabled_Override_True(t *testing.T) {
	cfg := Config{PerceptRelevanceGate: boolPtr(false)}
	ac := AgentConfig{Limits: &AgentLimits{PerceptRelevanceGate: boolPtr(true)}}
	if got := cfg.AgentPerceptRelevanceGateEnabled(ac); !got {
		t.Error("expected per-agent true to override global disabled")
	}
}

func TestAgentMaxToolIterations_Default(t *testing.T) {
	cfg := Config{}
	ac := AgentConfig{}
	if got := cfg.AgentMaxToolIterations(ac); got != 40 {
		t.Errorf("expected default 40 (no known window), got %d", got)
	}
}

func TestAgentMaxToolIterations_GlobalOverride(t *testing.T) {
	cfg := Config{LocalMaxToolIterations: 30}
	ac := AgentConfig{}
	if got := cfg.AgentMaxToolIterations(ac); got != 30 {
		t.Errorf("expected 30, got %d", got)
	}
}

func TestAgentMaxToolIterations_PerAgentOverride(t *testing.T) {
	cfg := Config{LocalMaxToolIterations: 30}
	ac := AgentConfig{Limits: &AgentLimits{MaxToolIterations: intPtr(5)}}
	if got := cfg.AgentMaxToolIterations(ac); got != 5 {
		t.Errorf("expected per-agent 5, got %d", got)
	}
}

func TestAgentMaxToolIterations_Unlimited(t *testing.T) {
	cfg := Config{LocalMaxToolIterations: -1}
	ac := AgentConfig{}
	if got := cfg.AgentMaxToolIterations(ac); got != 0 {
		t.Errorf("expected 0 (unlimited) for -1 global, got %d", got)
	}
}

func TestAgentMaxToolIterations_PerAgentUnlimited(t *testing.T) {
	cfg := Config{}
	ac := AgentConfig{Limits: &AgentLimits{MaxToolIterations: intPtr(-1)}}
	if got := cfg.AgentMaxToolIterations(ac); got != 0 {
		t.Errorf("expected 0 (unlimited) for per-agent -1, got %d", got)
	}
}

func TestAgentReturningFreshStartLocalTurns_Default(t *testing.T) {
	cfg := Config{}
	ac := AgentConfig{}
	if got := cfg.AgentReturningFreshStartLocalTurns(ac); got != 8 {
		t.Errorf("expected default 8, got %d", got)
	}
}

func TestAgentReturningFreshStartLocalTurns_GlobalOverride(t *testing.T) {
	cfg := Config{ReturningFreshStartLocalTurns: 5}
	ac := AgentConfig{}
	if got := cfg.AgentReturningFreshStartLocalTurns(ac); got != 5 {
		t.Errorf("expected global 5, got %d", got)
	}
}

func TestAgentReturningFreshStartLocalTurns_PerAgent(t *testing.T) {
	cfg := Config{ReturningFreshStartLocalTurns: 5}
	ac := AgentConfig{Limits: &AgentLimits{ReturningFreshStartLocalTurns: intPtr(3)}}
	if got := cfg.AgentReturningFreshStartLocalTurns(ac); got != 3 {
		t.Errorf("expected per-agent 3, got %d", got)
	}
}

func TestAgentReturningFreshStartLocalTurns_Disabled(t *testing.T) {
	cfg := Config{}
	ac := AgentConfig{Limits: &AgentLimits{ReturningFreshStartLocalTurns: intPtr(-1)}}
	if got := cfg.AgentReturningFreshStartLocalTurns(ac); got != 0 {
		t.Errorf("expected 0 (disabled) for -1 per-agent, got %d", got)
	}
}

// --- EffectiveToolAgents tests ---

func TestEffectiveToolAgents_NoToolsConfigured(t *testing.T) {
	cfg := Config{
		Agents: []AgentConfig{{Name: "local", URL: "http://local", Model: "m"}},
	}
	got := cfg.EffectiveToolAgents("local")
	if len(got) != 0 {
		t.Errorf("expected empty list, got %v", got)
	}
}

func TestEffectiveToolAgents_GlobalOnly(t *testing.T) {
	cfg := Config{
		Agents: []AgentConfig{
			{Name: "primary", URL: "http://primary", Model: "m"},
			{Name: "helper", URL: "http://helper", Model: "m2"},
		},
		AgentTools: []AgentToolEntry{
			{Agent: "helper", Description: "A helper agent"},
		},
	}
	got := cfg.EffectiveToolAgents("primary")
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d: %v", len(got), got)
	}
	if got[0].Agent != "helper" || got[0].Description != "A helper agent" {
		t.Errorf("unexpected entry: %+v", got[0])
	}
}

func TestEffectiveToolAgents_PerAgentShadowsGlobal(t *testing.T) {
	cfg := Config{
		Agents: []AgentConfig{
			{
				Name:  "primary",
				URL:   "http://primary",
				Model: "m",
				Tools: []AgentToolEntry{
					{Agent: "helper", Description: "overridden description"},
				},
			},
			{Name: "helper", URL: "http://helper", Model: "m2"},
		},
		AgentTools: []AgentToolEntry{
			{Agent: "helper", Description: "global description"},
		},
	}
	got := cfg.EffectiveToolAgents("primary")
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d: %v", len(got), got)
	}
	if got[0].Description != "overridden description" {
		t.Errorf("expected per-agent override, got %q", got[0].Description)
	}
}

func TestEffectiveToolAgents_PerAgentDisablesGlobal(t *testing.T) {
	disabled := false
	cfg := Config{
		Agents: []AgentConfig{
			{
				Name:  "primary",
				URL:   "http://primary",
				Model: "m",
				Tools: []AgentToolEntry{
					{Agent: "helper", Description: "desc", Enabled: &disabled},
				},
			},
			{Name: "helper", URL: "http://helper", Model: "m2"},
		},
		AgentTools: []AgentToolEntry{
			{Agent: "helper", Description: "global description"},
		},
	}
	got := cfg.EffectiveToolAgents("primary")
	if len(got) != 0 {
		t.Errorf("expected empty list (disabled), got %v", got)
	}
}

func TestEffectiveToolAgents_PerAgentAddsEntry(t *testing.T) {
	cfg := Config{
		Agents: []AgentConfig{
			{
				Name:  "primary",
				URL:   "http://primary",
				Model: "m",
				Tools: []AgentToolEntry{
					{Agent: "extra", Description: "extra agent"},
				},
			},
			{Name: "extra", URL: "http://extra", Model: "m2"},
		},
	}
	got := cfg.EffectiveToolAgents("primary")
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d: %v", len(got), got)
	}
	if got[0].Agent != "extra" {
		t.Errorf("expected extra, got %q", got[0].Agent)
	}
}

func TestEffectiveToolAgents_CycleGuard(t *testing.T) {
	cfg := Config{
		Agents: []AgentConfig{
			{Name: "primary", URL: "http://primary", Model: "m"},
		},
		AgentTools: []AgentToolEntry{
			{Agent: "primary", Description: "self-call"},
		},
	}
	got := cfg.EffectiveToolAgents("primary")
	if len(got) != 0 {
		t.Errorf("cycle guard: expected empty list, got %v", got)
	}
}

func TestEffectiveToolAgents_UnknownAgentDropped(t *testing.T) {
	cfg := Config{
		Agents: []AgentConfig{
			{Name: "primary", URL: "http://primary", Model: "m"},
		},
		AgentTools: []AgentToolEntry{
			{Agent: "nonexistent", Description: "ghost agent"},
		},
	}
	got := cfg.EffectiveToolAgents("primary")
	if len(got) != 0 {
		t.Errorf("expected unknown agent to be dropped, got %v", got)
	}
}

func TestEffectiveMaxBackgroundAgents_DefaultsWhenUnset(t *testing.T) {
	cfg := Config{}
	if got := cfg.EffectiveMaxBackgroundAgents(); got != 3 {
		t.Errorf("expected default 3, got %d", got)
	}
}

func TestEffectiveMaxBackgroundAgents_ExplicitValueHonored(t *testing.T) {
	cfg := Config{MaxBackgroundAgents: 7}
	if got := cfg.EffectiveMaxBackgroundAgents(); got != 7 {
		t.Errorf("expected 7, got %d", got)
	}
}

func TestEffectiveMaxBackgroundAgents_NonPositiveFallsBackToDefault(t *testing.T) {
	cfg := Config{MaxBackgroundAgents: -1}
	if got := cfg.EffectiveMaxBackgroundAgents(); got != 3 {
		t.Errorf("expected non-positive value to fall back to default 3, got %d", got)
	}
}

func TestEffectiveBackgroundAgentTimeout_DefaultsWhenUnset(t *testing.T) {
	cfg := Config{}
	if got := cfg.EffectiveBackgroundAgentTimeout(); got != 20*time.Minute {
		t.Errorf("expected default 20m, got %v", got)
	}
}

func TestEffectiveBackgroundAgentTimeout_ExplicitValueHonored(t *testing.T) {
	cfg := Config{BackgroundAgentTimeoutMinutes: 45}
	if got := cfg.EffectiveBackgroundAgentTimeout(); got != 45*time.Minute {
		t.Errorf("expected 45m, got %v", got)
	}
}

func TestEffectiveBackgroundAgentTimeout_NonPositiveFallsBackToDefault(t *testing.T) {
	cfg := Config{BackgroundAgentTimeoutMinutes: -1}
	if got := cfg.EffectiveBackgroundAgentTimeout(); got != 20*time.Minute {
		t.Errorf("expected non-positive value to fall back to default 20m, got %v", got)
	}
}

// --- Sprint 4: context_window_tokens auto-derivation tests ---

func TestAgentContextWindowTokens_Unset(t *testing.T) {
	cfg := Config{}
	ac := AgentConfig{}
	if got := cfg.AgentContextWindowTokens(ac); got != 0 {
		t.Errorf("expected 0 when unset, got %d", got)
	}
}

func TestAgentContextWindowTokens_Set(t *testing.T) {
	cfg := Config{}
	ac := AgentConfig{ContextWindowTokens: 8192}
	if got := cfg.AgentContextWindowTokens(ac); got != 8192 {
		t.Errorf("expected 8192, got %d", got)
	}
}

// TestAgentMessageBudget table-driven tests for context_window_tokens auto-derivation.
func TestAgentMessageBudget_ContextWindowTokens(t *testing.T) {
	tests := []struct {
		name       string
		cfg        Config
		ac         AgentConfig
		wantBudget int
	}{
		{
			name:       "no context_window_tokens falls back to global default",
			cfg:        Config{},
			ac:         AgentConfig{},
			wantBudget: 24000, // global default
		},
		{
			name:       "context_window_tokens 8192 auto-derives 24576",
			cfg:        Config{},
			ac:         AgentConfig{ContextWindowTokens: 8192},
			wantBudget: 8192 * 3, // 24576
		},
		{
			name:       "context_window_tokens 32768 auto-derives 98304",
			cfg:        Config{},
			ac:         AgentConfig{ContextWindowTokens: 32768},
			wantBudget: 32768 * 3, // 98304
		},
		{
			name: "explicit limits.message_budget_chars overrides auto-derivation",
			cfg:  Config{},
			ac: AgentConfig{
				ContextWindowTokens: 8192,
				Limits:              &AgentLimits{MessageBudgetChars: intPtr(50000)},
			},
			wantBudget: 50000,
		},
		{
			name: "explicit limits.message_budget_chars=0 uses built-in default (not auto-derived)",
			cfg:  Config{},
			ac: AgentConfig{
				ContextWindowTokens: 8192,
				Limits:              &AgentLimits{MessageBudgetChars: intPtr(0)},
			},
			wantBudget: 24000, // intOr(0, 24000) returns 24000
		},
		{
			name:       "global local_context_budget_chars wins when no context_window_tokens",
			cfg:        Config{LocalContextBudgetChars: 10000},
			ac:         AgentConfig{},
			wantBudget: 10000,
		},
		{
			name:       "context_window_tokens wins over global local_context_budget_chars",
			cfg:        Config{LocalContextBudgetChars: 10000},
			ac:         AgentConfig{ContextWindowTokens: 8192},
			wantBudget: 8192 * 3, // auto-derive beats global fallback when ctw is set
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.AgentMessageBudget(tt.ac)
			if got != tt.wantBudget {
				t.Errorf("AgentMessageBudget() = %d, want %d", got, tt.wantBudget)
			}
		})
	}
}

// TestAgentMaxToolIterations table-driven tests for context_window_tokens cap.
func TestAgentMaxToolIterations_ContextWindowTokens(t *testing.T) {
	tests := []struct {
		name     string
		cfg      Config
		ac       AgentConfig
		wantIter int
	}{
		{
			name:     "no context_window_tokens falls back to default 40",
			cfg:      Config{},
			ac:       AgentConfig{},
			wantIter: 40,
		},
		{
			name:     "context_window_tokens 8192 → max(20, 8192/4096)=2 → floored to 20",
			cfg:      Config{},
			ac:       AgentConfig{ContextWindowTokens: 8192},
			wantIter: 20, // 8192/4096=2, max(20,2)=20 — a small-window model still gets a real budget
		},
		{
			name:     "context_window_tokens 32768 → max(20, 32768/4096)=20",
			cfg:      Config{},
			ac:       AgentConfig{ContextWindowTokens: 32768},
			wantIter: 20, // 32768/4096=8 → floored to 20 (was 8 — capable models were being cut off mid-task)
		},
		{
			name:     "context_window_tokens 65536 → max(20, 65536/4096)=20",
			cfg:      Config{},
			ac:       AgentConfig{ContextWindowTokens: 65536},
			wantIter: 20, // 65536/4096=16 → floored to 20
		},
		{
			name:     "context_window_tokens 131072 → max(20, 131072/4096)=32",
			cfg:      Config{},
			ac:       AgentConfig{ContextWindowTokens: 131072},
			wantIter: 32, // scaling above the floor unchanged
		},
		{
			name: "explicit limits.max_tool_iterations overrides auto-derivation",
			cfg:  Config{},
			ac: AgentConfig{
				ContextWindowTokens: 131072,
				Limits:              &AgentLimits{MaxToolIterations: intPtr(10)},
			},
			wantIter: 10,
		},
		{
			name:     "explicit global local_max_tool_iterations overrides auto-derivation",
			cfg:      Config{LocalMaxToolIterations: 15},
			ac:       AgentConfig{ContextWindowTokens: 131072},
			wantIter: 15,
		},
		{
			name: "per-agent -1 means unlimited (0), wins over context_window_tokens",
			cfg:  Config{},
			ac: AgentConfig{
				ContextWindowTokens: 131072,
				Limits:              &AgentLimits{MaxToolIterations: intPtr(-1)},
			},
			wantIter: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.AgentMaxToolIterations(tt.ac)
			if got != tt.wantIter {
				t.Errorf("AgentMaxToolIterations() = %d, want %d", got, tt.wantIter)
			}
		})
	}
}

// TestAgentIterationBudgetNotice pins the session-start advisory: it fires
// only for milk-loop agents that land on the built-in fallback with nothing
// defined, and stays silent when the window is known, the budget is forced,
// or the agent runs its own tool loop (claude-cli / subprocess).
func TestAgentIterationBudgetNotice(t *testing.T) {
	unknownModel := func() AgentConfig {
		return AgentConfig{Name: "qwen-local", Provider: "local", Model: "milk-test-unknown-model"}
	}
	tests := []struct {
		name      string
		cfg       Config
		ac        AgentConfig
		roleLabel string
		wantSub   string // "" = expect no notice
	}{
		{
			name:      "local agent, no window, no forced budget → notice with the fallback value",
			cfg:       Config{DisableModelsDevLookup: true},
			ac:        unknownModel(),
			roleLabel: "primary",
			wantSub:   "falls back to 40",
		},
		{
			name:      "unnamed agent is labelled by the caller's role",
			cfg:       Config{DisableModelsDevLookup: true},
			ac:        AgentConfig{Provider: "local", Model: "milk-test-unknown-model"},
			roleLabel: "escalation",
			wantSub:   `agent "escalation" has no known context window`,
		},
		{
			name:      "window known → silent (budget derives from it)",
			cfg:       Config{DisableModelsDevLookup: true},
			ac:        AgentConfig{Name: "qwen-local", Provider: "local", Model: "m", ContextWindowTokens: 32768},
			roleLabel: "primary",
		},
		{
			name: "explicit per-agent budget → silent (user already defined it)",
			cfg:  Config{DisableModelsDevLookup: true},
			ac: func() AgentConfig {
				a := unknownModel()
				a.Limits = &AgentLimits{MaxToolIterations: intPtr(-1)}
				return a
			}(),
			roleLabel: "primary",
		},
		{
			name:      "explicit global budget → silent",
			cfg:       Config{DisableModelsDevLookup: true, LocalMaxToolIterations: 60},
			ac:        unknownModel(),
			roleLabel: "primary",
		},
		{
			name:      "claude-cli escalation → silent (runs its own tool loop)",
			cfg:       Config{DisableModelsDevLookup: true},
			ac:        AgentConfig{Name: "claude", Provider: "claude-cli", Model: "claude-sonnet"},
			roleLabel: "escalation",
		},
		{
			name:      "subprocess agent → silent (runs its own tool loop)",
			cfg:       Config{DisableModelsDevLookup: true},
			ac:        AgentConfig{Name: "smol", Provider: "subprocess", Model: "x"},
			roleLabel: "primary",
		},
		{
			name:      "bearer cloud agent with unknown window → notice (the capable-model case)",
			cfg:       Config{DisableModelsDevLookup: true},
			ac:        AgentConfig{Name: "cloud", Provider: "bearer", Model: "milk-test-unknown-model"},
			roleLabel: "primary",
			wantSub:   "context_window_tokens",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.AgentIterationBudgetNotice(tt.ac, tt.roleLabel)
			if tt.wantSub == "" {
				if got != "" {
					t.Errorf("AgentIterationBudgetNotice() = %q, want no notice", got)
				}
				return
			}
			if !strings.Contains(got, tt.wantSub) {
				t.Errorf("AgentIterationBudgetNotice() = %q, want it to contain %q", got, tt.wantSub)
			}
		})
	}
}

func TestValidate_BothPromptAndPromptFile(t *testing.T) {
	cfg := Config{
		Agents: []AgentConfig{
			{
				Name:       "local",
				URL:        "http://localhost:8080",
				Model:      "qwen",
				Prompt:     "inline text",
				PromptFile: "/some/file.md",
			},
		},
	}
	warnings := Validate(cfg)
	found := false
	for _, w := range warnings {
		if w.Agent == "local" && containsSubstr(w.Message, "prompt_file takes precedence") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected warning about prompt_file taking precedence; got warnings: %v", warnings)
	}
}

func TestValidate_OnlyPromptNoWarning(t *testing.T) {
	cfg := Config{
		Agents: []AgentConfig{
			{
				Name:   "local",
				URL:    "http://localhost:8080",
				Model:  "qwen",
				Prompt: "inline text only",
			},
		},
	}
	warnings := Validate(cfg)
	for _, w := range warnings {
		if containsSubstr(w.Message, "prompt") {
			t.Errorf("unexpected prompt-related warning: %v", w)
		}
	}
}

func TestValidateMCPServerAuth_OAuth(t *testing.T) {
	cfg := Config{
		MCPServers: []MCPServerConfig{
			{
				Name: "my-oauth-server",
				URL:  "https://mcp.example.com",
				Auth: "oauth",
			},
		},
	}
	warnings := Validate(cfg)
	for _, w := range warnings {
		if containsSubstr(w.Message, "auth") {
			t.Errorf("unexpected auth-related warning for oauth: %v", w)
		}
	}
}

func TestValidateMCPServerAuth_InvalidRejectd(t *testing.T) {
	cfg := Config{
		MCPServers: []MCPServerConfig{
			{
				Name: "bad-server",
				URL:  "https://mcp.example.com",
				Auth: "magic",
			},
		},
	}
	warnings := Validate(cfg)
	found := false
	for _, w := range warnings {
		if containsSubstr(w.Message, "unknown auth") {
			found = true
		}
	}
	if !found {
		t.Error("expected 'unknown auth' warning for invalid auth value, got none")
	}
}

func TestValidateMCPServerAuth_KnownValues(t *testing.T) {
	knownAuths := []string{"", "none", "bearer", "token_cmd", "oauth"}
	for _, auth := range knownAuths {
		cfg := Config{
			MCPServers: []MCPServerConfig{
				{Name: "srv", URL: "https://example.com", Auth: auth},
			},
		}
		warnings := Validate(cfg)
		for _, w := range warnings {
			if containsSubstr(w.Message, "unknown auth") {
				t.Errorf("auth=%q produced unexpected 'unknown auth' warning: %v", auth, w)
			}
		}
	}
}

func containsSubstr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || findSub(s, sub))
}

func findSub(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestUpsertMCPServer_AppendsNew(t *testing.T) {
	cfg := &Config{}
	updated := UpsertMCPServer(cfg, MCPServerConfig{Name: "github", URL: "https://x"})
	if updated {
		t.Error("expected updated=false for a brand-new server")
	}
	if len(cfg.MCPServers) != 1 || cfg.MCPServers[0].Name != "github" {
		t.Errorf("MCPServers = %+v, want one entry named github", cfg.MCPServers)
	}
}

func TestUpsertMCPServer_ReplacesCaseInsensitive(t *testing.T) {
	cfg := &Config{MCPServers: []MCPServerConfig{{Name: "GitHub", URL: "https://old"}}}
	updated := UpsertMCPServer(cfg, MCPServerConfig{Name: "github", URL: "https://new"})
	if !updated {
		t.Error("expected updated=true for a case-insensitive name match")
	}
	if len(cfg.MCPServers) != 1 {
		t.Fatalf("MCPServers = %+v, want exactly one entry (replaced in place)", cfg.MCPServers)
	}
	if cfg.MCPServers[0].URL != "https://new" {
		t.Errorf("URL = %q, want %q", cfg.MCPServers[0].URL, "https://new")
	}
}

func TestUpsertMCPServer_NormalizesAuthNone(t *testing.T) {
	cfg := &Config{}
	UpsertMCPServer(cfg, MCPServerConfig{Name: "x", Auth: "None"})
	if cfg.MCPServers[0].Auth != "" {
		t.Errorf("Auth = %q, want empty (canonical form of \"none\")", cfg.MCPServers[0].Auth)
	}
}

func TestLoadFrom_ValidJSON_RefreshesBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"agent":"good"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFrom(path)
	if err != nil {
		t.Fatalf("LoadFrom() error = %v, want nil", err)
	}
	if cfg.Agent != "good" {
		t.Errorf("Agent = %q, want %q", cfg.Agent, "good")
	}

	bakData, err := os.ReadFile(path + configBackupSuffix)
	if err != nil {
		t.Fatalf("expected a backup file to be written, got: %v", err)
	}
	if string(bakData) != `{"agent":"good"}` {
		t.Errorf("backup content = %q, want the just-loaded bytes", bakData)
	}
}

func TestLoadFrom_InvalidJSON_NoBackup_ReturnsHardError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{not valid json`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := LoadFrom(path)
	if err == nil {
		t.Fatal("expected an error when config.json is invalid and no backup exists")
	}
	var recovered *ErrConfigRecovered
	if errors.As(err, &recovered) {
		t.Errorf("did not expect ErrConfigRecovered when no backup exists, got %v", err)
	}
}

func TestLoadFrom_InvalidJSON_WithBackup_Recovers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	// First load with valid content seeds the backup.
	if err := os.WriteFile(path, []byte(`{"agent":"good"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFrom(path); err != nil {
		t.Fatalf("seeding LoadFrom() error = %v, want nil", err)
	}

	// Simulate a bad automated edit: the primary file becomes invalid JSON.
	if err := os.WriteFile(path, []byte(`{"agent":"good",}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFrom(path)
	if err == nil {
		t.Fatal("expected a non-nil error (ErrConfigRecovered) when recovering from backup")
	}
	var recovered *ErrConfigRecovered
	if !errors.As(err, &recovered) {
		t.Fatalf("err = %v (%T), want *ErrConfigRecovered", err, err)
	}
	if recovered.Path != path {
		t.Errorf("recovered.Path = %q, want %q", recovered.Path, path)
	}
	if cfg.Agent != "good" {
		t.Errorf("recovered cfg.Agent = %q, want %q (from backup)", cfg.Agent, "good")
	}
}

func TestSave_RefreshesBackup(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := Save(Config{Agent: "saved"}); err != nil {
		t.Fatal(err)
	}
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	bakData, err := os.ReadFile(path + configBackupSuffix)
	if err != nil {
		t.Fatalf("expected Save to refresh the backup, got: %v", err)
	}
	var got Config
	if err := json.Unmarshal(bakData, &got); err != nil {
		t.Fatal(err)
	}
	if got.Agent != "saved" {
		t.Errorf("backup Agent = %q, want %q", got.Agent, "saved")
	}
}

func TestLoad_RecoversFromBackupEndToEnd(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := Save(Config{Agent: "good"}); err != nil {
		t.Fatal(err)
	}
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"agent":"good",}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load()
	var recovered *ErrConfigRecovered
	if !errors.As(err, &recovered) {
		t.Fatalf("Load() err = %v, want *ErrConfigRecovered", err)
	}
	if cfg.Agent != "good" {
		t.Errorf("recovered cfg.Agent = %q, want %q", cfg.Agent, "good")
	}
}

// --- DeepMerge tests ---

func TestDeepMerge_ScalarsOverride(t *testing.T) {
	global := Config{Agent: "global-agent", Colorization: "balanced", ContextBudgetChars: 5000}
	local := Config{Agent: "local-agent", ContextBudgetChars: 8000}
	merged := DeepMerge(global, local)
	if merged.Agent != "local-agent" {
		t.Errorf("expected local-agent, got %q", merged.Agent)
	}
	if merged.ContextBudgetChars != 8000 {
		t.Errorf("expected 8000, got %d", merged.ContextBudgetChars)
	}
	// Unset local field should keep global.
	if merged.Colorization != "balanced" {
		t.Errorf("expected global Colorization preserved, got %q", merged.Colorization)
	}
}

func TestDeepMerge_PointerOverride(t *testing.T) {
	global := Config{}
	local := Config{ShowReasoning: boolPtr(true)}
	merged := DeepMerge(global, local)
	if merged.ShowReasoning == nil || !*merged.ShowReasoning {
		t.Error("expected ShowReasoning=true from local")
	}
}

func TestDeepMerge_PointerNilKeepsGlobal(t *testing.T) {
	global := Config{ShowReasoning: boolPtr(true)}
	local := Config{}
	merged := DeepMerge(global, local)
	if merged.ShowReasoning == nil || !*merged.ShowReasoning {
		t.Error("expected ShowReasoning=true preserved from global")
	}
}

func TestDeepMerge_MCPServersMergeByName(t *testing.T) {
	enabled := true
	global := Config{MCPServers: []MCPServerConfig{
		{Name: "keep", URL: "http://keep"},
		{Name: "override", URL: "http://old"},
	}}
	local := Config{MCPServers: []MCPServerConfig{
		{Name: "override", URL: "http://new", Enabled: &enabled},
		{Name: "add", URL: "http://add"},
	}}
	merged := DeepMerge(global, local)
	if len(merged.MCPServers) != 3 {
		t.Fatalf("expected 3 servers, got %d", len(merged.MCPServers))
	}
	// "keep" preserved
	if merged.MCPServers[0].URL != "http://keep" {
		t.Errorf("keep URL = %q", merged.MCPServers[0].URL)
	}
	// "override" replaced
	if merged.MCPServers[1].URL != "http://new" {
		t.Errorf("override URL = %q", merged.MCPServers[1].URL)
	}
	// "add" appended
	if merged.MCPServers[2].Name != "add" {
		t.Errorf("add name = %q", merged.MCPServers[2].Name)
	}
}

func TestDeepMerge_AgentsMergeByName(t *testing.T) {
	global := Config{Agents: []AgentConfig{
		{Name: "keep", URL: "http://keep"},
		{Name: "override", URL: "http://old", Model: "old-model"},
	}}
	local := Config{Agents: []AgentConfig{
		{Name: "override", URL: "http://new", Model: "new-model"},
	}}
	merged := DeepMerge(global, local)
	if len(merged.Agents) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(merged.Agents))
	}
	if merged.Agents[1].Model != "new-model" {
		t.Errorf("override model = %q", merged.Agents[1].Model)
	}
}

func TestDeepMerge_RulesOverride(t *testing.T) {
	global := Config{Rules: Rules{EscalateAboveTokens: 2000, ClassifierFallback: "local"}}
	local := Config{Rules: Rules{ClassifierFallback: "claude"}}
	merged := DeepMerge(global, local)
	if merged.Rules.ClassifierFallback != "claude" {
		t.Errorf("expected claude, got %q", merged.Rules.ClassifierFallback)
	}
	if merged.Rules.EscalateAboveTokens != 2000 {
		t.Errorf("expected 2000 preserved, got %d", merged.Rules.EscalateAboveTokens)
	}
}

func TestDeepMerge_NilSlicesKeepGlobal(t *testing.T) {
	global := Config{MCPServers: []MCPServerConfig{{Name: "s1"}}, Agents: []AgentConfig{{Name: "a1"}}}
	local := Config{} // nil slices
	merged := DeepMerge(global, local)
	if len(merged.MCPServers) != 1 {
		t.Errorf("expected 1 MCP server preserved, got %d", len(merged.MCPServers))
	}
	if len(merged.Agents) != 1 {
		t.Errorf("expected 1 agent preserved, got %d", len(merged.Agents))
	}
}

// --- LoadWithLocal tests ---

func TestLoadWithLocal_NoLocalConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MILK_CONFIG", filepath.Join(dir, "config.json"))
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"agent":"global"}`), 0o644)

	// Ensure no .milk dir in cwd (use temp dir as cwd).
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	global, local, merged, hasLocal, err := LoadWithLocal()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hasLocal {
		t.Error("expected hasLocal=false")
	}
	if global.Agent != "global" {
		t.Errorf("global agent = %q", global.Agent)
	}
	if local.Agent != "" {
		t.Errorf("expected empty local, got agent %q", local.Agent)
	}
	if merged.Agent != "global" {
		t.Errorf("merged agent = %q", merged.Agent)
	}
}

func TestLoadWithLocal_WithLocalOverride(t *testing.T) {
	dir := t.TempDir()
	globalPath := filepath.Join(dir, "config.json")
	os.WriteFile(globalPath, []byte(`{"agent":"global","context_budget_chars":5000}`), 0o644)
	t.Setenv("MILK_CONFIG", globalPath)

	// Create .milk/config.json in a subdir, then chdir there.
	localDir := filepath.Join(dir, "project")
	milkDir := filepath.Join(localDir, ".milk")
	os.MkdirAll(milkDir, 0o700)
	os.WriteFile(filepath.Join(milkDir, "config.json"), []byte(`{"agent":"local","context_budget_chars":8000}`), 0o644)

	orig, _ := os.Getwd()
	os.Chdir(localDir)
	defer os.Chdir(orig)

	global, local, merged, hasLocal, err := LoadWithLocal()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hasLocal {
		t.Error("expected hasLocal=true")
	}
	if global.Agent != "global" {
		t.Errorf("global agent = %q", global.Agent)
	}
	if local.Agent != "local" {
		t.Errorf("local agent = %q", local.Agent)
	}
	if merged.Agent != "local" {
		t.Errorf("merged agent = %q", merged.Agent)
	}
	if merged.ContextBudgetChars != 8000 {
		t.Errorf("merged context_budget = %d", merged.ContextBudgetChars)
	}
}

func TestLoadWithLocal_LocalPartialOverride(t *testing.T) {
	dir := t.TempDir()
	globalPath := filepath.Join(dir, "config.json")
	os.WriteFile(globalPath, []byte(`{"agent":"global","context_budget_chars":5000,"colorization":"balanced"}`), 0o644)
	t.Setenv("MILK_CONFIG", globalPath)

	localDir := filepath.Join(dir, "project")
	milkDir := filepath.Join(localDir, ".milk")
	os.MkdirAll(milkDir, 0o700)
	// Only override agent, leave rest to inherit.
	os.WriteFile(filepath.Join(milkDir, "config.json"), []byte(`{"agent":"local"}`), 0o644)

	orig, _ := os.Getwd()
	os.Chdir(localDir)
	defer os.Chdir(orig)

	_, _, merged, hasLocal, err := LoadWithLocal()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hasLocal {
		t.Error("expected hasLocal=true")
	}
	if merged.Agent != "local" {
		t.Errorf("merged agent = %q", merged.Agent)
	}
	if merged.ContextBudgetChars != 5000 {
		t.Errorf("expected inherited 5000, got %d", merged.ContextBudgetChars)
	}
	if merged.Colorization != "balanced" {
		t.Errorf("expected inherited balanced, got %q", merged.Colorization)
	}
}

// --- SaveLocal tests ---

func TestSaveLocal_CreatesDirAndFile(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	cfg := Config{Agent: "local-agent"}
	if err := SaveLocal(cfg); err != nil {
		t.Fatalf("SaveLocal: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".milk", "config.json"))
	if err != nil {
		t.Fatalf("read local config: %v", err)
	}
	var got Config
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Agent != "local-agent" {
		t.Errorf("agent = %q", got.Agent)
	}
}

func TestSaveScope_Local(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(orig)

	cfg := Config{Agent: "scoped"}
	if err := SaveScope(cfg, "local"); err != nil {
		t.Fatalf("SaveScope local: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".milk", "config.json")); err != nil {
		t.Fatal("local config file not created")
	}
}

// TestLoopDetectionCfg_ConvertsEveryField guards against the class of bug
// where a documented, implemented loop-detection knob (ReasoningChunkFloodThreshold
// — real signal in internal/loop, shown in CLAUDE.md's example config) had no
// corresponding field on config.LoopDetectionConfig, so any value a user set
// for it in config.json was silently dropped by json.Unmarshal and the
// detector always fell back to its hardcoded default. One assertion per
// field so a future field added to loop.Config without a matching line here
// (and in LoopDetectionCfg itself) fails loudly instead of silently.
func TestLoopDetectionCfg_ConvertsEveryField(t *testing.T) {
	enabled := true
	similarity := 0.42
	velocityThreshold := int64(123)
	silentBurn := int64(456)
	autoInterrupt := true

	cfg := Config{
		LoopDetection: &LoopDetectionConfig{
			Enabled:                                 &enabled,
			MaxConsecutiveSimilarResponses:          11,
			ResponseSimilarityThreshold:             &similarity,
			TokenVelocityWindowSeconds:              22,
			TokenVelocityThreshold:                  &velocityThreshold,
			MaxSilentBurnTokens:                     &silentBurn,
			MaxConsecutiveTurnsWithoutUser:          33,
			ToolEchoThreshold:                       44,
			ChunkRepetitionThreshold:                55,
			ChunkWindowSize:                         66,
			ReasoningChunkFloodThreshold:            77,
			ReasoningChunkRepetitionThreshold:       88,
			ChunkRepetitionMinScatteredLength:       99,
			ReasoningMaxConsecutiveSimilarResponses: 111,
			AutoInterrupt:                           &autoInterrupt,
		},
	}

	got := cfg.LoopDetectionCfg()
	want := loop.Config{
		Enabled:                                 true,
		MaxConsecutiveSimilarResponses:          11,
		ResponseSimilarityThreshold:             0.42,
		TokenVelocitySeconds:                    22,
		TokenVelocityThreshold:                  123,
		MaxSilentBurnTokens:                     456,
		MaxConsecutiveTurnsWithoutUser:          33,
		ToolEchoThreshold:                       44,
		ChunkRepetitionThreshold:                55,
		ChunkWindowSize:                         66,
		ReasoningChunkFloodThreshold:            77,
		ReasoningChunkRepetitionThreshold:       88,
		ChunkRepetitionMinScatteredLength:       99,
		ReasoningMaxConsecutiveSimilarResponses: 111,
		AutoInterrupt:                           true,
	}
	if got != want {
		t.Errorf("LoopDetectionCfg() = %+v, want %+v", got, want)
	}
}

func TestAgentToolResultMaxByteCount_DerivedFromWindow(t *testing.T) {
	cfg := Config{DisableModelsDevLookup: true}
	for _, tc := range []struct {
		window int
		want   int
	}{
		{0, 20000},          // unknown window: the conservative built-in
		{8000, 4096},        // floor
		{32000, 12800},      // ~10% of the window
		{128000, 50 * 1024}, // capped at the standard 50 KB
		{1000000, 50 * 1024},
	} {
		ac := AgentConfig{ContextWindowTokens: tc.window}
		if got := cfg.AgentToolResultMaxByteCount(ac); got != tc.want {
			t.Errorf("window %d: got %d, want %d", tc.window, got, tc.want)
		}
	}
	// Explicit settings still win over the derived value.
	ac := AgentConfig{ContextWindowTokens: 1000000, Limits: &AgentLimits{ToolResultMaxBytes: intPtr(8000)}}
	if got := cfg.AgentToolResultMaxByteCount(ac); got != 8000 {
		t.Errorf("explicit limit: got %d, want 8000", got)
	}
	cfg.LocalToolResultMaxBytes = 30000
	if got := cfg.AgentToolResultMaxByteCount(AgentConfig{ContextWindowTokens: 1000000}); got != 30000 {
		t.Errorf("global setting: got %d, want 30000", got)
	}
}

func TestAgentCompactionTriggerTokens(t *testing.T) {
	cfg := Config{DisableModelsDevLookup: true}
	if got := cfg.AgentCompactionTriggerTokens(AgentConfig{}); got != 0 {
		t.Errorf("unknown window must leave the token trigger off, got %d", got)
	}
	// 1M window, default 20K reserve: 0.85 * 980K.
	if got, want := cfg.AgentCompactionTriggerTokens(AgentConfig{ContextWindowTokens: 1000000}), 833000; got != want {
		t.Errorf("1M window: got %d, want %d", got, want)
	}
	// Small window: the reserve is clamped to a quarter of it.
	if got, want := cfg.AgentCompactionTriggerTokens(AgentConfig{ContextWindowTokens: 8000}), 5100; got != want {
		t.Errorf("8K window: got %d, want %d", got, want)
	}
	explicit := AgentConfig{ContextWindowTokens: 1000000, Limits: &AgentLimits{CompactionTriggerTokens: intPtr(300000)}}
	if got := cfg.AgentCompactionTriggerTokens(explicit); got != 300000 {
		t.Errorf("explicit: got %d", got)
	}
	explicit.Limits.CompactionTriggerTokens = intPtr(-1)
	if got := cfg.AgentCompactionTriggerTokens(explicit); got != 0 {
		t.Errorf("negative disables the token trigger, got %d", got)
	}
}

func TestAgentCompactionTriggerTokens_UsesCatalogOutputLimit(t *testing.T) {
	// mimo-v2.5-pro: 1048576 context, 131072 output in the embedded catalog —
	// the reserve is capped at 32K, not the full 128K output limit.
	cfg := Config{}
	got := cfg.AgentCompactionTriggerTokens(AgentConfig{Model: "mimo-v2.5-pro"})
	if want := (1048576 - 32000) * 85 / 100; got != want {
		t.Errorf("got %d, want %d", got, want)
	}
}

func TestAgentMaxPayloadBytes_FollowsWindow(t *testing.T) {
	cfg := Config{DisableModelsDevLookup: true}
	if got := cfg.AgentMaxPayloadBytes(AgentConfig{}); got != DefaultMaxPayloadBytes {
		t.Errorf("no window: got %d", got)
	}
	if got := cfg.AgentMaxPayloadBytes(AgentConfig{ContextWindowTokens: 128000}); got != DefaultMaxPayloadBytes {
		t.Errorf("128K window must keep the proxy-safe default, got %d", got)
	}
	if got := cfg.AgentMaxPayloadBytes(AgentConfig{ContextWindowTokens: 1000000}); got != 4000000 {
		t.Errorf("1M window: got %d, want 4000000", got)
	}
	ac := AgentConfig{ContextWindowTokens: 1000000, Limits: &AgentLimits{MaxPayloadBytes: intPtr(500000)}}
	if got := cfg.AgentMaxPayloadBytes(ac); got != 500000 {
		t.Errorf("explicit must win, got %d", got)
	}
}

// TestPermissionsTimedAnswerDefaults (ADR-0052): the timed answer defaults to
// 360s / deny for both ask kinds, and the safety default is warnable.
func TestPermissionsTimedAnswerDefaults(t *testing.T) {
	cfg := Config{}
	if got := cfg.PermBackgroundTimeout(); got != 360*time.Second {
		t.Errorf("PermBackgroundTimeout = %s, want 360s", got)
	}
	if got := cfg.PermSafetyTimeout(); got != 360*time.Second {
		t.Errorf("PermSafetyTimeout = %s, want 360s", got)
	}
	if cfg.PermBackgroundAllow() || cfg.PermSafetyAllow() {
		t.Error("both timed defaults must be deny out of the box")
	}

	cfg.Permissions = &PermissionsConfig{
		BackgroundTimeoutSecs: 90,
		BackgroundDefault:     "allow",
		SafetyTimeoutSecs:     120,
		SafetyDefault:         "deny",
	}
	if got := cfg.PermBackgroundTimeout(); got != 90*time.Second {
		t.Errorf("PermBackgroundTimeout = %s, want 90s", got)
	}
	if !cfg.PermBackgroundAllow() {
		t.Error("BackgroundDefault allow must surface")
	}
	if got := cfg.PermSafetyTimeout(); got != 120*time.Second {
		t.Errorf("PermSafetyTimeout = %s, want 120s", got)
	}

	ws := Validate(cfg)
	var sawSafetyWarn bool
	for _, w := range ws {
		if strings.Contains(w.Message, "safety_default is \"allow\"") {
			sawSafetyWarn = true
		}
	}
	if sawSafetyWarn {
		t.Error("safety_default deny must not warn")
	}
	cfg.Permissions.SafetyDefault = "allow"
	ws = Validate(cfg)
	sawSafetyWarn = false
	for _, w := range ws {
		if strings.Contains(w.Message, "safety_default is \"allow\"") {
			sawSafetyWarn = true
		}
	}
	if !sawSafetyWarn {
		t.Error("safety_default allow must warn at startup")
	}
}

// TestInitConfig_GeneratedFileIncludesPermissions pins the "generated config
// is self-documenting" contract: the permissions knobs (ADR-0052) must be
// written out with their default values, not hidden behind code defaults —
// same convention as the prepopulated Otel/Rules blocks.
func TestInitConfig_GeneratedFileIncludesPermissions(t *testing.T) {
	cfg := InitConfig(AgentConfig{Name: "primary", Provider: "local", URL: "http://x", Model: "m"}, nil)
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := string(data)
	for _, want := range []string{
		`"permissions"`,
		`"background_timeout_secs": 360`,
		`"background_default": "deny"`,
		`"safety_timeout_secs": 360`,
		`"safety_default": "deny"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("generated config missing %s", want)
		}
	}
	if got := cfg.PermBackgroundTimeout(); got != 360*time.Second {
		t.Errorf("PermBackgroundTimeout = %v, want 360s", got)
	}
	if cfg.PermBackgroundAllow() || cfg.PermSafetyAllow() {
		t.Error("timed defaults must be deny")
	}
}

// TestInitConfig_GeneratedFileIncludesMemory pins the same self-documenting
// contract for the memory weight-model knobs (ADR-0090): the generated file
// must show them with the memory package's exact effective values.
func TestInitConfig_GeneratedFileIncludesMemory(t *testing.T) {
	cfg := InitConfig(AgentConfig{Name: "primary", Provider: "local", URL: "http://x", Model: "m"}, nil)
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := string(data)
	for _, want := range []string{
		`"memory"`,
		`"session_half_life_days": 3`,
		`"global_half_life_days": 60`,
		`"reconfirm_boost": 0.05`,
		`"reconfirm_cap": 0.9`,
		`"promote_threshold": 0.8`,
		`"prune_threshold": 0.2`,
		`"recency_boost": 0.1`,
		`"recency_half_life_days": 30`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("generated config missing %s", want)
		}
	}
	if cfg.Memory == nil || cfg.Memory.ReconfirmCap != 0.90 {
		t.Errorf("Memory block must be prepopulated with effective values, got %+v", cfg.Memory)
	}
}

// TestDir_RootOverride pins the --config-root sandbox: SetRoot (the flag's
// mechanism) and $MILK_CONFIG_ROOT must both redirect the state root, and the
// derived paths (config.json, sessions, memory, otel, mcp_oauth) must follow.
func TestDir_RootOverride(t *testing.T) {
	sandbox := t.TempDir()

	prev := SetRoot(sandbox)
	defer SetRoot(prev)

	dir, err := Dir()
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	if want, _ := filepath.Abs(sandbox); dir != want {
		t.Errorf("Dir() = %q, want %q", dir, want)
	}
	if got := filepath.Base(dir); got == ".milk" {
		t.Error("sandboxed Dir() must not be the default ~/.milk")
	}
	for _, derived := range []func() (string, error){OtelDir, MCPOAuthDir, ModelsDevCachePath} {
		p, err := derived()
		if err != nil {
			t.Fatalf("derived path: %v", err)
		}
		if !strings.HasPrefix(p, dir+string(filepath.Separator)) {
			t.Errorf("derived path %q must live under the state root %q", p, dir)
		}
	}
}

// TestDir_EnvFallback pins $MILK_CONFIG_ROOT (what child processes inherit)
// when no explicit override is set.
func TestDir_EnvFallback(t *testing.T) {
	sandbox := t.TempDir()
	t.Setenv("MILK_CONFIG_ROOT", sandbox)

	prev := SetRoot("")
	defer SetRoot(prev)

	dir, err := Dir()
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	if want, _ := filepath.Abs(sandbox); dir != want {
		t.Errorf("Dir() = %q, want %q", dir, want)
	}
}

// TestConfigPathLabel_FollowsStateRoot pins the display label: it must name
// the real state-root config.json (honoring $MILK_CONFIG_ROOT and SetRoot,
// the --config-root mechanism) — never a hardcoded ~/.milk claim.
func TestConfigPathLabel_FollowsStateRoot(t *testing.T) {
	envRoot := t.TempDir()
	t.Setenv("MILK_CONFIG_ROOT", envRoot)

	prev := SetRoot("")
	defer SetRoot(prev)

	if want, _ := filepath.Abs(filepath.Join(envRoot, "config.json")); ConfigPathLabel() != want {
		t.Errorf("ConfigPathLabel() = %q, want %q", ConfigPathLabel(), want)
	}

	flagRoot := t.TempDir()
	prev2 := SetRoot(flagRoot)
	defer SetRoot(prev2)
	if want, _ := filepath.Abs(filepath.Join(flagRoot, "config.json")); ConfigPathLabel() != want {
		t.Errorf("ConfigPathLabel() under SetRoot = %q, want %q", ConfigPathLabel(), want)
	}
}

// TestDirLabel_HomeCompressedFollowsStateRoot pins the banner's dir label:
// "~/.milk" byte-identical to the old hardcoded wording on the default root,
// absolute for any root outside home, "~/"-compressed for a root under home,
// and a bare fallback when nothing resolves — never a ~ claim for a
// redirected root (the live-tested $MILK_CONFIG_ROOT nit).
func TestDirLabel_HomeCompressedFollowsStateRoot(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}

	prev := SetRoot("")
	defer SetRoot(prev)
	t.Setenv("MILK_CONFIG_ROOT", "")

	if got, want := DirLabel(), "~/.milk"; got != want {
		t.Errorf("default DirLabel() = %q, want %q", got, want)
	}

	// Root outside home (SetRoot / --config-root) → absolute, never "~/…".
	flagRoot := t.TempDir()
	SetRoot(flagRoot)
	if got, want := DirLabel(), mustAbs(flagRoot); got != want {
		t.Errorf("SetRoot DirLabel() = %q, want %q", got, want)
	}
	SetRoot("")

	// Root under home → "~/"-compressed (statusCwd's pattern).
	child := filepath.Join(home, "milk-label-test")
	SetRoot(child)
	if got, want := DirLabel(), "~/milk-label-test"; got != want {
		t.Errorf("home-child DirLabel() = %q, want %q", got, want)
	}
	SetRoot("")

	// $MILK_CONFIG_ROOT → absolute as well.
	envRoot := t.TempDir()
	t.Setenv("MILK_CONFIG_ROOT", envRoot)
	if got, want := DirLabel(), mustAbs(envRoot); got != want {
		t.Errorf("env-root DirLabel() = %q, want %q", got, want)
	}
	t.Setenv("MILK_CONFIG_ROOT", "")

	// Unresolvable home with no override → bare fallback, not a guess.
	t.Setenv("HOME", "")
	if got, want := DirLabel(), ".milk"; got != want {
		t.Errorf("no-home DirLabel() = %q, want %q", got, want)
	}
}

// mustAbs mirrors Dir()'s filepath.Abs so tests compare the same normalization.
func mustAbs(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// TestLoadWithLocalIn_FollowsGivenDir pins the session-dir semantics of the
// project-local overlay: it resolves against the *given* dir (the bound
// session's cwd — the currently selected directory), not the process cwd.
// Resuming a session stored under another project must merge THAT project's
// .milk/config.json over the global base.
func TestLoadWithLocalIn_FollowsGivenDir(t *testing.T) {
	root := t.TempDir()
	globalPath := filepath.Join(root, "config.json")
	if err := os.WriteFile(globalPath, []byte(`{"agent":"global","context_budget_chars":5000}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Setenv("MILK_CONFIG", globalPath)

	mkProj := func(name, agent string) string {
		dir := filepath.Join(root, name)
		milkDir := filepath.Join(dir, ".milk")
		if err := os.MkdirAll(milkDir, 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(filepath.Join(milkDir, "config.json"), []byte(`{"agent":"`+agent+`"}`), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		return dir
	}
	projA := mkProj("projA", "agent-a")
	projB := mkProj("projB", "agent-b")

	// The process cwd is projA while the session lives under projB.
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(projA); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	defer os.Chdir(orig)

	merged, err := LoadMergedIn(projB)
	if err != nil {
		t.Fatalf("LoadMergedIn: %v", err)
	}
	if merged.Agent != "agent-b" {
		t.Errorf("merged agent = %q, want the given dir's overlay (agent-b) — the process-cwd overlay leaked in", merged.Agent)
	}
	if merged.ContextBudgetChars != 5000 {
		t.Errorf("merged context_budget = %d, want the global base value 5000", merged.ContextBudgetChars)
	}
	if !HasLocalConfigIn(projB) {
		t.Error("HasLocalConfigIn(projB) = false, want true")
	}

	// The empty-dir fallback is the process cwd (projA).
	_, _, m2, hasLocal, err := LoadWithLocalIn("")
	if err != nil || !hasLocal || m2.Agent != "agent-a" {
		t.Errorf("LoadWithLocalIn(\"\") = (agent=%q, hasLocal=%v, err=%v), want the process-cwd overlay agent-a", m2.Agent, hasLocal, err)
	}
}

func TestEscalationAgentIsImplicit(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"nothing configured", Config{}, true},
		{"only a primary configured", Config{Agents: []AgentConfig{{Name: "p", URL: "http://x"}}}, true},
		{"escalation_agent names a missing agent", Config{EscalationAgent: "ghost"}, true},
		{"escalation_agent claude", Config{EscalationAgent: "claude"}, false},
		{"claude entry configured", Config{Agents: []AgentConfig{{Name: "claude", Provider: "claude-cli"}}}, false},
		{"named inference agent", Config{Agents: []AgentConfig{{Name: "big", URL: "http://x"}}, EscalationAgent: "big"}, false},
	}
	for _, c := range cases {
		if got := c.cfg.EscalationAgentIsImplicit(); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCopyOnSelect_DefaultAndMerge(t *testing.T) {
	if (Config{}).CopyOnSelectEnabled() {
		t.Error("copy_on_select must default to off")
	}
	global := Config{CopyOnSelect: boolPtr(true)}
	merged := DeepMerge(global, Config{})
	if !merged.CopyOnSelectEnabled() {
		t.Error("expected global true preserved when local is unset")
	}
	merged = DeepMerge(global, Config{CopyOnSelect: boolPtr(false)})
	if merged.CopyOnSelectEnabled() {
		t.Error("expected local false to override global true")
	}
}

// --- Layered context reduction (issue #236) ---

func contextReductionEqual(got, want ContextReductionConfig) bool {
	return got.Mode == want.Mode &&
		got.InstructionThresholdPct == want.InstructionThresholdPct &&
		got.ToolCompactThresholdPct == want.ToolCompactThresholdPct &&
		got.ToolPruneThresholdPct == want.ToolPruneThresholdPct &&
		got.InstructionsTrimChars == want.InstructionsTrimChars &&
		strings.Join(got.ProtectedTools, ",") == strings.Join(want.ProtectedTools, ",")
}

func TestAgentContextReduction_Defaults(t *testing.T) {
	cfg := Config{DisableModelsDevLookup: true}
	want := ContextReductionConfig{
		Mode:                    "auto",
		InstructionThresholdPct: 55,
		ToolCompactThresholdPct: 65,
		ToolPruneThresholdPct:   75,
		InstructionsTrimChars:   4000,
		ProtectedTools:          []string{"bash", "read_file"},
	}
	cases := []struct {
		name string
		ac   *AgentConfig
	}{
		{"nil override", &AgentConfig{}},
		{"nil agent", nil},
		{"zero-value override", &AgentConfig{ContextReduction: &ContextReductionConfig{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cfg.AgentContextReduction(tc.ac)
			if !contextReductionEqual(got, want) {
				t.Errorf("AgentContextReduction() = %+v, want %+v", got, want)
			}
		})
	}
}

func TestAgentContextReduction_PerAgentOverrideRoundTrip(t *testing.T) {
	const agentJSON = `{
		"name": "reducer",
		"context_reduction": {
			"mode": "off",
			"instruction_threshold_pct": 40,
			"tool_compact_threshold_pct": 55,
			"tool_prune_threshold_pct": 90,
			"instructions_trim_chars": 1234,
			"protected_tools": ["bash", "grep"]
		}
	}`
	want := ContextReductionConfig{
		Mode:                    "off",
		InstructionThresholdPct: 40,
		ToolCompactThresholdPct: 55,
		ToolPruneThresholdPct:   90,
		InstructionsTrimChars:   1234,
		ProtectedTools:          []string{"bash", "grep"},
	}

	var ac AgentConfig
	if err := json.Unmarshal([]byte(agentJSON), &ac); err != nil {
		t.Fatalf("unmarshal agent entry: %v", err)
	}

	// Marshal + unmarshal again to prove the override survives a JSON round trip.
	data, err := json.Marshal(ac)
	if err != nil {
		t.Fatalf("marshal agent entry: %v", err)
	}
	var back AgentConfig
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("re-unmarshal agent entry: %v", err)
	}

	cfg := Config{DisableModelsDevLookup: true}
	for name, agent := range map[string]AgentConfig{"direct": ac, "round-tripped": back} {
		got := cfg.AgentContextReduction(&agent)
		if !contextReductionEqual(got, want) {
			t.Errorf("%s: AgentContextReduction() = %+v, want %+v", name, got, want)
		}
	}
}

func TestAgentContextReduction_ClampsAndOrdersThresholds(t *testing.T) {
	cfg := Config{DisableModelsDevLookup: true}
	cases := []struct {
		name string
		in   ContextReductionConfig
		want [3]int // instruction, tool_compact, tool_prune
	}{
		{"zeros take defaults", ContextReductionConfig{}, [3]int{55, 65, 75}},
		{"explicit in-order values kept", ContextReductionConfig{
			InstructionThresholdPct: 40, ToolCompactThresholdPct: 50, ToolPruneThresholdPct: 60,
		}, [3]int{40, 50, 60}},
		{"values above 99 clamped down", ContextReductionConfig{
			InstructionThresholdPct: 150, ToolCompactThresholdPct: 200, ToolPruneThresholdPct: 100,
		}, [3]int{99, 99, 99}},
		{"values below 1 clamped up", ContextReductionConfig{
			InstructionThresholdPct: -5, ToolCompactThresholdPct: -1, ToolPruneThresholdPct: 2,
		}, [3]int{1, 1, 2}},
		{"compact below instruction raised to instruction", ContextReductionConfig{
			InstructionThresholdPct: 80, ToolCompactThresholdPct: 30, ToolPruneThresholdPct: 20,
		}, [3]int{80, 80, 80}},
		{"prune below compact raised to compact", ContextReductionConfig{
			InstructionThresholdPct: 70, ToolCompactThresholdPct: 50, ToolPruneThresholdPct: 90,
		}, [3]int{70, 70, 90}},
		{"instruction at clamp ceiling drags the rest up", ContextReductionConfig{
			InstructionThresholdPct: 200,
		}, [3]int{99, 99, 99}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ac := AgentConfig{ContextReduction: &tc.in}
			got := cfg.AgentContextReduction(&ac)
			if got.InstructionThresholdPct != tc.want[0] ||
				got.ToolCompactThresholdPct != tc.want[1] ||
				got.ToolPruneThresholdPct != tc.want[2] {
				t.Errorf("thresholds = [%d %d %d], want [%d %d %d]",
					got.InstructionThresholdPct, got.ToolCompactThresholdPct, got.ToolPruneThresholdPct,
					tc.want[0], tc.want[1], tc.want[2])
			}
			if got.InstructionsTrimChars != 4000 {
				t.Errorf("InstructionsTrimChars = %d, want default 4000", got.InstructionsTrimChars)
			}
		})
	}
}

func TestAgentContextPressureDenominator(t *testing.T) {
	cfg := Config{DisableModelsDevLookup: true}
	cases := []struct {
		name   string
		window int
		want   int
	}{
		{"unknown window yields 0", 0, 0},
		{"8192 window: reserve 2048 (quarter) → 6144", 8192, 6144},
		{"32000 window: reserve 8000 (quarter) → 24000", 32000, 24000},
		{"1M window: reserve 20000 (default) → 980000", 1000000, 980000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ac := AgentConfig{ContextWindowTokens: tc.window}
			if got := cfg.AgentContextPressureDenominator(&ac); got != tc.want {
				t.Errorf("AgentContextPressureDenominator() = %d, want %d", got, tc.want)
			}
		})
	}
	// Catalog path: mimo-v2.5-pro has a 1048576 window and a 131072 output
	// limit in the embedded models.dev catalog — the reserve is capped at 32K.
	catalog := Config{}
	got := catalog.AgentContextPressureDenominator(&AgentConfig{Model: "mimo-v2.5-pro"})
	if want := 1048576 - 32000; got != want {
		t.Errorf("catalog denominator = %d, want %d", got, want)
	}
}

// TestAgentCompactionTriggerTokens_MatchesRefactoredReserve is the regression
// net for the shared output-reserve helper: the trigger keeps computing
// 0.85 × (window − reserve) — i.e. 0.85 × the pressure denominator — for
// every window shape, exactly as before the refactor.
func TestAgentCompactionTriggerTokens_MatchesRefactoredReserve(t *testing.T) {
	cfg := Config{DisableModelsDevLookup: true}
	hardcoded := map[int]int{
		0:       0, // unknown window: trigger stays off
		8192:    5222,
		32000:   20400,
		1000000: 833000,
	}
	for window, want := range hardcoded {
		ac := AgentConfig{ContextWindowTokens: window}
		if got := cfg.AgentCompactionTriggerTokens(ac); got != want {
			t.Errorf("window %d: AgentCompactionTriggerTokens() = %d, want %d", window, got, want)
		}
		if window > 0 {
			den := cfg.AgentContextPressureDenominator(&ac)
			if want2 := int(float64(den) * 0.85); want2 != want {
				t.Errorf("window %d: trigger %d disagrees with 0.85 × denominator %d (= %d)", window, want, den, want2)
			}
		}
	}
}

func TestContextReductionModeValidation(t *testing.T) {
	for _, mode := range []string{"", "auto", "off", "Auto", "OFF"} {
		var ac AgentConfig
		err := json.Unmarshal([]byte(`{"context_reduction": {"mode": "`+mode+`"}}`), &ac)
		if err != nil {
			t.Errorf("mode %q: unexpected error %v", mode, err)
		}
	}

	var ac AgentConfig
	err := json.Unmarshal([]byte(`{"context_reduction": {"mode": "sometimes"}}`), &ac)
	if err == nil {
		t.Fatal("expected an error for unknown context_reduction.mode")
	}
	if !strings.Contains(err.Error(), "mode") {
		t.Errorf("error %q should name the offending key", err)
	}

	// End to end: the same typo in a config file is a config-load error.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"agents":[{"name":"a","context_reduction":{"mode":"bogus"}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFrom(path); err == nil {
		t.Fatal("expected LoadFrom to reject unknown context_reduction.mode")
	}
}

func TestBusyIndicatorEnabled(t *testing.T) {
	if !((*RemoteOversightConfig)(nil)).BusyIndicatorEnabled() {
		t.Error("nil config: want default true")
	}
	if !(&RemoteOversightConfig{}).BusyIndicatorEnabled() {
		t.Error("unset: want default true")
	}
	on, off := true, false
	if !(&RemoteOversightConfig{BusyIndicator: &on}).BusyIndicatorEnabled() {
		t.Error("explicit true: want true")
	}
	if (&RemoteOversightConfig{BusyIndicator: &off}).BusyIndicatorEnabled() {
		t.Error("explicit false: want false")
	}
}

// --- issue #278: think_fold_default ---------------------------------------

func TestThinkFoldExpanded_Accessor(t *testing.T) {
	cases := []struct {
		v    string
		want bool
	}{
		{"", false},         // unset — the folded default, exactly as before
		{"folded", false},   // explicit default
		{"expanded", true},  // the knob
		{"garbage", false},  // unknown values are lenient: folded
		{"Expanded", false}, // case-sensitive, like other enums
	}
	for _, c := range cases {
		if got := (Config{ThinkFoldDefault: c.v}).ThinkFoldExpanded(); got != c.want {
			t.Errorf("think_fold_default=%q: ThinkFoldExpanded()=%v, want %v", c.v, got, c.want)
		}
	}
	// Zero-value config (every test fixture, fresh install) behaves as today.
	if (Config{}).ThinkFoldExpanded() {
		t.Error("zero-value config must seed folded")
	}
}

func TestDeepMerge_ThinkFoldDefault(t *testing.T) {
	// local overrides global
	merged := DeepMerge(Config{ThinkFoldDefault: "expanded"}, Config{ThinkFoldDefault: "folded"})
	if merged.ThinkFoldDefault != "folded" {
		t.Errorf("local override lost: %q", merged.ThinkFoldDefault)
	}
	// an absent local field never clobbers the base ("" = unset)
	merged = DeepMerge(Config{ThinkFoldDefault: "expanded"}, Config{})
	if merged.ThinkFoldDefault != "expanded" {
		t.Errorf("empty local clobbered the base: %q", merged.ThinkFoldDefault)
	}
	// both unset → default
	merged = DeepMerge(Config{}, Config{})
	if merged.ThinkFoldDefault != "" {
		t.Errorf("unset must stay empty: %q", merged.ThinkFoldDefault)
	}
}

func TestPanelAutoOpenEnabled(t *testing.T) {
	var c Config
	if !c.PanelAutoOpenEnabled() {
		t.Error("unset panel_auto_open should default to on")
	}
	off := false
	c.PanelAutoOpen = &off
	if c.PanelAutoOpenEnabled() {
		t.Error("panel_auto_open: false should be off")
	}
	on := true
	c.PanelAutoOpen = &on
	if !c.PanelAutoOpenEnabled() {
		t.Error("panel_auto_open: true should be on")
	}
}

func TestPanelAutoOpenJSONRoundTrip(t *testing.T) {
	var c Config
	if err := json.Unmarshal([]byte(`{"panel_auto_open": false}`), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.PanelAutoOpenEnabled() {
		t.Error("panel_auto_open: false did not survive the JSON tag")
	}
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"panel_auto_open":false`) {
		t.Errorf("marshal lost the key: %s", data)
	}
}

func TestDeepMerge_PanelAutoOpen(t *testing.T) {
	off := false
	on := true
	base := Config{PanelAutoOpen: &on}
	merged := DeepMerge(base, Config{PanelAutoOpen: &off})
	if merged.PanelAutoOpenEnabled() {
		t.Error("DeepMerge should overwrite panel_auto_open when the source sets it")
	}
	merged = DeepMerge(base, Config{})
	if !merged.PanelAutoOpenEnabled() {
		t.Error("DeepMerge must not touch panel_auto_open when the source leaves it unset")
	}
}
