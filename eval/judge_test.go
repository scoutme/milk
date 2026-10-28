package eval

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mockJudgeServer creates an httptest.Server that returns a canned chat completion
// response containing the given judge scores JSON array.
func mockJudgeServer(scores []JudgeScore) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify it's a POST to the right endpoint.
		if r.Method != http.MethodPost {
			http.Error(w, "expected POST", http.StatusMethodNotAllowed)
			return
		}

		// Verify the request body parses.
		var req chatRequest
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad request body", http.StatusBadRequest)
			return
		}

		// Return the scores as the LLM's content.
		scoresJSON, _ := json.Marshal(scores)
		resp := chatResponse{
			Choices: []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			}{
				{Message: struct {
					Content string `json:"content"`
				}{Content: string(scoresJSON)}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
}

func TestJudgeScore_SingleTurn(t *testing.T) {
	expectedScores := []JudgeScore{
		{Criterion: "correctness", Score: 1.0, Reasoning: "Fixed the typo correctly."},
		{Criterion: "efficiency", Score: 4.0, Reasoning: "Used minimal tool calls."},
	}
	srv := mockJudgeServer(expectedScores)
	defer srv.Close()

	judge := &Judge{
		URL:        srv.URL,
		HTTPClient: srv.Client(),
	}

	scenario := Scenario{
		Name:        "fix-typo",
		Description: "Agent should fix a typo in a README.",
		Prompt:      "Fix the typo in README.md",
		Rubric: []RubricCriterion{
			{Criterion: "correctness", Description: "Did the agent fix the actual typo?", Weight: 3, Scoring: "binary"},
			{Criterion: "efficiency", Description: "Did the agent use minimal tool calls?", Weight: 1, Scoring: "scale_1_5"},
		},
	}
	results := []RunResult{
		{Response: "I fixed the typo: 'projcet' -> 'project'."},
	}

	scores, missing, err := judge.Score(context.Background(), scenario, results)
	if err != nil {
		t.Fatalf("Score() error: %v", err)
	}
	if len(scores) != 2 {
		t.Fatalf("got %d scores, want 2", len(scores))
	}
	if len(missing) != 0 {
		t.Errorf("missing = %v, want none (full rubric coverage)", missing)
	}
	if scores[0].Criterion != "correctness" || scores[0].Score != 1.0 {
		t.Errorf("scores[0] = %+v, want correctness=1.0", scores[0])
	}
	if scores[1].Criterion != "efficiency" || scores[1].Score != 4.0 {
		t.Errorf("scores[1] = %+v, want efficiency=4.0", scores[1])
	}
}

func TestJudgeScore_MultiTurn(t *testing.T) {
	// First call returns correctness=1, second call returns correctness=0.
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var scores []JudgeScore
		if callCount == 0 {
			scores = []JudgeScore{{Criterion: "correctness", Score: 1.0, Reasoning: "Correct refactor."}}
		} else {
			scores = []JudgeScore{{Criterion: "correctness", Score: 0.0, Reasoning: "Forgot validation."}}
		}
		callCount++

		scoresJSON, _ := json.Marshal(scores)
		resp := chatResponse{
			Choices: []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			}{
				{Message: struct {
					Content string `json:"content"`
				}{Content: string(scoresJSON)}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	judge := &Judge{URL: srv.URL, HTTPClient: srv.Client()}

	scenario := Scenario{
		Name:        "multi-turn-refactor",
		Description: "Refactor code across multiple turns.",
		Turns: []Turn{
			{
				Prompt: "Refactor auth.go to use a struct.",
				Rubric: []RubricCriterion{
					{Criterion: "correctness", Description: "Struct is correct.", Weight: 3, Scoring: "binary"},
				},
			},
			{
				Prompt: "Add input validation.",
				Rubric: []RubricCriterion{
					{Criterion: "correctness", Description: "Validation is correct.", Weight: 2, Scoring: "binary"},
				},
			},
		},
	}
	results := []RunResult{
		{Response: "Refactored to Auth struct."},
		{Response: "No changes needed."},
	}

	scores, missing, err := judge.Score(context.Background(), scenario, results)
	if err != nil {
		t.Fatalf("Score() error: %v", err)
	}
	if len(scores) != 2 {
		t.Fatalf("got %d scores, want 2", len(scores))
	}
	if len(missing) != 0 {
		t.Errorf("missing = %v, want none (full rubric coverage)", missing)
	}
	if scores[0].Score != 1.0 {
		t.Errorf("turn 0 score = %f, want 1.0", scores[0].Score)
	}
	if scores[1].Score != 0.0 {
		t.Errorf("turn 1 score = %f, want 0.0", scores[1].Score)
	}
}

func TestJudgeScore_WithCodeFences(t *testing.T) {
	// LLM wraps JSON in markdown code fences.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		content := "```json\n[{\"criterion\":\"correctness\",\"score\":1.0,\"reasoning\":\"Good.\"}]\n```"
		resp := chatResponse{
			Choices: []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			}{
				{Message: struct {
					Content string `json:"content"`
				}{Content: content}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	judge := &Judge{URL: srv.URL, HTTPClient: srv.Client()}
	scenario := Scenario{
		Prompt: "Test",
		Rubric: []RubricCriterion{
			{Criterion: "correctness", Description: "Test", Weight: 1, Scoring: "binary"},
		},
	}
	scores, missing, err := judge.Score(context.Background(), scenario, []RunResult{{Response: "ok"}})
	if err != nil {
		t.Fatalf("Score() error: %v", err)
	}
	if len(scores) != 1 || scores[0].Score != 1.0 {
		t.Errorf("got %+v, want [correctness=1.0]", scores)
	}
	if len(missing) != 0 {
		t.Errorf("missing = %v, want none", missing)
	}
}

func TestJudgeScore_ScoreClamping(t *testing.T) {
	// LLM returns out-of-range scores; judge should clamp them.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scoresJSON := `[{"criterion":"correctness","score":1.5,"reasoning":"over"},{"criterion":"quality","score":0.5,"reasoning":"under"}]`
		resp := chatResponse{
			Choices: []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			}{
				{Message: struct {
					Content string `json:"content"`
				}{Content: scoresJSON}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	judge := &Judge{URL: srv.URL, HTTPClient: srv.Client()}
	scenario := Scenario{
		Prompt: "Test",
		Rubric: []RubricCriterion{
			{Criterion: "correctness", Description: "Test", Weight: 1, Scoring: "binary"},
			{Criterion: "quality", Description: "Test", Weight: 1, Scoring: "scale_1_5"},
		},
	}
	scores, missing, err := judge.Score(context.Background(), scenario, []RunResult{{Response: "ok"}})
	if err != nil {
		t.Fatalf("Score() error: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("missing = %v, want none", missing)
	}
	if scores[0].Score != 1.0 {
		t.Errorf("binary score clamping: got %f, want 1.0", scores[0].Score)
	}
	if scores[1].Score != 1.0 {
		t.Errorf("scale_1_5 score clamping: got %f, want 1.0", scores[1].Score)
	}
}

func TestWeightedScore(t *testing.T) {
	scores := []JudgeScore{
		{Criterion: "correctness", Score: 1.0},
		{Criterion: "efficiency", Score: 4.0},
	}
	rubric := []RubricCriterion{
		{Criterion: "correctness", Weight: 3},
		{Criterion: "efficiency", Weight: 1},
	}
	// (1.0*3 + 4.0*1) / (3+1) = 7/4 = 1.75
	got := WeightedScore(scores, rubric)
	if got != 1.75 {
		t.Errorf("WeightedScore() = %f, want 1.75", got)
	}
}

func TestWeightedScore_EmptyScores(t *testing.T) {
	if got := WeightedScore(nil, []RubricCriterion{{Criterion: "x", Weight: 1}}); got != 0 {
		t.Errorf("WeightedScore(nil, ...) = %f, want 0", got)
	}
}

func TestWeightedScore_EmptyRubric(t *testing.T) {
	if got := WeightedScore([]JudgeScore{{Criterion: "x", Score: 1}}, nil); got != 0 {
		t.Errorf("WeightedScore(..., nil) = %f, want 0", got)
	}
}

func TestWeightedScore_AllZeroWeights(t *testing.T) {
	scores := []JudgeScore{{Criterion: "x", Score: 5.0}}
	rubric := []RubricCriterion{{Criterion: "x", Weight: 0}}
	if got := WeightedScore(scores, rubric); got != 0 {
		t.Errorf("WeightedScore with zero weights = %f, want 0", got)
	}
}

func TestWeightedScore_CaseInsensitiveMatch(t *testing.T) {
	scores := []JudgeScore{{Criterion: "Correctness", Score: 0.0}}
	rubric := []RubricCriterion{{Criterion: "correctness", Weight: 2}}
	// 0.0 * 2 / 2 = 0.0
	if got := WeightedScore(scores, rubric); got != 0 {
		t.Errorf("WeightedScore case-insensitive = %f, want 0", got)
	}
}

func TestParseJudgeScores_PlainJSON(t *testing.T) {
	input := `[{"criterion":"test","score":3.0,"reasoning":"ok"}]`
	scores, err := parseJudgeScores(input)
	if err != nil {
		t.Fatalf("parseJudgeScores() error: %v", err)
	}
	if len(scores) != 1 || scores[0].Score != 3.0 {
		t.Errorf("got %+v", scores)
	}
}

func TestParseJudgeScores_WithPrefix(t *testing.T) {
	input := "Here are the scores:\n[{\"criterion\":\"test\",\"score\":2.0,\"reasoning\":\"ok\"}]"
	scores, err := parseJudgeScores(input)
	if err != nil {
		t.Fatalf("parseJudgeScores() error: %v", err)
	}
	if len(scores) != 1 || scores[0].Score != 2.0 {
		t.Errorf("got %+v", scores)
	}
}

func TestParseJudgeScores_NoArray(t *testing.T) {
	_, err := parseJudgeScores("no json here")
	if err == nil {
		t.Error("expected error for non-JSON input, got nil")
	}
}

func TestParseJudgeScores_UnescapedQuoteInReasoning(t *testing.T) {
	input := `[{"criterion": "correctness", "score": 1.0, "reasoning": "checks port with "if c.Port < 1"` +
		` before returning"}]`
	scores, err := parseJudgeScores(input)
	if err != nil {
		t.Fatalf("parseJudgeScores() error: %v", err)
	}
	if len(scores) != 1 || scores[0].Score != 1.0 || scores[0].Criterion != "correctness" {
		t.Errorf("got %+v", scores)
	}
}

func TestJudgeScore_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model not loaded", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	judge := &Judge{URL: srv.URL, HTTPClient: srv.Client()}
	scenario := Scenario{
		Prompt: "Test",
		Rubric: []RubricCriterion{
			{Criterion: "correctness", Weight: 1, Scoring: "binary"},
		},
	}
	_, _, err := judge.Score(context.Background(), scenario, []RunResult{{Response: "ok"}})
	if err == nil {
		t.Error("expected error for 503 response, got nil")
	}
}

func TestParseJudgeScores_TrailingComma(t *testing.T) {
	input := `[{"criterion": "correctness", "score": 1.0, "reasoning": "ok",},]`
	scores, err := parseJudgeScores(input)
	if err != nil {
		t.Fatalf("parseJudgeScores() error: %v", err)
	}
	if len(scores) != 1 || scores[0].Score != 1.0 || scores[0].Criterion != "correctness" {
		t.Errorf("got %+v", scores)
	}
}

func TestParseJudgeScores_UnescapedQuoteAndTrailingComma(t *testing.T) {
	input := `[{"criterion": "correctness", "score": 0.0, "reasoning": "used "ref" as-is",},]`
	scores, err := parseJudgeScores(input)
	if err != nil {
		t.Fatalf("parseJudgeScores() error: %v", err)
	}
	if len(scores) != 1 || scores[0].Score != 0.0 || scores[0].Criterion != "correctness" {
		t.Errorf("got %+v", scores)
	}
}

// seqJudgeServer serves contents[i] for the i-th request (the last entry
// repeats) and records request bodies and call count.
type seqJudgeServer struct {
	srv    *httptest.Server
	calls  int
	bodies []string
}

func mockJudgeServerSequence(contents ...string) *seqJudgeServer {
	s := &seqJudgeServer{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.bodies = append(s.bodies, string(body))
		i := s.calls
		if i >= len(contents) {
			i = len(contents) - 1
		}
		s.calls++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"content": contents[i]}},
			},
		})
	}))
	return s
}

