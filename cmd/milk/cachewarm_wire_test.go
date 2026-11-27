package main

// Wire-level equivalence test for the idle cache-warm replay (issue #471).
//
// The replay re-sends "system message of the settled turn's request" followed
// by the history the reshape seam was handed after the turn was persisted. For
// that to re-prime anything the NEXT turn needs, the bytes must be a prefix of
// what the next turn's first request actually puts on the wire. The unit tests
// in cachewarm_reshape_test.go pin the seam's wiring but compare the builder
// with itself; this test drives two real turns of a real local agent against a
// scripted OpenAI-compatible server and compares against the captured bodies.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/session"
)

// wireScriptServer records the raw JSON of every chat request's messages and
// answers from a fixed script: the first request asks for a read_file tool
// call, every later one is a plain text reply.
type wireScriptServer struct {
	mu       sync.Mutex
	messages [][]json.RawMessage // per request, per message raw JSON
	readPath string
}

func (s *wireScriptServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []json.RawMessage `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		s.mu.Lock()
		s.messages = append(s.messages, req.Messages)
		n := len(s.messages)
		s.mu.Unlock()
		// Script: a request whose last message is a user turn asking to
		// "read" gets a read_file call; everything else (tool results,
		// plain follow-ups) gets a text reply.
		wantTool, wantWrite := false, false
		if k := len(req.Messages); k > 0 {
			var last struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			}
			_ = json.Unmarshal(req.Messages[k-1], &last)
			wantTool = last.Role == "user" && (strings.Contains(last.Content, "read the big file") || strings.Contains(last.Content, "create the notes file"))
			wantWrite = last.Role == "user" && strings.Contains(last.Content, "create the notes file")
		}

		w.Header().Set("Content-Type", "text/event-stream")
		var delta map[string]any
		finish := "stop"
		if wantTool {
			name, a := "read_file", map[string]any{"path": s.readPath}
			if wantWrite {
				name, a = "write_file", map[string]any{"path": filepath.Join(filepath.Dir(s.readPath), "NOTES.md"), "content": "notes\n"}
			}
			args, _ := json.Marshal(a)
			delta = map[string]any{"tool_calls": []map[string]any{{
				"index": 0, "id": "call_1", "type": "function",
				"function": map[string]any{"name": name, "arguments": string(args)},
			}}}
			finish = "tool_calls"
		} else {
			delta = map[string]any{"content": fmt.Sprintf("reply %d", n)}
		}
		b, _ := json.Marshal(map[string]any{"choices": []map[string]any{{"delta": delta, "finish_reason": finish}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
	})
}

// warmCaptureRunner forwards to the real localRunner and records the history
// the dispatch layer hands the cache-warm reshape seam after each settled turn.
type warmCaptureRunner struct {
	*localRunner
	reshaped [][]local.Message
}

func (c *warmCaptureRunner) ReshapeCacheWarm(ctx context.Context, sess *session.Session, h []local.Message) {
	c.reshaped = append(c.reshaped, h)
	c.localRunner.ReshapeCacheWarm(ctx, sess, h)
}

func TestCacheWarmReplayIsPrefixOfNextTurnWire(t *testing.T) {
	cases := []struct {
		name    string
		turns   []string // the last one is the "next turn" the replay must prefix
		percept []string // global percepts in the memory store
		need    string
		// diverge is the message index where the replay is EXPECTED to stop
		// matching the next request (-1: whole replay must be a prefix). The one
		// known case is the per-prompt percept block at message 1: the next
		// prompt selects different percepts, which no settle-time replay can
		// predict — the system prompt in front of it must still match.
		diverge int
	}{
		{name: "single tool turn", turns: []string{"read the big file", "thanks, now summarize it"}, diverge: -1},
		{name: "three tool turns", turns: []string{"read the big file", "read the big file again", "read the big file once more", "thanks, now summarize it"}, diverge: -1},
		{name: "tool changes the project tree", turns: []string{"create the notes file", "thanks, now summarize it"}, diverge: -1},
		{name: "current need set", turns: []string{"read the big file", "thanks, now summarize it"}, need: "summarize the big file", diverge: -1},
		{name: "percepts, same topic next", turns: []string{"read the big file", "summarize the big file"}, percept: []string{"the big file lists fox lines", "user prefers terse answers"}, diverge: -1},
		{name: "percepts, other topic next", turns: []string{"read the big file", "thanks, now what about the weather"}, percept: []string{"the big file lists fox lines", "weather in rome is mild"}, diverge: 1},
	}
	for _, layout := range []string{"stable", "classic"} {
		for _, tc := range cases {
			t.Run(layout+"/"+tc.name, func(t *testing.T) {
				// The per-prompt percept divergence is a classic-layout limit only:
				// the stable layout keeps percepts out of the cached prefix.
				tc := tc
				if layout == "stable" {
					tc.diverge = -1
				}
				t.Setenv("MILK_CONFIG_ROOT", t.TempDir())
				dir := t.TempDir()
				big := filepath.Join(dir, "big.txt")
				var sb strings.Builder
				for i := 0; i < 1200; i++ {
					fmt.Fprintf(&sb, "line %04d: the quick brown fox jumps over the lazy dog\n", i)
				}
				if err := os.WriteFile(big, []byte(sb.String()), 0o644); err != nil {
					t.Fatal(err)
				}
				srv := &wireScriptServer{readPath: big}
				ts := httptest.NewServer(srv.handler())
				defer ts.Close()

				// ContextWindowTokens is pinned huge so the message-budget
				// trim (AgentMessageBudget = window*3 chars) can never fire
				// inside the fixture: when it does, the next turn's request
				// becomes a compaction Summarize call and legitimately stops
				// matching the replay. Tool results embed absolute paths, so
				// the fixture's history size is platform-dependent (deeper
				// macOS temp dirs pushed it over the default budget on CI).
				ac := config.AgentConfig{Name: "primary", URL: ts.URL, Model: "m", CacheWarming: "auto", PromptLayout: layout, ContextWindowTokens: 1 << 26}
				cfg := config.Config{Agent: "primary", Agents: []config.AgentConfig{ac}}
				agent := local.NewFromConfig(ac).WithCacheWarming("auto").WithSkipPermissions(true)
				runner := &warmCaptureRunner{localRunner: newLocalRunner(agent, "primary")}

				var mem *memory.Store
				if len(tc.percept) > 0 {
					var err error
					mem, err = memory.NewStore(t.TempDir(), "")
					if err != nil {
						t.Fatal(err)
					}
					for _, c := range tc.percept {
						if _, err := mem.RecordGlobal(context.Background(), c, memory.ProducerUser, memory.ConsumerAll, memory.Roles{}); err != nil {
							t.Fatal(err)
						}
					}
				}
				sess := &session.Session{CWD: dir}
				if tc.need != "" {
					sess.RecordNeed(tc.need)
				}
				var out strings.Builder
				for _, prompt := range tc.turns[:len(tc.turns)-1] {
					if err := runPrimary(context.Background(), cfg, sess, runner, nil, mem, prompt, &out, nil, nil, nil, nil); err != nil {
						t.Fatalf("runPrimary(%q): %v", prompt, err)
					}
				}
				settled := len(runner.reshaped) - 1
				if settled < 0 {
					t.Fatal("no reshape happened")
				}
				settledReplay := agent.CacheWarmMessages()
				if len(settledReplay) == 0 {
					t.Fatal("no replay pending after the settled turn")
				}
				firstOfLast := func() int { srv.mu.Lock(); defer srv.mu.Unlock(); return len(srv.messages) }()
				if err := runPrimary(context.Background(), cfg, sess, runner, nil, mem, tc.turns[len(tc.turns)-1], &out, nil, nil, nil, nil); err != nil {
					t.Fatalf("runPrimary(next): %v", err)
				}

				srv.mu.Lock()
				reqs := srv.messages
				srv.mu.Unlock()
				// Layout contract on the wire: stable keeps the system prompt free of
				// per-turn context and leads the user message with the block; classic
				// does the opposite. The persisted user turns are clean either way.
				lastReq := next0(reqs, firstOfLast)
				sysText, userText := rawContent(lastReq[0]), rawContent(lastReq[len(lastReq)-1])
				if layout == "stable" {
					if strings.Contains(sysText, "Project orientation") {
						t.Error("stable layout: system prompt must not carry the project orientation block")
					}
					if !strings.HasPrefix(userText, "<milk-context>") {
						t.Errorf("stable layout: the new user message must lead with the <milk-context> block, got %.80q", userText)
					}
					for _, p := range tc.percept {
						_ = p // percept presence depends on the relevance gate; covered by unit tests
					}
				} else if !strings.Contains(sysText, "Project orientation") || strings.Contains(userText, "<milk-context>") {
					t.Error("classic layout: project orientation belongs in the system prompt and the user message must stay bare")
				}
				for _, turn := range sess.History {
					if turn.Role == session.RoleUser && strings.Contains(turn.Content, "<milk-context>") {
						t.Errorf("persisted user turn must stay clean, got %.80q", turn.Content)
					}
				}

				// The replay body, as the agent itself would send it (snapshot taken
				// after the settled turn, before the next turn starts).
				replay := make([]json.RawMessage, 0, len(settledReplay))
				for _, m := range settledReplay {
					b, err := json.Marshal(m)
					if err != nil {
						t.Fatal(err)
					}
					replay = append(replay, b)
				}
				next := reqs[firstOfLast] // the next turn's first request

				canon := func(raw json.RawMessage) string {
					var v any
					_ = json.Unmarshal(raw, &v)
					b, _ := json.Marshal(v)
					return string(b)
				}
				t.Logf("replay %d msgs, next request %d msgs", len(replay), len(next))
				for i := range replay {
					if i >= len(next) {
						t.Errorf("replay has %d messages but the next request only %d", len(replay), len(next))
						return
					}
					a, b := canon(replay[i]), canon(next[i])
					if a != b && i == tc.diverge {
						return // the documented limit — everything before it matched
					}
					if a != b {
						t.Errorf("replay diverges from the next turn's wire at message %d/%d (replay %d B, wire %d B)\nreplay: %.200s\nwire:   %.200s",
							i, len(replay), len(a), len(b), a, b)
						return
					}
				}
				if tc.diverge >= 0 {
					t.Errorf("replay fully matched, but the known percept divergence at message %d was expected — the limit is gone, drop the exception", tc.diverge)
				}
			})
		}
	}
}

func next0(reqs [][]json.RawMessage, i int) []json.RawMessage { return reqs[i] }

func rawContent(m json.RawMessage) string {
	var v struct {
		Content string `json:"content"`
	}
	_ = json.Unmarshal(m, &v)
	return v.Content
}
