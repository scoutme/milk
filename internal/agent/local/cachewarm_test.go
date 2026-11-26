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
