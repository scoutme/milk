package local

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/session"
)

// warmRecorder captures request bodies by kind: streaming chat requests are
// the "real" traffic, non-streaming max_tokens=1 requests are the idle cache
// re-prime (issue #471).
type warmRecorder struct {
	mu     sync.Mutex
	real   [][]byte
	warm   [][]byte
	warmCh chan []byte
}

func (r *warmRecorder) counts() (real, warm int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.real), len(r.warm)
}

func cacheWarmServer(t *testing.T) (*httptest.Server, *warmRecorder) {
	t.Helper()
	rec := &warmRecorder{warmCh: make(chan []byte, 8)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &probe)

		rec.mu.Lock()
		if probe.Stream {
			rec.real = append(rec.real, body)
		} else {
			rec.warm = append(rec.warm, body)
		}
		rec.mu.Unlock()

		if probe.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":1,\"prompt_tokens_details\":{\"cached_tokens\":80}}}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		select {
		case rec.warmCh <- body:
		default:
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":90}}}`)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func decodeBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	return m
}

// TestCacheWarm_ReplaysExactPrefixAfterIdle is the happy path: a settled turn
// with cache_warming=auto schedules a replay, and the replay's
// model/messages/tools are the real request's — only stream/max_tokens differ.
func TestCacheWarm_ReplaysExactPrefixAfterIdle(t *testing.T) {
	srv, rec := cacheWarmServer(t)

	agent := New(srv.URL, "test-model").
		WithCacheWarming("auto").
		withCacheWarmDelay(60 * time.Millisecond).
		withCacheWarmMaxIdle(80 * time.Millisecond). // shorter than 2x delay: exactly one send
		withCacheWarmMinBytes(1)
	sess := &session.Session{}
	var out strings.Builder

	if _, err := agent.Run(context.Background(), nil, "do the thing", &out, sess, nil); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	select {
	case body := <-rec.warmCh:
		warm := decodeBody(t, body)
		if warm["stream"] != false {
			t.Errorf("warm replay must not stream, got stream=%v", warm["stream"])
		}
		if warm["max_tokens"] != float64(1) {
			t.Errorf("warm replay must cap completion at 1 token, got max_tokens=%v", warm["max_tokens"])
		}
		if warm["model"] != "test-model" {
			t.Errorf("warm replay model = %v, want test-model", warm["model"])
		}

		realN, warmN := rec.counts()
		if realN == 0 {
			t.Fatal("no real request captured — test server is broken")
		}
		rec.mu.Lock()
		lastReal := decodeBody(t, rec.real[realN-1])
		rec.mu.Unlock()
		if !reflect.DeepEqual(warm["messages"], lastReal["messages"]) {
			t.Errorf("warm replay messages diverge from the sent prefix:\nwarm: %v\nreal: %v", warm["messages"], lastReal["messages"])
		}
		if !reflect.DeepEqual(warm["tools"], lastReal["tools"]) {
			t.Errorf("warm replay tools diverge from the sent prefix")
		}
		if warmN != 1 {
			t.Errorf("expected exactly 1 warm replay, got %d", warmN)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no cache warm replay arrived after the turn settled")
	}
}

// TestCacheWarm_OffByDefault guards that unset/"" mode never replays.
func TestCacheWarm_OffByDefault(t *testing.T) {
	srv, rec := cacheWarmServer(t)

	agent := New(srv.URL, "test-model").
		withCacheWarmDelay(60 * time.Millisecond).
		withCacheWarmMinBytes(1)
	sess := &session.Session{}
	var out strings.Builder

	if _, err := agent.Run(context.Background(), nil, "do the thing", &out, sess, nil); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	time.Sleep(300 * time.Millisecond)
	_, warm := rec.counts()
	if warm != 0 {
		t.Errorf("cache warming is off by default but %d replay(s) were sent", warm)
	}
}

// TestCacheWarm_CanceledByNextTurn pins the turn-start cancellation: once a
// new real request is about to go out, a pending replay is redundant and must
// not fire.
func TestCacheWarm_CanceledByNextTurn(t *testing.T) {
	srv, rec := cacheWarmServer(t)

	agent := New(srv.URL, "test-model").
		WithCacheWarming("auto").
		withCacheWarmDelay(80 * time.Millisecond).
		withCacheWarmMinBytes(1)

	agent.observeWarmable(chatRequest{
		Model:    "test-model",
		Messages: []Message{{Role: "user", Content: "hello"}},
	}, 5000)
	agent.armCacheWarm()
	agent.cancelCacheWarm()

	time.Sleep(300 * time.Millisecond)
	_, warm := rec.counts()
	if warm != 0 {
		t.Errorf("canceled replay still fired (%d sent)", warm)
	}
}

// TestCacheWarm_SkipsSmallPrefixes pins the floor: tiny prompts never earn a
// replay — the cold/warm delta is worth less than the request costs.
func TestCacheWarm_SkipsSmallPrefixes(t *testing.T) {
	srv, rec := cacheWarmServer(t)

	agent := New(srv.URL, "test-model").
		WithCacheWarming("auto").
		withCacheWarmDelay(60 * time.Millisecond).
		withCacheWarmMinBytes(1 << 20) // unreachable floor
	sess := &session.Session{}
	var out strings.Builder

	if _, err := agent.Run(context.Background(), nil, "do the thing", &out, sess, nil); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	time.Sleep(300 * time.Millisecond)
	_, warm := rec.counts()
	if warm != 0 {
		t.Errorf("prefix below the floor still triggered %d replay(s)", warm)
	}
}

// TestCacheWarm_ReArmsWhileIdleBounded pins the idle chain: while no new turn
// starts, each successful replay schedules the next one — and the chain stops
// at the max-idle bound instead of warming forever.
func TestCacheWarm_ReArmsWhileIdleBounded(t *testing.T) {
	srv, rec := cacheWarmServer(t)

	agent := New(srv.URL, "test-model").
		WithCacheWarming("auto").
		withCacheWarmDelay(50 * time.Millisecond).
		withCacheWarmMinBytes(1).
		withCacheWarmMaxIdle(170 * time.Millisecond)
	sess := &session.Session{}
	var out strings.Builder

	if _, err := agent.Run(context.Background(), nil, "do the thing", &out, sess, nil); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	time.Sleep(700 * time.Millisecond)
	_, warm := rec.counts()
	if warm < 2 {
		t.Errorf("idle chain did not re-arm: %d replay(s) in the idle window, want >= 2", warm)
	}
	if warm > 5 {
		t.Errorf("idle chain ran unbounded: %d replay(s), max-idle bound not enforced", warm)
	}
}

// warmMessages decodes the messages of a captured replay body.
func warmMessages(t *testing.T, body []byte) []Message {
	t.Helper()
	var got struct {
		Messages []Message `json:"messages"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode warm replay: %v", err)
	}
	return got.Messages
}

// TestCacheWarm_ReshapeReplacesSnapshot pins the shape fix (issue #471): the
// post-persist reshape replaces the snapshot's post-system messages with the
// settled next-turn history, so the replay goes out with system + that history
// — the prefix the next request actually sends — instead of the in-loop shape
// whose tool results the session rebuild shrinks.
func TestCacheWarm_ReshapeReplacesSnapshot(t *testing.T) {
	srv, rec := cacheWarmServer(t)

	agent := New(srv.URL, "test-model").
		WithCacheWarming("auto").
		withCacheWarmDelay(60 * time.Millisecond).
		withCacheWarmMaxIdle(80 * time.Millisecond). // shorter than 2x delay: one send
		withCacheWarmMinBytes(1)

	system := Message{Role: "system", Content: "the system prompt"}
	agent.observeWarmable(chatRequest{
		Model: "test-model",
		Messages: []Message{
			system,
			{Role: "user", Content: "in-loop prompt"},
			{Role: "tool", Content: "raw 17K-token tool result", ToolCallID: "t1"},
		},
	}, 5000)
	reshaped := []Message{
		{Role: "user", Content: "[user to primary] in-loop prompt"},
		{Role: "tool", Content: "shrunk tool result", ToolCallID: "t1"},
		{Role: "assistant", Content: "settled reply"},
	}
	agent.ReshapeCacheWarm(context.Background(), nil, reshaped)
	agent.armCacheWarm()

	select {
	case body := <-rec.warmCh:
		want := append([]Message{system}, reshaped...)
		if got := warmMessages(t, body); !reflect.DeepEqual(got, want) {
			t.Errorf("warm replay messages = %v, want system + settled history %v", got, want)
		}
		if _, warm := rec.counts(); warm != 1 {
			t.Errorf("expected exactly 1 warm replay, got %d", warm)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no cache warm replay arrived after the reshape")
	}
}

// TestCacheWarm_ReshapeNoOpGuards pins the reshape guards: nothing reshapes
// without a snapshot, empty history is ignored, and clone runs (which
// observeWarmable itself skips) must never rewrite the shared snapshot.
func TestCacheWarm_ReshapeNoOpGuards(t *testing.T) {
	srv, rec := cacheWarmServer(t)

	agent := New(srv.URL, "test-model").
		WithCacheWarming("auto").
		withCacheWarmDelay(60 * time.Millisecond).
		withCacheWarmMaxIdle(80 * time.Millisecond).
		withCacheWarmMinBytes(1)

	// No snapshot yet: the reshape must not arm a replay of its own.
	agent.ReshapeCacheWarm(context.Background(), nil, []Message{{Role: "user", Content: "settled"}})
	agent.armCacheWarm()
	time.Sleep(200 * time.Millisecond)
	if _, warm := rec.counts(); warm != 0 {
		t.Fatalf("reshape without a snapshot produced %d replay(s)", warm)
	}

	system := Message{Role: "system", Content: "sys"}
	inLoop := []Message{system, {Role: "user", Content: "in-loop"}}
	agent.observeWarmable(chatRequest{Model: "test-model", Messages: inLoop}, 5000)

	agent.ReshapeCacheWarm(context.Background(), nil, nil)
	agent.ReshapeCacheWarm(context.Background(), nil, []Message{})
	// Clone runs share the cacheWarmState through the copy chain but never
	// write it (same guards as observeWarmable).
	agent.AsWorkflowExecutor().ReshapeCacheWarm(context.Background(), nil, []Message{{Role: "user", Content: "workflow"}})
	agent.WithToolAgentRole().ReshapeCacheWarm(context.Background(), nil, []Message{{Role: "user", Content: "tool-agent"}})

	agent.armCacheWarm()
	select {
	case body := <-rec.warmCh:
		if got := warmMessages(t, body); !reflect.DeepEqual(got, inLoop) {
			t.Errorf("warm replay messages = %v, want the untouched in-loop snapshot %v", got, inLoop)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no cache warm replay arrived")
	}
}

// TestCacheWarm_ReshapeBelowFloorDropsReplay pins the floor on the reshaped
// size: armCacheWarm's cacheWarmMinBodyBytes rule still applies after the
// snapshot shrinks — a prefix that small is not worth a replay.
func TestCacheWarm_ReshapeBelowFloorDropsReplay(t *testing.T) {
	srv, rec := cacheWarmServer(t)

	agent := New(srv.URL, "test-model").
		WithCacheWarming("auto").
		withCacheWarmDelay(60 * time.Millisecond).
		withCacheWarmMinBytes(1024)

	agent.observeWarmable(chatRequest{
		Model:    "test-model",
		Messages: []Message{{Role: "system", Content: "sys"}, {Role: "user", Content: "big enough to warm"}},
	}, 5000)
	agent.armCacheWarm()
	agent.ReshapeCacheWarm(context.Background(), nil, []Message{{Role: "user", Content: "tiny"}})

	time.Sleep(300 * time.Millisecond)
	if _, warm := rec.counts(); warm != 0 {
		t.Errorf("reshaped snapshot below the floor still sent %d replay(s)", warm)
	}
}

// TestCacheWarm_ReshapeReachesCopyChain pins the pointer sharing the cmd/milk
// wiring relies on: the turn runs on a WithMemConfig copy of the runner's
// wrapped agent, while the post-persist reshape goes through the wrapped
// agent itself — both must see the same cacheWarmState.
func TestCacheWarm_ReshapeReachesCopyChain(t *testing.T) {
	srv, rec := cacheWarmServer(t)

	agent := New(srv.URL, "test-model").
		WithCacheWarming("auto").
		withCacheWarmDelay(60 * time.Millisecond).
		withCacheWarmMaxIdle(80 * time.Millisecond).
		withCacheWarmMinBytes(1)
	turnAgent := agent.WithMemConfig(MemConfig{})

	system := Message{Role: "system", Content: "sys"}
	turnAgent.observeWarmable(chatRequest{
		Model:    "test-model",
		Messages: []Message{system, {Role: "user", Content: "in-loop"}},
	}, 5000)

	reshaped := []Message{
		{Role: "user", Content: "[user to primary] in-loop"},
		{Role: "assistant", Content: "settled"},
	}
	agent.ReshapeCacheWarm(context.Background(), nil, reshaped) // through the wrapped agent, not the turn copy
	turnAgent.armCacheWarm()

	select {
	case body := <-rec.warmCh:
		want := append([]Message{system}, reshaped...)
		if got := warmMessages(t, body); !reflect.DeepEqual(got, want) {
			t.Errorf("warm replay messages = %v, want %v", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no cache warm replay arrived — the reshape missed the turn copy's state")
	}
}
