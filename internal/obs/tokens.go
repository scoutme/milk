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
	"time"

	"go.opentelemetry.io/otel/attribute"
)

// TokenEntry accumulates token counts for one (model, agent-role) pair.
type TokenEntry struct {
	Model         string
	Agent         string // "primary", "escalation", "router"
	Prompt        int64
	Completion    int64
	CacheRead     int64
	CacheCreation int64
	// Throughput timing (see RecordTiming): summed per-request generation
	// window and time-to-first-token, plus the count of measured requests.
	DecodeSeconds float64
	TTFTSeconds   float64
	Requests      int64
}

var sessionAccumulator struct {
	mu      sync.Mutex
	entries map[string]*TokenEntry
	turns   atomic.Int64
}

func init() { sessionAccumulator.entries = map[string]*TokenEntry{} }

// requestInputTracker holds, per exact agent-role string, the total input size
// (prompt + cache read + cache creation) of the most recent single inference
// request for that role's conversation. Unlike the additive token counters,
// this is last-write-wins: a conversation's current size is the size of its
// most recent request, never the sum over the turn's requests (a tool-loop
// turn re-sends the whole conversation on every request, so summing them
// multiplies the true context size by the request count).
var requestInputTracker struct {
	mu   sync.Mutex
	last map[string]int64
}

func init() { requestInputTracker.last = map[string]int64{} }

// RecordRequestInput records the total input size of a single inference
// request (its prompt + cache read + cache creation tokens) under the given
// agent role. Keyed by the exact role string: traffic tagged with a suffixed
// role (":subagent", ":toolagent", ":workflow", ":compaction") lives in its own
// bucket and never pollutes the plain "primary"/"escalation" conversation
// sizes the ctx:x/y indicator reads.
func RecordRequestInput(role string, input int64) {
	if input <= 0 || role == "" {
		return
	}
	requestInputTracker.mu.Lock()
	requestInputTracker.last[role] = input
	requestInputTracker.mu.Unlock()
}

// SessionLastRequestInput returns the input size of the most recent single
// request recorded for exactly this role (no prefix matching), or 0 when no
// request has been recorded since the last reset.
func SessionLastRequestInput(role string) int64 {
	requestInputTracker.mu.Lock()
	defer requestInputTracker.mu.Unlock()
	return requestInputTracker.last[role]
}

// ClearRequestInput drops the single-request input recorded for one exact
// role. It exists for the ctx:x/y pairing invariant (issue #429): a request
// measured against one agent's context window must never be displayed
// against another's, so when a turn is dispatched under a different agent
// identity than the previous one, the previous identity's recorded size is
// discarded here instead of lingering as the role's "last request" — the
// value is keyed by role alone and would otherwise survive an /agent switch.
func ClearRequestInput(role string) {
	requestInputTracker.mu.Lock()
	delete(requestInputTracker.last, role)
	requestInputTracker.mu.Unlock()
}

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

func AccumulateCacheTokens(model, agent string, cacheRead, cacheCreation int64) {
	if cacheRead == 0 && cacheCreation == 0 {
		return
	}
	key := model + "\x00" + agent
	sessionAccumulator.mu.Lock()
	e, ok := sessionAccumulator.entries[key]
	if !ok {
		e = &TokenEntry{Model: model, Agent: agent}
		sessionAccumulator.entries[key] = e
	}
	e.CacheRead += cacheRead
	e.CacheCreation += cacheCreation
	sessionAccumulator.mu.Unlock()
}

// RecordTiming records one completion request's streaming timing: a summed
// decode-seconds counter (milk.tokens.decode_seconds — the denominator for
// /usage's tok/s column), ttft/decode duration histograms, and the in-memory
// session accumulator used for per-turn deltas. No-op when both durations are
// zero (paths with no timing, e.g. generic subprocess agents).
func RecordTiming(ctx context.Context, model, agentRole string, ttft, decode time.Duration) {
	if model == "" || agentRole == "" || (ttft <= 0 && decode <= 0) {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String("model", model),
		attribute.String("agent", agentRole),
	}
	AddFloat(ctx, instrumentationScope, "milk.tokens.decode_seconds", decode.Seconds(), attrs...)
	if ttft > 0 {
		RecordDuration(ctx, instrumentationScope, "milk.tokens.ttft_ms", ttft, attrs...)
	}
	if decode > 0 {
		RecordDuration(ctx, instrumentationScope, "milk.tokens.decode_ms", decode, attrs...)
	}
	key := model + "\x00" + agentRole
	sessionAccumulator.mu.Lock()
	e, ok := sessionAccumulator.entries[key]
	if !ok {
		e = &TokenEntry{Model: model, Agent: agentRole}
		sessionAccumulator.entries[key] = e
	}
	e.TTFTSeconds += ttft.Seconds()
	e.DecodeSeconds += decode.Seconds()
	e.Requests++
	sessionAccumulator.mu.Unlock()
}

