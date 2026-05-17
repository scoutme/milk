package local

import (
	"strings"
	"testing"
)

// feedAll feeds a string token-by-token (one character at a time) to simulate
// worst-case streaming and returns all flushed text concatenated.
func feedAll(d *StreamDetector, input string) (flushed string, complete bool) {
	var buf strings.Builder
	for _, ch := range input {
		fl, done := d.Feed(string(ch))
		buf.Write(fl)
		if done {
			complete = true
		}
	}
	return buf.String(), complete
}

// feedTokens feeds a slice of tokens (as a server might emit them).
func feedTokens(d *StreamDetector, tokens []string) (flushed string, complete bool) {
	var buf strings.Builder
	for _, tok := range tokens {
		fl, done := d.Feed(tok)
		buf.Write(fl)
		if done {
			complete = true
		}
	}
	return buf.String(), complete
}

// --- Plain text (no tool call) ---

func TestDetector_PlainText_PassedThrough(t *testing.T) {
	d := NewStreamDetector(ToolFormatUnknown)
	flushed, complete := feedAll(d, "Hello, world!")
	if flushed != "Hello, world!" {
		t.Errorf("expected plain text passed through, got %q", flushed)
	}
	if complete {
		t.Error("complete should be false for plain text")
	}
}

func TestDetector_PlainTextWithAngle_NotSuppressed(t *testing.T) {
	// "<" followed by non-delimiter should flush normally
	d := NewStreamDetector(ToolFormatUnknown)
	flushed, complete := feedAll(d, "<not a tool>")
	if complete {
		t.Error("should not complete on non-delimiter angle bracket")
	}
	if !strings.Contains(flushed, "<not a tool>") {
		t.Errorf("non-delimiter prefix should be flushed: got %q", flushed)
	}
}

// --- tool_call_tag format ---

func TestDetector_ToolCallTag_Detected(t *testing.T) {
	d := NewStreamDetector(ToolFormatUnknown)
	input := `<tool_call>{"name":"bash","arguments":{"command":"ls"}}</tool_call>`
	flushed, complete := feedAll(d, input)
	if !complete {
		t.Fatal("expected complete=true for tool_call_tag")
	}
	if flushed != "" {
		t.Errorf("tool markup should not be flushed to out, got %q", flushed)
	}
	if d.Format != ToolFormatToolCallTag {
		t.Errorf("expected format tool_call_tag, got %q", d.Format)
	}
	calls := d.Extract()
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	if calls[0].Function.Name != "bash" {
		t.Errorf("expected name bash, got %q", calls[0].Function.Name)
	}
}

func TestDetector_ToolCallTag_FormatRemembered(t *testing.T) {
	d := NewStreamDetector(ToolFormatUnknown)
	feedAll(d, `<tool_call>{"name":"bash","arguments":{}}</tool_call>`) //nolint:errcheck
	d.Reset()
	if d.Format != ToolFormatToolCallTag {
		t.Error("format should be preserved after Reset()")
	}
}

// --- tools_tag format ---

func TestDetector_ToolsTag_Detected(t *testing.T) {
	d := NewStreamDetector(ToolFormatUnknown)
	input := `<tools>{"name":"list_dir","arguments":{"path":"/tmp"}}</tools>`
	_, complete := feedAll(d, input)
	if !complete {
		t.Fatal("expected complete for tools_tag")
	}
	if d.Format != ToolFormatToolsTag {
		t.Errorf("expected tools_tag, got %q", d.Format)
	}
	calls := d.Extract()
	if len(calls) != 1 || calls[0].Function.Name != "list_dir" {
		t.Errorf("unexpected tool calls: %+v", calls)
	}
}

// --- gemma_special format ---

func TestDetector_GemmaSpecial_Detected(t *testing.T) {
	d := NewStreamDetector(ToolFormatUnknown)
	input := `<|tool_call>call:bash{"command":<|"|>ls<|"|>}<tool_call|>`
	_, complete := feedAll(d, input)
	if !complete {
		t.Fatal("expected complete for gemma_special")
	}
	if d.Format != ToolFormatGemmaSpecial {
		t.Errorf("expected gemma_special, got %q", d.Format)
	}
}

// --- fenced_json format ---

func TestDetector_FencedJSON_Detected(t *testing.T) {
	d := NewStreamDetector(ToolFormatUnknown)
	// Feed as tokens matching how a model streams this
	tokens := []string{"```", "json\n", `{"name":"grep","arguments":{"pattern":"TODO","path":"."}}`, "```"}
	_, complete := feedTokens(d, tokens)
	if !complete {
		t.Fatal("expected complete for fenced_json")
	}
	if d.Format != ToolFormatFencedJSON {
		t.Errorf("expected fenced_json, got %q", d.Format)
	}
}

func TestDetector_FencedJSON_UnclosedFence(t *testing.T) {
	// Model omits closing ``` — stream ends with detector still in_block.
	// The caller handles this via InBlock()+Extract() at end-of-stream.
	d := NewStreamDetector(ToolFormatUnknown)
	tokens := []string{"```json\n", `{"name":"bash","arguments":{"command":"ls"}}`}
	_, _ = feedTokens(d, tokens)
	if !d.InBlock() {
		t.Fatal("expected detector to be InBlock after unclosed fence")
	}
	calls := d.Extract()
	if len(calls) != 1 || calls[0].Function.Name != "bash" {
		t.Errorf("expected bash tool call from unclosed fence, got %+v", calls)
	}
}

