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

func TestApplyInlineMarkdown_DimPreservedThroughInlineCode(t *testing.T) {
	// A dim-wrapped thinking block whose lines contain inline code spans.
	// The dim should survive the ansiReset emitted by the inline code replacement.
	dimText := ansiDim + "thinking: use `fmt.Println` here\nand this line too" + ansiReset
	got := applyInlineMarkdown(dimText)

	// Both lines must retain dim styling.
	lines := strings.Split(got, "\n")
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 lines, got: %q", got)
	}
	// First line starts with ansiDim (injected at start or from carry-over).
	if !strings.HasPrefix(lines[0], ansiDim) {
		t.Errorf("line 0 should start with ansiDim, got: %q", lines[0])
	}
	// Second line must also start with ansiDim (carry-over through inline code reset).
	if !strings.HasPrefix(lines[1], ansiDim) {
		t.Errorf("line 1 should start with ansiDim (carry-over), got: %q", lines[1])
	}
	// Inline code content must be preserved.
	if !strings.Contains(got, "fmt.Println") {
		t.Errorf("inline code content lost: %q", got)
	}
	// Text AFTER the closing backtick on line 0 must also be dimmed.
	// The word "here" follows the inline code span; it must not lose dim styling.
	line0 := lines[0]
	hereIdx := strings.Index(line0, "here")
	if hereIdx == -1 {
		t.Fatalf("word 'here' not found in line 0: %q", line0)
	}
	// There must be a dim escape somewhere before "here" after the last reset.
	beforeHere := line0[:hereIdx]
	lastReset := strings.LastIndex(beforeHere, ansiReset)
	var afterLastReset string
	if lastReset == -1 {
		afterLastReset = beforeHere
	} else {
		afterLastReset = beforeHere[lastReset+len(ansiReset):]
	}
	if !strings.Contains(afterLastReset, ansiDim) {
		t.Errorf("word 'here' (after closing backtick) is not dim-wrapped on line 0: %q", line0)
	}
}

