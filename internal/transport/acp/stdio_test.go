package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHandler records every request/notification it receives and lets a
// test control the response via responder (nil responder returns a trivial
// ok result).
type fakeHandler struct {
	mu        sync.Mutex
	requests  []string
	notifs    []string
	responder func(method string, params json.RawMessage) (any, error)
}

func (h *fakeHandler) HandleRequest(ctx context.Context, method string, params json.RawMessage) (any, error) {
	h.mu.Lock()
	h.requests = append(h.requests, method)
	h.mu.Unlock()
	if h.responder != nil {
		return h.responder(method, params)
	}
	return map[string]any{"ok": true}, nil
}

func (h *fakeHandler) HandleNotification(method string, params json.RawMessage) {
	h.mu.Lock()
	h.notifs = append(h.notifs, method)
	h.mu.Unlock()
}

func (h *fakeHandler) notifications() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.notifs...)
}

// newLoopback wires two StdioConns together over a pair of io.Pipes, each
// running its own Serve loop: a writes to b's reader and vice versa. A
// message a.Notify/a.Request sends is handled by hB (b's handler); one b
// sends is handled by hA.
func newLoopback(t *testing.T, hA, hB Handler) (a, b *StdioConn, stop func()) {
	t.Helper()
	rA, wA := io.Pipe() // b writes here, a reads
	rB, wB := io.Pipe() // a writes here, b reads
	a = NewStdioConn(wB, hA)
	b = NewStdioConn(wA, hB)
	ctx, cancel := context.WithCancel(context.Background())
	go a.Serve(ctx, rA) //nolint:errcheck
	go b.Serve(ctx, rB) //nolint:errcheck
	return a, b, func() {
		cancel()
		wA.Close()
		wB.Close()
	}
}

func TestStdioConn_RequestResponseRoundTrip(t *testing.T) {
	hB := &fakeHandler{responder: func(method string, params json.RawMessage) (any, error) {
		var p map[string]string
		json.Unmarshal(params, &p) //nolint:errcheck
		return map[string]string{"method": method, "echo": p["x"]}, nil
	}}
	a, _, stop := newLoopback(t, &fakeHandler{}, hB)
	defer stop()

	var result map[string]string
	err := a.Request(context.Background(), "test/echo", map[string]string{"x": "y"}, &result)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if result["method"] != "test/echo" || result["echo"] != "y" {
		t.Errorf("result = %v, want method=test/echo echo=y", result)
	}
}

func TestStdioConn_RequestError(t *testing.T) {
	hB := &fakeHandler{responder: func(method string, params json.RawMessage) (any, error) {
		return nil, fmt.Errorf("boom")
	}}
	a, _, stop := newLoopback(t, &fakeHandler{}, hB)
	defer stop()

	err := a.Request(context.Background(), "test/fail", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("Request error = %v, want it to mention \"boom\"", err)
	}
}

func TestStdioConn_Notify(t *testing.T) {
	hA, hB := &fakeHandler{}, &fakeHandler{}
	a, _, stop := newLoopback(t, hA, hB)
	defer stop()

	if err := a.Notify("test/event", map[string]int{"n": 1}); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	deadline := time.After(2 * time.Second)
	for {
		if n := hB.notifications(); len(n) == 1 && n[0] == "test/event" {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("notification never arrived, got %v", hB.notifications())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestStdioConn_ConcurrentRequestsDontSerialize proves a slow request
// doesn't block a second, unrelated request's response from coming back
// first — the exact property session/prompt (held open for a whole turn)
// needs from a second session's session/new.
func TestStdioConn_ConcurrentRequestsDontSerialize(t *testing.T) {
	release := make(chan struct{})
	hB := &fakeHandler{responder: func(method string, params json.RawMessage) (any, error) {
		if method == "test/slow" {
			<-release
		}
		return map[string]string{"method": method}, nil
	}}
	a, _, stop := newLoopback(t, &fakeHandler{}, hB)
	defer stop()

	slowDone := make(chan error, 1)
	go func() {
		var r map[string]string
		slowDone <- a.Request(context.Background(), "test/slow", nil, &r)
	}()

	// The fast request must complete without waiting for the slow one.
	var fast map[string]string
	if err := a.Request(context.Background(), "test/fast", nil, &fast); err != nil {
		t.Fatalf("fast Request: %v", err)
	}
	if fast["method"] != "test/fast" {
		t.Errorf("fast result = %v", fast)
	}

	select {
	case err := <-slowDone:
		t.Fatalf("slow request completed before being released (err=%v) — requests are serializing", err)
	default:
	}

	close(release)
	if err := <-slowDone; err != nil {
		t.Fatalf("slow Request: %v", err)
	}
}

func TestStdioConn_ManyConcurrentRequestsMatchCorrectly(t *testing.T) {
	hB := &fakeHandler{responder: func(method string, params json.RawMessage) (any, error) {
		var p map[string]int
		json.Unmarshal(params, &p) //nolint:errcheck
		return map[string]int{"n": p["n"] * 10}, nil
	}}
	a, _, stop := newLoopback(t, &fakeHandler{}, hB)
	defer stop()

	const n = 50
	var wg sync.WaitGroup
	errs := make([]error, n)
	results := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var r map[string]int
			errs[i] = a.Request(context.Background(), "test/mul", map[string]int{"n": i}, &r)
			results[i] = r["n"]
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("request %d: %v", i, errs[i])
		}
		if results[i] != i*10 {
			t.Errorf("request %d: result = %d, want %d (cross-wired response)", i, results[i], i*10)
		}
	}
}

// TestStdioConn_LargePayload exercises a payload well past the default
// bufio.Scanner 64KB token limit, confirming the 8MB buffer growth in Serve
// actually takes effect.
func TestStdioConn_LargePayload(t *testing.T) {
	big := strings.Repeat("x", 500*1024)
	hA, hB := &fakeHandler{}, &fakeHandler{}
	a, _, stop := newLoopback(t, hA, hB)
	defer stop()

	if err := a.Notify("test/big", map[string]string{"data": big}); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	deadline := time.After(2 * time.Second)
	for {
		if len(hB.notifications()) == 1 {
			return
		}
		select {
		case <-deadline:
			t.Fatal("large notification never arrived")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestStdioConn_RequestContextCancel(t *testing.T) {
	hB := &fakeHandler{responder: func(method string, params json.RawMessage) (any, error) {
		<-context.Background().Done() // never responds
		return nil, nil
	}}
	a, _, stop := newLoopback(t, &fakeHandler{}, hB)
	defer stop()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- a.Request(ctx, "test/never", nil, nil)
	}()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("expected a context-cancellation error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Request did not return after ctx cancel")
	}
}
