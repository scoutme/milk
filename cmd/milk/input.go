package main

import (
	"strings"
	"unicode/utf8"

	rw "github.com/mattn/go-runewidth"
	"github.com/rivo/uniseg"

	"github.com/charmbracelet/x/ansi"
)

func (m *model) colorizeInput(view string) string {
	if !isTTY {
		return view
	}

	// The textarea cursor is rendered as \x1b[7m<char>\x1b[m (reverse-video).
	// Extract it before colorizing so ANSI width math stays correct, then
	// re-inject at the same visual column afterward.
	type cursorSave struct {
		lineIdx   int
		visualCol int // 0-based visual column of the cursor character
		escape    string
	}
	var cursor *cursorSave

	cursorRE := "\x1b[7m"
	if idx := strings.Index(view, cursorRE); idx >= 0 {
		// Find the full cursor sequence: \x1b[7m<char>\x1b[m
		end := strings.Index(view[idx:], "\x1b[m")
		var seq string
		if end >= 0 {
			seq = view[idx : idx+end+3] // include trailing \x1b[m
		} else {
			seq = view[idx:]
		}
		// Determine which line and visual column the cursor sits on.
		before := view[:idx]
		lineIdx := strings.Count(before, "\n")
		lastNL := strings.LastIndex(before, "\n")
		linePrefix := before[lastNL+1:]
		visualCol := ansi.StringWidth(linePrefix)
		cursor = &cursorSave{lineIdx: lineIdx, visualCol: visualCol, escape: seq}
		// Remove cursor from view so subsequent ANSI stripping is clean.
		view = view[:idx] + ansi.Strip(seq) + view[idx+len(seq):]
	}

	lines := strings.Split(view, "\n")
	if len(lines) == 0 {
		return view
	}

	// Strip ANSI from line 0 to find the visual position of "❯ ".
	// line0Plain is used for measurement only; line 0 itself is re-written below.
	line0Plain := ansi.Strip(lines[0])
	promptEnd := strings.Index(line0Plain, "❯ ")
	if promptEnd < 0 {
		return view
	}
	// indentVisual is the number of visible columns up to and including "❯ ".
	indentVisual := promptEnd + 2

	// For line 0: find the byte offset in the raw (ANSI-containing) line that
	// corresponds to indentVisual visible columns, then split there.
	// We advance through the raw line, counting visible chars by stripping ANSI.
	line0ByteSplit := func(raw string, visualCols int) int {
		vis := 0
		i := 0
		for i < len(raw) {
			if raw[i] == '\x1b' {
				// skip ANSI escape sequence
				j := i + 1
				for j < len(raw) && raw[j] != 'm' {
					j++
				}
				if j < len(raw) {
					j++ // consume 'm'
				}
				i = j
				continue
			}
			vis++
			if vis == visualCols {
				_, runeSize := utf8.DecodeRuneInString(raw[i:])
				return i + runeSize
			}
			_, runeSize := utf8.DecodeRuneInString(raw[i:])
			i += runeSize
		}
		return len(raw)
	}

	for i, line := range lines {
		// Strip ANSI from this line to work with visible characters only.
		plain := ansi.Strip(line)

		var prefix, inputPart string
		if i == 0 {
			split := line0ByteSplit(line, indentVisual)
			prefix = line[:split]
			// promptEnd is a byte offset in plain; len("❯ ") is the byte width.
			// Using indentVisual here would slice mid-rune for multi-byte prompt chars.
			inputPart = plain[promptEnd+len("❯ "):]
		} else {
			// Continuation lines have indentVisual plain spaces as indent.
			if len(plain) <= indentVisual {
				continue
			}
			prefix = strings.Repeat(" ", indentVisual)
			inputPart = plain[indentVisual:]
		}

		var colored string
		if m.searching && m.searchQuery.Len() > 0 {
			colored = highlightMatch(inputPart, m.searchQuery.String())
		} else {
			colored = colorizeTokens(inputPart)
		}
		lines[i] = prefix + colored
	}

	// Apply keyboard selection highlight (before cursor re-injection so cursor
	// takes visual precedence over the selection background).
	if m.taSelAnchor >= 0 && m.taSelEnd >= 0 && m.taSelAnchor != m.taSelEnd {
		loRune, hiRune := m.taSelAnchor, m.taSelEnd
		if loRune > hiRune {
			loRune, hiRune = hiRune, loRune
		}
		// Walk display lines, tracking which global rune offset each line starts at.
		// Each display line corresponds to exactly the plain chars in inputPart.
		// We reconstruct the mapping by re-splitting the value.
		valueRunes := []rune(m.ta.Value())
		logicalLines := strings.Split(m.ta.Value(), "\n")
		displayLineOffset := 0 // global rune offset at the start of this logical line
		displayLineIdx := 0    // index into lines[]
		for _, logLine := range logicalLines {
			logRunes := []rune(logLine)
			// memoizedWrap-equivalent: split logLine into display rows using taWrapRows logic.
			wrapW := m.ta.Width()
			if wrapW <= 0 {
				wrapW = 1
			}
			displayRows := wrapLineIntoRows(logRunes, wrapW)
			rowOffset := 0 // rune offset within the logical line for this display row
			for _, row := range displayRows {
				rowLen := len(row)
				rowStart := displayLineOffset + rowOffset
				// Clamp sel range to this row.
				selLo := loRune - rowStart
				selHi := hiRune - rowStart
				if selLo < rowLen && selHi >= 0 {
					if selLo < 0 {
						selLo = 0
					}
					if selHi > rowLen {
						selHi = rowLen
					}
					// Apply highlight to lines[displayLineIdx] between selLo and selHi (rune positions in inputPart).
					if displayLineIdx < len(lines) {
						lines[displayLineIdx] = applyInputHighlight(lines[displayLineIdx], indentVisual, selLo, selHi)
					}
				}
				rowOffset += rowLen
				displayLineIdx++
			}
			// +1 for the '\n' separator between logical lines.
			displayLineOffset += len(logRunes) + 1
		}
		_ = valueRunes // used only for length context; actual work done via logicalLines
	}

	// Re-inject the cursor escape at the saved visual column.
	if cursor != nil && cursor.lineIdx < len(lines) {
		line := lines[cursor.lineIdx]
		targetCol := cursor.visualCol
		byteOff := 0
		vis := 0
		for byteOff < len(line) {
			if line[byteOff] == '\x1b' {
				// skip ANSI escape sequence
				j := byteOff + 1
				for j < len(line) && line[j] != 'm' {
					j++
				}
				if j < len(line) {
					j++
				}
				byteOff = j
				continue
			}
			if vis == targetCol {
				break
			}
			// Advance by one full UTF-8 rune.
			_, runeSize := utf8.DecodeRuneInString(line[byteOff:])
			vis++
			byteOff += runeSize
		}
		// Wrap the rune at byteOff with reverse-video on/off.
		// Use \x1b[27m (reverse off) instead of \x1b[m (full reset) so active
		// color codes (e.g. yellow on a slash command) remain in effect after
		// the cursor character.
		if byteOff < len(line) {
			_, runeSize := utf8.DecodeRuneInString(line[byteOff:])
			ch := line[byteOff : byteOff+runeSize]
			lines[cursor.lineIdx] = line[:byteOff] + "\x1b[7m" + ch + "\x1b[27m" + line[byteOff+runeSize:]
		}
	}

	return strings.Join(lines, "\n")
}