func TestJudgeScore_RetriesOnMalformedOutput(t *testing.T) {
	s := mockJudgeServerSequence(
		"Great job! The agent nailed it.", // no JSON array at all
		`[{"criterion":"correctness","score":1.0,"reasoning":"fixed"}]`,
	)
	defer s.srv.Close()

	judge := &Judge{URL: s.srv.URL, HTTPClient: s.srv.Client()}
	scenario := Scenario{
		Prompt: "Test",
		Rubric: []RubricCriterion{
			{Criterion: "correctness", Weight: 1, Scoring: "binary"},
		},
	}
	scores, missing, err := judge.Score(context.Background(), scenario, []RunResult{{Response: "ok"}})
	if err != nil {
		t.Fatalf("Score() error: %v", err)
	}
	if len(scores) != 1 || scores[0].Score != 1.0 {
		t.Errorf("got %+v", scores)
	}
	if len(missing) != 0 {
		t.Errorf("missing = %v, want none after coverage-complete retry", missing)
	}
	if s.calls != 2 {
		t.Errorf("judge called %d times, want 2", s.calls)
	}
	if len(s.bodies) < 2 || !strings.Contains(s.bodies[1], "could not be parsed as JSON") {
		t.Errorf("retry request did not carry the correction prompt: %v", s.bodies)
	}
}

