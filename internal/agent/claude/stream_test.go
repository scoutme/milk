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
	res, err := Stream(strings.NewReader(input), &out, nil, StreamOpts{})
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
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Hello, world"}]}}`,
		`{"type":"result","is_error":false,"session_id":"s1"}`,
	)
	var out strings.Builder
	res, err := Stream(strings.NewReader(input), &out, nil, StreamOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Hello, world") {
		t.Errorf("output missing text, got %q", out.String())
	}
	if res.Text != "Hello, world" {
		t.Errorf("want text %q, got %q", "Hello, world", res.Text)
	}
}

func TestStream_SeparatesConsecutiveAssistantEvents(t *testing.T) {
	input := ndjson(
		`{"type":"system","session_id":"s1"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"First turn"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Second turn"}]}}`,
		`{"type":"result","is_error":false,"session_id":"s1"}`,
	)
	var out strings.Builder
	res, err := Stream(strings.NewReader(input), &out, nil, StreamOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "First turn\nSecond turn" {
		t.Errorf("want newline between assistant events, got %q", res.Text)
	}
}

func TestStream_EndsWithQuestion(t *testing.T) {
	input := ndjson(
		`{"type":"system","session_id":"s1"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"What do you mean?"}]}}`,
		`{"type":"result","is_error":false,"session_id":"s1"}`,
	)
	var out strings.Builder
	res, _ := Stream(strings.NewReader(input), &out, nil, StreamOpts{})
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
	res, _ := Stream(strings.NewReader(input), &out, nil, StreamOpts{})
	if res.EndsWithQ {
		t.Error("expected EndsWithQ=false for response not ending with '?'")
	}
}

func TestEndsWithQuestion(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"What do you mean?", true},
		{"What do you mean? ", true}, // trailing space
		{"Done.", false},
		{"foo? bar", false},        // ? mid-sentence, not last line
		{"foo?\nDone.", false},     // ? on earlier line
		{"foo\nWhat now?", true},   // ? on last line
		{"foo\nWhat now?  ", true}, // ? on last line with trailing spaces
		{"", false},
		{"?", true},
		{"Is this ok?\nSure.", false}, // last line has no ?
	}
	for _, tc := range cases {
		got := endsWithQuestion(tc.text)
		if got != tc.want {
			t.Errorf("endsWithQuestion(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

func TestStream_ErrorResult(t *testing.T) {
	input := ndjson(
		`{"type":"system","session_id":"s1"}`,
		`{"type":"result","is_error":true,"session_id":"s1"}`,
	)
	var out strings.Builder
	res, _ := Stream(strings.NewReader(input), &out, nil, StreamOpts{})
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
	res, err := Stream(strings.NewReader(input), &out, nil, StreamOpts{})
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
	res, _ := Stream(strings.NewReader(input), &out, nil, StreamOpts{})
	if strings.Contains(res.Text, "internal") {
		t.Error("thinking block should not appear in text output")
	}
	if res.Text != "visible" {
		t.Errorf("want text visible, got %q", res.Text)
	}
}

// --- stream_event (--include-partial-messages) tests ---

func wrapStreamEvent(inner string) string {
	return `{"type":"stream_event","event":` + inner + `}`
}

func TestStream_TextDeltaStreamsText(t *testing.T) {
	input := ndjson(
		`{"type":"system","session_id":"s1"}`,
		wrapStreamEvent(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"hel"}}`),
		wrapStreamEvent(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"lo"}}`),
		`{"type":"result","is_error":false,"session_id":"s1"}`,
	)
	var out strings.Builder
	res, err := Stream(strings.NewReader(input), &out, nil, StreamOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "hello" {
		t.Errorf("want text hello, got %q", res.Text)
	}
	if !strings.Contains(out.String(), "hello") {
		t.Errorf("output missing streamed text, got %q", out.String())
	}
}

func TestStream_TextDeltaSkipsAssistantEvent(t *testing.T) {
	// When text_delta events are received the final assistant event must be skipped.
	input := ndjson(
		`{"type":"system","session_id":"s1"}`,
		wrapStreamEvent(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"streamed"}}`),
		`{"type":"assistant","message":{"content":[{"type":"text","text":"streamed"}]}}`,
		`{"type":"result","is_error":false,"session_id":"s1"}`,
	)
	var out strings.Builder
	res, _ := Stream(strings.NewReader(input), &out, nil, StreamOpts{})
	if res.Text != "streamed" {
		t.Errorf("want streamed once, got %q", res.Text)
	}
	if strings.Count(out.String(), "streamed") != 1 {
		t.Errorf("text printed more than once: %q", out.String())
	}
}

func TestStream_ThinkingNewlineBeforeText(t *testing.T) {
	// First text_delta after thinking tokens must be preceded by a newline.
	input := ndjson(
		`{"type":"system","session_id":"s1"}`,
		wrapStreamEvent(`{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"thought"}}`),
		wrapStreamEvent(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"answer"}}`),
		`{"type":"result","is_error":false,"session_id":"s1"}`,
	)
	var out strings.Builder
	_, err := Stream(strings.NewReader(input), &out, nil, StreamOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "\n") {
		t.Errorf("want leading newline before first text after thinking, got %q", out.String())
	}
}

