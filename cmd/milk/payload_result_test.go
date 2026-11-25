package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/router"
	"github.com/scoutme/milk/internal/transport/streamjson"
)

// fakePayloadRunner satisfies TurnRunner through the embedded (nil) interface
// and adds the narrow FirstPayload method turnPayloadInfo probes for.
type fakePayloadRunner struct {
	TurnRunner
	payload local.PayloadInfo
	has     bool
}

func (f fakePayloadRunner) FirstPayload() (local.PayloadInfo, bool) { return f.payload, f.has }

// TestEmitOneShotOutcome_Payload pins result.payload on the batch wire
// (issue #462's meter): present and faithful when the caller has a
// composition, entirely absent (no key) when it doesn't.
func TestEmitOneShotOutcome_Payload(t *testing.T) {
	payload := &streamjson.PayloadInfo{
		SystemBytes:   6400,
		ToolsBytes:    15100,
		MessagesBytes: 6700,
		Diet:          "compact",
	}

	var buf bytes.Buffer
	enc := streamjson.NewEncoder(&buf)
	emitOneShotOutcome(enc, "sess_1", router.TargetLocal, "default", nil, "the answer", 42, 1, nil, nil, payload)

	line := lastLine(t, buf.Bytes())
	if !strings.Contains(line, `"payload":`) {
		t.Fatalf("result line missing payload: %s", line)
	}
	for _, want := range []string{`"system_bytes":6400`, `"tools_bytes":15100`, `"messages_bytes":6700`, `"diet":"compact"`} {
		if !strings.Contains(line, want) {
			t.Errorf("result payload missing %s: %s", want, line)
		}
	}

	buf.Reset()
	enc = streamjson.NewEncoder(&buf)
	emitOneShotOutcome(enc, "sess_1", router.TargetLocal, "default", nil, "the answer", 42, 1, nil, nil, nil)
	if line := lastLine(t, buf.Bytes()); strings.Contains(line, `"payload"`) {
		t.Errorf("nil payload must be omitted, got: %s", line)
	}
}

// TestTurnPayloadInfo pins the selection rules: the runner that served the
// turn reports (escalation target → escalation runner), non-local runners
// and empty local runners report nothing.
func TestTurnPayloadInfo(t *testing.T) {
	want := local.PayloadInfo{SystemBytes: 1, ToolsBytes: 2, MessagesBytes: 3, Diet: "lean"}
	reporting := fakePayloadRunner{payload: want, has: true}
	silent := fakePayloadRunner{has: false}
	cli := &cliRunner{}

	got := turnPayloadInfo(router.TargetLocal, reporting, silent)
	if got == nil || *got != (streamjson.PayloadInfo{SystemBytes: 1, ToolsBytes: 2, MessagesBytes: 3, Diet: "lean"}) {
		t.Errorf("primary target: got %+v, want mapped payload", got)
	}

	// Escalation target reads the escalation runner, not the primary's.
	if p := turnPayloadInfo(router.TargetEscalation, reporting, silent); p != nil {
		t.Errorf("escalation target with silent escalation runner: got %+v, want nil", p)
	}
	if p := turnPayloadInfo(router.TargetEscalation, silent, reporting); p == nil {
		t.Errorf("escalation target with reporting escalation runner: got nil, want payload")
	}

	// No FirstPayload method (claude-cli runner) → nil.
	if p := turnPayloadInfo(router.TargetLocal, cli, silent); p != nil {
		t.Errorf("cli runner reported a payload: %+v", p)
	}

	// localRunner with no agent → no payload, no panic.
	if p := turnPayloadInfo(router.TargetLocal, newLocalRunner(nil, "empty"), silent); p != nil {
		t.Errorf("nil-agent local runner reported a payload: %+v", p)
	}
}

// TestTurnPayloadInfo_FromLocalAgent exercises the real localRunner path:
// an agent built from config reports its (unrecorded → absent) composition.
func TestTurnPayloadInfo_FromLocalAgent(t *testing.T) {
	agent := local.NewFromConfig(config.AgentConfig{Name: "p", Provider: "local", URL: "http://127.0.0.1:1"})
	if p := turnPayloadInfo(router.TargetLocal, newLocalRunner(agent, "p"), nil); p != nil {
		t.Errorf("agent that never assembled a request reported a payload: %+v", p)
	}
}

func lastLine(t *testing.T, b []byte) string {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(b), []byte("\n"))
	if len(lines) == 0 || len(lines[len(lines)-1]) == 0 {
		t.Fatalf("no output lines")
	}
	return string(lines[len(lines)-1])
}
