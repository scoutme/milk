package main

// Direct unit tests of acpServer's request/notification handlers against a
// fake acp.Conn and a stubbed local-agent HTTP backend — no subprocess, no
// stdio transport. Fast and 100% deterministic (this exact scenario was
// also what isolated the real-subprocess e2e suite's flakiness to the OS
// pipe/process layer rather than to this code: 15/15 runs here complete in
// 5-9ms with zero failures). Complements, not replaces,
// serve_acp_e2e_test.go's real-binary round trip.

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/loop"
	"github.com/scoutme/milk/internal/router"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/transport/acp"
)

// acpTestServer spins up a stub OpenAI-compatible backend and a fresh
// scratch $HOME pointing at it, loads the resulting config, and returns a
// ready-to-use acpServer backed by a fakeACPConn.
func acpTestServer(t *testing.T, reply string) (*acpServer, *fakeACPConn) {
	t.Helper()
	return acpTestServerWithHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", reply)
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
}

// acpTestServerWithHandler is acpTestServer with a caller-supplied stub
// backend, for tests that need to inspect what milk sent the model.
func acpTestServerWithHandler(t *testing.T, h http.HandlerFunc) (*acpServer, *fakeACPConn) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	home := t.TempDir()
	milkDir := filepath.Join(home, ".milk")
	if err := os.MkdirAll(milkDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	cfgJSON := fmt.Sprintf(`{"agent":"test-local","agents":[{"name":"test-local","url":%q,"model":"test-model","provider":"local"}]}`, srv.URL)
	if err := os.WriteFile(filepath.Join(milkDir, "config.json"), []byte(cfgJSON), 0o600); err != nil {
		t.Fatalf("WriteFile config: %v", err)
	}
	t.Setenv("HOME", home)

	cfg, err := config.LoadMerged()
	if err != nil {
		t.Fatalf("LoadMerged: %v", err)
	}

	conn := &fakeACPConn{}
	return newACPServer(cfg, conn), conn
}

func acpRequest(t *testing.T, s *acpServer, method string, params any) any {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	result, err := s.HandleRequest(context.Background(), method, raw)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return result
}

func TestACPServer_Initialize(t *testing.T) {
	server, _ := acpTestServer(t, "ok")

	result := acpRequest(t, server, "initialize", acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersion})
	resp, ok := result.(acp.InitializeResponse)
	if !ok {
		t.Fatalf("result = %T, want acp.InitializeResponse", result)
	}
	if resp.ProtocolVersion != acp.ProtocolVersion {
		t.Errorf("ProtocolVersion = %d, want %d", resp.ProtocolVersion, acp.ProtocolVersion)
	}
	if resp.Capabilities.Session == nil {
		t.Error("Capabilities.Session is nil, want the baseline SessionCapabilities{}")
	}
}

func TestACPServer_SessionNewAndPrompt(t *testing.T) {
	server, conn := acpTestServer(t, "hello from the stub")
	cwd := t.TempDir()

	result := acpRequest(t, server, "session/new", acp.NewSessionRequest{CWD: cwd})
	newResp, ok := result.(acp.NewSessionResponse)
	if !ok || newResp.SessionID == "" {
		t.Fatalf("session/new result = %+v (ok=%v)", result, ok)
	}

	result = acpRequest(t, server, "session/prompt", acp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt:    []acp.ContentBlock{acp.TextBlock("say hi")},
	})
	promptResp, ok := result.(acp.PromptResponse)
	if !ok || promptResp.MessageID == "" {
		t.Fatalf("session/prompt result = %+v (ok=%v)", result, ok)
	}

	// Notifications must show running -> agent_message_chunk -> idle, in
	// that order, all before the response above was ever returned (proven
	// by HandleRequest already having returned the response synchronously
	// to this point — session/prompt blocks for the whole turn per the
	// upstream schema).
	sent := conn.sent()
	var sawRunning, sawChunk, sawIdle bool
	var chunkText string
	for _, n := range sent {
		// acpSession.notify always sends session/update wrapped in
		// UpdateSessionNotification (via acp.Mapper.Update) — never the bare
		// SessionUpdate.
		wrapped, ok := n.Params.(acp.UpdateSessionNotification)
		if !ok {
			continue
		}
		switch u := wrapped.Update.(type) {
		case acp.StateUpdate:
			if u.State == acp.SessionStateRunning {
				sawRunning = true
			}
			if u.State == acp.SessionStateIdle {
				sawIdle = true
			}
		case acp.ContentChunk:
			sawChunk = true
			chunkText = u.Content.Text
		}
	}
	if !sawRunning {
		t.Error("never saw a running state_update")
	}
	if !sawChunk || chunkText != "hello from the stub" {
		t.Errorf("agent_message_chunk missing or wrong text: saw=%v text=%q", sawChunk, chunkText)
	}
	if !sawIdle {
		t.Error("never saw an idle state_update")
	}
}

func TestACPServer_PromptUnknownSession(t *testing.T) {
	server, _ := acpTestServer(t, "ok")

	raw, _ := json.Marshal(acp.PromptRequest{SessionID: "does-not-exist", Prompt: []acp.ContentBlock{acp.TextBlock("hi")}})
	_, err := server.HandleRequest(context.Background(), "session/prompt", raw)
	if err == nil {
		t.Fatal("expected an error for an unknown session")
	}
}

