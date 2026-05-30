package memory

import "testing"

func makePercept(content string, w float64) Percept {
	return Percept{Content: content, W: w}
}

func TestFilterByRelevance_EmptyPromptPassesAll(t *testing.T) {
	percepts := []Percept{makePercept("routing logic in Go", 0.8), makePercept("memory store", 0.7)}
	got := FilterByRelevance(percepts, "")
	if len(got) != 2 {
		t.Errorf("expected 2, got %d", len(got))
	}
}

func TestFilterByRelevance_StopWordOnlyPromptPassesAll(t *testing.T) {
	percepts := []Percept{makePercept("routing logic", 0.8)}
	got := FilterByRelevance(percepts, "the a and in")
	if len(got) != 1 {
		t.Errorf("expected 1, got %d", len(got))
	}
}

func TestFilterByRelevance_DropsUnrelated(t *testing.T) {
	percepts := []Percept{
		makePercept("Go routing session state", 0.8),
		makePercept("AWS credentials bedrock region", 0.7),
	}
	got := FilterByRelevance(percepts, "routing session")
	if len(got) != 1 {
		t.Errorf("expected 1 (related), got %d", len(got))
	}
	if got[0].Content != "Go routing session state" {
		t.Errorf("wrong percept kept: %q", got[0].Content)
	}
}

func TestFilterByRelevance_KeepsAllMatching(t *testing.T) {
	percepts := []Percept{
		makePercept("memory consolidation NREM", 0.9),
		makePercept("memory percept decay", 0.8),
		makePercept("escalation agent prompt", 0.5),
	}
	got := FilterByRelevance(percepts, "memory percept")
	if len(got) != 2 {
		t.Errorf("expected 2, got %d", len(got))
	}
}

func TestLimitInjection_NoLimits(t *testing.T) {
	percepts := []Percept{makePercept("a", 1.0), makePercept("b", 0.9)}
	got := LimitInjection(percepts, 0, 0)
	if len(got) != 2 {
		t.Errorf("expected 2, got %d", len(got))
	}
}

func TestLimitInjection_CountCap(t *testing.T) {
	percepts := []Percept{makePercept("a", 1.0), makePercept("bb", 0.9), makePercept("ccc", 0.8)}
	got := LimitInjection(percepts, 2, 0)
	if len(got) != 2 {
		t.Errorf("expected 2, got %d", len(got))
	}
}

func TestLimitInjection_ByteCap(t *testing.T) {
	// "hello" = 5 bytes, "world" = 5 bytes; cap at 7 → only first fits
	percepts := []Percept{makePercept("hello", 1.0), makePercept("world", 0.9)}
	got := LimitInjection(percepts, 0, 7)
	if len(got) != 1 {
		t.Errorf("expected 1, got %d", len(got))
	}
	if got[0].Content != "hello" {
		t.Errorf("expected hello, got %q", got[0].Content)
	}
}

func TestLimitInjection_BothCaps_CountWins(t *testing.T) {
	percepts := []Percept{makePercept("hi", 1.0), makePercept("there", 0.9), makePercept("world", 0.8)}
	// bytes would allow 3 ("hi"=2, "there"=5, "world"=5 → 12 < 100), count caps at 2
	got := LimitInjection(percepts, 2, 100)
	if len(got) != 2 {
		t.Errorf("expected 2, got %d", len(got))
	}
}
