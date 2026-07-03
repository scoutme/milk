package main

import (
	"bytes"
	"regexp"
	"strings"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/x/ansi"
	rw "github.com/mattn/go-runewidth"
)

// highlightStyle is the chroma theme used for code blocks.
const highlightStyle = "monokai"

// ColorizeMode controls how much of the transcript is syntax-highlighted.
type ColorizeMode string

const (
	ColorizeOff      ColorizeMode = "off"
	ColorizeFenced   ColorizeMode = "fenced"
	ColorizeBalanced ColorizeMode = "balanced"
	ColorizeFull     ColorizeMode = "full"
)

// ParseColorizeMode maps a config string to a ColorizeMode, defaulting to fenced.
func ParseColorizeMode(s string) ColorizeMode {
	switch strings.ToLower(s) {
	case "off":
		return ColorizeOff
	case "balanced":
		return ColorizeBalanced
	case "full":
		return ColorizeFull
	default:
		return ColorizeFenced
	}
}

// colorizeTranscriptWrapped is the efficient entry point used by wrappedTranscript.
// text is the raw (unwrapped) transcript; wrapping happens after colorization so
// that multi-line constructs like tables are detected on intact rows. It splits on
// turn boundaries (blank lines outside fenced blocks) so each turn is colorized
// independently, preventing cross-turn greedy fence matching.
func colorizeTranscriptWrapped(text string, mode ColorizeMode) string {
	if mode == ColorizeOff {
		return text
	}
	segments := splitTurns(text)
	for i, seg := range segments {
		segments[i] = colorizeSingle(seg, mode)
	}
	return strings.Join(segments, "")
}

// splitTurns splits text on "\n\n" boundaries that are NOT inside a fenced code
// block. This prevents splitting a code block whose body contains blank lines,
// while still isolating turns from each other for independent colorization.
func splitTurns(text string) []string {
	var segments []string
	inFence := false
	start := 0
	i := 0
	for i < len(text) {
		if !inFence && i+3 <= len(text) && text[i:i+3] == "```" && (i == 0 || text[i-1] == '\n') {
			inFence = true
			i += 3
			// skip the language tag to the end of the opening-fence line
			for i < len(text) && text[i] != '\n' {
				i++
			}
			continue
		}
		if inFence && i+4 <= len(text) && text[i:i+4] == "\n```" {
			after := text[i+4:]
			if isClosingFence(after) {
				inFence = false
				i += 4
				continue
			}
		}
		if !inFence && i+2 <= len(text) && text[i:i+2] == "\n\n" {
			segments = append(segments, text[start:i+2])
			i += 2
			start = i
			continue
		}
		i++
	}
	if start < len(text) {
		segments = append(segments, text[start:])
	}
	return segments
}

// colorizeSingle applies colorization to one transcript segment (turn).
// For full (glamour) mode the segment is ANSI-stripped first since glamour
// treats escape bytes as literal characters, except for dim-wrapped blocks
// (thinking output) which are preserved and handled separately.
// Other modes preserve existing ANSI (e.g. agent label colors, dim thinking)
// and only add new highlights.
func colorizeSingle(seg string, mode ColorizeMode) string {
	switch mode {
	case ColorizeFenced:
		return colorizeCodeBlocks(seg)
	case ColorizeBalanced:
		return colorizeCodeBlocks(colorizeMarkdown(seg))
	case ColorizeFull:
		return colorizeFull(seg)
	default:
		return colorizeCodeBlocks(seg)
	}
}

// colorizeFull applies glamour to non-dim portions and colorizeCodeBlocks to
// dim-wrapped portions (thinking output). This preserves the dim styling of
// thinking blocks which ansi.Strip would otherwise erase.
func colorizeFull(seg string) string {
	if !strings.Contains(seg, ansiDim) {
		return colorizeGlamour(ansi.Strip(seg))
	}
	var sb strings.Builder
	remaining := seg
	for {
		dimStart := strings.Index(remaining, ansiDim)
		if dimStart == -1 {
			sb.WriteString(colorizeGlamour(ansi.Strip(remaining)))
			break
		}
		if dimStart > 0 {
			sb.WriteString(colorizeGlamour(ansi.Strip(remaining[:dimStart])))
		}
		remaining = remaining[dimStart:] // starts with ansiDim
		resetIdx := strings.Index(remaining, ansiReset)
		if resetIdx == -1 {
			sb.WriteString(remaining)
			break
		}
		dimBlock := remaining[:resetIdx+len(ansiReset)]
		sb.WriteString(colorizeCodeBlocks(dimBlock))
		remaining = remaining[resetIdx+len(ansiReset):]
	}
	return sb.String()
}