func TestACPServer_UnknownMethod(t *testing.T) {
	server, _ := acpTestServer(t, "ok")

	_, err := server.HandleRequest(context.Background(), "session/load", json.RawMessage(`{}`))
	if _, ok := err.(*acp.MethodNotFoundError); !ok {
		t.Fatalf("err = %v (%T), want *acp.MethodNotFoundError", err, err)
	}
}

func TestACPServer_Cancel(t *testing.T) {
	server, _ := acpTestServer(t, "ok")
	cwd := t.TempDir()

	result := acpRequest(t, server, "session/new", acp.NewSessionRequest{CWD: cwd})
	sessionID := result.(acp.NewSessionResponse).SessionID

	// No turn is in flight, so cancel must be a safe no-op (not a panic/error) —
	// HandleNotification has no return value to assert on; reaching the next
	// line without panicking is the assertion.
	raw, _ := json.Marshal(acp.CancelSessionNotification{SessionID: sessionID})
	server.HandleNotification("session/cancel", raw)
}

func TestACPServer_AfterResponseAdvertisesCommands(t *testing.T) {
	server, conn := acpTestServer(t, "ok")
	result := acpRequest(t, server, "session/new", acp.NewSessionRequest{CWD: t.TempDir()})

	if n := len(conn.sent()); n != 0 {
		t.Fatalf("session/new itself sent %d notifications, want 0 (commands go out after the response)", n)
	}
	server.AfterResponse("session/new", result)

	sent := conn.sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d notifications, want 1", len(sent))
	}
	wrapped, ok := sent[0].Params.(acp.UpdateSessionNotification)
	if !ok {
		t.Fatalf("params = %T, want UpdateSessionNotification", sent[0].Params)
	}
	upd, ok := wrapped.Update.(acp.AvailableCommandsUpdate)
	if !ok || len(upd.AvailableCommands) == 0 {
		t.Fatalf("update = %+v, want non-empty AvailableCommandsUpdate", wrapped.Update)
	}

	server.AfterResponse("session/prompt", result)
	if n := len(conn.sent()); n != 1 {
		t.Errorf("non-session/new method sent commands: %d notifications", n)
	}
}

func acpPromptText(t *testing.T, server *acpServer, id acp.SessionID, text string) {
	t.Helper()
	acpRequest(t, server, "session/prompt", acp.PromptRequest{
		SessionID: id,
		Prompt:    []acp.ContentBlock{acp.TextBlock(text)},
	})
}

// acpChunks returns the agent_message_chunk texts sent so far.
func acpChunks(conn *fakeACPConn) []string {
	var out []string
	for _, n := range conn.sent() {
		if w, ok := n.Params.(acp.UpdateSessionNotification); ok {
			if c, ok := w.Update.(acp.ContentChunk); ok {
				out = append(out, c.Content.Text)
			}
		}
	}
	return out
}

func acpNewSession(t *testing.T, server *acpServer) acp.SessionID {
	t.Helper()
	result := acpRequest(t, server, "session/new", acp.NewSessionRequest{CWD: t.TempDir()})
	return result.(acp.NewSessionResponse).SessionID
}

func TestACPCommands_AdvertisedEqualsExecutable(t *testing.T) {
	server, _ := acpTestServer(t, "ok")
	id := acpNewSession(t, server)
	as := server.session(id)

	adv := acpAdvertisedCommands()
	if len(adv) == 0 {
		t.Fatal("no commands advertised")
	}
	for _, c := range adv {
		if c.Description == "" || strings.HasPrefix(c.Name, "/") {
			t.Errorf("bad wire command: %+v", c)
		}
		handled, out, _ := as.runSlashCommand(&acpTurn{ctx: context.Background(), as: as, say: func(string) {}}, "/"+c.Name)
		if !handled {
			t.Errorf("/%s is advertised but not handled", c.Name)
		}
		if strings.Contains(out, "only available in the milk TUI") {
			t.Errorf("/%s is advertised but rejected as TUI-only", c.Name)
		}
	}
}

func TestACPCommands_HelpAndUnsupportedDontReachModel(t *testing.T) {
	server, conn := acpTestServer(t, "model reply")
	id := acpNewSession(t, server)

	acpPromptText(t, server, id, "/help")
	acpPromptText(t, server, id, "/panel memory")

	chunks := acpChunks(conn)
	if len(chunks) != 2 {
		t.Fatalf("chunks = %q, want exactly the two command outputs (model must not be called)", chunks)
	}
	if !strings.Contains(chunks[0], "/think") {
		t.Errorf("/help output = %q", chunks[0])
	}
	if !strings.Contains(chunks[1], "only available in the milk TUI") {
		t.Errorf("/panel output = %q", chunks[1])
	}
	for _, c := range chunks {
		if strings.Contains(c, "model reply") || strings.Contains(c, "\x1b[") {
			t.Errorf("unexpected content in %q", c)
		}
	}
}

