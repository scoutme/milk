package claude

import (
	"strings"
	"testing"
)

func ndjson(lines ...string) string {
	return strings.Join(lines, "\n") + "\n"
}

func TestStream_ExtractsSessionID(t *testing.T) {
	input := ndjson(
		`{"type":"system","subtype":"init","session_id":"sess-abc123"}`,
		`{"type":"result","subtype":"success","is_error":false,"session_id":"sess-abc123","result":"hi"}`,
	)
	var out strings.Builder
	res, err := Stream(strings.NewReader(input), &out, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.SessionID != "sess-abc123" {
		t.Errorf("want session_id sess-abc123, got %q", res.SessionID)
	}
}

func TestStream_WritesTextToOut(t *testing.T) {
	input := ndjson(
		`{"type":"system","session_id":"s1"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Hello, "}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"world"}]}}`,
		`{"type":"result","is_error":false,"session_id":"s1"}`,
	)
	var out strings.Builder
	res, err := Stream(strings.NewReader(input), &out, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Hello, ") {
		t.Errorf("output missing first chunk, got %q", out.String())
	}
	if !strings.Contains(out.String(), "world") {
		t.Errorf("output missing second chunk, got %q", out.String())
	}
	if res.Text != "Hello, world" {
		t.Errorf("want text %q, got %q", "Hello, world", res.Text)
	}
}

func TestStream_EndsWithQuestion(t *testing.T) {
	input := ndjson(
		`{"type":"system","session_id":"s1"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"What do you mean?"}]}}`,
		`{"type":"result","is_error":false,"session_id":"s1"}`,
	)
	var out strings.Builder
	res, _ := Stream(strings.NewReader(input), &out, nil)
	if !res.EndsWithQ {
		t.Error("expected EndsWithQ=true for response ending with '?'")
	}
}

func TestStream_NotEndsWithQuestion(t *testing.T) {
	input := ndjson(
		`{"type":"system","session_id":"s1"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Done."}]}}`,
		`{"type":"result","is_error":false,"session_id":"s1"}`,
	)
	var out strings.Builder
	res, _ := Stream(strings.NewReader(input), &out, nil)
	if res.EndsWithQ {
		t.Error("expected EndsWithQ=false for response not ending with '?'")
	}
}

func TestStream_ErrorResult(t *testing.T) {
	input := ndjson(
		`{"type":"system","session_id":"s1"}`,
		`{"type":"result","is_error":true,"session_id":"s1"}`,
	)
	var out strings.Builder
	res, _ := Stream(strings.NewReader(input), &out, nil)
	if !res.IsError {
		t.Error("expected IsError=true")
	}
}

func TestStream_IgnoresMalformedLines(t *testing.T) {
	input := ndjson(
		`{"type":"system","session_id":"s1"}`,
		`not json at all`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"ok"}]}}`,
		`{"type":"result","is_error":false,"session_id":"s1"}`,
	)
	var out strings.Builder
	res, err := Stream(strings.NewReader(input), &out, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "ok" {
		t.Errorf("want text ok, got %q", res.Text)
	}
}

func TestStream_SkipsNonTextContentBlocks(t *testing.T) {
	input := ndjson(
		`{"type":"system","session_id":"s1"}`,
		`{"type":"assistant","message":{"content":[{"type":"thinking","text":"internal"},{"type":"text","text":"visible"}]}}`,
		`{"type":"result","is_error":false,"session_id":"s1"}`,
	)
	var out strings.Builder
	res, _ := Stream(strings.NewReader(input), &out, nil)
	if strings.Contains(res.Text, "internal") {
		t.Error("thinking block should not appear in text output")
	}
	if res.Text != "visible" {
		t.Errorf("want text visible, got %q", res.Text)
	}
}
