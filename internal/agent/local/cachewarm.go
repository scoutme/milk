package local

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/scoutme/milk/internal/obs"
)

// Idle prompt-cache re-prime (issue #471, following pi's cache-warmer.ts
// pattern): OpenAI-compatible providers cache the request prefix server-side
// and discount cached tokens, but the cache expires after minutes of idle. A
// session that pauses between turns (thinking, reading, meetings) pays a cold
// cache on the next request even though nothing about the prefix changed.
//
// When the per-agent mode is "auto", a settled turn schedules a replay of
// its last request prefix (same model/messages/tools, max_tokens=1,
// non-streaming) after cacheWarmIdleDelay — cheap enough to matter, timed to
// land before typical provider TTLs (5–10 min). While the session stays idle
// the replay re-arms after each success (pi's cache-warmer.ts max-age
// pattern), bounded by cacheWarmMaxIdle, so the prefix survives pauses of any
// length. The next real turn reads the prefix from cache; the chain is
// cancelled the moment a new turn starts: a real request re-primes the
// prefix by itself.
//
// Config: AgentConfig.CacheWarming ("off" default | "auto"). Not applicable to
// the Bedrock native path (which has its own PromptCaching knob) or to clone
// runs (tool agents, workflow executors, background jobs) — only the agent a
// user is actually waiting on warms.
const (
	// cacheWarmIdleDelay is the gap between turn settle and the replay. A
	// little under the common 5-minute provider TTL keeps the prefix hot for
	// a user who returns shortly after the cache would have lapsed.
	cacheWarmIdleDelay = 4 * time.Minute
	// cacheWarmMaxIdle bounds the re-arm chain: replays stop once the
	// session has been continuously idle this long (pi's max-age analog).
	// Covers a lunch break; beyond that the session is likely parked and
	// the replays are waste.
	cacheWarmMaxIdle = 30 * time.Minute
	// cacheWarmMinBodyBytes skips warming for small prefixes: at a few
	// hundred tokens the cold/warm delta is less than the replay costs.
	cacheWarmMinBodyBytes = 2048
	// cacheWarmSendTimeout bounds the replay request itself.
	cacheWarmSendTimeout = 60 * time.Second
)

// cacheWarmState holds the pending replay across the turn boundary. Created
// eagerly by the Agent constructors and held via pointer on Agent: the
// configuration chain (WithMemConfig et al.) shallow-copies the Agent, so the
// turn goroutine fills it, a timer goroutine reads it, and the whole copy tree
// (including background clones) shares the pointer — clones never write (see
// the warmEligible guards).
type cacheWarmState struct {
	mu      sync.Mutex
	timer   *time.Timer
	model   string
	msgs    []Message
	tools   []map[string]any
	bodyLen int
	// idleSince is when the current idle window opened (last turn settle).
	// The re-arm chain runs only while it is non-zero: cancelCacheWarm
	// zeroes it so a send finishing concurrently with a new turn cannot
	// schedule behind it.
	idleSince time.Time
}

// warmUsage is the slice of a non-streaming chat completion response the
// re-prime cares about: how much of the replayed prompt hit cache.
type warmUsage struct {
	Usage struct {
		PromptTokens        int64 `json:"prompt_tokens"`
		CompletionTokens    int64 `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details,omitempty"`
	} `json:"usage"`
}

func (a *Agent) cacheWarmingMode() string {
	if a.cacheWarming != "auto" {
		return "off"
	}
	return "auto"
}

// warmEligible reports whether this instance may write the idle cache-warm
// state. The Agent configuration chain (WithMemConfig et al.) shallow-copies
// the Agent, so the cacheWarmState pointer is shared across the copies — the
// turn goroutine fills it, a timer goroutine reads it, and background clones
// share the pointer but must never write it. Every write path
// (observeWarmable, armCacheWarm, cancelCacheWarm, ReshapeCacheWarm) guards
// with this: only the agent a user is actually waiting on warms (never clones
// or the Bedrock native path, which has its own prompt-caching knob), and only
// while the mode is "auto".
func (a *Agent) warmEligible() bool {
	if a.cacheWarmingMode() != "auto" {
		return false
	}
	return !a.useBedrockNative && a.jobID == "" && !a.workflowRole && !a.isToolAgent
}