func TestACPCommands_ThinkGatesThoughtChunks(t *testing.T) {
	server, conn := acpTestServer(t, "ok")
	id := acpNewSession(t, server)
	as := server.session(id)

	acpPromptText(t, server, id, "/think off")
	before := len(conn.sent())
	as.onThinking("secret reasoning")
	if len(conn.sent()) != before {
		t.Fatal("thought forwarded while /think off")
	}

	acpPromptText(t, server, id, "/think on")
	before = len(conn.sent())
	as.onThinking("visible reasoning")
	if len(conn.sent()) != before+1 {
		t.Error("thought not forwarded after /think on")
	}
}

func TestACPCommands_PrimaryWithMessageDispatchesTurn(t *testing.T) {
	server, conn := acpTestServer(t, "model reply")
	id := acpNewSession(t, server)

	acpPromptText(t, server, id, "/primary say hi")

	chunks := acpChunks(conn)
	found := false
	for _, c := range chunks {
		if strings.Contains(c, "model reply") {
			found = true
		}
	}
	if !found {
		t.Errorf("/primary <msg> did not dispatch a model turn; chunks = %q", chunks)
	}
}

func TestACPCommands_EscalatePinsStickyRouting(t *testing.T) {
	server, conn := acpTestServer(t, "ok")
	id := acpNewSession(t, server)
	as := server.session(id)

	acpPromptText(t, server, id, "/escalate")
	if !as.st.stickyEscalate {
		t.Error("/escalate did not pin routing")
	}
	acpPromptText(t, server, id, "/primary")
	if as.st.stickyEscalate || !as.st.stickyPrimary {
		t.Errorf("/primary: stickyEscalate=%v stickyPrimary=%v", as.st.stickyEscalate, as.st.stickyPrimary)
	}
	if n := len(acpChunks(conn)); n != 2 {
		t.Errorf("want one confirmation chunk per command, got %d", n)
	}
}

func TestACPCommands_MidSentenceSlashIsPlainPrompt(t *testing.T) {
	server, conn := acpTestServer(t, "model reply")
	id := acpNewSession(t, server)

	acpPromptText(t, server, id, "what does /panel do?")

	chunks := acpChunks(conn)
	if len(chunks) != 1 || chunks[0] != "model reply" {
		t.Errorf("chunks = %q, want the model's reply only", chunks)
	}
}

// acpPlanUpdates returns the plan-ish session updates (v2 PlanUpdate, v1 PlanV1Update) sent so far.
func acpPlanUpdates(conn *fakeACPConn) []acp.SessionUpdate {
	var out []acp.SessionUpdate
	for _, n := range conn.sent() {
		if w, ok := n.Params.(acp.UpdateSessionNotification); ok {
			switch w.Update.(type) {
			case acp.PlanUpdate, acp.PlanV1Update:
				out = append(out, w.Update)
			}
		}
	}
	return out
}

func TestACPTasks_CommandsAndPlanUpdates(t *testing.T) {
	server, conn := acpTestServer(t, "ok")
	id := acpNewSession(t, server)
	as := server.session(id)

	task, err := as.taskStore.Create("write the report", nil)
	if err != nil {
		t.Fatal(err)
	}
	plans := acpPlanUpdates(conn)
	if len(plans) != 1 {
		t.Fatalf("task create emitted %d plan updates, want 1", len(plans))
	}
	pu, ok := plans[0].(acp.PlanUpdate)
	if !ok || len(pu.Plan.Entries) != 1 || pu.Plan.Entries[0].Content != "write the report" {
		t.Fatalf("plan = %+v", plans[0])
	}

	acpPromptText(t, server, id, "/tasks")
	acpPromptText(t, server, id, "/task done "+task.ID)
	chunks := strings.Join(acpChunks(conn), "\n")
	if !strings.Contains(chunks, "write the report") || !strings.Contains(chunks, "marked done") {
		t.Errorf("command output = %q", chunks)
	}
	plans = acpPlanUpdates(conn)
	last := plans[len(plans)-1].(acp.PlanUpdate)
	if last.Plan.Entries[0].Status != acp.PlanEntryCompleted {
		t.Errorf("after /task done, plan status = %s", last.Plan.Entries[0].Status)
	}
}

func TestACPTasks_V1ClientGetsV1PlanShape(t *testing.T) {
	server, conn := acpTestServer(t, "ok")
	acpRequest(t, server, "initialize", acp.InitializeRequest{ProtocolVersion: 1})
	id := acpNewSession(t, server)
	as := server.session(id)

	if _, err := as.taskStore.Create("v1 task", nil); err != nil {
		t.Fatal(err)
	}
	plans := acpPlanUpdates(conn)
	if len(plans) != 1 {
		t.Fatalf("plans = %d", len(plans))
	}
	if _, ok := plans[0].(acp.PlanV1Update); !ok {
		t.Fatalf("v1 client got %T, want PlanV1Update", plans[0])
	}

	// A second plan must be merged into the single v1 plan, not replace it.
	as.notifyPlan("workflow-1", []acp.PlanEntry{{Content: "stage", Priority: acp.PlanPriorityHigh, Status: acp.PlanEntryInProgress}})
	plans = acpPlanUpdates(conn)
	merged := plans[len(plans)-1].(acp.PlanV1Update)
	if len(merged.Entries) != 2 {
		t.Errorf("merged v1 plan has %d entries, want tasks + workflow = 2: %+v", len(merged.Entries), merged.Entries)
	}
}

