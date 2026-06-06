package router

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/obs"
)

var reCodeFence = regexp.MustCompile("```")
var reOpenQuestion = regexp.MustCompile(`(?i)^(what|why|how|when|where|who|which|could you|can you|would you|should|is it|are there|do you|does)`)

// Signal represents a single detected routing cue.
type Signal struct {
	Name   string
	Score  int // positive → escalate, negative → local
	Detail string
}

// signalResult carries the scored signals and the final conclusive decision (if any).
type signalResult struct {
	Signals    []Signal
	TotalScore int
}

// rulesDecision returns a routing decision if the rules layer is conclusive,
// or Decision{} with Conclusive=false if the configured fallback should be consulted.
func rulesDecision(prompt string, cfg config.Config) Decision {
	return rulesDecisionCtx(context.Background(), prompt, cfg)
}

func rulesDecisionCtx(ctx context.Context, prompt string, cfg config.Config) Decision {
	r := cfg.Rules
	lower := strings.ToLower(prompt)
	approxTokens := utf8.RuneCountInString(prompt) / 4

	// --- Hard conclusive rules (always win) ---

	// Long prompt → escalate
	if approxTokens > r.EscalateAboveTokens {
		return Decision{Target: TargetEscalation, Conclusive: true, Reason: "prompt exceeds token threshold"}
	}

	// Escalate keyword hit
	for _, kw := range r.EscalateKeywords {
		if strings.Contains(lower, strings.ToLower(kw)) {
			return Decision{Target: TargetEscalation, Conclusive: true, Reason: "escalate keyword: " + kw}
		}
	}

	// Very short prompt → local (shell one-liners, quick queries)
	if r.LocalBelowTokens > 0 && approxTokens <= r.LocalBelowTokens {
		return Decision{Target: TargetLocal, Conclusive: true, Reason: "short prompt (local)"}
	}

	// --- Soft signal scoring ---

	result := scoreSignals(prompt, lower, cfg)

	// Record the raw score whenever soft scoring runs (no hard rule fired first).
	obs.RecordScore(ctx, float64(result.TotalScore))

	var reasons []string
	for _, s := range result.Signals {
		reasons = append(reasons, s.Name)
	}
	reason := strings.Join(reasons, ", ")

	if result.TotalScore >= r.EscalateThreshold {
		return Decision{Target: TargetEscalation, Conclusive: true, Reason: "signals: " + reason}
	}
	if result.TotalScore <= r.LocalThreshold {
		return Decision{Target: TargetLocal, Conclusive: true, Reason: "signals: " + reason}
	}

	return Decision{Conclusive: false, Reason: "inconclusive (" + reason + ")"}
}

func scoreSignals(prompt, lower string, cfg config.Config) signalResult {
	r := cfg.Rules
	var signals []Signal
	total := 0

	add := func(s Signal) {
		signals = append(signals, s)
		total += s.Score
	}

	// Local verb match: shell/task-oriented verbs → local
	for _, v := range r.LocalVerbs {
		if strings.Contains(lower, strings.ToLower(v)) {
			add(Signal{Name: "local-verb:" + v, Score: r.LocalVerbWeight, Detail: v})
			break // one match is enough
		}
	}

	// Escalate verb match
	for _, v := range r.EscalateVerbs {
		if strings.Contains(lower, strings.ToLower(v)) {
			add(Signal{Name: "escalate-verb:" + v, Score: r.EscalateVerbWeight, Detail: v})
			break
		}
	}

	// Path reference: word that looks like a file path and resolves on disk
	if score, detail := pathRefScore(prompt, r.PathRefWeight); score != 0 {
		add(Signal{Name: "path-ref", Score: score, Detail: detail})
	}

	// Code block presence → suggests mechanical/local task
	if reCodeFence.MatchString(prompt) {
		add(Signal{Name: "code-block", Score: r.CodeBlockWeight})
	}

	// Open question start → conceptual, escalate
	if reOpenQuestion.MatchString(strings.TrimSpace(prompt)) {
		add(Signal{Name: "open-question", Score: r.OpenQuestionWeight})
	}

	return signalResult{Signals: signals, TotalScore: total}
}

// pathRefScore scans for path-like tokens and checks if any exist on disk.
// Returns the configured weight and the first matched path, or 0 if none found.
func pathRefScore(prompt string, weight int) (int, string) {
	cwd, _ := os.Getwd()

	for _, word := range strings.Fields(prompt) {
		// Must look like a path: contains / or starts with . or ~
		if !looksLikePath(word) {
			continue
		}
		candidates := []string{word}
		if cwd != "" && !filepath.IsAbs(word) {
			candidates = append(candidates, filepath.Join(cwd, word))
		}
		for _, p := range candidates {
			if _, err := os.Stat(p); err == nil {
				return weight, p
			}
		}
	}
	return 0, ""
}

func looksLikePath(s string) bool {
	return filepath.IsAbs(s) ||
		strings.HasPrefix(s, "./") ||
		strings.HasPrefix(s, "../") ||
		strings.HasPrefix(s, "~/") ||
		strings.ContainsAny(s, "/\\")
}