// findOpenFence returns the byte index of the next ``` that starts at a line
// boundary (position 0 or immediately after \n). Returns -1 if not found.
// This prevents treating ``` embedded mid-line (e.g. inside an inline code
// span like `ansiDim + "```" + lang`) as a block-level code fence.
func findOpenFence(text string) int {
	if strings.HasPrefix(text, "```") {
		return 0
	}
	idx := strings.Index(text, "\n```")
	if idx == -1 {
		return -1
	}
	return idx + 1 // position of the first backtick
}

// findClosingFence returns the byte index of the first closing code fence in s.
// A closing fence is \n``` followed by only optional whitespace then \n or end
// of string — never a language tag. This prevents treating an opening fence
// (e.g. \n```go) inside a block's body as the closing fence.
// Returns -1 if no closing fence is found.
func findClosingFence(s string) int {
	start := 0
	for {
		idx := strings.Index(s[start:], "\n```")
		if idx == -1 {
			return -1
		}
		idx += start
		afterFence := s[idx+4:] // bytes after the three backticks
		if isClosingFence(afterFence) {
			return idx
		}
		start = idx + 1
	}
}

// isClosingFence reports whether the bytes immediately after a ``` are only
// whitespace (spaces/tabs) followed by a newline or end of string.
func isClosingFence(after string) bool {
	for _, ch := range after {
		switch ch {
		case '\n':
			return true
		case ' ', '\t':
			// trailing whitespace is allowed
		default:
			return false
		}
	}
	return true // ``` at end of string
}

// colorizeCodeBlocks finds complete fenced code blocks in text and returns the
// text with syntax-highlighted replacements. Incomplete blocks (no closing ```)
// are left as-is so streaming output is unaffected.
func colorizeCodeBlocks(text string) string {
	if !strings.Contains(text, "```") {
		return text
	}

	var sb strings.Builder
	remaining := text
	for {
		openStart := findOpenFence(remaining)
		if openStart == -1 {
			sb.WriteString(remaining)
			break
		}
		// Write everything before the opening fence.
		sb.WriteString(remaining[:openStart])
		remaining = remaining[openStart+3:]

		// Extract the optional language tag (up to the first newline).
		lang := ""
		if nl := strings.IndexByte(remaining, '\n'); nl != -1 {
			lang = strings.TrimSpace(remaining[:nl])
			remaining = remaining[nl+1:]
		} else {
			// Opening fence with no newline — treat as incomplete.
			sb.WriteString("```")
			sb.WriteString(remaining)
			remaining = ""
			break
		}

		// Look for the closing fence (must not be followed by a language tag).
		closeIdx := findClosingFence(remaining)
		if closeIdx == -1 {
			// Block not closed yet — emit verbatim and stop.
			sb.WriteString("```")
			sb.WriteString(lang)
			sb.WriteByte('\n')
			sb.WriteString(remaining)
			remaining = ""
			break
		}

		code := remaining[:closeIdx+1]     // include trailing newline
		remaining = remaining[closeIdx+4:] // skip past "\n```"

		// Consume an optional newline right after the closing fence.
		if len(remaining) > 0 && remaining[0] == '\n' {
			remaining = remaining[1:]
		}

		sb.WriteString(highlightBlock(lang, code))
		sb.WriteByte('\n')
	}
	return sb.String()
}

