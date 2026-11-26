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
// When the per-agent mode is "auto", a settled turn schedules exactly one
// replay of its last request prefix (same model/messages/tools, max_tokens=1,
// non-streaming) after cacheWarmIdleDelay — cheap enough to matter, timed to
// land before typical provider TTLs (5–10 min). The next real turn then reads
// the prefix from cache. The replay is cancelled the moment a new turn starts:
// a real request re-primes the prefix by itself.
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
	// cacheWarmMinBodyBytes skips warming for small prefixes: at a few
	// hundred tokens the cold/warm delta is less than the replay costs.
	cacheWarmMinBodyBytes = 2048
	// cacheWarmSendTimeout bounds the replay request itself.
	cacheWarmSendTimeout = 60 * time.Second
)

// cacheWarmState holds the pending replay across the turn boundary. Held via
// pointer on Agent: the turn goroutine fills it, a timer goroutine reads it,
// and background clones share the pointer but never write (see the guards in
// observeWarmable).
type cacheWarmState struct {
	mu      sync.Mutex
	timer   *time.Timer
	model   string
	msgs    []Message
	tools   []map[string]any
	bodyLen int
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

func (a *Agent) cacheWarmDelay() time.Duration {
	if a.warmDelay > 0 {
		return a.warmDelay
	}
	return cacheWarmIdleDelay
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

// observeWarmable records the request streamCompletionOnce is about to send,
// so the replay later can re-prime exactly that prefix. Must see the final
// message slice: the payload-size trim in streamCompletionOnce rewrites
// req.Messages before the request goes out, so callers pass the post-trim
// request and body size.
func (a *Agent) observeWarmable(req chatRequest, bodyLen int) {
	if a.cacheWarmingMode() != "auto" {
		return
	}
	// Bedrock native has its own prompt-caching knob; clones (tool agents,
	// workflow executors, background jobs) are not something a user waits on
	// between turns, and a job's next request follows immediately anyway.
	if a.useBedrockNative || a.jobID != "" || a.workflowRole || a.isToolAgent {
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

// cancelCacheWarm drops a pending replay. Called at turn start: the real
// request about to go out re-primes the prefix on its own.
func (a *Agent) cancelCacheWarm() {
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
}

// armCacheWarm schedules the single idle replay after a successfully settled
// turn. One replay per turn at most: re-arming replaces any earlier timer.
func (a *Agent) armCacheWarm() {
	if a.cacheWarmingMode() != "auto" {
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
}
