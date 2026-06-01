package obs

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// TokenEntry accumulates token counts for one (model, agent-role) pair.
type TokenEntry struct {
	Model      string
	Agent      string // "primary", "escalation", "router"
	Prompt     int64
	Completion int64
}

// sessionAccumulator holds per-session in-memory token totals, thread-safe.
var sessionAccumulator struct {
	mu      sync.Mutex
	entries map[string]*TokenEntry // key: model+"\x00"+agent
	turns   atomic.Int64
}

func init() {
	sessionAccumulator.entries = map[string]*TokenEntry{}
}

// accumulateSessionTokens adds token counts to the in-memory session accumulator.
// Called automatically by RecordTokens.
func accumulateSessionTokens(model, agent string, prompt, completion int64) {
	key := model + "\x00" + agent
	sessionAccumulator.mu.Lock()
	e, ok := sessionAccumulator.entries[key]
	if !ok {
		e = &TokenEntry{Model: model, Agent: agent}
		sessionAccumulator.entries[key] = e
	}
	e.Prompt += prompt
	e.Completion += completion
	sessionAccumulator.mu.Unlock()
}

// IncrementTurnCount increments the session turn counter. Call once per completed turn.
func IncrementTurnCount() {
	sessionAccumulator.turns.Add(1)
}

// SessionTotals returns a snapshot of the current session's token accumulator.
func SessionTotals() (entries []TokenEntry, turns int64) {
	sessionAccumulator.mu.Lock()
	defer sessionAccumulator.mu.Unlock()
	for _, e := range sessionAccumulator.entries {
		entries = append(entries, *e)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Agent != entries[j].Agent {
			return entries[i].Agent < entries[j].Agent
		}
		return entries[i].Model < entries[j].Model
	})
	return entries, sessionAccumulator.turns.Load()
}

// ResetSessionTokens clears the in-memory session accumulator (call on /new).
func ResetSessionTokens() {
	sessionAccumulator.mu.Lock()
	sessionAccumulator.entries = map[string]*TokenEntry{}
	sessionAccumulator.mu.Unlock()
	sessionAccumulator.turns.Store(0)
}

// SessionPromptTotal returns the total prompt tokens across all roles this session.
func SessionPromptTotal() int64 {
	sessionAccumulator.mu.Lock()
	defer sessionAccumulator.mu.Unlock()
	var t int64
	for _, e := range sessionAccumulator.entries {
		t += e.Prompt
	}
	return t
}

// SessionCompletionTotal returns the total completion tokens across all roles this session.
func SessionCompletionTotal() int64 {
	sessionAccumulator.mu.Lock()
	defer sessionAccumulator.mu.Unlock()
	var t int64
	for _, e := range sessionAccumulator.entries {
		t += e.Completion
	}
	return t
}

// SessionTokensByRole returns the cumulative prompt and completion tokens for a
// given agent role ("primary", "escalation", "router") this session.
func SessionTokensByRole(role string) (prompt, completion int64) {
	sessionAccumulator.mu.Lock()
	defer sessionAccumulator.mu.Unlock()
	for _, e := range sessionAccumulator.entries {
		if e.Agent == role {
			prompt += e.Prompt
			completion += e.Completion
		}
	}
	return
}

// tkey is the composite key for token metric aggregation.
type tkey struct{ metric, model, agent string }

