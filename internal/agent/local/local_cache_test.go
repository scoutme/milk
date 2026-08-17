package local

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/session"
)

// sseServerWithUsage returns a test server that streams text as a content
// delta chunk, then a final usage-only chunk (mirroring how real OpenAI-compat
// servers report usage in the last SSE event with stream_options.include_usage),
// then [DONE]. usageJSON is embedded verbatim as the "usage" field value so
// tests can exercise arbitrary usage shapes, including the optional
// prompt_tokens_details.cached_tokens field.
func sseServerWithUsage(t *testing.T, text, usageJSON string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", text)
		fmt.Fprintf(w, "data: {\"choices\":[],\"usage\":%s}\n\n", usageJSON)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

// TestOnTokens_CachedTokensReported verifies that when the final SSE usage
// chunk includes prompt_tokens_details.cached_tokens (confirmed live against
// mimo-pro-local, both chat-completions streaming and non-streaming shapes),
// the value reaches the onTokens callback as cacheRead, with cacheCreation
// always 0 (OpenAI-style automatic caching reports no write/creation size).
func TestOnTokens_CachedTokensReported(t *testing.T) {
	srv := sseServerWithUsage(t, "hello",
		`{"completion_tokens":17,"prompt_tokens":619,"prompt_tokens_details":{"cached_tokens":576}}`)
	defer srv.Close()

	agent := New(srv.URL, "test-model")

	var gotPrompt, gotCompletion, gotCacheRead, gotCacheCreation int64
	var calls int
	agent.WithOnTokens(func(model, role string, prompt, completion, cacheRead, cacheCreation int64) {
		calls++
		gotPrompt, gotCompletion, gotCacheRead, gotCacheCreation = prompt, completion, cacheRead, cacheCreation
	})

	sess := &session.Session{}
	var out strings.Builder
	if _, err := agent.Run(context.Background(), nil, "hi", &out, sess, nil); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if calls != 1 {
		t.Fatalf("want onTokens called once, got %d", calls)
	}
	if gotPrompt != 619 || gotCompletion != 17 {
		t.Errorf("want prompt=619 completion=17, got prompt=%d completion=%d", gotPrompt, gotCompletion)
	}
	if gotCacheRead != 576 {
		t.Errorf("want cacheRead=576, got %d", gotCacheRead)
	}
	if gotCacheCreation != 0 {
		t.Errorf("want cacheCreation=0, got %d", gotCacheCreation)
	}
}

// TestOnTokens_NoPromptTokensDetails_RegressionGuard verifies that when
// prompt_tokens_details is absent entirely from the usage chunk (every
// provider that doesn't report automatic-caching stats), cacheRead reported
// to onTokens is 0 and no parse error occurs.
func TestOnTokens_NoPromptTokensDetails_RegressionGuard(t *testing.T) {
	srv := sseServerWithUsage(t, "hello",
		`{"completion_tokens":5,"prompt_tokens":100}`)
	defer srv.Close()

	agent := New(srv.URL, "test-model")

	var gotPrompt, gotCompletion, gotCacheRead, gotCacheCreation int64
	agent.WithOnTokens(func(model, role string, prompt, completion, cacheRead, cacheCreation int64) {
		gotPrompt, gotCompletion, gotCacheRead, gotCacheCreation = prompt, completion, cacheRead, cacheCreation
	})

	sess := &session.Session{}
	var out strings.Builder
	if _, err := agent.Run(context.Background(), nil, "hi", &out, sess, nil); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if gotPrompt != 100 || gotCompletion != 5 {
		t.Errorf("want prompt=100 completion=5, got prompt=%d completion=%d", gotPrompt, gotCompletion)
	}
	if gotCacheRead != 0 || gotCacheCreation != 0 {
		t.Errorf("want cacheRead=0 cacheCreation=0 when prompt_tokens_details is absent, got cacheRead=%d cacheCreation=%d", gotCacheRead, gotCacheCreation)
	}
}