func TestACPTools_TaskAndBackgroundToolsAreOffered(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	server, _ := acpTestServerWithHandler(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	acpPromptText(t, server, acpNewSession(t, server), "hello")

	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(bodies, "\n")
	for _, tool := range []string{"create_task", "spawn_background_agent", "start_workflow"} {
		if !strings.Contains(joined, tool) {
			t.Errorf("model was not offered %s", tool)
		}
	}
}

// acpWaitFor polls cond for up to 10 s.
func acpWaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func acpJobCompleted(conn *fakeACPConn, callID acp.ToolCallID) func() bool {
	return func() bool {
		for _, n := range conn.sent() {
			if w, ok := n.Params.(acp.UpdateSessionNotification); ok {
				if u, ok := w.Update.(acp.ToolCallUpdate); ok && u.ToolCallID == callID && u.Status == acp.ToolCallCompleted {
					return true
				}
			}
		}
		return false
	}
}

func TestACPBackground_StartNotifiesAndFollowsUpAutomatically(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	server, conn := acpTestServerWithHandler(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"job-finding-xyz\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	id := acpNewSession(t, server)

	acpPromptText(t, server, id, "/bg start investigate the thing")
	acpWaitFor(t, "job row completed", acpJobCompleted(conn, "job:job_1"))

	// No further client prompt: milk must run the follow-up turn itself.
	acpWaitFor(t, "automatic follow-up request", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(strings.Join(bodies, "\n"), local.BackgroundFollowupPrompt)
	})
	mu.Lock()
	last := bodies[len(bodies)-1]
	mu.Unlock()
	if !strings.Contains(last, "job-finding-xyz") {
		t.Errorf("follow-up turn was not given the job result:\n%.400s", last)
	}

	// The follow-up is bracketed like any turn: running ... idle, with a reply between.
	acpWaitFor(t, "follow-up to finish", func() bool { return len(acpStates(conn)) >= 4 })
	if st := acpStates(conn); !slices.Equal(st[len(st)-2:], []acp.SessionState{acp.SessionStateRunning, acp.SessionStateIdle}) {
		t.Errorf("state sequence = %v, want the follow-up to end running→idle", st)
	}
	for _, c := range acpChunks(conn) {
		if strings.Contains(c, "will be included with your next message") {
			t.Errorf("old status message still sent: %q", c)
		}
	}
}

func TestACPBackground_FinishedDuringTurnFollowsUpAfterIt(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	release := make(chan struct{})
	server, conn := acpTestServerWithHandler(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		if strings.Contains(string(b), "hold-this-turn") {
			<-release
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	id := acpNewSession(t, server)
	as := server.session(id)

	turnDone := make(chan struct{})
	go func() { defer close(turnDone); acpPromptText(t, server, id, "hold-this-turn") }()
	acpWaitFor(t, "held turn to reach the model", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(strings.Join(bodies, ""), "hold-this-turn")
	})

	as.spawnUserJob("side task") // finishes while the held turn still owns the session
	acpWaitFor(t, "job row completed", acpJobCompleted(conn, "job:job_1"))
	mu.Lock()
	early := strings.Contains(strings.Join(bodies, "\n"), local.BackgroundFollowupPrompt)
	mu.Unlock()
	if early {
		t.Fatal("follow-up ran while a turn was in flight")
	}

	close(release)
	<-turnDone
	acpWaitFor(t, "deferred follow-up", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(strings.Join(bodies, "\n"), local.BackgroundFollowupPrompt)
	})
}

func TestACPBackground_TurnsNeverOverlap(t *testing.T) {
	server, conn := acpTestServer(t, "ok")
	id := acpNewSession(t, server)

	acpPromptText(t, server, id, "/bg start a")
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); acpPromptText(t, server, id, "hello") }()
	}
	wg.Wait()
	time.Sleep(300 * time.Millisecond) // let any trailing follow-up finish

	st := acpStates(conn)
	for i, s := range st {
		want := acp.SessionStateRunning
		if i%2 == 1 {
			want = acp.SessionStateIdle
		}
		if s != want {
			t.Fatalf("state sequence %v breaks running/idle alternation at %d: turns overlapped", st, i)
		}
	}
}

func acpStates(conn *fakeACPConn) []acp.SessionState {
	var out []acp.SessionState
	for _, n := range conn.sent() {
		if w, ok := n.Params.(acp.UpdateSessionNotification); ok {
			if su, ok := w.Update.(acp.StateUpdate); ok {
				out = append(out, su.State)
			}
		}
	}
	return out
}

// acpRowContent collects what the client would show for a tool-call row:
// appended chunks (v2) and replaced content (v1), in order.
func acpRowContent(conn *fakeACPConn, callID acp.ToolCallID) (chunks []string, replaced []string) {
	for _, n := range conn.sent() {
		w, ok := n.Params.(acp.UpdateSessionNotification)
		if !ok {
			continue
		}
		switch u := w.Update.(type) {
		case acp.ToolCallContentChunk:
			if u.ToolCallID == callID {
				chunks = append(chunks, u.Content.Content.Text)
			}
		case acp.ToolCallUpdate:
			if u.ToolCallID == callID && len(u.Content) > 0 {
				replaced = append(replaced, u.Content[0].Content.Text)
			}
		}
	}
	return
}