func TestJudgeScore_RetryExhausted(t *testing.T) {
	s := mockJudgeServerSequence("no json here, ever")
	defer s.srv.Close()

	judge := &Judge{URL: s.srv.URL, HTTPClient: s.srv.Client()}
	scenario := Scenario{
		Prompt: "Test",
		Rubric: []RubricCriterion{
			{Criterion: "correctness", Weight: 1, Scoring: "binary"},
		},
	}
	_, _, err := judge.Score(context.Background(), scenario, []RunResult{{Response: "ok"}})
	if err == nil {
		t.Fatal("expected error after exhausted retries, got nil")
	}
	if !strings.Contains(err.Error(), "after 3 attempts") {
		t.Errorf("error should mention attempts: %v", err)
	}
	if s.calls != 3 {
		t.Errorf("judge called %d times, want 3", s.calls)
	}
}

// coverageRubric mirrors the routing rubric that exposed the gap: a weight-3
// criterion the judge silently dropped while still returning a parseable reply.
func coverageRubric() []RubricCriterion {
	return []RubricCriterion{
		{Criterion: "design-sketch", Weight: 1, Scoring: "scale_1_5"},
		{Criterion: "confirmed-depth", Weight: 3, Scoring: "scale_1_5"},
		{Criterion: "continuity", Weight: 1, Scoring: "scale_1_5"},
	}
}

