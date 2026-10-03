package main

// Integration test for the --output-format stream-json wiring at the
// dispatch layer: a real onResponse closure (the one main.go's run()
// constructs) feeds buildAssistantEvent/buildResultEvent into a
// streamjson.Encoder, and the resulting lines are decoded back via
// streamjson.NewDecoder — the same path eval/adapter_milk.go exercises
// against the real binary, without needing a real agent or subprocess.

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/router"
	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/transport/streamjson"
)

func TestOutputFormatStreamJSON_DispatchWiring(t *testing.T) {
	withTempHome(t)

	runner := &fakeTokenRunner{
		name: "test-primary",
		res: TurnResult{
			Text:         "the final answer",
			InputTokens:  100,
			OutputTokens: 20,
		},
	}
	sess := &session.Session{ID: "test-session"}
	cfg := config.Config{Agents: []config.AgentConfig{{Name: "test-primary", Provider: "local"}}}

	var stdout bytes.Buffer
	enc := streamjson.NewEncoder(&stdout)
	emit(enc, buildInitEvent(sess.ID, "/repo", cfg, router.Decision{Target: router.TargetLocal, Reason: "default", Conclusive: true}, router.TargetLocal, nil))

	var lastText string
	onResponse := func(text string) {
		lastText = text
		emit(enc, buildAssistantEvent(sess.ID, text))
	}

	before := sess.TokensSnapshot()
	if err := runPrimaryWithSession(context.Background(), cfg, sess, runner, nil, nil,
		"hi", "hi", io.Discard, nil, onResponse, nil, nil); err != nil {
		t.Fatalf("runPrimaryWithSession returned error: %v", err)
	}
	emit(enc, buildResultEvent(sess.ID, router.TargetLocal, nil, lastText, 42, before, sess.TokensSnapshot()))

	// Decode the whole stream back and assert the exact shape eval/adapter_milk.go relies on.
	dec := streamjson.NewDecoder(&stdout)

	initEv, err := dec.Next()
	if err != nil {
		t.Fatalf("decoding init event: %v", err)
	}
	if initEv.Type != streamjson.TypeSystem || initEv.Subtype != streamjson.SubtypeInit || initEv.Seq != 0 {
		t.Errorf("first event = %+v, want system/init at seq 0", initEv)
	}

	assistantEv, err := dec.Next()
	if err != nil {
		t.Fatalf("decoding assistant event: %v", err)
	}
	if assistantEv.Type != streamjson.TypeAssistant {
		t.Fatalf("second event type = %q, want assistant", assistantEv.Type)
	}
	msg, err := assistantEv.AsMessage()
	if err != nil || msg == nil || len(msg.Content) != 1 || msg.Content[0].Text != "the final answer" {
		t.Errorf("assistant message = %+v (err %v), want one text block %q", msg, err, "the final answer")
	}

	resultEv, err := dec.Next()
	if err != nil {
		t.Fatalf("decoding result event: %v", err)
	}
	if resultEv.Type != streamjson.TypeResult || resultEv.Subtype != streamjson.ResultSuccess {
		t.Errorf("result event = %+v, want result/success", resultEv)
	}
	if resultEv.IsError == nil || *resultEv.IsError {
		t.Errorf("IsError = %v, want false", resultEv.IsError)
	}
	if resultEv.Result != "the final answer" {
		t.Errorf("Result = %q, want %q", resultEv.Result, "the final answer")
	}
	if resultEv.Usage == nil || resultEv.Usage.InputTokens != 100 || resultEv.Usage.OutputTokens != 20 {
		t.Errorf("Usage = %+v, want InputTokens=100 OutputTokens=20", resultEv.Usage)
	}

	if _, err := dec.Next(); err != io.EOF {
		t.Errorf("expected exactly 3 lines, got an extra one (err=%v)", err)
	}
}