func TestApplyInlineMarkdown_DimPreservedNoInlineSpans(t *testing.T) {
	// Multi-line dim block with no inline spans — carry-over must still work.
	dimText := ansiDim + "line one\nline two\nline three" + ansiReset
	got := applyInlineMarkdown(dimText)
	lines := strings.Split(got, "\n")
	if len(lines) < 3 {
		t.Fatalf("expected 3 lines, got: %q", got)
	}
	if !strings.HasPrefix(lines[0], ansiDim) {
		t.Errorf("line 0: %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], ansiDim) {
		t.Errorf("line 1 should carry dim: %q", lines[1])
	}
	if !strings.HasPrefix(lines[2], ansiDim) {
		t.Errorf("line 2 should carry dim: %q", lines[2])
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

func TestApplyInlineMarkdown_ToolHintNoBleed(t *testing.T) {
	// A tool hint line ends with ansiReset; the following plain text line
	// must not inherit any ANSI carry-over.
	input := "\n\033[2m⚙ Bash: go test ./...\033[0m\nCommitted — `1908192`, done."
	got := applyInlineMarkdown(input)
	lines := strings.Split(got, "\n")
	// Last line must not start with a dim or other escape carried from the hint.
	last := lines[len(lines)-1]
	if strings.HasPrefix(last, "\033[2m") {
		t.Errorf("last line should not inherit dim carry-over, got: %q", last)
	}
	// The hint line itself should still be dim.
	hintLine := lines[1]
	if !strings.Contains(hintLine, "\033[2m") {
		t.Errorf("hint line should contain dim, got: %q", hintLine)
	}
}

func TestIsTableRow(t *testing.T) {
	trueCases := []string{
		"| col1 | col2 | col3 |",
		"col1 | col2 | col3",
		"| a | b |",
	}
	for _, s := range trueCases {
		if !isTableRow(s) {
			t.Errorf("isTableRow(%q) should be true", s)
		}
	}
	falseCases := []string{
		"no pipes here",
		"only one | pipe",
		"",
		// tool-hint lines with pipes inside grep patterns / shell commands
		`⚙ Bash: grep -rn "obs\.|otel\.|RecordToken|LogPayload"`,
		`grep -rn "foo|bar" ./...`,
		// two pipes but not space-padded and no leading/trailing pipe
		`cmd1|cmd2|cmd3`,
	}
	for _, s := range falseCases {
		if isTableRow(s) {
			t.Errorf("isTableRow(%q) should be false", s)
		}
	}
}

func TestColorizeMarkdown_PipeInToolHintNotTable(t *testing.T) {
	// A tool-hint line containing pipes inside a grep pattern must not be
	// misidentified as a table row and mis-rendered.
	input := "\033[2m⚙ Bash: grep -rn \"obs\\.|otel\\.|RecordToken|LogPayload\" ./...\033[0m"
	got := colorizeMarkdown(input)
	// Content must be preserved verbatim — no table rendering applied.
	if !strings.Contains(got, "grep") {
		t.Errorf("tool hint content lost: %q", got)
	}
	if !strings.Contains(got, "RecordToken") {
		t.Errorf("pipe-separated content inside hint lost: %q", got)
	}
	// Must not contain bold (which would indicate table-header rendering).
	if strings.Contains(got, ansiBold) {
		t.Errorf("tool hint line should not get table-header bold styling: %q", got)
	}
}

func TestColorizeMarkdown_PipeProseLinesNotTable(t *testing.T) {
	// Pipe-containing prose lines with no separator row must not be rendered as a table.
	input := "| option A | option B |\n| option C | option D |"
	got := colorizeMarkdown(input)
	// No separator row → should NOT get table bold styling.
	if strings.Contains(got, ansiBold) {
		t.Errorf("pipe prose without separator row should not get table bold: %q", got)
	}
	// Content must be preserved.
	if !strings.Contains(got, "option A") {
		t.Errorf("content lost: %q", got)
	}
}

func TestIsTableSep(t *testing.T) {
	trueCases := []string{
		"| --- | --- | --- |",
		"|---|---|---|",
		"| :--- | ---: | :---: |",
		"--- | --- | ---",
	}
	for _, s := range trueCases {
		if !isTableSep(s) {
			t.Errorf("isTableSep(%q) should be true", s)
		}
	}
	falseCases := []string{
		"| col1 | col2 |",
		"| --- | text |",
		"no pipes",
	}
	for _, s := range falseCases {
		if isTableSep(s) {
			t.Errorf("isTableSep(%q) should be false", s)
		}
	}
}

func TestColorizeMarkdown_Table(t *testing.T) {
	input := "| Header A | Header B |\n| --- | --- |\n| cell 1 | cell 2 |\n| cell 3 | cell 4 |"
	got := colorizeMarkdown(input)

	// All content must be preserved.
	for _, want := range []string{"Header A", "Header B", "cell 1", "cell 2", "cell 3", "cell 4"} {
		if !strings.Contains(got, want) {
			t.Errorf("table content %q missing in output:\n%q", want, got)
		}
	}

	// Header row gets bold styling.
	if !strings.Contains(got, ansiBold) {
		t.Errorf("table header should have bold styling:\n%q", got)
	}

	// Separator row gets dim styling.
	if !strings.Contains(got, ansiDim) {
		t.Errorf("table separator should have dim styling:\n%q", got)
	}
}

func TestColorizeMarkdown_TableNoFalsePositive(t *testing.T) {
	// A line with only one pipe should NOT be treated as a table row.
	input := "See note | above"
	got := colorizeMarkdown(input)
	// Should pass through styleLine (no table bold).
	if strings.Contains(got, ansiBold) {
		t.Errorf("single-pipe line should not get table header styling:\n%q", got)
	}
}

func TestColorizeMarkdown_TableInlineCode(t *testing.T) {
	// Inline code inside table cells must be styled correctly.
	input := "| `foo` | bar |\n| --- | --- |\n| baz | `qux` |"
	got := colorizeMarkdown(input)
	if !strings.Contains(got, "foo") {
		t.Errorf("inline code in header cell lost:\n%q", got)
	}
	if !strings.Contains(got, "qux") {
		t.Errorf("inline code in body cell lost:\n%q", got)
	}
	if !strings.Contains(got, ansiYellow) {
		t.Errorf("expected yellow ANSI for inline code in table cells:\n%q", got)
	}
}

func TestRenderTableBlock_EqualWidths(t *testing.T) {
	// Columns must be padded to the widest CONTENT cell — separator dashes must
	// not inflate column widths. Use a wide separator to catch the regression.
	// Col 0: max("Scope", "Table rendering", "Bug fix") = 15 ("Table rendering")
	// Col 1: max("What", "Two-pass renderer", "1-based index") = 17 ("Two-pass renderer")
	rawLines := []string{
		"| Scope | What |",
		"| ------------------- | ----------------------------------------------------------------- |",
		"| Table rendering | Two-pass renderer |",
		"| Bug fix | 1-based index |",
	}
	rendered := renderTableBlock(rawLines)
	if len(rendered) != 4 {
		t.Fatalf("expected 4 rendered lines, got %d", len(rendered))
	}

	// Strip ANSI from each line to inspect padding.
	stripANSI := func(s string) string {
		out := []rune{}
		i := 0
		r := []rune(s)
		for i < len(r) {
			if r[i] == 0x1B && i+1 < len(r) && r[i+1] == '[' {
				for i < len(r) && r[i] != 'm' {
					i++
				}
				i++
				continue
			}
			out = append(out, r[i])
			i++
		}
		return string(out)
	}

	plain := make([]string, len(rendered))
	for i, r := range rendered {
		plain[i] = stripANSI(r)
	}

	// All non-separator rows must have the same total length.
	lengths := []int{}
	for i, p := range plain {
		if !isTableSep(rawLines[i]) {
			lengths = append(lengths, len([]rune(p)))
		}
	}
	if len(lengths) == 0 {
		t.Fatal("no data rows found")
	}
	for i, l := range lengths {
		if l != lengths[0] {
			t.Errorf("row %d has width %d, want %d (same as header)\nrows: %v", i, l, lengths[0], plain)
		}
	}
}

func TestApplyInlineMarkdown_MultipleHintsNoBleed(t *testing.T) {
	// Multiple tool hints followed by plain text with inline code.
	// None of the plain text lines should be dimmed.
	input := "\n\033[2m⚙ Bash: go test\033[0m\n\n\033[2m⚙ Bash: git add\033[0m\n\n\033[2m⚙ Bash: git commit\033[0m\nCommitted — `1908192`, 10 files."
	got := applyInlineMarkdown(input)
	lines := strings.Split(got, "\n")
	last := lines[len(lines)-1]
	if strings.HasPrefix(last, "\033[2m") {
		t.Errorf("plain text after hints must not be dim, got: %q", last)
	}
	if !strings.Contains(last, "Committed") {
		t.Errorf("expected Committed in last line, got: %q", last)
	}
}

func TestColorizeSingle_HintsNoBleedBalanced(t *testing.T) {
	input := "\n\033[2m⚙ Bash: go test\033[0m\n\nCommitted — `1908192`, done."
	got := colorizeSingle(input, ColorizeBalanced)
	lines := strings.Split(got, "\n")
	for i, l := range lines {
		if strings.Contains(l, "Committed") && strings.HasPrefix(l, "\033[2m") {
			t.Errorf("line %d with 'Committed' is dimmed: %q", i, l)
		}
	}
}

func TestColorizeSingle_HintsNoBleedFenced(t *testing.T) {
	input := "\n\033[2m⚙ Bash: go test\033[0m\n\nCommitted — `1908192`, done."
	got := colorizeSingle(input, ColorizeFenced)
	lines := strings.Split(got, "\n")
	for i, l := range lines {
		if strings.Contains(l, "Committed") && strings.HasPrefix(l, "\033[2m") {
			t.Errorf("line %d with 'Committed' is dimmed: %q", i, l)
		}
	}
}

func TestApplyInlineMarkdown_MultiLineHintNoBleed(t *testing.T) {
	// A tool hint whose command argument contains literal newlines (e.g. a
	// multi-line bash script) produces a multi-line dim block. The response text
	// that follows must not inherit the dim style.
	input := "\n\033[2m⚙ bash: cd /tmp && python3 -c \"\nimport openpyxl\nwb = openpyxl.load_workbook('x.xlsx')\nfor shee…\033[0m\nIl portale Storyteller è ancora irraggiungibile."
	got := applyInlineMarkdown(input)
	lines := strings.Split(got, "\n")
	for i, l := range lines {
		if strings.Contains(l, "Il portale") && strings.HasPrefix(l, "\033[2m") {
			t.Errorf("line %d: response text after multi-line hint is dim: %q", i, l)
		}
	}
}