func TestStream_OnThinkingCallback(t *testing.T) {
	var got []string
	input := ndjson(
		`{"type":"system","session_id":"s1"}`,
		wrapStreamEvent(`{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"deep thought"}}`),
		`{"type":"result","is_error":false,"session_id":"s1"}`,
	)
	var out strings.Builder
	Stream(strings.NewReader(input), &out, nil, StreamOpts{ //nolint:errcheck
		OnThinking: func(t string) { got = append(got, t) },
	})
	if len(got) != 1 || got[0] != "deep thought" {
		t.Errorf("OnThinking: want [deep thought], got %v", got)
	}
}

func TestStream_OnToolUseCallback(t *testing.T) {
	var got []string
	input := ndjson(
		`{"type":"system","session_id":"s1"}`,
		wrapStreamEvent(`{"type":"content_block_start","content_block":{"type":"tool_use","name":"Bash"}}`),
		`{"type":"result","is_error":false,"session_id":"s1"}`,
	)
	var out strings.Builder
	Stream(strings.NewReader(input), &out, nil, StreamOpts{ //nolint:errcheck
		OnToolUse: func(name string) { got = append(got, name) },
	})
	if len(got) != 1 || got[0] != "Bash" {
		t.Errorf("OnToolUse: want [Bash], got %v", got)
	}
}

func TestStream_MessageStartSeparatesMessages(t *testing.T) {
	input := ndjson(
		`{"type":"system","session_id":"s1"}`,
		wrapStreamEvent(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"first"}}`),
		wrapStreamEvent(`{"type":"message_start"}`),
		wrapStreamEvent(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"second"}}`),
		`{"type":"result","is_error":false,"session_id":"s1"}`,
	)
	var out strings.Builder
	res, _ := Stream(strings.NewReader(input), &out, nil, StreamOpts{})
	if res.Text != "first\nsecond" {
		t.Errorf("want newline between messages, got %q", res.Text)
	}
}

func TestStripPerceptTags(t *testing.T) {
	const nonce = "xtest1"
	open := "<milk:percept:" + nonce + ">"
	close_ := "</milk:percept:" + nonce + ">"
	cases := []struct{ in, want string }{
		{"hello " + open + "fact" + close_ + " world", "hello  world"},
		{"no tags here", "no tags here"},
		{open + "only tag" + close_, ""},
		{"a " + open + "f1" + close_ + " b " + open + "f2" + close_ + " c", "a  b  c"},
		{"unclosed " + open + "dangling", "unclosed"},
		// Legacy tags (no nonce) must NOT be stripped
		{"hello <milk:percept>fact</milk:percept> world", "hello <milk:percept>fact</milk:percept> world"},
		// Stale nonce (different from current turn) must ALSO be stripped — prevents leaking old-context tags.
		{"hello <milk:percept:bx7201>stale fact</milk:percept:bx7201> world", "hello  world"},
	}
	for _, tc := range cases {
		got := strings.TrimSpace(stripPerceptTags(tc.in, nonce))
		want := strings.TrimSpace(tc.want)
		if got != want {
			t.Errorf("stripPerceptTags(%q): want %q, got %q", tc.in, want, got)
		}
	}
}

func newTestPerceptWriter(out *strings.Builder, nonce string, percepts *[]string) *perceptWriter {
	return &perceptWriter{
		w:           out,
		onPercept:   func(s, _ string) { *percepts = append(*percepts, s) },
		recordNonce: nonce,
	}
}