func TestACPStreaming_BackgroundJobOutputV2AndV1(t *testing.T) {
	for _, tc := range []struct {
		name     string
		protocol int
	}{{"v2 client appends chunks", 2}, {"v1 client gets replaced content", 1}} {
		t.Run(tc.name, func(t *testing.T) {
			server, conn := acpTestServer(t, "job-output-marker")
			acpRequest(t, server, "initialize", acp.InitializeRequest{ProtocolVersion: tc.protocol})
			id := acpNewSession(t, server)

			acpPromptText(t, server, id, "/bg start investigate")
			acpWaitFor(t, "job row completed", acpJobCompleted(conn, "job:job_1"))

			chunks, replaced := acpRowContent(conn, "job:job_1")
			if tc.protocol == 1 {
				if len(chunks) != 0 || len(replaced) == 0 || !strings.Contains(replaced[len(replaced)-1], "job-output-marker") {
					t.Errorf("v1: chunks=%q replaced=%q", chunks, replaced)
				}
				return
			}
			if len(replaced) != 0 || !strings.Contains(strings.Join(chunks, ""), "job-output-marker") {
				t.Errorf("v2: chunks=%q replaced=%q", chunks, replaced)
			}
		})
	}
}

func TestACPStreaming_WorkflowStageOutputPrecedesCompletion(t *testing.T) {
	server, conn := acpTestServer(t, "stage-output-marker")
	writeTinyWorkflow(t)
	id := acpNewSession(t, server)

	acpPromptText(t, server, id, "/workflow tiny go --writer test-local")

	chunks, _ := acpRowContent(conn, "workflow:0")
	joined := strings.Join(chunks, "")
	if !strings.Contains(joined, "stage-output-marker") || !strings.Contains(joined, "── ") {
		t.Fatalf("workflow row content = %q, want stage output under a role header", joined)
	}

	// Ordering: every content chunk is sent before the row's terminal status.
	lastChunk, completed := -1, -1
	for i, n := range conn.sent() {
		w, ok := n.Params.(acp.UpdateSessionNotification)
		if !ok {
			continue
		}
		switch u := w.Update.(type) {
		case acp.ToolCallContentChunk:
			if u.ToolCallID == "workflow:0" {
				lastChunk = i
			}
		case acp.ToolCallUpdate:
			if u.ToolCallID == "workflow:0" && u.Status == acp.ToolCallCompleted {
				completed = i
			}
		}
	}
	if completed < 0 || lastChunk > completed {
		t.Errorf("last chunk at %d, completed at %d: output must precede completion", lastChunk, completed)
	}
}

func acpExtMethods(conn *fakeACPConn) []string {
	var out []string
	for _, n := range conn.sent() {
		if strings.HasPrefix(n.Method, "_milk/") {
			out = append(out, n.Method)
		}
	}
	return out
}

func TestACPSignals_RouteIsAnnouncedAndAttachedToIdle(t *testing.T) {
	server, conn := acpTestServer(t, "hello")
	id := acpNewSession(t, server)

	acpPromptText(t, server, id, "hi")

	if got := acpExtMethods(conn); len(got) != 1 || got[0] != acp.ExtMethodRoute {
		t.Fatalf("ext notifications = %v, want one %s", got, acp.ExtMethodRoute)
	}
	var idle *acp.StateUpdate
	for _, n := range conn.sent() {
		if w, ok := n.Params.(acp.UpdateSessionNotification); ok {
			if su, ok := w.Update.(acp.StateUpdate); ok && su.State == acp.SessionStateIdle {
				idle = &su
			}
		}
	}
	if idle == nil || idle.Meta["milk/route"] == nil {
		t.Fatalf("idle state has no milk/route _meta: %+v", idle)
	}
	for _, c := range acpChunks(conn) {
		if strings.Contains(c, "now handled by") {
			t.Errorf("first turn narrated its route: %q", c)
		}
	}
}

func TestACPSignals_RouteChangeIsNarratedOnce(t *testing.T) {
	server, conn := acpTestServer(t, "ok")
	as := server.session(acpNewSession(t, server))

	as.announceRoute(router.Decision{Target: router.TargetLocal}, router.TargetLocal, "small")
	as.announceRoute(router.Decision{Target: router.TargetLocal}, router.TargetLocal, "small")
	if len(acpChunks(conn)) != 0 {
		t.Fatalf("unchanged route was narrated: %q", acpChunks(conn))
	}
	as.announceRoute(router.Decision{Target: router.TargetEscalation, Reason: "complex task"}, router.TargetEscalation, "big")
	chunks := acpChunks(conn)
	if len(chunks) != 1 || !strings.Contains(chunks[0], "now handled by big") || !strings.Contains(chunks[0], "complex task") {
		t.Errorf("route change message = %q", chunks)
	}
}

func TestACPSignals_InitializeAdvertisesExtensions(t *testing.T) {
	server, _ := acpTestServer(t, "ok")
	resp := acpRequest(t, server, "initialize", acp.InitializeRequest{ProtocolVersion: 1}).(acp.InitializeResponse)
	milk, _ := resp.Capabilities.Meta["milk"].(map[string]any)
	advertised, _ := milk["notifications"].([]string)
	if !slices.Contains(advertised, acp.ExtMethodRoute) || !slices.Contains(advertised, acp.ExtMethodWarning) {
		t.Errorf("capabilities _meta = %v", resp.Capabilities.Meta)
	}
	for _, m := range advertised {
		if !strings.HasPrefix(m, "_") {
			t.Errorf("custom method %q lacks the required underscore prefix", m)
		}
	}
}