// highlightBlock applies chroma ANSI highlighting to a single code block.
// Falls back to a dim-colored fenced block on any error.
func highlightBlock(lang, code string) string {
	lexer := lexers.Get(lang)
	if lexer == nil {
		lexer = lexers.Analyse(code)
	}
	if lexer == nil {
		lexer = lexers.Fallback
	}
	lexer = chroma.Coalesce(lexer)

	style := styles.Get(highlightStyle)
	if style == nil {
		style = styles.Fallback
	}

	formatter := formatters.Get("terminal256")
	if formatter == nil {
		formatter = formatters.Fallback
	}

	iterator, err := lexer.Tokenise(nil, code)
	if err != nil {
		return fencedFallback(lang, code)
	}

	var buf bytes.Buffer
	if err := formatter.Format(&buf, style, iterator); err != nil {
		return fencedFallback(lang, code)
	}

	highlighted := buf.String()
	// Strip trailing newline added by chroma so callers control spacing.
	highlighted = strings.TrimRight(highlighted, "\n")

	// Wrap in a dim-bordered fence header/footer for context.
	header := ansiDim + "```" + lang + ansiReset
	footer := ansiDim + "```" + ansiReset
	return header + "\n" + highlighted + "\n" + footer
}

func fencedFallback(lang, code string) string {
	return ansiDim + "```" + lang + ansiReset + "\n" +
		strings.TrimRight(code, "\n") + "\n" +
		ansiDim + "```" + ansiReset
}

// --- balanced mode: inline Markdown transforms ---

// reInlineCode matches `inline code` spans (not crossing newlines).
var reInlineCode = regexp.MustCompile("`([^`\n]+)`")

// reStrongEM matches ***text*** (bold+italic).
var reStrongEM = regexp.MustCompile(`\*\*\*([^*\n]+)\*\*\*`)

// reStrong matches **text** only (double asterisk). __text__ is excluded to
// avoid false positives on snake_case identifiers with underscores.
var reStrong = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)

// reSetextEquals / reSetextDash detect setext-style heading underlines.
var reSetextEquals = regexp.MustCompile(`^[=]{3,}\s*$`)
var reSetextDash = regexp.MustCompile(`^[-]{3,}\s*$`)

// reOrderedItem matches the numeric prefix of an ordered list item.
var reOrderedItem = regexp.MustCompile(`^(\s*)(\d+\.) `)

// reHRule detects horizontal rules.
var reHRule = regexp.MustCompile(`^(\*\*\*|---|___)\s*$`)

// reTableSepCell matches a GFM table separator cell: optional leading colon,
// one or more dashes, optional trailing colon (e.g. "---", ":---:", "---:").
var reTableSepCell = regexp.MustCompile(`^:?-+:?$`)

// colorizeMarkdown applies light ANSI styling to inline Markdown constructs
// outside fenced code blocks. Fenced blocks are passed through untouched so
// they can be handled by colorizeCodeBlocks afterwards.
func colorizeMarkdown(text string) string {
	var sb strings.Builder
	remaining := text

	for {
		fenceIdx := findOpenFence(remaining)
		if fenceIdx == -1 {
			// No more fences — style the rest.
			sb.WriteString(applyInlineMarkdown(remaining))
			break
		}
		// Style the text before the fence, then preserve the fenced block verbatim.
		sb.WriteString(applyInlineMarkdown(remaining[:fenceIdx]))
		remaining = remaining[fenceIdx:]

		// Find the end of this fenced block (closing fence on its own line,
		// not followed by a language tag).
		afterOpen := remaining[3:]
		closeIdx := findClosingFence(afterOpen)
		if closeIdx == -1 {
			// Incomplete block — emit verbatim and stop.
			sb.WriteString(remaining)
			remaining = ""
			break
		}
		// Include the opening "```lang\n", block body, and closing "\n```".
		blockEnd := 3 + closeIdx + 4 // len("```") + closeIdx + len("\n```")
		sb.WriteString(remaining[:blockEnd])
		remaining = remaining[blockEnd:]
		if len(remaining) > 0 && remaining[0] == '\n' {
			sb.WriteByte('\n')
			remaining = remaining[1:]
		}
	}
	return sb.String()
}