func IncrementTurnCount() { sessionAccumulator.turns.Add(1) }

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

func ResetSessionTokens() {
	sessionAccumulator.mu.Lock()
	sessionAccumulator.entries = map[string]*TokenEntry{}
	sessionAccumulator.mu.Unlock()
	sessionAccumulator.turns.Store(0)
	requestInputTracker.mu.Lock()
	requestInputTracker.last = map[string]int64{}
	requestInputTracker.mu.Unlock()
}

func SessionPromptTotal() int64 {
	sessionAccumulator.mu.Lock()
	defer sessionAccumulator.mu.Unlock()
	var t int64
	for _, e := range sessionAccumulator.entries {
		t += e.Prompt
	}
	return t
}

func SessionCompletionTotal() int64 {
	sessionAccumulator.mu.Lock()
	defer sessionAccumulator.mu.Unlock()
	var t int64
	for _, e := range sessionAccumulator.entries {
		t += e.Completion
	}
	return t
}

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

// SessionTokensByRolePrefix sums prompt and completion tokens for all roles
// matching the prefix (exact or colon-separated sub-roles). E.g. prefix
// "escalation" matches "escalation", "escalation:subagent", "escalation:workflow".
func SessionTokensByRolePrefix(prefix string) (prompt, completion int64) {
	sessionAccumulator.mu.Lock()
	defer sessionAccumulator.mu.Unlock()
	for _, e := range sessionAccumulator.entries {
		if e.Agent == prefix || strings.HasPrefix(e.Agent, prefix+":") {
			prompt += e.Prompt
			completion += e.Completion
		}
	}
	return
}

func SessionCacheByRole(role string) (cacheRead, cacheCreation int64) {
	sessionAccumulator.mu.Lock()
	defer sessionAccumulator.mu.Unlock()
	for _, e := range sessionAccumulator.entries {
		if e.Agent == role {
			cacheRead += e.CacheRead
			cacheCreation += e.CacheCreation
		}
	}
	return
}

// SessionCacheByRolePrefix sums cache tokens for all roles matching the prefix
// (exact or colon-separated sub-roles). Mirrors SessionTokensByRolePrefix.
func SessionCacheByRolePrefix(prefix string) (cacheRead, cacheCreation int64) {
	sessionAccumulator.mu.Lock()
	defer sessionAccumulator.mu.Unlock()
	for _, e := range sessionAccumulator.entries {
		if e.Agent == prefix || strings.HasPrefix(e.Agent, prefix+":") {
			cacheRead += e.CacheRead
			cacheCreation += e.CacheCreation
		}
	}
	return
}

// SessionTimingByRole sums throughput timing for exactly one role.
func SessionTimingByRole(role string) (ttftSec, decodeSec float64, reqs int64) {
	sessionAccumulator.mu.Lock()
	defer sessionAccumulator.mu.Unlock()
	for _, e := range sessionAccumulator.entries {
		if e.Agent == role {
			ttftSec += e.TTFTSeconds
			decodeSec += e.DecodeSeconds
			reqs += e.Requests
		}
	}
	return
}

// SessionTimingByRolePrefix sums throughput timing for all roles matching the
// prefix (exact or colon-separated sub-roles). Mirrors SessionTokensByRolePrefix.
func SessionTimingByRolePrefix(prefix string) (ttftSec, decodeSec float64, reqs int64) {
	sessionAccumulator.mu.Lock()
	defer sessionAccumulator.mu.Unlock()
	for _, e := range sessionAccumulator.entries {
		if e.Agent == prefix || strings.HasPrefix(e.Agent, prefix+":") {
			ttftSec += e.TTFTSeconds
			decodeSec += e.DecodeSeconds
			reqs += e.Requests
		}
	}
	return
}

// tkey identifies one metric's data point for one (model, agent) pair from
// one writer instance — instance-scoped so concurrent sessions' cumulative
// counters are summed instead of overwriting each other (#456). session is
// the data point's session_id label ("" for records written before #459's
// session-labeled token counters), so the current session's already-flushed
// share can be separated from the rest of the file layer.
type tkey struct{ metric, model, agent, instance, session string }

type SessionTokenEntry struct {
	Model, Agent             string
	Prompt, Completion       int64
	CacheRead, CacheCreation int64
	// Throughput timing (see RecordTiming); tok/s = Completion / DecodeSeconds.
	DecodeSeconds float64
	TTFTSeconds   float64
	Requests      int64
}