func TestACPSignals_WarningsAreMessagesPlusExtAndInterruptCancels(t *testing.T) {
	server, conn := acpTestServer(t, "ok")
	as := server.session(acpNewSession(t, server))

	cancelled := false
	as.mu.Lock()
	as.cancel = func() { cancelled = true }
	as.mu.Unlock()

	as.surfaceVerdicts([]loop.Verdict{{Signal: loop.SignalSilentBurn, Confidence: 0.4, Message: "ignored"}})
	if len(acpChunks(conn)) != 0 || len(acpExtMethods(conn)) != 0 {
		t.Fatal("low-confidence verdict was surfaced")
	}

	as.surfaceVerdicts([]loop.Verdict{{Signal: loop.SignalSilentBurn, Confidence: 0.9, Message: "20000 input tokens, no output"}})
	chunks := acpChunks(conn)
	if len(chunks) != 1 || !strings.Contains(chunks[0], "consumption") || strings.Contains(chunks[0], "auto-interrupted") {
		t.Errorf("warning message = %q", chunks)
	}
	if got := acpExtMethods(conn); len(got) != 1 || got[0] != acp.ExtMethodWarning {
		t.Errorf("ext notifications = %v", got)
	}
	if cancelled {
		t.Error("warn-only verdict cancelled the turn")
	}

	as.surfaceVerdicts([]loop.Verdict{{Signal: loop.SignalReasoningChunkFlood, Confidence: 0.9, Message: "stuck", ShouldInterrupt: true}})
	if !cancelled {
		t.Error("auto-interrupt verdict did not cancel the turn")
	}
	if last := acpChunks(conn); !strings.Contains(last[len(last)-1], "auto-interrupted") {
		t.Errorf("interrupt not explained: %q", last)
	}
}

func TestACPSignals_SilentBurnDetectedFromSessionTokens(t *testing.T) {
	server, conn := acpTestServer(t, "ok")
	as := server.session(acpNewSession(t, server))

	before, beforeC := as.sumTokens()
	as.sess.AddTokensFull("test-model", "primary", 30000, 0, 0, 0)
	as.endTurnSignals(before, beforeC)

	out := strings.Join(acpChunks(conn), "\n")
	if !strings.Contains(out, "consumption") {
		t.Errorf("30k input / 0 output tokens produced no consumption warning: %q", out)
	}
}

func TestACPSignals_RepeatedReasoningIsFlaggedEvenWhenHidden(t *testing.T) {
	server, conn := acpTestServer(t, "ok")
	id := acpNewSession(t, server)
	as := server.session(id)
	acpPromptText(t, server, id, "/think off")

	for i := 0; i < 60; i++ {
		as.onThinking("I should check the config again and then check the config again. ")
	}

	out := strings.Join(acpChunks(conn), "\n")
	if !strings.Contains(out, "loop detected") {
		t.Errorf("repeating reasoning was not flagged with /think off: %q", out)
	}
	for _, n := range conn.sent() {
		if w, ok := n.Params.(acp.UpdateSessionNotification); ok {
			if c, ok := w.Update.(acp.ContentChunk); ok && c.SessionUpdate == "agent_thought_chunk" {
				t.Fatal("hidden reasoning was forwarded")
			}
		}
	}
}