// applyInlineMarkdown applies ANSI escapes for headings, bullets, bold, italic,
// and inline code on a plain-text (no fenced blocks) segment.
// It carries unclosed ANSI state across newlines so that dim-wrapped blocks
// (e.g. thinking output written as dim(text)) are preserved on every line.
// When an outer ANSI context is active (e.g. dim from a thinking block), any
// ansiReset produced by an inline span (e.g. inline code) is followed by a
// re-injection of the outer context so the dim is not lost within the line or
// across subsequent lines.
func applyInlineMarkdown(text string) string {
	lines := strings.Split(text, "\n")
	activeANSI := ""
	i := 0
	for i < len(lines) {
		originalLine := lines[i]
		strippedOrig := strings.TrimSpace(originalLine)

		// Table block: collect all contiguous pipe-containing lines, then only
		// render as a table if a GFM separator row (|---|---| etc.) is present.
		// Without a separator this is prose that happens to contain pipe chars
		// (e.g. tool-hint lines, shell commands, regex patterns).
		if isTableRow(strippedOrig) {
			j := i
			for j < len(lines) && isTableRow(strings.TrimSpace(lines[j])) {
				j++
			}
			hasSep := false
			for k := i; k < j; k++ {
				if isTableSep(strings.TrimSpace(lines[k])) {
					hasSep = true
					break
				}
			}
			if hasSep {
				rendered := renderTableBlock(lines[i:j])
				for k, r := range rendered {
					lines[i+k] = r
				}
				activeANSI = "" // tables always end with ansiReset
				i = j
				continue
			}
			// No separator — fall through and style each line normally.
		}

		line := originalLine
		if activeANSI != "" {
			line = activeANSI + line
		}
		styled := styleLine(line)

		// Determine the effective ANSI context for this line: either the
		// carried-over context from the previous line, or the line's own
		// leading escape (e.g. a diff line that opens with its own color).
		// This ensures ansiReset injected by inline spans (e.g. backtick
		// matches) is followed by a re-injection of the active color.
		lineContext := activeANSI
		localContext := false // true when lineContext came from leadingANSI, not carry-over
		if lineContext == "" {
			lineContext = leadingANSI(originalLine)
			localContext = lineContext != ""
		}
		if lineContext != "" {
			styled = injectANSIAfterResets(styled, lineContext)
			// When the context was sourced from the line's own leading ANSI
			// (not a carry-over from the previous line), ensure the styled line
			// ends with ansiReset so the color does not bleed into the next line.
			if localContext && !strings.HasSuffix(styled, ansiReset) {
				styled += ansiReset
			}
		}
		lines[i] = styled
		// Carry-over: compute from the ORIGINAL (pre-prepend) line so that
		// inline span replacements (which emit their own ansiReset) do not
		// affect the carry-over decision. Only a true ansiReset from the
		// source text closes the outer context.
		activeANSI = trailingOpenANSI(originalLine)
		// If the original line had no ANSI of its own but we entered this line
		// with a carry-over context (e.g. a dim thinking block), preserve that
		// context — inline span resets within the line do not end the block.
		// But if the source line contains an explicit reset, the dim block ended
		// here and the carry-over must not bleed into the next line.
		if activeANSI == "" && lineContext != "" && !localContext {
			if !strings.Contains(originalLine, ansiReset) {
				activeANSI = lineContext
			}
		}
		// If there was no carry-over at all, check whether styleLine opened a
		// new context (e.g. a dim heading). Only consult the styled line when no
		// context injection ran (lineContext == ""), because injectANSIAfterResets
		// leaves a trailing context code after the last reset that would otherwise
		// falsely propagate dim into the line after a closing tool-hint block.
		if activeANSI == "" && lineContext == "" {
			activeANSI = trailingOpenANSI(lines[i])
		}
		i++
	}
	return strings.Join(lines, "\n")
}

// leadingANSI returns the first ANSI escape sequence at the start of s, or "".
// Used to detect lines that open with their own color (e.g. diff output) so
// that inline-span resets can be patched without a cross-line carry-over.
func leadingANSI(s string) string {
	if len(s) < 3 || s[0] != 0x1B || s[1] != '[' {
		return ""
	}
	end := strings.IndexByte(s[2:], 'm')
	if end == -1 {
		return ""
	}
	return s[:end+3]
}

// injectANSIAfterResets re-injects context after every ansiReset in s.
// This preserves the outer ANSI context (e.g. dim) when inline span
// replacements emit their own ansiReset within a dim-wrapped block.
func injectANSIAfterResets(s, context string) string {
	if !strings.Contains(s, ansiReset) {
		return s
	}
	resetLen := len(ansiReset)
	var sb strings.Builder
	offset := 0
	for {
		idx := strings.Index(s[offset:], ansiReset)
		if idx == -1 {
			sb.WriteString(s[offset:])
			break
		}
		abs := offset + idx
		sb.WriteString(s[offset : abs+resetLen])
		sb.WriteString(context)
		offset = abs + resetLen
	}
	return sb.String()
}