// wrapLineIntoRows splits a logical line (as rune slice) into display rows of
// at most `width` display columns, matching the bubbles/textarea wrap() logic.
// Returns each row as a rune slice.
func wrapLineIntoRows(runes []rune, width int) [][]rune {
	var (
		rows   [][]rune
		curRow []rune
		word   []rune
		curW   int
		spaces int
	)
	flush := func() {
		ww := uniseg.StringWidth(string(word))
		sw := spaces
		if curW+ww+sw > width {
			rows = append(rows, curRow)
			curRow = append(append([]rune{}, word...), []rune(strings.Repeat(" ", spaces))...)
			curW = ww + sw
		} else {
			curRow = append(append(curRow, word...), []rune(strings.Repeat(" ", spaces))...)
			curW += ww + sw
		}
		word = nil
		spaces = 0
	}
	for _, r := range runes {
		if r == ' ' || r == '\t' {
			if len(word) > 0 || spaces > 0 {
				spaces++
			}
		} else {
			if spaces > 0 {
				flush()
			}
			word = append(word, r)
			lastW := rw.RuneWidth(word[len(word)-1])
			if uniseg.StringWidth(string(word))+lastW > width {
				if curW > 0 {
					rows = append(rows, curRow)
					curRow = nil
					curW = 0
				}
				curRow = append(curRow, word...)
				curW += uniseg.StringWidth(string(word))
				word = nil
			}
		}
	}
	// flush remaining
	ww := uniseg.StringWidth(string(word))
	sw := spaces
	if curW+ww+sw >= width {
		rows = append(rows, curRow)
		curRow = append(append([]rune{}, word...), []rune(strings.Repeat(" ", spaces))...)
	} else {
		curRow = append(append(curRow, word...), []rune(strings.Repeat(" ", spaces))...)
	}
	rows = append(rows, curRow)
	return rows
}

