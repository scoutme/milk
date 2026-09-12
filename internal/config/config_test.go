package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
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
	if got := cfg.AgentMaxToolIterations(ac); got != 20 {
		t.Errorf("expected default 20, got %d", got)
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
			name:     "no context_window_tokens falls back to default 20",
			cfg:      Config{},
			ac:       AgentConfig{},
			wantIter: 20,
		},
		{
			name:     "context_window_tokens 8192 → max(5, 8192/4096)=2 → clamped to 5",
			cfg:      Config{},
			ac:       AgentConfig{ContextWindowTokens: 8192},
			wantIter: 5, // 8192/4096=2, max(5,2)=5
		},
		{
			name:     "context_window_tokens 32768 → max(5, 32768/4096)=8",
			cfg:      Config{},
			ac:       AgentConfig{ContextWindowTokens: 32768},
			wantIter: 8, // 32768/4096=8
		},
		{
			name:     "context_window_tokens 131072 → max(5, 131072/4096)=32",
			cfg:      Config{},
			ac:       AgentConfig{ContextWindowTokens: 131072},
			wantIter: 32,
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
	global := Config{Agent: "global-agent", DefaultRoute: "local", ContextBudgetChars: 5000}
	local := Config{Agent: "local-agent", ContextBudgetChars: 8000}
	merged := DeepMerge(global, local)
	if merged.Agent != "local-agent" {
		t.Errorf("expected local-agent, got %q", merged.Agent)
	}
	if merged.ContextBudgetChars != 8000 {
		t.Errorf("expected 8000, got %d", merged.ContextBudgetChars)
	}
	// Unset local field should keep global.
	if merged.DefaultRoute != "local" {
		t.Errorf("expected global DefaultRoute preserved, got %q", merged.DefaultRoute)
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