// trailingOpenANSI returns the last un-reset ANSI escape code in s, or ""
// if all opened codes were closed with ansiReset. Used to carry dim/color
// state across newline splits in multi-line ANSI-wrapped blocks.
func trailingOpenANSI(s string) string {
	last := ""
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1B && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && s[j] != 'm' {
				j++
			}
			if j < len(s) {
				code := s[i : j+1]
				if code == ansiReset {
					last = ""
				} else {
					last = code
				}
				i = j
			}
		}
	}
	return last
}

func styleLine(line string) string {
	stripped := strings.TrimLeft(line, " \t")

	// ATX headings.
	if strings.HasPrefix(stripped, "### ") {
		return ansiBold + ansiCyan + line + ansiReset
	}
	if strings.HasPrefix(stripped, "## ") {
		return ansiBold + ansiCyan + line + ansiReset
	}
	if strings.HasPrefix(stripped, "# ") {
		return ansiBold + ansiMagenta + line + ansiReset
	}

	// Setext headings (===, ---) — style the preceding line's underline.
	if reSetextEquals.MatchString(stripped) {
		return ansiBold + ansiMagenta + line + ansiReset
	}
	if reSetextDash.MatchString(stripped) {
		return ansiBold + ansiCyan + line + ansiReset
	}

	// Unordered list bullets.
	if strings.HasPrefix(stripped, "- ") || strings.HasPrefix(stripped, "* ") || strings.HasPrefix(stripped, "+ ") {
		prefix := line[:len(line)-len(stripped)]
		marker := stripped[0]
		rest := stripped[2:]
		rest = styleInlineSpans(rest)
		return prefix + ansiDim + string(marker) + ansiReset + " " + rest
	}

	// Ordered list items (1. 2. etc.).
	if m := reOrderedItem.FindStringIndex(line); m != nil {
		return line[:m[1]] + styleInlineSpans(line[m[1]:])
	}

	// Blockquote.
	if strings.HasPrefix(stripped, "> ") {
		return ansiDim + ansiItalic + line + ansiReset
	}

	// Horizontal rule.
	if reHRule.MatchString(stripped) {
		return ansiDim + line + ansiReset
	}

	return styleInlineSpans(line)
}

// styleInlineSpans applies bold/italic/inline-code ANSI to a single line span.
func styleInlineSpans(s string) string {
	// Inline code first — suppress further styling inside backtick spans.
	s = reInlineCode.ReplaceAllStringFunc(s, func(m string) string {
		inner := m[1 : len(m)-1]
		return ansiDim + "`" + ansiReset + ansiYellow + inner + ansiReset + ansiDim + "`" + ansiReset
	})

	// ***bold+italic*** before ** or * so we don't consume the markers twice.
	s = reStrongEM.ReplaceAllStringFunc(s, func(m string) string {
		inner := m[3 : len(m)-3]
		return ansiBold + ansiItalic + inner + ansiReset
	})

	// **bold** / __bold__.
	s = reStrong.ReplaceAllStringFunc(s, func(m string) string {
		// Trim the two-char delimiters on each side.
		inner := m[2 : len(m)-2]
		return ansiBold + inner + ansiReset
	})

	return s
}

// --- table rendering (balanced mode) ---

// isTableRow reports whether line looks like a GFM table row.
// A valid row must either start or end with "|", or contain " | " (space-padded
// separator), AND have at least two pipe characters total. This rejects lines
// that merely contain pipes inside shell commands, regex patterns, or tool hints.
func isTableRow(line string) bool {
	s := strings.TrimSpace(line)
	if strings.Count(s, "|") < 2 {
		return false
	}
	return strings.HasPrefix(s, "|") || strings.HasSuffix(s, "|") || strings.Contains(s, " | ")
}