func (a *Agent) cacheWarmDelay() time.Duration {
	if a.warmDelay > 0 {
		return a.warmDelay
	}
	return cacheWarmIdleDelay
}

func (a *Agent) cacheWarmMaxIdleWindow() time.Duration {
	if a.warmMaxIdle > 0 {
		return a.warmMaxIdle
	}
	return cacheWarmMaxIdle
}

func (a *Agent) cacheWarmFloor() int {
	if a.warmMinBytes > 0 {
		return a.warmMinBytes
	}
	return cacheWarmMinBodyBytes
}

func (a *Agent) warmState() *cacheWarmState {
	if a.warm == nil {
		a.warm = &cacheWarmState{}
	}
	return a.warm
}

// WithCacheWarming sets the idle prompt-cache re-prime mode: "auto" enables
// it, anything else (including "") keeps it off (issue #471).
func (a *Agent) WithCacheWarming(mode string) *Agent {
	a.cacheWarming = mode
	return a
}

// withCacheWarmDelay and withCacheWarmMinBytes override the scheduling
// constants in tests.
func (a *Agent) withCacheWarmDelay(d time.Duration) *Agent {
	a.warmDelay = d
	return a
}

func (a *Agent) withCacheWarmMinBytes(n int) *Agent {
	a.warmMinBytes = n
	return a
}

func (a *Agent) withCacheWarmMaxIdle(d time.Duration) *Agent {
	a.warmMaxIdle = d
	return a
}

// observeWarmable records the request streamCompletionOnce is about to send,
// so the replay later can re-prime exactly that prefix. Must see the final
// message slice: the payload-size trim in streamCompletionOnce rewrites
// req.Messages before the request goes out, so callers pass the post-trim
// request and body size.
func (a *Agent) observeWarmable(req chatRequest, bodyLen int) {
	if !a.warmEligible() {
		return
	}
	// Inlined base64 images would be re-uploaded by the replay — too
	// expensive to duplicate, and image attachments are one-off turns.
	if imageDataBytes(req.Messages) > 0 {
		return
	}
	st := a.warmState()
	st.mu.Lock()
	defer st.mu.Unlock()
	st.model = req.Model
	// Copy the slice header contents: runToolLoop appends to msgs as the
	// turn progresses and the replay must hold the sent prefix, not a
	// window into a grown buffer.
	st.msgs = append([]Message(nil), req.Messages...)
	st.tools = req.Tools
	st.bodyLen = bodyLen
}

// ReshapeCacheWarm re-primes the pending replay with the history shape the
// NEXT turn's request will actually carry (issue #471's shape fix).
//
// observeWarmable snapshots the request as the turn sent it — but the next
// turn's request is rebuilt from the persisted session by cmd/milk's history
// builder (sessionToUnifiedMessagesWithBoundary / buildAgentHistoryWithBoundary
// and friends), which re-renders tool trails and shrinks tool results. The
// in-loop snapshot therefore replays a shape the next request never sends, and
// the session-specific cache blocks after tool results stay cold. After the
// turn is persisted the runner rebuilds the next-turn history with that same
// builder and hands it here: the snapshot keeps its system message (the
// rebuilt history is the post-system slice, exactly what the runner passes to
// Run) and takes the settled shape for everything after it, so the replay's
// prefix is the one the next request sends.
//
// No-op unless a snapshot exists, history is empty, or the run is one
// observeWarmable itself skips (clone runs, Bedrock native). The
// cacheWarmMinBodyBytes floor still applies on the reshaped size: a shape that
// shrinks below it drops the pending replay — just as armCacheWarm refuses to
// arm one, a prefix this small is not worth the replay.
func (a *Agent) ReshapeCacheWarm(history []Message) {
	if !a.warmEligible() {
		return
	}
	if len(history) == 0 || imageDataBytes(history) > 0 {
		return
	}
	st := a.warm
	if st == nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.msgs) == 0 {
		return
	}
	msgs := append([]Message{st.msgs[0]}, history...)
	// Recompute bodyLen for the floor check below (and for armCacheWarm's,
	// when a caller reshapes before arming): approximate the replay body the
	// same way sendCacheWarm builds it.
	body, err := json.Marshal(chatRequest{
		Model:       st.model,
		Messages:    msgs,
		Tools:       st.tools,
		Stream:      false,
		MaxTokens:   1,
		Temperature: 0,
	})
	if err != nil {
		return // keep the old snapshot; the replay would fail to marshal too
	}
	st.msgs = msgs
	st.bodyLen = len(body)
	if st.bodyLen < a.cacheWarmFloor() {
		if st.timer != nil {
			st.timer.Stop()
			st.timer = nil
		}
		st.idleSince = time.Time{}
	}
}