func TestJudgeScore_CoverageShortfallReaskedThenComplete(t *testing.T) {
	// First reply parses but skips the weight-3 criterion; the judge must be
	// corrected with the missing name and returns full coverage on the retry.
	s := mockJudgeServerSequence(
		`[{"criterion":"design-sketch","score":5,"reasoning":"ok"},{"criterion":"continuity","score":5,"reasoning":"ok"}]`,
		`[{"criterion":"design-sketch","score":5,"reasoning":"ok"},{"criterion":"confirmed-depth","score":4,"reasoning":"deep"},{"criterion":"continuity","score":5,"reasoning":"ok"}]`,
	)
	defer s.srv.Close()

	judge := &Judge{URL: s.srv.URL, HTTPClient: s.srv.Client()}
	scenario := Scenario{Prompt: "Design it.", Rubric: coverageRubric()}
	scores, missing, err := judge.Score(context.Background(), scenario, []RunResult{{Response: "ok"}})
	if err != nil {
		t.Fatalf("Score() error: %v", err)
	}
	if len(scores) != 3 {
		t.Fatalf("got %d scores, want 3", len(scores))
	}
	if len(missing) != 0 {
		t.Errorf("missing = %v, want none after a successful coverage retry", missing)
	}
	if s.calls != 2 {
		t.Errorf("judge called %d times, want 2", s.calls)
	}
	if len(s.bodies) < 2 || !strings.Contains(s.bodies[1], "did not score every criterion") || !strings.Contains(s.bodies[1], "confirmed-depth") {
		t.Errorf("coverage retry did not carry the missing criterion name: %v", s.bodies)
	}
}

