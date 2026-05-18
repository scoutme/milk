package memory

import (
	"context"
	"encoding/json"
	"testing"
)

func newToolsStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir(), "")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func TestDispatchRecordMemory_DefaultProducer(t *testing.T) {
	s := newToolsStore(t)
	result := DispatchRecordMemory(context.Background(), s, `{"content":"test fact"}`)

	var out map[string]any
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if _, ok := out["error"]; ok {
		t.Fatalf("unexpected error: %v", out["error"])
	}
	if len(s.global.Percepts) != 1 {
		t.Fatalf("expected 1 percept, got %d", len(s.global.Percepts))
	}
	if s.global.Percepts[0].Producer != ProducerLocal {
		t.Errorf("expected ProducerLocal, got %q", s.global.Percepts[0].Producer)
	}
}

func TestDispatchRecordMemory_UserProducer(t *testing.T) {
	s := newToolsStore(t)
	result := DispatchRecordMemory(context.Background(), s, `{"content":"user stated fact","producer":"user"}`)

	var out map[string]any
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if _, ok := out["error"]; ok {
		t.Fatalf("unexpected error: %v", out["error"])
	}
	if len(s.global.Percepts) != 1 {
		t.Fatalf("expected 1 percept, got %d", len(s.global.Percepts))
	}
	p := s.global.Percepts[0]
	if p.Producer != ProducerUser {
		t.Errorf("expected ProducerUser, got %q", p.Producer)
	}
	if p.W != 1.0 {
		t.Errorf("expected W=1.0 for user producer, got %v", p.W)
	}
}

func TestDispatchRecordMemory_ClaudeProducer(t *testing.T) {
	s := newToolsStore(t)
	result := DispatchRecordMemory(context.Background(), s, `{"content":"claude fact","producer":"claude"}`)

	var out map[string]any
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if _, ok := out["error"]; ok {
		t.Fatalf("unexpected error: %v", out["error"])
	}
	if len(s.global.Percepts) != 1 {
		t.Fatalf("expected 1 percept, got %d", len(s.global.Percepts))
	}
	if s.global.Percepts[0].Producer != ProducerClaude {
		t.Errorf("expected ProducerClaude, got %q", s.global.Percepts[0].Producer)
	}
}

func TestDispatchRecordMemory_EmptyContent(t *testing.T) {
	s := newToolsStore(t)
	result := DispatchRecordMemory(context.Background(), s, `{"content":""}`)

	var out map[string]any
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if _, ok := out["error"]; !ok {
		t.Errorf("expected error key in result for empty content, got: %v", out)
	}
}