// isTableSep reports whether line is a GFM table separator row ("|---|---|").
// Every non-empty cell must match reTableSepCell after trimming spaces.
func isTableSep(line string) bool {
	s := strings.TrimSpace(line)
	if !isTableRow(s) {
		return false
	}
	s = strings.Trim(s, "|")
	for _, cell := range strings.Split(s, "|") {
		cell = strings.TrimSpace(cell)
		if cell == "" {
			continue
		}
		if !reTableSepCell.MatchString(cell) {
			return false
		}
	}
	return true
}

// splitTableCells splits a raw table row into cell strings, stripping the
// outer pipes and trimming whitespace from each cell.
func splitTableCells(line string) []string {
	s := strings.TrimSpace(line)
	s = strings.Trim(s, "|")
	parts := strings.Split(s, "|")
	cells := make([]string, len(parts))
	for i, p := range parts {
		cells[i] = strings.TrimSpace(p)
	}
	return cells
}

// renderTableBlock performs a two-pass render of a contiguous slice of raw
// table lines. Pass 1: collect all cell text and compute the max visual width
// per column. Pass 2: emit each row with cells padded to the column width.
// The first non-separator row is treated as the header (bold+cyan). Separator
// rows are rendered as a dim rule whose dashes fill each column width.
func renderTableBlock(rawLines []string) []string {
	// Pass 1 — split every non-separator row into cells and find per-column max
	// widths. Separator rows are excluded so their dash counts don't inflate the
	// column widths beyond what the actual content requires.
	allCells := make([][]string, len(rawLines))
	colWidths := []int{}
	for i, raw := range rawLines {
		cells := splitTableCells(raw)
		allCells[i] = cells
		if isTableSep(strings.TrimSpace(raw)) {
			continue
		}
		for c, cell := range cells {
			w := rw.StringWidth(ansi.Strip(styleInlineSpans(cell)))
			if c >= len(colWidths) {
				colWidths = append(colWidths, w)
			} else if w > colWidths[c] {
				colWidths[c] = w
			}
		}
	}

	// Determine header row index: first non-separator row followed by a sep.
	headerIdx := -1
	for i, raw := range rawLines {
		if !isTableSep(raw) {
			if i+1 < len(rawLines) && isTableSep(rawLines[i+1]) {
				headerIdx = i
			}
			break
		}
	}

	// Pass 2 — render each row with equal-width columns.
	out := make([]string, len(rawLines))
	for i, raw := range rawLines {
		var sb strings.Builder
		if isTableSep(raw) {
			// Separator row: dim dashes filling each column width.
			sb.WriteString(ansiDim)
			sb.WriteString("|")
			for c, w := range colWidths {
				sb.WriteString(" ")
				sb.WriteString(strings.Repeat("-", w))
				sb.WriteString(" |")
				_ = c
			}
			sb.WriteString(ansiReset)
		} else {
			isHeader := i == headerIdx
			cells := allCells[i]
			if isHeader {
				sb.WriteString(ansiBold + ansiCyan)
			}
			sb.WriteString("|")
			for c, w := range colWidths {
				sb.WriteString(" ")
				cell := ""
				if c < len(cells) {
					cell = cells[c]
				}
				styled := styleInlineSpans(cell)
				sb.WriteString(styled)
				pad := w - rw.StringWidth(ansi.Strip(styled))
				if pad > 0 {
					sb.WriteString(strings.Repeat(" ", pad))
				}
				sb.WriteString(" |")
			}
			if isHeader {
				sb.WriteString(ansiReset)
			}
		}
		out[i] = sb.String()
	}
	return out
}

// --- full mode: glamour ---

var glamourRenderer *glamour.TermRenderer

func init() {
	r, err := glamour.NewTermRenderer(
		glamour.WithAutoStyle(),
		glamour.WithWordWrap(0), // milk handles its own wrapping
	)
	if err == nil {
		glamourRenderer = r
	}
}

// colorizeGlamour renders the entire text through glamour. Falls back to
// colorizeCodeBlocks if glamour is unavailable.
func colorizeGlamour(text string) string {
	if glamourRenderer == nil {
		return colorizeCodeBlocks(text)
	}
	out, err := glamourRenderer.Render(text)
	if err != nil {
		return colorizeCodeBlocks(text)
	}
	return strings.TrimRight(out, "\n")
}