func TestJudgeScore_CoverageShortfallAcceptedAndReported(t *testing.T) {
	// The judge never returns the weight-3 criterion across all attempts:
	// accept the partial judgement (data to surface, not a lost turn) and
	// report exactly which criteria are missing.
	s := mockJudgeServerSequence(
		`[{"criterion":"design-sketch","score":5,"reasoning":"ok"},{"criterion":"continuity","score":5,"reasoning":"ok"}]`,
	)
	defer s.srv.Close()

	judge := &Judge{URL: s.srv.URL, HTTPClient: s.srv.Client()}
	scenario := Scenario{Prompt: "Design it.", Rubric: coverageRubric()}
	scores, missing, err := judge.Score(context.Background(), scenario, []RunResult{{Response: "ok"}})
	if err != nil {
		t.Fatalf("Score() error: %v", err)
	}
	if len(scores) != 2 {
		t.Fatalf("got %d scores, want the 2 the judge did return", len(scores))
	}
	if len(missing) != 1 || missing[0] != "confirmed-depth" {
		t.Errorf("missing = %v, want [confirmed-depth]", missing)
	}
	if s.calls != 3 {
		t.Errorf("judge called %d times, want 3 (all coverage attempts used)", s.calls)
	}
	// The weighted score covers the returned criteria only: (5*1+5*1)/(1+1) = 5,
	// not the full-rubric (5+15+5)/5 = 5 — equal here by coincidence, but the
	// renormalization is exactly what must be flagged rather than hidden.
	if w := WeightedScore(scores, scenario.Rubric); w != 5.0 {
		t.Errorf("WeightedScore = %v, want 5.0 over the partial rubric", w)
	}
}

func TestJudgeScore_MissingDedupedAcrossTurns(t *testing.T) {
	// Both turns' rubrics contain "correctness" (case variants) and the judge
	// scores nothing at all: the coverage gap must come back deduplicated and
	// case-insensitively, in first-seen order.
	s := mockJudgeServerSequence(`[]`)
	defer s.srv.Close()

	judge := &Judge{URL: s.srv.URL, HTTPClient: s.srv.Client()}
	scenario := Scenario{
		Turns: []Turn{
			{Prompt: "one", Rubric: []RubricCriterion{
				{Criterion: "correctness", Weight: 1, Scoring: "binary"},
				{Criterion: "efficiency", Weight: 1, Scoring: "scale_1_5"},
			}},
			{Prompt: "two", Rubric: []RubricCriterion{
				{Criterion: "Correctness", Weight: 1, Scoring: "binary"},
			}},
		},
	}
	scores, missing, err := judge.Score(context.Background(), scenario, []RunResult{{Response: "a"}, {Response: "b"}})
	if err != nil {
		t.Fatalf("Score() error: %v", err)
	}
	if len(scores) != 0 {
		t.Errorf("got %d scores, want 0", len(scores))
	}
	if len(missing) != 2 || missing[0] != "correctness" || missing[1] != "efficiency" {
		t.Errorf("missing = %v, want [correctness efficiency] (deduped, case-insensitive, rubric order)", missing)
	}
}

func TestMissingCriteria(t *testing.T) {
	rubric := []RubricCriterion{
		{Criterion: "a"}, {Criterion: "b"}, {Criterion: "c"},
	}
	cases := []struct {
		name   string
		scores []JudgeScore
		want   []string
	}{
		{"full coverage", []JudgeScore{{Criterion: "a"}, {Criterion: "B"}, {Criterion: "c"}}, nil},
		{"empty scores", []JudgeScore{}, []string{"a", "b", "c"}},
		{"one gap in rubric order", []JudgeScore{{Criterion: "c"}, {Criterion: "a"}}, []string{"b"}},
		{"unknown extra ignored", []JudgeScore{{Criterion: "a"}, {Criterion: "b"}, {Criterion: "c"}, {Criterion: "zzz"}}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := missingCriteria(tc.scores, rubric)
			if len(got) != len(tc.want) {
				t.Fatalf("missingCriteria() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("missingCriteria() = %v, want %v", got, tc.want)
				}
			}
		})
	}
}
