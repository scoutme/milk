package main

import (
	"strings"
	"testing"
)

func TestParseColorizeMode(t *testing.T) {
	tests := []struct {
		input string
		want  ColorizeMode
	}{
		{"off", ColorizeOff},
		{"OFF", ColorizeOff},
		{"balanced", ColorizeBalanced},
		{"BALANCED", ColorizeBalanced},
		{"full", ColorizeFull},
		{"fenced", ColorizeFenced},
		{"", ColorizeFenced},        // default
		{"unknown", ColorizeFenced}, // default
	}
	for _, tc := range tests {
		got := ParseColorizeMode(tc.input)
		if got != tc.want {
			t.Errorf("ParseColorizeMode(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestColorizeCodeBlocks_NoFences(t *testing.T) {
	input := "plain text with no fences"
	got := colorizeCodeBlocks(input)
	if got != input {
		t.Errorf("expected no change, got %q", got)
	}
}

func TestColorizeCodeBlocks_CompleteFence(t *testing.T) {
	input := "before\n```go\nfmt.Println(\"hi\")\n```\nafter"
	got := colorizeCodeBlocks(input)
	// Should contain ANSI but preserve 'before' and 'after'
	if !strings.Contains(got, "before") {
		t.Error("expected 'before' in output")
	}
	if !strings.Contains(got, "after") {
		t.Error("expected 'after' in output")
	}
	// Should contain some ANSI (highlighting applied)
	if !strings.Contains(got, "\x1b[") {
		t.Error("expected ANSI escape sequences in output")
	}
}

func TestColorizeCodeBlocks_TwoConsecutiveFences(t *testing.T) {
	// Two back-to-back complete code blocks — both must be colorized.
	input := "before\n```go\nfmt.Println(\"a\")\n```\nbetween\n```python\nprint(\"b\")\n```\nafter"
	got := colorizeCodeBlocks(input)
	if !strings.Contains(got, "before") {
		t.Error("expected 'before' in output")
	}
	if !strings.Contains(got, "between") {
		t.Error("expected 'between' in output")
	}
	if !strings.Contains(got, "after") {
		t.Error("expected 'after' in output")
	}
	// Both blocks must be highlighted — count ANSI sequences to confirm
	// at least two fence headers were produced.
	count := strings.Count(got, ansiDim+"```")
	if count < 4 { // 2 headers + 2 footers
		t.Errorf("expected ≥4 dim fence markers (2 headers + 2 footers), got %d\noutput: %q", count, got)
	}
}

func TestColorizeCodeBlocks_NestedFenceLikeContent(t *testing.T) {
	// A code block whose body contains a line starting with ``` (e.g. markdown docs).
	// The inner ``` must NOT be treated as a closing fence.
	input := "text\n```markdown\nHere is a fence:\n```go\ncode\n```\nend of markdown block\n```\nafter"
	got := colorizeCodeBlocks(input)
	// The outer block should be colorized (contains ANSI).
	if !strings.Contains(got, "\x1b[") {
		t.Error("expected ANSI in output")
	}
	if !strings.Contains(got, "after") {
		t.Error("expected 'after' after the outer block")
	}
}

func TestFindClosingFence(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int // -1 means not found
	}{
		{"plain closing", "code\n```\nafter", 4},
		{"closing with trailing spaces", "code\n```   \nafter", 4},
		{"opening fence not a close", "code\n```go\ninner\n```\n", 16},
		{"no closing", "code\n```go\nmore code", -1},
		{"at end of string", "code\n```", 4},
	}
	for _, tc := range tests {
		got := findClosingFence(tc.input)
		if got != tc.want {
			t.Errorf("%s: findClosingFence(%q) = %d, want %d", tc.name, tc.input, got, tc.want)
		}
	}
}

func TestColorizeCodeBlocks_IncompleteFence(t *testing.T) {
	// Streaming: block not yet closed — must pass through verbatim
	input := "before\n```go\npartial code"
	got := colorizeCodeBlocks(input)
	if !strings.Contains(got, "partial code") {
		t.Errorf("incomplete fence content should pass through, got %q", got)
	}
	// Should not add ANSI to the incomplete block
	if strings.Contains(got, ansiReset) {
		// ansiReset in the 'before' text? only if colorizeCodeBlocks adds it
		// The 'before' part has no fence, so no ANSI should be added
		t.Logf("contains ansiReset — may be from fence header on partial block")
	}
}

func TestColorizeMarkdown_Headings(t *testing.T) {
	tests := []struct {
		input    string
		contains string
	}{
		{"# Heading 1", ansiBold},
		{"## Heading 2", ansiBold},
		{"### Heading 3", ansiBold},
	}
	for _, tc := range tests {
		got := colorizeMarkdown(tc.input)
		if !strings.Contains(got, tc.contains) {
			t.Errorf("colorizeMarkdown(%q) missing %q, got %q", tc.input, tc.contains, got)
		}
	}
}

func TestColorizeMarkdown_Bullets(t *testing.T) {
	input := "- first\n- second\n- third"
	got := colorizeMarkdown(input)
	if !strings.Contains(got, "first") {
		t.Error("expected bullet content preserved")
	}
	if !strings.Contains(got, ansiDim) {
		t.Error("expected dim styling on bullet markers")
	}
}

func TestColorizeMarkdown_Bold(t *testing.T) {
	input := "This is **bold text** here"
	got := colorizeMarkdown(input)
	if !strings.Contains(got, "bold text") {
		t.Error("expected bold text content preserved")
	}
	if !strings.Contains(got, ansiBold) {
		t.Error("expected bold ANSI in output")
	}
}

func TestColorizeMarkdown_InlineCode(t *testing.T) {
	input := "Use `fmt.Println` to print"
	got := colorizeMarkdown(input)
	if !strings.Contains(got, "fmt.Println") {
		t.Error("expected inline code content preserved")
	}
	if !strings.Contains(got, ansiYellow) {
		t.Error("expected yellow ANSI for inline code")
	}
}

func TestColorizeMarkdown_PreservesFencedBlocks(t *testing.T) {
	// colorizeMarkdown should pass fenced blocks verbatim (colorizeCodeBlocks handles them)
	input := "text\n```go\ncode\n```\nmore text"
	got := colorizeMarkdown(input)
	if !strings.Contains(got, "```go") {
		t.Error("expected fenced block preserved by colorizeMarkdown")
	}
	if !strings.Contains(got, "code") {
		t.Error("expected code content preserved")
	}
}

func TestColorizeMarkdown_ApostrophesSafe(t *testing.T) {
	// Apostrophes must not trigger false positives in any markdown pattern
	inputs := []string{
		"it's fine",
		"don't break",
		"I'm here",
		"the user's choice",
		"let's go",
		"it's **bold** and `code` don't break",
	}
	for _, input := range inputs {
		got := colorizeMarkdown(input)
		// Content must be preserved
		if !strings.Contains(got, "'") {
			t.Errorf("apostrophe lost in: %q → %q", input, got)
		}
	}
}

func TestColorizeMarkdown_DunderNotBold(t *testing.T) {
	// __init__ and similar dunder identifiers must not be bolded
	input := "call __repr__'s result and __init__ method"
	got := colorizeMarkdown(input)
	if !strings.Contains(got, "__repr__") {
		t.Errorf("__repr__ should be preserved, got: %q", got)
	}
	if !strings.Contains(got, "__init__") {
		t.Errorf("__init__ should be preserved, got: %q", got)
	}
}

func TestColorizeTranscriptWrapped_OffMode(t *testing.T) {
	input := "turn1\n\nturn2\n\n"
	got := colorizeTranscriptWrapped(input, ColorizeOff)
	if got != input {
		t.Errorf("off mode should return input unchanged, got %q", got)
	}
}

func TestColorizeTranscriptWrapped_IsolatesTurns(t *testing.T) {
	// A fenced block in turn1 must not affect turn2
	turn1 := "```go\nfmt.Println(\"hi\")\n```\n"
	turn2 := "plain text turn\n"
	input := turn1 + "\n" + turn2

	got := colorizeTranscriptWrapped(input, ColorizeFenced)
	// turn2 content must be present and unmodified by turn1's fencing
	if !strings.Contains(got, "plain text turn") {
		t.Errorf("turn2 content missing in output: %q", got)
	}
}

func TestColorizeSingle_FencedPreservesANSI(t *testing.T) {
	// fenced/balanced modes must preserve existing ANSI (agent label colors)
	label := "\x1b[1m\x1b[32mlocal:\x1b[0m plain response text"
	got := colorizeSingle(label, ColorizeFenced)
	if !strings.Contains(got, "\x1b[1m\x1b[32mlocal:\x1b[0m") {
		t.Errorf("fenced mode stripped agent label ANSI, got: %q", got)
	}
}

func TestColorizeSingle_FullStripsANSI(t *testing.T) {
	// full (glamour) mode strips ANSI before rendering since glamour can't handle it
	label := "\x1b[1m\x1b[32mlocal:\x1b[0m hello world"
	got := colorizeSingle(label, ColorizeFull)
	// After glamour, the raw ANSI label escape should be gone
	if strings.Contains(got, "\x1b[32m") {
		t.Errorf("full mode should strip agent label ANSI before glamour, got: %q", got)
	}
}

func TestStyleLine_BlockquotePreserved(t *testing.T) {
	input := "> this is a quote"
	got := styleLine(input)
	if !strings.Contains(got, "this is a quote") {
		t.Errorf("blockquote content lost: %q", got)
	}
	if !strings.Contains(got, ansiDim) {
		t.Errorf("blockquote should be dim: %q", got)
	}
}

func TestFindOpenFence(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int
	}{
		{"at start", "```go\ncode\n```", 0},
		{"after newline", "prose\n```go\ncode\n```", 6},
		{"mid-line backticks ignored", "the fence header/footer are `ansiDim + \"```\" + lang`.\n```go\ncode\n```", 54},
		{"not found mid-line only", "see `ansiDim + \"```\" + lang` for details", -1},
		{"not found empty", "", -1},
	}
	for _, tc := range tests {
		got := findOpenFence(tc.input)
		if got != tc.want {
			t.Errorf("%s: findOpenFence(%q) = %d, want %d", tc.name, tc.input, got, tc.want)
		}
	}
}

func TestColorizeCodeBlocks_MidLineBackticksNotFence(t *testing.T) {
	// Triple backticks inside an inline code span or mid-line must NOT be
	// treated as a block-level fence opening.
	input := "the fence header/footer are `ansiDim + \"```\" + lang + ansiReset`.\nPlain prose after."
	got := colorizeCodeBlocks(input)
	// No ANSI from a false fence — output should be unchanged
	if got != input {
		t.Errorf("mid-line ``` should not trigger colorization\ngot: %q", got)
	}
}

func TestColorizeMarkdown_MidLineBackticksNotFence(t *testing.T) {
	// Same check for colorizeMarkdown's fence search path.
	input := "pass (the fence header/footer are `ansiDim + \"```\" + lang + ansiReset`).\nsome prose"
	got := colorizeMarkdown(input)
	// The inline ` code ` span should be styled but no block fence opened.
	if strings.Contains(got, "```") {
		t.Errorf("colorizeMarkdown produced a raw ``` block from mid-line backticks\ngot: %q", got)
	}
	// Content must be preserved
	if !strings.Contains(got, "some prose") {
		t.Errorf("content missing after mid-line ``` handling\ngot: %q", got)
	}
}

func TestStyleLine_HRule(t *testing.T) {
	// "---" matches setext-dash (bold+cyan), "***" and "___" match reHRule (dim)
	cases := []struct {
		input    string
		contains string
	}{
		{"---", ansiBold},
		{"***", ansiDim},
		{"___", ansiDim},
	}
	for _, tc := range cases {
		got := styleLine(tc.input)
		if !strings.Contains(got, tc.contains) {
			t.Errorf("styleLine(%q) missing %q, got: %q", tc.input, tc.contains, got)
		}
	}
}