// FormatTokenUsage renders /usage's tables. The cumulative table merges the
// flushed file layer (milk.tokens.* datapoints, summed across writer
// instances) with the session store's totals — but only after subtracting
// this session's already-flushed share of the file layer, so a token counts
// exactly once while the unflushed tail (since the last metrics_flush_minutes
// flush) still shows (#459 option C). The subtraction needs both sides of the
// match: the data point's session_id label (stamped by RecordTokens when the
// turn ctx carries one) and the current session id on ctx
// (ContextWithSessionID — the /usage host passes st.sess.ID). A row is only
// corrected when the session store has an entry for it: the store is
// authoritative for whatever it tracks, and file-only rows (or legacy
// unlabeled data / hosts without a session ctx) fall back to the file layer
// alone rather than losing real usage.
func FormatTokenUsage(ctx context.Context, otelDir string, sessEntries []SessionTokenEntry, turns int64) string {
	sessionID := SessionIDFromContext(ctx)
	type rowKey struct{ agent, model string }
	type row struct {
		agent, model             string
		prompt, completion       float64
		cacheRead, cacheCreation int64
		decodeSec                float64
	}
	rowMap := map[rowKey]*row{}
	// fileCur holds the current session's own flushed share of the file
	// layer, per row — subtracted from the row before the session store's
	// full totals are added (see the doc comment; #459).
	type flushed struct{ prompt, completion float64 }
	fileCur := map[rowKey]flushed{}
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
		// Sum across writer instances: each process's cumulative counter
		// covers its own lifetime, so the sum is the true all-session
		// total — last-writer-wins (the old behavior) let one session's
		// flush hide another's (#456).
		for k, v := range latest {
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
				r.prompt += v
			} else {
				r.completion += v
			}
			if sessionID != "" && k.session == sessionID {
				fc := fileCur[rk]
				if k.metric == "milk.tokens.prompt" {
					fc.prompt += v
				} else {
					fc.completion += v
				}
				fileCur[rk] = fc
			}
		}
	}
	for _, e := range sessEntries {
		rk := rowKey{e.Agent, e.Model}
		r, ok := rowMap[rk]
		if !ok {
			r = &row{agent: e.Agent, model: e.Model}
			rowMap[rk] = r
		}
		// #459 option C: the store is authoritative for this session's rows —
		// drop the file layer's already-flushed share of the same tokens
		// before adding the store's full totals, or every flushed token
		// counts twice. Only rows the store has are corrected; file-only
		// rows keep their file values untouched.
		if fc, ok := fileCur[rk]; ok {
			r.prompt -= fc.prompt
			r.completion -= fc.completion
		}
		r.prompt += float64(e.Prompt)
		r.completion += float64(e.Completion)
		r.cacheRead += e.CacheRead
		r.cacheCreation += e.CacheCreation
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
	rule := "  " + strings.Repeat("─", 119)
	header := func(title string) {
		fmt.Fprintf(&b, "\n%s\n", title)
		fmt.Fprintf(&b, "  %-16s  %-30s  %10s  %10s  %10s  %10s  %10s  %6s  %7s\n", "role", "model", "prompt", "completion", "total", "cache_read", "cache_wrt", "hit%", "tok/s")
		fmt.Fprintln(&b, rule)
	}
	// hitRate is the fraction of TOTAL input tokens (fresh prompt + cacheRead +
	// cacheCreation) served from cache. prompt must already be fresh-only (not
	// inclusive of cacheRead) — every provider path normalizes to that
	// convention before recording, so cacheRead/cacheCreation are additive with
	// it, not a subset of it.
	hitRate := func(prompt float64, read, creation int64) string {
		total := prompt + float64(read+creation)
		if total == 0 {
			return "—"
		}
		return fmt.Sprintf("%.1f%%", 100*float64(read)/total)
	}
	cacheRow := func(read, creation int64) (string, string) {
		if read == 0 && creation == 0 {
			return "—", "—"
		}
		return fmt.Sprintf("%d", read), fmt.Sprintf("%d", creation)
	}
	// tokPerSec is output tokens divided by the summed generation window
	// (decode seconds) of the measured requests behind them. "—" whenever no
	// timing was recorded (e.g. generic subprocess agents) rather than a
	// misleading effective rate.
	tokPerSec := func(completion, decodeSec float64) string {
		if decodeSec <= 0 {
			return "—"
		}
		return fmt.Sprintf("%.0f", completion/decodeSec)
	}
	footer := func(p, c float64, cacheRead, cacheCreation int64, decodeSec float64) {
		cr, cw := cacheRow(cacheRead, cacheCreation)
		fmt.Fprintln(&b, rule)
		fmt.Fprintf(&b, "  %-48s  %10.0f  %10.0f  %10.0f  %10s  %10s  %6s  %7s\n", "total", p, c, p+c, cr, cw, hitRate(p, cacheRead, cacheCreation), tokPerSec(c, decodeSec))
	}
	header("token usage (cumulative):")
	var grandPrompt, grandCompletion float64
	var grandCacheRead, grandCacheCreation int64
	var grandDecode float64
	for _, r := range rows {
		grandPrompt += r.prompt
		grandCompletion += r.completion
		grandCacheRead += r.cacheRead
		grandCacheCreation += r.cacheCreation
		grandDecode += r.decodeSec
		cr, cw := cacheRow(r.cacheRead, r.cacheCreation)
		fmt.Fprintf(&b, "  %-16s  %-30s  %10.0f  %10.0f  %10.0f  %10s  %10s  %6s  %7s\n", r.agent, r.model, r.prompt, r.completion, r.prompt+r.completion, cr, cw, hitRate(r.prompt, r.cacheRead, r.cacheCreation), tokPerSec(r.completion, r.decodeSec))
	}
	footer(grandPrompt, grandCompletion, grandCacheRead, grandCacheCreation, grandDecode)
	if len(sessEntries) > 0 {
		header(fmt.Sprintf("this session (%d turns):", turns))
		var sp, sc float64
		var sCacheRead, sCacheCreation int64
		var sDecode float64
		for _, e := range sessEntries {
			cr, cw := cacheRow(e.CacheRead, e.CacheCreation)
			fmt.Fprintf(&b, "  %-16s  %-30s  %10d  %10d  %10d  %10s  %10s  %6s  %7s\n", e.Agent, e.Model, e.Prompt, e.Completion, e.Prompt+e.Completion, cr, cw, hitRate(float64(e.Prompt), e.CacheRead, e.CacheCreation), tokPerSec(float64(e.Completion), e.DecodeSeconds))
			sp += float64(e.Prompt)
			sc += float64(e.Completion)
			sCacheRead += e.CacheRead
			sCacheCreation += e.CacheCreation
			sDecode += e.DecodeSeconds
		}
		footer(sp, sc, sCacheRead, sCacheCreation, sDecode)
	}
	procEntries, procTurns := SessionTotals()
	if len(procEntries) > 0 {
		header(fmt.Sprintf("since start (%d turns):", procTurns))
		var pp, pc float64
		var pCacheRead, pCacheCreation int64
		var pDecode float64
		for _, e := range procEntries {
			cr, cw := cacheRow(e.CacheRead, e.CacheCreation)
			fmt.Fprintf(&b, "  %-16s  %-30s  %10d  %10d  %10d  %10s  %10s  %6s  %7s\n", e.Agent, e.Model, e.Prompt, e.Completion, e.Prompt+e.Completion, cr, cw, hitRate(float64(e.Prompt), e.CacheRead, e.CacheCreation), tokPerSec(float64(e.Completion), e.DecodeSeconds))
			pp += float64(e.Prompt)
			pc += float64(e.Completion)
			pCacheRead += e.CacheRead
			pCacheCreation += e.CacheCreation
			pDecode += e.DecodeSeconds
		}
		footer(pp, pc, pCacheRead, pCacheCreation, pDecode)
	}
	return strings.TrimRight(b.String(), "\n")
}

func parseTokenMetricLine(line string, out map[tkey]float64) {
	var root struct {
		Resource []struct {
			Key   string `json:"Key"`
			Value struct {
				Value any `json:"Value"`
			} `json:"Value"`
		} `json:"Resource"`
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
	instance := ""
	for _, a := range root.Resource {
		if a.Key == "service.instance.id" {
			instance = fmt.Sprintf("%v", a.Value.Value)
		}
	}
	for _, sm := range root.ScopeMetrics {
		for _, m := range sm.Metrics {
			if !strings.HasPrefix(m.Name, "milk.tokens.") {
				continue
			}
			for _, dp := range m.Data.DataPoints {
				var model, agent, session string
				for _, a := range dp.Attributes {
					switch a.Key {
					case "model":
						model = fmt.Sprintf("%v", a.Value.Value)
					case "agent":
						agent = fmt.Sprintf("%v", a.Value.Value)
					case "session_id":
						session = fmt.Sprintf("%v", a.Value.Value)
					}
				}
				out[tkey{metric: m.Name, model: model, agent: agent, instance: instance, session: session}] = dp.Value
			}
		}
	}
}