// FormatTokenUsage returns a human-readable token usage report.
// The cumulative table merges flushed metrics.jsonl data with the unflushed
// in-memory session accumulator so the totals are always current.
func FormatTokenUsage(ctx context.Context, otelDir string) string {
	_ = ctx

	type rowKey struct{ agent, model string }
	type row struct {
		agent, model       string
		prompt, completion float64
	}
	rowMap := map[rowKey]*row{}

	// Layer 1: flushed data from metrics.jsonl (may lag by up to flush interval).
	path := filepath.Join(otelDir, "metrics.jsonl")
	if f, err := os.Open(path); err == nil {
		latest := map[tkey]float64{}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 256*1024), 256*1024)
		for scanner.Scan() {
			if line := scanner.Text(); line != "" {
				parseTokenMetricLine(line, latest)
			}
		}
		f.Close()
		for k, v := range latest { //nolint:gocritic
			if k.metric != "milk.tokens.prompt" && k.metric != "milk.tokens.completion" {
				continue
			}
			rk := rowKey{k.agent, k.model}
			r, ok := rowMap[rk]
			if !ok {
				r = &row{agent: k.agent, model: k.model}
				rowMap[rk] = r
			}
			if k.metric == "milk.tokens.prompt" {
				r.prompt = v
			} else {
				r.completion = v
			}
		}
	}

	// Layer 2: in-memory session accumulator (always current, not yet flushed).
	// We add session tokens on top of the disk values so the cumulative total is accurate.
	sessionEntries, turns := SessionTotals()
	for _, e := range sessionEntries {
		rk := rowKey{e.Agent, e.Model}
		r, ok := rowMap[rk]
		if !ok {
			r = &row{agent: e.Agent, model: e.Model}
			rowMap[rk] = r
		}
		r.prompt += float64(e.Prompt)
		r.completion += float64(e.Completion)
	}

	if len(rowMap) == 0 {
		return "no token metrics recorded yet (run a few turns first)"
	}

	rows := make([]row, 0, len(rowMap))
	for _, r := range rowMap {
		rows = append(rows, *r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].agent != rows[j].agent {
			return rows[i].agent < rows[j].agent
		}
		return rows[i].model < rows[j].model
	})

	var b strings.Builder
	fmt.Fprintln(&b, "token usage (cumulative):")
	fmt.Fprintf(&b, "  %-16s  %-30s  %10s  %10s  %10s\n", "role", "model", "prompt", "completion", "total")
	fmt.Fprintf(&b, "  %s\n", strings.Repeat("─", 82))
	var grandPrompt, grandCompletion float64
	for _, r := range rows {
		grandPrompt += r.prompt
		grandCompletion += r.completion
		fmt.Fprintf(&b, "  %-16s  %-30s  %10.0f  %10.0f  %10.0f\n",
			r.agent, r.model, r.prompt, r.completion, r.prompt+r.completion)
	}
	fmt.Fprintf(&b, "  %s\n", strings.Repeat("─", 82))
	fmt.Fprintf(&b, "  %-48s  %10.0f  %10.0f  %10.0f\n",
		"total", grandPrompt, grandCompletion, grandPrompt+grandCompletion)

	if len(sessionEntries) > 0 {
		fmt.Fprintf(&b, "\nthis session (%d turns):\n", turns)
		var sp, sc float64
		for _, e := range sessionEntries {
			fmt.Fprintf(&b, "  %-16s  %-30s  %10d  %10d  %10d\n",
				e.Agent, e.Model, e.Prompt, e.Completion, e.Prompt+e.Completion)
			sp += float64(e.Prompt)
			sc += float64(e.Completion)
		}
		fmt.Fprintf(&b, "  %-48s  %10.0f  %10.0f  %10.0f\n", "total", sp, sc, sp+sc)
	}
	return strings.TrimRight(b.String(), "\n")
}

// parseTokenMetricLine extracts milk.tokens.* data points from one OTLP JSON metrics line.
func parseTokenMetricLine(line string, out map[tkey]float64) {
	var root struct {
		ScopeMetrics []struct {
			Metrics []struct {
				Name string `json:"Name"`
				Data struct {
					DataPoints []struct {
						Attributes []struct {
							Key   string `json:"Key"`
							Value struct {
								Value any `json:"Value"`
							} `json:"Value"`
						} `json:"Attributes"`
						Value float64 `json:"Value"`
					} `json:"DataPoints"`
				} `json:"Data"`
			} `json:"Metrics"`
		} `json:"ScopeMetrics"`
	}
	if err := json.Unmarshal([]byte(line), &root); err != nil {
		return
	}
	for _, sm := range root.ScopeMetrics {
		for _, m := range sm.Metrics {
			if !strings.HasPrefix(m.Name, "milk.tokens.") {
				continue
			}
			for _, dp := range m.Data.DataPoints {
				var model, agent string
				for _, a := range dp.Attributes {
					switch a.Key {
					case "model":
						model = fmt.Sprintf("%v", a.Value.Value)
					case "agent":
						agent = fmt.Sprintf("%v", a.Value.Value)
					}
				}
				out[tkey{m.Name, model, agent}] = dp.Value
			}
		}
	}
}
