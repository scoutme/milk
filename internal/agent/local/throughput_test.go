package local

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestStreamCompletion_Timing measures StreamTiming against a mock
// OpenAI-compatible SSE server with known ground truth: a simulated prefill
// delay followed by a fixed-rate token stream. Mirrors the validation probe
// that established the measurement math (completion_tokens / generation
// window) as accurate to <0.2% against a simulated rate.
func TestStreamCompletion_Timing(t *testing.T) {
	const (
		prefill   = 300 * time.Millisecond
		chunks    = 20
		interval  = 25 * time.Millisecond // ~40 tok/s simulated decode
		simulated = 20                    // completion_tokens reported in usage
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) //nolint:errcheck
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		write := func(s string) {
			fmt.Fprint(w, s)
			flusher.Flush()
		}
		time.Sleep(prefill)
		for i := 0; i < chunks; i++ {
			write(`data: {"choices":[{"delta":{"content":"x"}}]}` + "\n\n")
			time.Sleep(interval)
		}
		write(fmt.Sprintf(`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":%d}}`, simulated) + "\n\n")
		write("data: [DONE]\n\n")
	}))
	defer srv.Close()

	agent := New(srv.URL, "test-model")
	var got StreamTiming
	agent.WithOnTokens(func(_, _ string, _, completion, _, _ int64, timing StreamTiming) {
		got = timing
		if completion != simulated {
			t.Errorf("completion = %d, want %d", completion, simulated)
		}
	})

	var out strings.Builder
	if _, _, _, _, _, err := agent.streamCompletion(context.Background(),
		[]Message{{Role: "user", Content: "hi"}}, nil, &out, 0); err != nil {
		t.Fatalf("streamCompletion: %v", err)
	}

	// TTFT ≈ prefill; Decode ≈ (chunks-1)*interval = 475ms. Bounds are
	// deliberately generous — this asserts the measurement wiring, not the
	// scheduler; the sub-percent accuracy claim was validated out-of-band.
	if got.TTFT < prefill || got.TTFT > 3*time.Second {
		t.Errorf("TTFT = %v, want >= %v and sane", got.TTFT, prefill)
	}
	decodeLo, decodeHi := 250*time.Millisecond, 3*time.Second
	if got.Decode < decodeLo || got.Decode > decodeHi {
		t.Errorf("Decode = %v, want within [%v, %v]", got.Decode, decodeLo, decodeHi)
	}
	rate := float64(simulated) / got.Decode.Seconds()
	if rate < 2 || rate > 200 {
		t.Errorf("implied rate = %.1f tok/s, want a sane value near %.0f", rate, float64(simulated)/((chunks-1)*interval).Seconds())
	}
}

// TestStreamCompletion_TimingZeroOnNoOutput: a stream with no output deltas
// at all (usage only) reports zero timing — no fabricated TTFT/decode.
func TestStreamCompletion_TimingZeroOnNoOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) //nolint:errcheck
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":0}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	agent := New(srv.URL, "test-model")
	var got StreamTiming
	got.TTFT = -1 // sentinel: onTokens must overwrite it
	agent.WithOnTokens(func(_, _ string, _, _, _, _ int64, timing StreamTiming) {
		got = timing
	})

	var out strings.Builder
	if _, _, _, _, _, err := agent.streamCompletion(context.Background(),
		[]Message{{Role: "user", Content: "hi"}}, nil, &out, 0); err != nil {
		t.Fatalf("streamCompletion: %v", err)
	}
	if got.TTFT != 0 || got.Decode != 0 {
		t.Errorf("timing = %+v, want zero for an output-less stream", got)
	}
}