// writeTinyWorkflow registers a one-stage user workflow under the test HOME.
func writeTinyWorkflow(t *testing.T) {
	t.Helper()
	dir := filepath.Join(os.Getenv("HOME"), ".milk", "workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	def := "name: tiny\nroles: [writer]\nstages:\n  - id: write\n    kind: agent_turn\n    role: writer\n    save_as: out\n    prompt: |\n      {{.task}}\n"
	if err := os.WriteFile(filepath.Join(dir, "tiny.yaml"), []byte(def), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestACPWorkflow_SlashCommandRunsAndReportsProgress(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	server, conn := acpTestServerWithHandler(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"stage output\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	writeTinyWorkflow(t)
	id := acpNewSession(t, server)

	acpPromptText(t, server, id, "/workflow tiny write the haiku --writer test-local")

	out := strings.Join(acpChunks(conn), "\n")
	if !strings.Contains(out, "Starting workflow tiny #0") || !strings.Contains(out, "Workflow tiny #0 complete.") {
		t.Errorf("messages = %q", out)
	}
	mu.Lock()
	joined := strings.Join(bodies, "\n")
	mu.Unlock()
	if !strings.Contains(joined, "write the haiku") {
		t.Error("workflow stage never received the task text")
	}

	var sawRow, sawPlan bool
	for _, n := range conn.sent() {
		w, ok := n.Params.(acp.UpdateSessionNotification)
		if !ok {
			continue
		}
		switch u := w.Update.(type) {
		case acp.ToolCallUpdate:
			if u.Name == "workflow" || u.Status == acp.ToolCallCompleted && u.ToolCallID == "workflow:0" {
				sawRow = true
			}
		case acp.PlanUpdate:
			if strings.HasPrefix(string(u.Plan.PlanID), "workflow-") && len(u.Plan.Entries) > 0 {
				sawPlan = true
			}
		}
	}
	if !sawRow || !sawPlan {
		t.Errorf("tool row seen=%v, workflow plan seen=%v", sawRow, sawPlan)
	}

	acpPromptText(t, server, id, "/workflow status")
	if out := strings.Join(acpChunks(conn), "\n"); !strings.Contains(out, "complete") {
		t.Errorf("status output missing: %q", out)
	}
}

func TestACPWorkflow_ModelStartWorkflowToolRuns(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	server, conn := acpTestServerWithHandler(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) //nolint:errcheck
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			args := `{"name":"tiny","task":"summarise the repo","roles":{"writer":"test-local"}}`
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"start_workflow\",\"arguments\":%q}}]},\"finish_reason\":\"tool_calls\"}]}\n\n", args)
		} else {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"stage output\"},\"finish_reason\":null}]}\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	writeTinyWorkflow(t)
	id := acpNewSession(t, server)

	acpPromptText(t, server, id, "run the tiny workflow on this repo")

	out := strings.Join(acpChunks(conn), "\n")
	if !strings.Contains(out, "Workflow tiny #0 complete.") {
		t.Errorf("start_workflow tool call did not run the workflow; messages = %q", out)
	}
	if strings.Contains(out, "aren't available over ACP") {
		t.Error("stale 'not available' message still present")
	}
}

func TestACPWorkflow_SessionCancelStopsRun(t *testing.T) {
	entered := make(chan struct{}, 1)
	server, conn := acpTestServerWithHandler(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) //nolint:errcheck // the server only notices a client disconnect once the body is consumed
		select {
		case entered <- struct{}{}:
		default:
		}
		<-r.Context().Done() // hold the stage open until milk cancels the request
	})
	writeTinyWorkflow(t)
	id := acpNewSession(t, server)

	done := make(chan struct{})
	go func() {
		defer close(done)
		acpPromptText(t, server, id, "/workflow tiny hang forever --writer test-local")
	}()

	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("workflow stage never reached the model")
	}
	raw, _ := json.Marshal(map[string]any{"sessionId": id})
	server.HandleNotification("session/cancel", raw)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("session/cancel did not stop the workflow")
	}
	if out := strings.Join(acpChunks(conn), "\n"); !strings.Contains(out, "cancelled") {
		t.Errorf("messages = %q, want a cancellation notice", out)
	}
}

func TestACPWorkflow_UsageAndUnknown(t *testing.T) {
	server, conn := acpTestServer(t, "ok")
	id := acpNewSession(t, server)
	acpPromptText(t, server, id, "/workflow")
	acpPromptText(t, server, id, "/workflow nope")
	out := strings.Join(acpChunks(conn), "\n")
	if !strings.Contains(out, "available workflows:") || !strings.Contains(out, `unknown workflow "nope"`) {
		t.Errorf("messages = %q", out)
	}
}

// permStub serves a model that calls `bash echo milk-perm-test` on every odd
// request and answers "done" on every even one, and records request bodies.
func permStub(t *testing.T) (*acpServer, *fakeACPConn, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var bodies []string
	server, conn := acpTestServerWithHandler(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		n := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if n%2 == 1 {
			args := `{"command":"echo milk-perm-test"}`
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_%d\",\"type\":\"function\",\"function\":{\"name\":\"bash\",\"arguments\":%q}}]},\"finish_reason\":\"tool_calls\"}]}\n\n", n, args)
		} else {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":null}]}\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	return server, conn, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), bodies...)
	}
}

func allowSelected() (any, error) {
	return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Outcome: "selected", OptionID: acpPermAllowOptionID}}, nil
}

func TestACPPermission_V2RequestIsSpecShapedAndApprovalRunsTool(t *testing.T) {
	server, conn, bodies := permStub(t)
	var got acp.RequestPermissionRequest
	conn.respond = func(method string, params any) (any, error) {
		if method != acp.MethodRequestPermission {
			t.Errorf("method = %s", method)
		}
		got = params.(acp.RequestPermissionRequest)
		return allowSelected()
	}
	acpRequest(t, server, "initialize", acp.InitializeRequest{ProtocolVersion: 2})
	id := acpNewSession(t, server)

	acpPromptText(t, server, id, "run it")

	if got.Title != "Allow bash?" {
		t.Errorf("title = %q, want a clean 'Allow bash?'", got.Title)
	}
	if !strings.Contains(got.Description, "echo milk-perm-test") {
		t.Errorf("description = %q, want the call summary", got.Description)
	}
	if got.Subject == nil || got.Subject.Type != "tool_call" || got.Subject.ToolCall.ToolCallID != "call_1" {
		t.Errorf("subject = %+v, want the pending bash call_1", got.Subject)
	}
	if got.ToolCall != nil {
		t.Errorf("v2 request carries the v1 toolCall field: %+v", got.ToolCall)
	}
	if b := bodies(); len(b) < 2 || !strings.Contains(b[1], "milk-perm-test") {
		t.Errorf("approved tool did not run; follow-up request:\n%.300s", b[len(b)-1])
	}
}