func TestPerceptWriter_SingleWrite(t *testing.T) {
	const nonce = "n0001"
	var out strings.Builder
	var percepts []string
	pw := newTestPerceptWriter(&out, nonce, &percepts)

	open, close_ := perceptTagPair(nonce)
	pw.Write([]byte("before " + open + "my fact" + close_ + " after")) //nolint:errcheck

	if out.String() != "before  after" {
		t.Errorf("output: want %q, got %q", "before  after", out.String())
	}
	if len(percepts) != 1 || percepts[0] != "my fact" {
		t.Errorf("percepts: want [my fact], got %v", percepts)
	}
}

func TestPerceptWriter_LegacyTagIgnored(t *testing.T) {
	// With a nonce set, legacy <milk:percept> tags (no nonce) must pass through unchanged.
	const nonce = "n0002"
	var out strings.Builder
	var percepts []string
	pw := newTestPerceptWriter(&out, nonce, &percepts)

	pw.Write([]byte("before <milk:percept>legacy</milk:percept> after")) //nolint:errcheck
	pw.flush()                                                           //nolint:errcheck

	if len(percepts) != 0 {
		t.Errorf("legacy tag should not be captured, got %v", percepts)
	}
	if !strings.Contains(out.String(), "<milk:percept>legacy</milk:percept>") {
		t.Errorf("legacy tag should pass through unchanged, got %q", out.String())
	}
}

func TestPerceptWriter_StaleNonceStrippedNotRecorded(t *testing.T) {
	// A tag with a different (stale) nonce must be stripped from display
	// but must NOT be passed to onPercept (memory recording).
	const currentNonce = "n0002"
	const staleNonce = "bx7201"
	var out strings.Builder
	var percepts []string
	pw := newTestPerceptWriter(&out, currentNonce, &percepts)

	input := "before <milk:percept:" + staleNonce + ">stale fact</milk:percept:" + staleNonce + "> after"
	pw.Write([]byte(input)) //nolint:errcheck
	pw.flush()              //nolint:errcheck

	if len(percepts) != 0 {
		t.Errorf("stale-nonce tag must not be recorded, got %v", percepts)
	}
	if strings.Contains(out.String(), "milk:percept") {
		t.Errorf("stale-nonce tag must be stripped from output, got %q", out.String())
	}
	if !strings.Contains(out.String(), "before") || !strings.Contains(out.String(), "after") {
		t.Errorf("surrounding text must be preserved, got %q", out.String())
	}
}

func TestPerceptWriter_FlushTrailingBytes(t *testing.T) {
	const nonce = "n0003"
	var out strings.Builder
	var percepts []string
	pw := newTestPerceptWriter(&out, nonce, &percepts)

	// Write text that ends mid-way through the open tag prefix — buffered, not yet flushed.
	open, _ := perceptTagPair(nonce)
	partial := open[:len(open)/2]
	before := "trailing text "
	pw.Write([]byte(before + partial)) //nolint:errcheck
	if out.String() != before {
		t.Errorf("before flush: want %q, got %q", before, out.String())
	}
	pw.flush() //nolint:errcheck
	if out.String() != before+partial {
		t.Errorf("after flush: want %q, got %q", before+partial, out.String())
	}
}

func TestPerceptWriter_FlushUnclosedTag(t *testing.T) {
	const nonce = "n0004"
	var out strings.Builder
	var percepts []string
	pw := newTestPerceptWriter(&out, nonce, &percepts)

	// Write a complete open tag but no close tag — content should be discarded on flush.
	open, _ := perceptTagPair(nonce)
	pw.Write([]byte("prefix " + open + "unclosed content")) //nolint:errcheck
	pw.flush()                                              //nolint:errcheck
	if out.String() != "prefix " {
		t.Errorf("want %q, got %q", "prefix ", out.String())
	}
}

