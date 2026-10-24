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

	"github.com/scoutme/milk/internal/sgr"
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
// block and NOT inside an open ANSI escape sequence. This prevents splitting a
// code block whose body contains blank lines, and prevents splitting a dim-wrapped
// thinking block mid-paragraph (which would cause the second paragraph to be
// colorized without the dim carry-over context).
func splitTurns(text string) []string {
	var segments []string
	inFence := false
	var open sgr.State // rendition left open by the text so far
	start := 0
	i := 0
	for i < len(text) {
		// Track open ANSI sequences so we know when we're inside a dim block.
		if text[i] == 0x1B && i+1 < len(text) && text[i+1] == '[' {
			j := i + 2
			for j < len(text) && (text[j] < 0x40 || text[j] > 0x7E) {
				j++
			}
			if j < len(text) {
				if text[j] == 'm' {
					open.Apply(text[i+2 : j])
				}
				i = j + 1
				continue
			}
		}

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
		if !inFence && !open.Active() && i+2 <= len(text) && text[i:i+2] == "\n\n" {
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
	return colorizeSingleWidth(seg, mode, 0)
}

// colorizeSingleWidth is colorizeSingle for a viewport width (0 = no limit):
// the one thing that depends on it is how wide markdown tables may get.
func colorizeSingleWidth(seg string, mode ColorizeMode, width int) string {
	switch mode {
	case ColorizeFenced:
		return colorizeCodeBlocks(seg)
	case ColorizeBalanced:
		return colorizeCodeBlocks(colorizeMarkdownWidth(seg, width))
	case ColorizeFull:
		return colorizeFull(seg)
	default:
		return colorizeCodeBlocks(seg)
	}
}

// colorizeFull applies glamour to unstyled portions and colorizeCodeBlocks
// to preserved styled spans — thinking-surface rows and dim-wrapped notes
// (thinking output of old) — which glamour would erase (it needs the segment
// ANSI-stripped) and reflow. Each span runs from its opening code to the
// first full reset after it: exactly one rendered line's styling.
func colorizeFull(seg string) string {
	if !strings.Contains(seg, ansiDim) && !strings.Contains(seg, ansiThinkBg) {
		return colorizeGlamour(ansi.Strip(seg))
	}
	var sb strings.Builder
	remaining := seg
	for {
		start := styledSpanStart(remaining)
		if start == -1 {
			sb.WriteString(glamourChunk(remaining))
			break
		}
		if start > 0 {
			sb.WriteString(glamourChunk(remaining[:start]))
		}
		remaining = remaining[start:] // starts with the span's opening code
		resetIdx := strings.Index(remaining, ansiReset)
		if resetIdx == -1 {
			sb.WriteString(remaining)
			break
		}
		styled := remaining[:resetIdx+len(ansiReset)]
		sb.WriteString(colorizeCodeBlocks(styled))
		remaining = remaining[resetIdx+len(ansiReset):]
	}
	return sb.String()
}

// glamourChunk runs one unstyled chunk through glamour — except a chunk that
// is whitespace only (typically the newline between two styled rows), which
// is passed through verbatim: glamour collapses it to nothing and would glue
// the rows together.
func glamourChunk(s string) string {
	if strings.TrimSpace(s) == "" {
		return s
	}
	return colorizeGlamour(ansi.Strip(s))
}

// styledSpanStart returns the byte index of the next preserved styled span in
// s — a thinking-surface row (ansiThinkBg) or a dim-wrapped note (ansiDim) —
// or -1 when none remains. A fold-header row opens with the surface code and
// carries the dim code inside its span (ansiThinkBg+ansiDim), so the span must
// start at the surface escape, not at the dim one.
func styledSpanStart(s string) int {
	i := strings.Index(s, ansiThinkBg)
	j := strings.Index(s, ansiDim)
	if i < 0 {
		return j
	}
	if j < 0 || i < j {
		return i
	}
	return j
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
func colorizeMarkdown(text string) string { return colorizeMarkdownWidth(text, 0) }

// colorizeMarkdownWidth is colorizeMarkdown for a viewport width: tables wider
// than width are reflowed into it (0 = no limit).
func colorizeMarkdownWidth(text string, width int) string {
	var sb strings.Builder
	remaining := text

	for {
		fenceIdx := findOpenFence(remaining)
		if fenceIdx == -1 {
			// No more fences — style the rest.
			sb.WriteString(applyInlineMarkdownWidth(remaining, width))
			break
		}
		// Style the text before the fence, then preserve the fenced block verbatim.
		sb.WriteString(applyInlineMarkdownWidth(remaining[:fenceIdx], width))
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
//
// Styling already present in the text is respected line by line: the
// rendition a line leaves open is carried into the next one (sgr.State), so a
// multi-line dim block keeps its dim on every line, and the resets that inline
// spans emit are followed by a re-injection of the line's outer context so they
// don't cut it short. Inside a dim context inline styling is skipped entirely:
// \033[1m (bold) overrides \033[2m (dim) in terminals, so **bold** in dim text
// would render bright instead of dim.
func applyInlineMarkdown(text string) string { return applyInlineMarkdownWidth(text, 0) }

// applyInlineMarkdownWidth is applyInlineMarkdown for a viewport width, see
// renderTableBlock for how a table is fitted into it.
func applyInlineMarkdownWidth(text string, width int) string {
	lines := strings.Split(text, "\n")
	var carried sgr.State // rendition the previous lines left open
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
				rendered := renderTableBlock(lines[i:j], width)
				for k, r := range rendered {
					lines[i+k] = r
				}
				carried = sgr.State{} // tables always end with ansiReset
				i = j
				continue
			}
			// No separator — fall through and style each line normally.
		}

		// The line's outer context: what the previous lines left open, or —
		// when nothing was — a row-owning rendition the line opens itself: a
		// dim note or coloured background (diff row) covering the line to its
		// end. A finite leading span like the turn label ("mimo-pro-local:" in
		// bold blue) is just the line's first span, NOT outer context: its own
		// reset ends it, and re-injecting it after that reset repaints the
		// response text that shares the label's row (the turn-prefix colour
		// bleed). Rows still balance either way, which is why only a
		// content-level check catches it.
		ctx, ownContext := carried, false
		if !ctx.Active() {
			ctx = sgr.Leading(originalLine)
			var endState sgr.State
			endState.Scan(originalLine)
			ownContext = ctx.Active() && (ctx.Dim() || ctx.BG != "" || endState.Includes(ctx))
		}

		line := originalLine
		if carried.Active() {
			line = carried.String() + line
		}
		styled := line
		if !ctx.Dim() {
			styled = styleLine(line)
		}
		if ctx.Active() && (carried.Active() || ownContext) {
			styled = injectANSIAfterResets(styled, ctx.String())
			// A context the line opened itself must not bleed into the next
			// line: close it. (A carried one stays open — it is the block.)
			if ownContext && !strings.HasSuffix(styled, ansiReset) {
				styled += ansiReset
			}
		}
		lines[i] = styled
		// What the next line inherits is decided by the source line alone —
		// the resets inline spans add never end an outer block.
		carried.Scan(originalLine)
		i++
	}
	return strings.Join(lines, "\n")
}

// injectANSIAfterResets re-injects context after every ansiReset in s.
// This preserves the outer ANSI context (e.g. dim) when inline span
// replacements emit their own ansiReset within a dim-wrapped block.
func injectANSIAfterResets(s, context string) string {
	return strings.ReplaceAll(s, ansiReset, ansiReset+context)
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

// tableMinColumnWidth is the narrowest a column is squeezed to when a table
// has to fit the viewport; below it the table is left at its natural width
// (the wrap fallback) rather than turned into one-character columns.
const tableMinColumnWidth = 6

// renderTableBlock performs a two-pass render of a contiguous slice of raw
// table lines. Pass 1: collect all cell text and compute the max visual width
// per column. Pass 2: emit each row with cells padded to the column width.
// The first non-separator row is treated as the header (bold+cyan). Separator
// rows are rendered as a dim rule whose dashes fill each column width.
//
// maxWidth > 0 is the viewport width: a table wider than it has its widest
// columns narrowed and the cell text wrapped inside them, so every row stays
// a row of aligned columns instead of being word-wrapped through its pipes. A
// row with wrapped cells becomes several physical lines in one entry.
func renderTableBlock(rawLines []string, maxWidth int) []string {
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
	colWidths = fitTableColumns(colWidths, maxWidth)

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
		if isTableSep(raw) {
			out[i] = tableSeparatorRow(colWidths)
			continue
		}
		out[i] = tableContentRow(allCells[i], colWidths, i == headerIdx)
	}
	return out
}

// fitTableColumns narrows the widest columns until the table (one "| " / " |"
// frame per column plus the leading pipe) fits maxWidth. It returns widths
// unchanged when no limit is set, the table already fits, or even the minimum
// column width would not fit.
func fitTableColumns(widths []int, maxWidth int) []int {
	if maxWidth <= 0 || len(widths) == 0 {
		return widths
	}
	frame := 1 + 3*len(widths)
	total := frame
	for _, w := range widths {
		total += w
	}
	if total <= maxWidth || frame+len(widths)*tableMinColumnWidth > maxWidth {
		return widths
	}
	fitted := append([]int(nil), widths...)
	for total > maxWidth {
		widest := 0
		for c, w := range fitted {
			if w > fitted[widest] {
				widest = c
			}
		}
		if fitted[widest] <= tableMinColumnWidth {
			break
		}
		fitted[widest]--
		total--
	}
	return fitted
}

// tableSeparatorRow is the dim rule under the header, dashes filling each
// column.
func tableSeparatorRow(colWidths []int) string {
	var sb strings.Builder
	sb.WriteString(ansiDim)
	sb.WriteString("|")
	for _, w := range colWidths {
		sb.WriteString(" ")
		sb.WriteString(strings.Repeat("-", w))
		sb.WriteString(" |")
	}
	sb.WriteString(ansiReset)
	return sb.String()
}

// tableContentRow renders one row. A cell wider than its column is wrapped
// into extra physical lines; shorter cells are padded so the pipes of every
// line stay in their columns. Each physical line is closed on its own.
func tableContentRow(cells []string, colWidths []int, isHeader bool) string {
	pieces := make([][]string, len(colWidths))
	height := 1
	for c, w := range colWidths {
		cell := ""
		if c < len(cells) {
			cell = cells[c]
		}
		styled := styleInlineSpans(cell)
		if w > 0 {
			styled = wrapRows(styled, w)
		}
		pieces[c] = strings.Split(styled, "\n")
		height = max(height, len(pieces[c]))
	}
	lines := make([]string, height)
	for line := range height {
		var sb strings.Builder
		if isHeader {
			sb.WriteString(ansiBold + ansiCyan)
		}
		sb.WriteString("|")
		for c, w := range colWidths {
			piece := ""
			if line < len(pieces[c]) {
				piece = pieces[c][line]
			}
			sb.WriteString(" ")
			sb.WriteString(piece)
			if pad := w - rw.StringWidth(ansi.Strip(piece)); pad > 0 {
				sb.WriteString(strings.Repeat(" ", pad))
			}
			sb.WriteString(" |")
		}
		if isHeader {
			sb.WriteString(ansiReset)
		}
		lines[line] = sb.String()
	}
	return strings.Join(lines, "\n")
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
// expandTabsForWrap replaces each \t with spaces to the next 8-column tab stop,
// accounting for ANSI escape sequences (which have zero visual width).
// This must be called before ansi.Wrap so that the wrapping width calculation
// matches what the terminal actually renders — ansi.StringWidth counts \t as 1
// column but terminals expand to the next multiple-of-8 boundary.
func expandTabsForWrap(s string) string {
	if !strings.ContainsRune(s, '\t') {
		return s
	}
	var sb strings.Builder
	col := 0
	i := 0
	for i < len(s) {
		// Skip ANSI escape sequences — they contribute zero visual columns.
		if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && s[j] != 'm' {
				j++
			}
			if j < len(s) {
				j++ // include 'm'
			}
			sb.WriteString(s[i:j])
			i = j
			continue
		}
		ch := s[i]
		if ch == '\n' {
			sb.WriteByte('\n')
			col = 0
			i++
			continue
		}
		if ch == '\t' {
			// Expand to next 8-column tab stop.
			spaces := 8 - (col % 8)
			for k := 0; k < spaces; k++ {
				sb.WriteByte(' ')
			}
			col += spaces
			i++
			continue
		}
		sb.WriteByte(ch)
		col++
		i++
	}
	return sb.String()
}

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