func TestACPPermission_V1ClientGetsToolCallObject(t *testing.T) {
	server, conn, _ := permStub(t)
	var got acp.RequestPermissionRequest
	conn.respond = func(method string, params any) (any, error) {
		got = params.(acp.RequestPermissionRequest)
		return allowSelected()
	}
	acpRequest(t, server, "initialize", acp.InitializeRequest{ProtocolVersion: 1})
	id := acpNewSession(t, server)

	acpPromptText(t, server, id, "run it")

	if got.ToolCall == nil || got.ToolCall.ToolCallID != "call_1" || got.ToolCall.Title != "Allow bash?" {
		t.Errorf("v1 toolCall = %+v", got.ToolCall)
	}
	if got.Subject != nil || got.Title != "" {
		t.Errorf("v1 request carries v2 fields: title=%q subject=%+v", got.Title, got.Subject)
	}
}

func TestACPPermission_UnanswerableRequestIsExplainedNotSilentlyDenied(t *testing.T) {
	server, conn, bodies := permStub(t)
	conn.respond = func(string, any) (any, error) { return nil, fmt.Errorf("method not found: session/request_permission") }
	id := acpNewSession(t, server)

	acpPromptText(t, server, id, "run it")

	out := strings.Join(acpChunks(conn), "\n")
	if !strings.Contains(out, "Could not ask for permission to run bash") || !strings.Contains(out, "/skip-permissions") {
		t.Errorf("no explanation for the denial: %q", out)
	}
	if b := bodies(); len(b) < 2 || strings.Contains(b[1], "milk-perm-test\\n") {
		t.Error("tool ran although the permission request failed")
	}
}

func TestACPPermission_SkipPermissionsToggle(t *testing.T) {
	server, conn, _ := permStub(t)
	asks := 0
	conn.respond = func(string, any) (any, error) {
		asks++
		return allowSelected()
	}
	id := acpNewSession(t, server)

	acpPromptText(t, server, id, "/skip-permissions on")
	acpPromptText(t, server, id, "run it")
	if asks != 0 {
		t.Fatalf("asked %d times with skip-permissions on", asks)
	}
	acpPromptText(t, server, id, "/skip-permissions off")
	acpPromptText(t, server, id, "run it again")
	if asks != 1 {
		t.Errorf("asked %d times after turning skip-permissions off, want 1", asks)
	}
	if out := strings.Join(acpChunks(conn), "\n"); !strings.Contains(out, "ON") || !strings.Contains(out, "OFF") {
		t.Errorf("command feedback = %q", out)
	}
}

func TestACPPermission_BackgroundJobAsksTheClientAboutItsOwnRow(t *testing.T) {
	server, conn, bodies := permStub(t)
	var mu sync.Mutex
	var asks []acp.RequestPermissionRequest
	conn.respond = func(method string, params any) (any, error) {
		mu.Lock()
		asks = append(asks, params.(acp.RequestPermissionRequest))
		mu.Unlock()
		return allowSelected()
	}
	id := acpNewSession(t, server)

	acpPromptText(t, server, id, "/bg start write the game")
	acpWaitFor(t, "job row completed", acpJobCompleted(conn, "job:job_1"))

	mu.Lock()
	defer mu.Unlock()
	if len(asks) == 0 {
		t.Fatal("background job never asked the client for permission (it was denied silently)")
	}
	a := asks[0]
	if a.Title != "Allow bash?" || a.Subject == nil || a.Subject.ToolCall.ToolCallID != "job:job_1" || !strings.Contains(a.Description, "background agent job_1") {
		t.Errorf("job permission request = %+v, want 'Allow bash?' about job:job_1", a)
	}
	if b := bodies(); len(b) < 2 || !strings.Contains(b[1], "milk-perm-test") {
		t.Error("approved job tool did not run")
	}
}

func TestACPPermission_SkipPermissionsReachesBackgroundJobs(t *testing.T) {
	server, conn, bodies := permStub(t)
	asks := 0
	conn.respond = func(string, any) (any, error) {
		asks++
		return allowSelected()
	}
	id := acpNewSession(t, server)

	acpPromptText(t, server, id, "/skip-permissions on")
	acpPromptText(t, server, id, "/bg start write the game")
	acpWaitFor(t, "job row completed", acpJobCompleted(conn, "job:job_1"))

	if asks != 0 {
		t.Errorf("asked %d times although skip-permissions is on", asks)
	}
	if b := bodies(); len(b) < 2 || !strings.Contains(b[1], "milk-perm-test") {
		t.Error("job tool did not run under skip-permissions")
	}
}

func TestACPPermission_BackgroundJobDenialIsExplainedWhenClientCannotAnswer(t *testing.T) {
	server, conn, _ := permStub(t)
	conn.respond = func(string, any) (any, error) { return nil, fmt.Errorf("method not found: session/request_permission") }
	id := acpNewSession(t, server)

	acpPromptText(t, server, id, "/bg start write the game")
	acpWaitFor(t, "job row completed", acpJobCompleted(conn, "job:job_1"))

	if out := strings.Join(acpChunks(conn), "\n"); !strings.Contains(out, "Could not ask for permission to run bash") {
		t.Errorf("no explanation for the denied job tool: %q", out)
	}
}