func TestPerceptWriter_SplitAcrossWrites(t *testing.T) {
	const nonce = "n0005"
	var out strings.Builder
	var percepts []string
	pw := newTestPerceptWriter(&out, nonce, &percepts)

	open, close_ := perceptTagPair(nonce)
	// Split the open tag across writes at the midpoint
	mid := len(open) / 2
	part1 := "hello " + open[:mid]
	part2 := open[mid:] + "split fact" + close_[:len(close_)/2]
	part3 := close_[len(close_)/2:] + " end"

	pw.Write([]byte(part1)) //nolint:errcheck
	pw.Write([]byte(part2)) //nolint:errcheck
	pw.Write([]byte(part3)) //nolint:errcheck

	if out.String() != "hello  end" {
		t.Errorf("output: want %q, got %q", "hello  end", out.String())
	}
	if len(percepts) != 1 || percepts[0] != "split fact" {
		t.Errorf("percepts: want [split fact], got %v", percepts)
	}
}

func TestStream_OnPerceptCallback(t *testing.T) {
	const nonce = "testn1"
	var percepts []string
	open, close_ := perceptTagPair(nonce)
	text := "Result. " + open + "user prefers Go" + close_
	input := ndjson(
		`{"type":"system","session_id":"s1"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"`+text+`"}]}}`,
		`{"type":"result","is_error":false,"session_id":"s1"}`,
	)
	var out strings.Builder
	res, err := Stream(strings.NewReader(input), &out, nil, StreamOpts{
		OnPercept:    func(s, _ string) { percepts = append(percepts, s) },
		PerceptNonce: nonce,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(percepts) != 1 || percepts[0] != "user prefers Go" {
		t.Errorf("OnPercept: want [user prefers Go], got %v", percepts)
	}
	if strings.Contains(res.Text, "milk:percept") {
		t.Errorf("percept tag must not appear in res.Text, got %q", res.Text)
	}
	if strings.Contains(out.String(), "milk:percept") {
		t.Errorf("percept tag must not appear in output, got %q", out.String())
	}
}

func TestStream_OnPerceptLegacyTagIgnored(t *testing.T) {
	// When a nonce is set, legacy tags without the nonce must NOT be captured.
	const nonce = "testn2"
	var percepts []string
	// This simulates Claude explaining the format in a code example
	input := ndjson(
		`{"type":"system","session_id":"s1"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Use `+"`<milk:percept>fact</milk:percept>`"+` to emit facts."}]}}`,
		`{"type":"result","is_error":false,"session_id":"s1"}`,
	)
	var out strings.Builder
	_, err := Stream(strings.NewReader(input), &out, nil, StreamOpts{
		OnPercept:    func(s, _ string) { percepts = append(percepts, s) },
		PerceptNonce: nonce,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(percepts) != 0 {
		t.Errorf("legacy tag should not be captured with nonce set, got %v", percepts)
	}
}

func TestStream_OnPerceptConsumerHint(t *testing.T) {
	const nonce = "testn3"
	type captured struct{ body, hint string }
	var got []captured
	open, close_ := perceptTagPair(nonce)
	text := open + "@primary: use primary for simple tasks" + close_ +
		" " + open + "user prefers Go" + close_
	input := ndjson(
		`{"type":"system","session_id":"s1"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"`+text+`"}]}}`,
		`{"type":"result","is_error":false,"session_id":"s1"}`,
	)
	var out strings.Builder
	_, err := Stream(strings.NewReader(input), &out, nil, StreamOpts{
		OnPercept:    func(body, hint string) { got = append(got, captured{body, hint}) },
		PerceptNonce: nonce,
		AgentNames:   []string{"primary", "escalation"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 percepts, got %v", got)
	}
	if got[0].body != "use primary for simple tasks" || got[0].hint != "primary" {
		t.Errorf("first percept: want body=%q hint=%q, got body=%q hint=%q",
			"use primary for simple tasks", "primary", got[0].body, got[0].hint)
	}
	if got[1].body != "user prefers Go" || got[1].hint != "" {
		t.Errorf("second percept: want body=%q hint=%q, got body=%q hint=%q",
			"user prefers Go", "", got[1].body, got[1].hint)
	}
}

func TestConsumerHintFrom(t *testing.T) {
	names := []string{"primary", "escalation"}
	cases := []struct{ in, wantBody, wantHint string }{
		{"@primary: some fact", "some fact", "primary"},
		{"@escalation: other fact", "other fact", "escalation"},
		{"plain fact", "plain fact", ""},
		{"@primary:no space", "@primary:no space", ""},
	}
	for _, c := range cases {
		body, hint := consumerHintFrom(c.in, names)
		if body != c.wantBody || hint != c.wantHint {
			t.Errorf("consumerHintFrom(%q) = (%q, %q), want (%q, %q)",
				c.in, body, hint, c.wantBody, c.wantHint)
		}
	}
}

func TestStream_OnNeedCallback(t *testing.T) {
	const nonce = "testneed1"
	var needs []string
	openTag := "<milk:need:" + nonce + ">"
	closeTag := "</milk:need:" + nonce + ">"
	text := "Working on it. " + openTag + "implement JWT auth" + closeTag
	input := ndjson(
		`{"type":"system","session_id":"s1"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"`+text+`"}]}}`,
		`{"type":"result","is_error":false,"session_id":"s1"}`,
	)
	var out strings.Builder
	res, err := Stream(strings.NewReader(input), &out, nil, StreamOpts{
		OnNeed:    func(s string) { needs = append(needs, s) },
		NeedNonce: nonce,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(needs) != 1 || needs[0] != "implement JWT auth" {
		t.Errorf("OnNeed: want [implement JWT auth], got %v", needs)
	}
	if strings.Contains(res.Text, "milk:need") {
		t.Errorf("need tag must not appear in res.Text, got %q", res.Text)
	}
	if strings.Contains(out.String(), "milk:need") {
		t.Errorf("need tag must not appear in output, got %q", out.String())
	}
}

func TestStream_OnNeedWrongNonceIgnored(t *testing.T) {
	const nonce = "testneed2"
	var needs []string
	// Tag uses a different nonce
	text := "<milk:need:othernonce>some goal</milk:need:othernonce>"
	input := ndjson(
		`{"type":"system","session_id":"s1"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"`+text+`"}]}}`,
		`{"type":"result","is_error":false,"session_id":"s1"}`,
	)
	var out strings.Builder
	_, err := Stream(strings.NewReader(input), &out, nil, StreamOpts{
		OnNeed:    func(s string) { needs = append(needs, s) },
		NeedNonce: nonce,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(needs) != 0 {
		t.Errorf("wrong-nonce need tag should not be captured, got %v", needs)
	}
}

func TestStripPerceptTags_CodeSpanPreserved(t *testing.T) {
	const nonce = "xtest2"
	open := "<milk:percept:" + nonce + ">"
	close_ := "</milk:percept:" + nonce + ">"
	cases := []struct{ in, want string }{
		// Tag inside backtick code span must pass through unchanged.
		{"`" + open + "fact" + close_ + "`", "`" + open + "fact" + close_ + "`"},
		// Tag outside code span is still stripped.
		{"before `code` " + open + "fact" + close_ + " after", "before `code`  after"},
		// Tag before and after code span are both stripped; span content preserved.
		{open + "f1" + close_ + " `code` " + open + "f2" + close_, "`code`"},
		// Triple-backtick fence.
		{"```\n" + open + "fact" + close_ + "\n```", "```\n" + open + "fact" + close_ + "\n```"},
	}
	for _, tc := range cases {
		got := strings.TrimSpace(stripPerceptTags(tc.in, nonce))
		want := strings.TrimSpace(tc.want)
		if got != want {
			t.Errorf("stripPerceptTags code-span(%q): want %q, got %q", tc.in, want, got)
		}
	}
}

func TestStripTagsByPrefix_CodeSpanPreserved(t *testing.T) {
	const nonce = "xtest3"
	openPrefix := needOpenPrefix
	open := openPrefix + nonce + ">"
	close_ := "</" + openPrefix[1:] + nonce + ">"
	cases := []struct{ in, want string }{
		{"`" + open + "need" + close_ + "`", "`" + open + "need" + close_ + "`"},
		{"before `code` " + open + "need" + close_ + " after", "before `code`  after"},
	}
	for _, tc := range cases {
		got := strings.TrimSpace(stripTagsByPrefix(tc.in, openPrefix))
		want := strings.TrimSpace(tc.want)
		if got != want {
			t.Errorf("stripTagsByPrefix code-span(%q): want %q, got %q", tc.in, want, got)
		}
	}
}

func TestPerceptWriter_CodeSpanPreserved(t *testing.T) {
	const nonce = "n9901"
	open, close_ := perceptTagPair(nonce)
	var out strings.Builder
	var percepts []string
	pw := newTestPerceptWriter(&out, nonce, &percepts)

	// Tag wrapped in backticks must pass through unchanged and NOT be recorded.
	input := "before `" + open + "fact" + close_ + "` after"
	pw.Write([]byte(input)) //nolint:errcheck
	pw.flush()              //nolint:errcheck

	if len(percepts) != 0 {
		t.Errorf("tag inside code span should not be captured, got %v", percepts)
	}
	if !strings.Contains(out.String(), open) {
		t.Errorf("tag inside code span should pass through, got %q", out.String())
	}
}

func TestTagWriter_CodeSpanPreserved(t *testing.T) {
	const nonce = "n9902"
	openPrefix := needOpenPrefix
	open := openPrefix + nonce + ">"
	closeTag := "</" + openPrefix[1:] + nonce + ">"
	var out strings.Builder
	var needs []string
	tw := &tagWriter{
		w:           &out,
		openPrefix:  openPrefix,
		onTag:       func(s string) { needs = append(needs, s) },
		recordNonce: nonce,
	}

	input := "before `" + open + "need" + closeTag + "` after"
	tw.Write([]byte(input)) //nolint:errcheck
	tw.flush()              //nolint:errcheck

	if len(needs) != 0 {
		t.Errorf("tag inside code span should not be captured, got %v", needs)
	}
	if !strings.Contains(out.String(), open) {
		t.Errorf("tag inside code span should pass through, got %q", out.String())
	}
}

func TestStream_PermissionDenialsInResult(t *testing.T) {
	input := ndjson(
		`{"type":"system","session_id":"s1"}`,
		`{"type":"result","is_error":false,"session_id":"s1","permission_denials":[{"tool_name":"Write","tool_use_id":"u1","tool_input":{"file_path":"/tmp/x"}}]}`,
	)
	var out strings.Builder
	res, _ := Stream(strings.NewReader(input), &out, nil, StreamOpts{})
	if len(res.PermissionDenials) != 1 || res.PermissionDenials[0].ToolName != "Write" {
		t.Errorf("want Write denial, got %v", res.PermissionDenials)
	}
}

// --- StreamClosedDenials (pre-flight directory-trust "Stream closed") tests ---

// toolUseEvents returns the stream_event lines needed to register a tool_use block
// in the per-turn registry (content_block_start + content_block_stop; no input delta needed).
func toolUseEvents(id, name string) []string {
	return []string{
		wrapStreamEvent(`{"type":"content_block_start","content_block":{"type":"tool_use","id":"` + id + `","name":"` + name + `"}}`),
		wrapStreamEvent(`{"type":"content_block_stop"}`),
	}
}

func TestStream_StreamClosedDenials_StringContent(t *testing.T) {
	// tool_result content is a plain string "Stream closed"
	lines := []string{`{"type":"system","session_id":"s1"}`}
	lines = append(lines, toolUseEvents("toolu_01", "Bash")...)
	lines = append(lines,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_01","content":"Stream closed","is_error":true}]}}`,
		`{"type":"result","is_error":false,"session_id":"s1"}`,
	)
	var out strings.Builder
	res, err := Stream(strings.NewReader(ndjson(lines...)), &out, nil, StreamOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.StreamClosedDenials) != 1 {
		t.Fatalf("want 1 StreamClosedDenial, got %d: %v", len(res.StreamClosedDenials), res.StreamClosedDenials)
	}
	d := res.StreamClosedDenials[0]
	if d.ToolUseID != "toolu_01" {
		t.Errorf("ToolUseID: want toolu_01, got %q", d.ToolUseID)
	}
	if d.Name != "Bash" {
		t.Errorf("Name: want Bash, got %q", d.Name)
	}
}

func TestStream_StreamClosedDenials_BlockContent(t *testing.T) {
	// tool_result content is an array of content blocks — the other common encoding
	lines := []string{`{"type":"system","session_id":"s1"}`}
	lines = append(lines, toolUseEvents("toolu_02", "Read")...)
	lines = append(lines,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_02","content":[{"type":"text","text":"Stream closed"}],"is_error":true}]}}`,
		`{"type":"result","is_error":false,"session_id":"s1"}`,
	)
	var out strings.Builder
	res, err := Stream(strings.NewReader(ndjson(lines...)), &out, nil, StreamOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.StreamClosedDenials) != 1 {
		t.Fatalf("want 1 StreamClosedDenial, got %d: %v", len(res.StreamClosedDenials), res.StreamClosedDenials)
	}
	if res.StreamClosedDenials[0].Name != "Read" {
		t.Errorf("Name: want Read, got %q", res.StreamClosedDenials[0].Name)
	}
}

func TestStream_StreamClosedDenials_UnknownToolUseID(t *testing.T) {
	// tool_use_id has no matching entry in the registry (no prior content_block_start) —
	// still recorded with just the ID so callers can surface it.
	input := ndjson(
		`{"type":"system","session_id":"s1"}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_unknown","content":"Stream closed","is_error":true}]}}`,
		`{"type":"result","is_error":false,"session_id":"s1"}`,
	)
	var out strings.Builder
	res, err := Stream(strings.NewReader(input), &out, nil, StreamOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.StreamClosedDenials) != 1 {
		t.Fatalf("want 1 StreamClosedDenial, got %d", len(res.StreamClosedDenials))
	}
	d := res.StreamClosedDenials[0]
	if d.ToolUseID != "toolu_unknown" {
		t.Errorf("ToolUseID: want toolu_unknown, got %q", d.ToolUseID)
	}
	if d.Name != "" {
		t.Errorf("Name: want empty for unknown id, got %q", d.Name)
	}
}

func TestStream_StreamClosedDenials_NonMatchingContentIgnored(t *testing.T) {
	// tool_result whose content does not contain "Stream closed" must not be captured.
	lines := []string{`{"type":"system","session_id":"s1"}`}
	lines = append(lines, toolUseEvents("toolu_03", "Bash")...)
	lines = append(lines,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_03","content":"ok","is_error":false}]}}`,
		`{"type":"result","is_error":false,"session_id":"s1"}`,
	)
	var out strings.Builder
	res, err := Stream(strings.NewReader(ndjson(lines...)), &out, nil, StreamOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.StreamClosedDenials) != 0 {
		t.Errorf("want no StreamClosedDenials for non-matching content, got %v", res.StreamClosedDenials)
	}
}

func TestStream_StreamClosedDenials_MultipleBlocks(t *testing.T) {
	// Two tools both hit "Stream closed" — both must be captured with correct names.
	lines := []string{`{"type":"system","session_id":"s1"}`}
	lines = append(lines, toolUseEvents("toolu_a", "Write")...)
	lines = append(lines, toolUseEvents("toolu_b", "Edit")...)
	lines = append(lines,
		`{"type":"user","message":{"content":[`+
			`{"type":"tool_result","tool_use_id":"toolu_a","content":"Stream closed","is_error":true},`+
			`{"type":"tool_result","tool_use_id":"toolu_b","content":"Stream closed","is_error":true}`+
			`]}}`,
		`{"type":"result","is_error":false,"session_id":"s1"}`,
	)
	var out strings.Builder
	res, err := Stream(strings.NewReader(ndjson(lines...)), &out, nil, StreamOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.StreamClosedDenials) != 2 {
		t.Fatalf("want 2 StreamClosedDenials, got %d: %v", len(res.StreamClosedDenials), res.StreamClosedDenials)
	}
	names := map[string]bool{res.StreamClosedDenials[0].Name: true, res.StreamClosedDenials[1].Name: true}
	if !names["Write"] || !names["Edit"] {
		t.Errorf("want Write+Edit, got %v", res.StreamClosedDenials)
	}
}

func TestStream_ToolRegistryPopulatedWithoutOnToolUseReadyCallback(t *testing.T) {
	// Registry must be populated even when OnToolUseReady is nil, so that
	// type:"user" Stream-closed correlation works unconditionally.
	lines := []string{`{"type":"system","session_id":"s1"}`}
	lines = append(lines, toolUseEvents("toolu_04", "Bash")...)
	lines = append(lines,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_04","content":"Stream closed","is_error":true}]}}`,
		`{"type":"result","is_error":false,"session_id":"s1"}`,
	)
	var out strings.Builder
	res, err := Stream(strings.NewReader(ndjson(lines...)), &out, nil, StreamOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.StreamClosedDenials) != 1 || res.StreamClosedDenials[0].Name != "Bash" {
		t.Errorf("want Bash denial even without OnToolUseReady, got %v", res.StreamClosedDenials)
	}
}