// cancelCacheWarm drops a pending replay. Called at turn start: the real
// request about to go out re-primes the prefix on its own.
func (a *Agent) cancelCacheWarm() {
	if !a.warmEligible() {
		return
	}
	st := a.warm
	if st == nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.timer != nil {
		st.timer.Stop()
		st.timer = nil
	}
	st.idleSince = time.Time{}
}

// armCacheWarm schedules the single idle replay after a successfully settled
// turn. One replay per turn at most: re-arming replaces any earlier timer.
func (a *Agent) armCacheWarm() {
	if !a.warmEligible() {
		return
	}
	st := a.warmState()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.bodyLen < a.cacheWarmFloor() || len(st.msgs) == 0 {
		return
	}
	if st.timer != nil {
		st.timer.Stop()
	}
	st.idleSince = time.Now()
	st.timer = time.AfterFunc(a.cacheWarmDelay(), a.sendCacheWarm)
}

// sendCacheWarm replays the recorded prefix with max_tokens=1, non-streaming.
// Runs on the timer goroutine with its own timeout context: the turn's context
// is long gone by the time a replay is due.
func (a *Agent) sendCacheWarm() {
	st := a.warm
	if st == nil {
		return
	}
	st.mu.Lock()
	req := chatRequest{
		Model:       st.model,
		Messages:    st.msgs,
		Tools:       st.tools,
		Stream:      false,
		MaxTokens:   1,
		Temperature: 0,
	}
	bodyLen := st.bodyLen
	st.mu.Unlock()

	body, err := json.Marshal(req)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), cacheWarmSendTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.inferenceURL(), bytes.NewReader(body))
	if err != nil {
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(httpReq)
	if err != nil {
		a.logDebug("cache warm replay failed", "err", err.Error())
		return
	}
	defer resp.Body.Close()

	var usage warmUsage
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&usage)
	cached := int64(-1)
	if usage.Usage.PromptTokensDetails != nil {
		cached = usage.Usage.PromptTokensDetails.CachedTokens
	}
	if resp.StatusCode != http.StatusOK {
		a.logWarn("cache warm replay rejected",
			"status", resp.StatusCode, "model", a.model, "prompt_bytes", bodyLen)
		return
	}
	a.logInfo("cache warm replay sent",
		"model", a.model, "prompt_bytes", bodyLen,
		"prompt_tokens", usage.Usage.PromptTokens,
		"cached_tokens", cached)
	obs.Inc(ctx, inferenceScope, "milk.inference.cache_warm",
		attribute.String("model", a.model),
		attribute.String("agent", a.logRole()))

	// Re-arm while the session stays idle, bounded by the max-idle window.
	// The mutex check against idleSince (cleared by cancelCacheWarm) keeps a
	// replay that raced a new turn start from scheduling behind it.
	st.mu.Lock()
	if !st.idleSince.IsZero() && time.Since(st.idleSince)+a.cacheWarmDelay() <= a.cacheWarmMaxIdleWindow() {
		st.timer = time.AfterFunc(a.cacheWarmDelay(), a.sendCacheWarm)
	}
	st.mu.Unlock()
}