// --- Pre-seeded format ---

func TestDetector_PreSeeded_SkipsOtherFormats(t *testing.T) {
	d := NewStreamDetector(ToolFormatToolCallTag)
	// Feed gemma_special opening — should NOT trigger (wrong format)
	flushed, complete := feedAll(d, `<|tool_call>call:bash{}<tool_call|>`)
	if complete {
		t.Error("pre-seeded tool_call_tag should not trigger on gemma_special input")
	}
	// The content should be flushed as plain text since it doesn't match tool_call_tag
	_ = flushed
}

func TestDetector_PreSeeded_TriggersOnCorrectFormat(t *testing.T) {
	d := NewStreamDetector(ToolFormatToolCallTag)
	_, complete := feedAll(d, `<tool_call>{"name":"bash","arguments":{}}</tool_call>`)
	if !complete {
		t.Fatal("pre-seeded detector should trigger on its own format")
	}
}

// --- False-start prefix handling ---

func TestDetector_FalseStartPrefix_Flushed(t *testing.T) {
	// "<tool_result>" is not a registered delimiter — pending bytes must be flushed
	d := NewStreamDetector(ToolFormatUnknown)
	flushed, complete := feedAll(d, "<tool_result>some content</tool_result>")
	if complete {
		t.Error("should not complete on unregistered tag")
	}
	if !strings.Contains(flushed, "<tool_result>") {
		t.Errorf("false-start prefix should be flushed to out, got %q", flushed)
	}
}

func TestDetector_PartialPrefixThenText_Flushed(t *testing.T) {
	// "<tool" then " hello" — partial match breaks, should flush "<tool hello"
	d := NewStreamDetector(ToolFormatUnknown)
	flushed, _ := feedTokens(d, []string{"<tool", " hello"})
	if !strings.Contains(flushed, "<tool") {
		t.Errorf("partial prefix should be flushed when match breaks, got %q", flushed)
	}
}

// --- No characters lost ---

func TestDetector_NoCharactersLost(t *testing.T) {
	// Plain text with embedded angle bracket that doesn't form a delimiter.
	d := NewStreamDetector(ToolFormatUnknown)
	input := "compare <A> and <B> values"
	flushed, _ := feedAll(d, input)
	if flushed != input {
		t.Errorf("all characters must be preserved: got %q, want %q", flushed, input)
	}
}

// --- GuessFormatFromModel ---

func TestGuessFormatFromModel(t *testing.T) {
	cases := []struct {
		model string
		want  ToolFormat
	}{
		{"gemma-4-e4b", ToolFormatGemmaSpecial},
		{"Gemma-2-9B-IT", ToolFormatGemmaSpecial},
		{"Qwen2.5-Coder-7B-Instruct", ToolFormatUnknown},
		{"qwen2.5-7b", ToolFormatUnknown},
		{"mistral-7b-instruct", ToolFormatBracketCalls},
		{"llama-3-8b", ToolFormatBracketCalls},
		{"unknown-model-xyz", ToolFormatUnknown},
	}
	for _, c := range cases {
		got := GuessFormatFromModel(c.model)
		if got != c.want {
			t.Errorf("GuessFormatFromModel(%q) = %q, want %q", c.model, got, c.want)
		}
	}
}

// --- bare_json format ---

func TestDetector_BareJSON_Detected(t *testing.T) {
	d := NewStreamDetector(ToolFormatBareJSON)
	input := `{"name": "read_file", "arguments": {"path": "/tmp/foo"}}` + "\n"
	_, complete := feedAll(d, input)
	if !d.InBlock() && !complete {
		t.Fatal("expected detector to capture bare JSON block")
	}
	calls := d.Extract()
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	if calls[0].Function.Name != "read_file" {
		t.Errorf("expected read_file, got %q", calls[0].Function.Name)
	}
}

func TestDetector_BareJSON_NoLeak(t *testing.T) {
	d := NewStreamDetector(ToolFormatBareJSON)
	input := `{"name": "bash", "arguments": {"command": "ls"}}` + "\n"
	flushed, _ := feedAll(d, input)
	if flushed != "" {
		t.Errorf("bare JSON should not be flushed to out, got %q", flushed)
	}
}

func TestDetector_BareJSON_PlainTextPassThrough(t *testing.T) {
	d := NewStreamDetector(ToolFormatBareJSON)
	input := "Hello, world!"
	flushed, complete := feedAll(d, input)
	if complete {
		t.Error("plain text should not complete")
	}
	if flushed != input {
		t.Errorf("plain text should pass through, got %q", flushed)
	}
}

// --- Reset ---

func TestDetector_Reset_ClearsBuffers(t *testing.T) {
	d := NewStreamDetector(ToolFormatUnknown)
	feedAll(d, `<tool_call>{"name":"bash","arguments":{}}</tool_call>`) //nolint:errcheck
	d.Reset()
	if d.RawBlock() != "" {
		t.Error("Reset should clear blockBuf")
	}
	if d.InBlock() {
		t.Error("Reset should clear in-block state")
	}
	if d.Format != ToolFormatToolCallTag {
		t.Error("Reset should preserve Format")
	}
}