// applyInputHighlight applies reverse-video highlight to rune positions [selLo, selHi)
// of the inputPart section of a colorized display line (after indentVisual prefix chars).
func applyInputHighlight(line string, indentVisual, selLo, selHi int) string {
	// Re-split at indentVisual to isolate the input part.
	// Work exclusively in runes: indentVisual is a visual column count, not a
	// byte offset. Using plainLine[indentVisual:] would slice mid-rune for
	// multi-byte prompt characters (e.g. ❯ is 3 bytes but 1 column/rune).
	plainRunes := []rune(ansi.Strip(line))
	if len(plainRunes) <= indentVisual {
		return line
	}
	inputRunes := plainRunes[indentVisual:]
	if selLo >= len(inputRunes) || selHi <= 0 {
		return line
	}
	if selLo < 0 {
		selLo = 0
	}
	if selHi > len(inputRunes) {
		selHi = len(inputRunes)
	}
	// Build the highlighted version of the plain input part.
	// Use a background-color highlight (not \x1b[7m reverse-video) so it doesn't
	// conflict with the cursor which also uses \x1b[7m.
	var sb strings.Builder
	sb.WriteString(string(inputRunes[:selLo]))
	sb.WriteString("\x1b[48;5;240m") // dark-gray bg for selection
	sb.WriteString(string(inputRunes[selLo:selHi]))
	sb.WriteString("\x1b[49m") // reset background only
	sb.WriteString(string(inputRunes[selHi:]))
	// Reconstruct with original prefix (rune-safe).
	return string(plainRunes[:indentVisual]) + sb.String()
}

// highlightMatch bolds and yellows the first occurrence of query inside s.
func highlightMatch(s, query string) string {
	idx := strings.Index(strings.ToLower(s), strings.ToLower(query))
	if idx < 0 {
		return s
	}
	return s[:idx] + colorize(s[idx:idx+len(query)], "\033[1;33m") + s[idx+len(query):]
}

func colorizeTokens(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = colorizeTokenLine(line)
	}
	return strings.Join(lines, "\n")
}

func colorizeTokenLine(s string) string {
	plain := ansi.Strip(s)
	var out strings.Builder
	i := 0
	for i < len(plain) {
		// Emit whitespace runs as-is.
		j := i
		for j < len(plain) && plain[j] == ' ' {
			j++
		}
		if j > i {
			out.WriteString(plain[i:j])
			i = j
			continue
		}
		// Collect non-whitespace token.
		for j < len(plain) && plain[j] != ' ' {
			j++
		}
		w := plain[i:j]
		switch {
		case isSlashCmdToken(w):
			out.WriteString(yellow(w))
		case strings.HasPrefix(w, "@"):
			out.WriteString(dim(w))
		default:
			out.WriteString(w)
		}
		i = j
	}
	return out.String()
}

func stripANSI(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		if inEsc {
			if r == 'm' {
				inEsc = false
			}
			continue
		}
		if r == '\033' {
			inEsc = true
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
