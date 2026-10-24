package main

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// hintDebounceMsg fires after the debounce delay to trigger a (potentially
// expensive) hint rebuild. The gen field must match model.hintDebounceGen —
// stale firings from a superseded keystroke are silently dropped.
type hintDebounceMsg struct{ gen int }

// fileIndexReadyMsg is sent when a background build of the @-file candidate
// index (fileindex.go) finishes, so hints shown from a flat listing or a stale
// snapshot are redrawn from the fresh one.
type fileIndexReadyMsg struct{}

// onAtToken reports whether the word under the cursor is an @-path.
func (m *model) onAtToken() bool {
	beforeCursor := m.ta.Value()
	if lines := strings.Split(beforeCursor, "\n"); m.ta.Line() < len(lines) {
		beforeCursor = lines[m.ta.Line()]
	}
	words := strings.Fields(beforeCursor)
	return len(words) > 0 && strings.HasPrefix(words[len(words)-1], "@")
}

// --- Tab completion ---

// cmdVariants is derived from interactiveHelp at init time so hints can never
// drift from the canonical help text.
var cmdVariants = buildCmdVariants()

type cmdVariant struct {
	sig  string // full signature, e.g. "/memory show <pat|#id>"
	desc string
}

// buildCmdVariants parses interactiveHelp and returns a map from bare slash
// command to its ordered list of variants (sig + desc). Help lines whose
// description starts with "ACP only" document a command the TUI does NOT
// execute (e.g. /think on|off — ACP's data-plane relay toggle): they stay
// in /help as annotated documentation but are excluded from completion, so
// the TUI never offers an argument its own handler rejects.
func buildCmdVariants() map[string][]cmdVariant {
	result := map[string][]cmdVariant{}

	for line := range strings.SplitSeq(interactiveHelp, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "/") {
			continue
		}
		parts := strings.SplitN(trimmed, "  ", 2)
		if len(parts) < 2 {
			continue
		}
		sig := strings.TrimSpace(parts[0])
		desc := strings.TrimSpace(parts[1])
		if desc == "" {
			continue
		}
		if strings.HasPrefix(desc, "ACP only") {
			continue // TUI-help documentation, not a TUI-completion variant
		}
		cmd := strings.Fields(sig)[0]
		result[cmd] = append(result[cmd], cmdVariant{sig: sig, desc: desc})
	}
	return result
}

// handleTab advances (dir=+1) or retreats (dir=-1) through the tab completion
// cycle. For slash commands: cycles entry by entry within a command's variants,
// then moves to the next/previous command. For @-paths: cycles the flat list.
func (m model) handleTab(dir int) model {
	fullInput := m.ta.Value()
	lines := strings.Split(fullInput, "\n")
	curLine := m.ta.Line()
	if curLine >= len(lines) {
		curLine = len(lines) - 1
	}
	lineInput := lines[curLine]

	// Split at cursor column so completion works on the token under the cursor.
	li := m.ta.LineInfo()
	col := li.StartColumn + li.ColumnOffset
	if col > len([]rune(lineInput)) {
		col = len([]rune(lineInput))
	}
	runes := []rune(lineInput)
	beforeCursor := string(runes[:col])
	afterCursor := string(runes[col:])

	// Build or reuse the match list.
	if len(m.tabMatches) == 0 || curLine != m.tabLine {
		res := buildTabMatches(beforeCursor, m.st.cwd, m.paramLookup)
		m.tabMatches = res.matches
		m.tabIdx = 0
		m.tabLine = curLine
		if len(m.tabMatches) == 0 {
			m.tabValueMode = false
			m.tabNsLabel = ""
			m.tabSegPrefix = ""
			return m
		}
		m.tabPrefix = tabInputPrefix(beforeCursor)
		m.tabSubcmdMode = res.replaceBase != ""
		m.tabValueMode = res.valueMode
		m.tabNsLabel = res.nsLabel
		m.tabSegPrefix = res.segPrefix
		switch {
		case res.valueMode:
			// Concrete parameter values (#166): apply against the full
			// beforeCursor snapshot so the value replaces the token under the
			// cursor (or is appended after whitespace).
			m.tabBeforeCursor = beforeCursor
		case res.replaceBase != "":
			// Subcommand completions: replaceBase is the slash-command token
			// (e.g. "/memory"). applyTabCompletion replaces from there,
			// discarding any partial subcommand the user had typed.
			m.tabBeforeCursor = res.replaceBase
		default:
			m.tabBeforeCursor = beforeCursor
		}
		m.tabAfterCursor = afterCursor
		m.tabCmdIdx = 0
		m.tabVarIdx = 0
		if dir < 0 {
			// Shift+Tab from scratch: start at the last entry of the last command.
			m.tabCmdIdx = len(m.tabMatches) - 1
			if !m.tabSubcmdMode && !m.tabValueMode {
				if vs, ok := cmdVariants[m.tabMatches[m.tabCmdIdx]]; ok && len(vs) > 0 {
					m.tabVarIdx = len(vs) - 1
				}
			}
		}
	} else {
		// Advance within the current command's variants, then wrap to next command.
		cmd := m.tabMatches[m.tabCmdIdx]
		varCount := 1
		if !m.tabValueMode {
			if vs, ok := cmdVariants[cmd]; ok && len(vs) > 0 {
				varCount = len(vs)
			}
		}
		m.tabVarIdx += dir
		if m.tabVarIdx >= varCount {
			// Move to first variant of next command.
			m.tabCmdIdx = (m.tabCmdIdx + 1) % len(m.tabMatches)
			m.tabVarIdx = 0
		} else if m.tabVarIdx < 0 {
			// Move to last variant of previous command.
			m.tabCmdIdx = (m.tabCmdIdx - 1 + len(m.tabMatches)) % len(m.tabMatches)
			if vs, ok := cmdVariants[m.tabMatches[m.tabCmdIdx]]; !m.tabValueMode && ok && len(vs) > 0 {
				m.tabVarIdx = len(vs) - 1
			} else {
				m.tabVarIdx = 0
			}
		}
		// For non-slash (@-path) completions keep the flat tabIdx in sync.
		m.tabIdx = m.tabCmdIdx
	}

	completed := m.tabMatches[m.tabCmdIdx]

	// Insert the full variant sig into the textarea so the user sees the
	// subcommand and parameter placeholders — except in value mode (#166),
	// where the match itself is the concrete parameter value to insert.
	// Always apply against the original beforeCursor snapshot so cycling
	// doesn't accumulate previous completions.
	completionToken := completed
	if !m.tabValueMode {
		if vs, ok := cmdVariants[completed]; ok && len(vs) > 0 && m.tabVarIdx < len(vs) {
			completionToken = vs[m.tabVarIdx].sig
		}
	}

	completedBefore := m.applyCompletionToken(completionToken)
	lines[curLine] = completedBefore + m.tabAfterCursor
	m.ta.SetValue(strings.Join(lines, "\n"))
	precedingLen := 0
	if curLine > 0 {
		precedingLen = len([]rune(strings.Join(lines[:curLine], "\n"))) + 1
	}
	// Cursor goes after the completed token (before afterCursor).
	m.ta.SetCursorColumn(precedingLen + len([]rune(completedBefore)))

	// Build hint panel base: all variants styled (yellow sig + dim desc) but
	// without any highlight. highlightHint then overlays the active entry.
	m.tabHintsBase = nil
	prefix := m.tabPrefix
	if m.tabSubcmdMode {
		// Each match is a full sig — one hint line per entry, desc looked up from cmdVariants.
		for _, sig := range m.tabMatches {
			styledSig := yellow(sig)
			desc := ""
			// Find the matching variant to get the description.
			cmd := strings.Fields(sig)[0]
			for _, v := range cmdVariants[cmd] {
				if v.sig == sig {
					desc = "  " + dim(v.desc)
					break
				}
			}
			m.tabHintsBase = append(m.tabHintsBase, " "+styledSig+desc)
		}
	} else {
		totalCmds := len(m.tabMatches)
		for ci, cmd := range m.tabMatches {
			if m.tabValueMode {
				// Concrete parameter value (#166) — not a command. Show the
				// value plus the name-space it came from so the user knows
				// what is being completed.
				line := " " + yellow(cmd)
				if m.tabNsLabel != "" {
					line += dim("  (" + m.tabNsLabel + ")")
				}
				if totalCmds > 1 {
					line += dim(fmt.Sprintf(" [%d/%d]", ci+1, totalCmds))
				}
				m.tabHintsBase = append(m.tabHintsBase, line)
				continue
			}
			vs := cmdVariants[cmd]
			if len(vs) == 0 {
				// No registered variants (e.g. @-path or unlisted command) — one entry.
				m.tabHintsBase = append(m.tabHintsBase, " "+yellow(cmd))
				continue
			}
			for vi, v := range vs {
				sig := v.sig
				if prefix != "" && strings.HasPrefix(sig, prefix) {
					sig = boldYellow(prefix) + yellow(sig[len(prefix):])
				} else {
					sig = yellow(sig)
				}
				desc := dim(v.desc)
				cmdCount := ""
				if vi == 0 && totalCmds > 1 {
					cmdCount = dim(fmt.Sprintf(" [%d/%d]", ci+1, totalCmds))
				}
				m.tabHintsBase = append(m.tabHintsBase, " "+sig+"  "+desc+cmdCount)
			}
		}
	}
	// hintIdx is the flat index of the active (tabCmdIdx, tabVarIdx) entry.
	// In subcommand mode each match is one line so flatIdx == tabCmdIdx.
	m.hintIdx = m.tabCmdIdx
	if !m.tabSubcmdMode {
		flatIdx := 0
		for ci, cmd := range m.tabMatches {
			vs := cmdVariants[cmd]
			count := 1
			if !m.tabValueMode && len(vs) > 0 {
				count = len(vs)
			}
			if ci == m.tabCmdIdx {
				flatIdx += m.tabVarIdx
				break
			}
			flatIdx += count
		}
		m.hintIdx = flatIdx
	}

	m.tabHints = make([]string, len(m.tabHintsBase))
	copy(m.tabHints, m.tabHintsBase)
	m.highlightHint()

	m.syncLayout()
	return m
}

// syncTabIdxFromHint converts the flat hintIdx back into (tabCmdIdx, tabVarIdx).
// Called when arrow keys move hintIdx while tab-cycling is active.
func (m *model) syncTabIdxFromHint() {
	if m.tabSubcmdMode {
		m.tabCmdIdx = m.hintIdx
		m.tabVarIdx = 0
		return
	}
	flat := m.hintIdx
	for ci, cmd := range m.tabMatches {
		vs := cmdVariants[cmd]
		count := 1
		if !m.tabValueMode && len(vs) > 0 {
			count = len(vs)
		}
		if flat < count {
			m.tabCmdIdx = ci
			m.tabVarIdx = flat
			m.tabIdx = ci
			return
		}
		flat -= count
	}
	// Fallback: clamp to last entry.
	m.tabCmdIdx = len(m.tabMatches) - 1
	m.tabVarIdx = 0
}

// insertActiveCompletion inserts the completion for the current (tabCmdIdx, tabVarIdx)
// into the textarea. Mirrors the insertion logic in handleTab so arrows and Tab
// produce identical textarea state.
func (m model) insertActiveCompletion() model {
	completed := m.tabMatches[m.tabCmdIdx]
	completionToken := completed
	if !m.tabSubcmdMode && !m.tabValueMode {
		if vs, ok := cmdVariants[completed]; ok && len(vs) > 0 && m.tabVarIdx < len(vs) {
			completionToken = vs[m.tabVarIdx].sig
		}
	}
	fullInput := m.ta.Value()
	lines := strings.Split(fullInput, "\n")
	curLine := m.tabLine
	if curLine >= len(lines) {
		curLine = len(lines) - 1
	}
	completedBefore := m.applyCompletionToken(completionToken)
	lines[curLine] = completedBefore + m.tabAfterCursor
	m.ta.SetValue(strings.Join(lines, "\n"))
	precedingLen := 0
	if curLine > 0 {
		precedingLen = len([]rune(strings.Join(lines[:curLine], "\n"))) + 1
	}
	m.ta.SetCursorColumn(precedingLen + len([]rune(completedBefore)))
	return m
}

// tabInputPrefix extracts the slash-command prefix the user had typed in input.
// Uses the last /cmd token so that "/help foo /exp<TAB>" completes /exp, not /help.
// isSlashCmdToken returns true when w looks like a slash command token
// (/word…), as opposed to bare slashes or paths like "////".
func isSlashCmdToken(w string) bool {
	return len(w) >= 2 && w[0] == '/' && w[1] != '/'
}

func tabInputPrefix(input string) string {
	if input == "" || input[len(input)-1] == ' ' || input[len(input)-1] == '\t' {
		return ""
	}
	words := strings.Fields(input)
	if len(words) == 0 {
		return ""
	}
	last := words[len(words)-1]
	if isSlashCmdToken(last) || strings.HasPrefix(last, "@") {
		return last
	}
	return ""
}

// applyTabCompletion replaces the relevant token in input with completed,
// preserving all surrounding whitespace (no space collapsing).
func applyTabCompletion(input, completed string) string {
	if strings.HasPrefix(completed, "@") {
		result, found := replaceLastToken(input, func(w string) bool { return strings.HasPrefix(w, "@") }, completed)
		if found {
			return result
		}
		return completed
	}
	result, found := replaceLastToken(input, isSlashCmdToken, completed)
	if found {
		return result
	}
	return completed
}

// applyCompletionToken renders an accepted completion into m.tabBeforeCursor:
// value mode (#166) replaces/appends the token under the cursor, every other
// mode goes through applyTabCompletion (slash-token replacement). In multi-value
// value mode (#165) the already-typed comma segments (m.tabSegPrefix) are kept
// and only the segment under the cursor is replaced.
func (m model) applyCompletionToken(token string) string {
	if m.tabValueMode {
		return applyValueCompletion(m.tabBeforeCursor, m.tabSegPrefix+token)
	}
	return applyTabCompletion(m.tabBeforeCursor, token)
}

// applyValueCompletion inserts a completed parameter value into line, which is
// the current line up to the cursor: after whitespace the value is appended,
// otherwise the partial token under the cursor is replaced.
func applyValueCompletion(line, value string) string {
	if line == "" {
		return value
	}
	if c := line[len(line)-1]; c == ' ' || c == '\t' {
		return line + value
	}
	if i := strings.LastIndexAny(line, " \t"); i >= 0 {
		return line[:i+1] + value
	}
	return value
}

// stripCompletionPlaceholders removes tab-completion placeholder syntax from s:
// <param> required-parameter markers and [optional] suggestion markers.
// Applied before submitting input so accepted completions like
// "/memory show <pat|#id>" dispatch as "/memory show".
//
// Stripping is surgical: placeholders are only removed from tokens that
// immediately follow a slash-command token anywhere in a line. Normal user
// text like "[foo]" or "<tag>" that does not follow a "/cmd" word is left
// untouched. Stripping mode exits as soon as a non-placeholder, non-slash
// word is encountered.
func stripCompletionPlaceholders(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		words := strings.Fields(line)
		if len(words) == 0 {
			continue
		}
		// Check if any word in the line is a slash-command token.
		hasSlash := false
		for _, w := range words {
			if strings.HasPrefix(w, "/") {
				hasSlash = true
				break
			}
		}
		if !hasSlash {
			continue
		}
		// Rebuild word-by-word: once a slash-command token is seen, drop every
		// placeholder token (<…> or […]) on the rest of that line. Non-placeholder
		// tokens are always kept. Stripping mode stays active for the whole line.
		out := make([]string, 0, len(words))
		stripping := false
		inGroup := byte(0) // '<' or '[' when inside a multi-word placeholder group
		for _, w := range words {
			if strings.HasPrefix(w, "/") {
				stripping = true
				inGroup = 0
				out = append(out, w)
				continue
			}
			if stripping {
				// Continue consuming a multi-word placeholder group.
				if inGroup != 0 {
					closer := map[byte]byte{'<': '>', '[': ']'}[inGroup]
					if strings.HasSuffix(w, string(closer)) {
						inGroup = 0
					}
					continue
				}
				// A placeholder is a word (or group start) entirely wrapped in <…> or […].
				if (strings.HasPrefix(w, "<") && strings.HasSuffix(w, ">")) ||
					(strings.HasPrefix(w, "[") && strings.HasSuffix(w, "]")) {
					continue
				}
				// Start of a multi-word placeholder group.
				if strings.HasPrefix(w, "<") {
					inGroup = '<'
					continue
				}
				if strings.HasPrefix(w, "[") {
					inGroup = '['
					continue
				}
			}
			out = append(out, w)
		}
		// Preserve leading whitespace from original line.
		lead := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		lines[i] = lead + strings.Join(out, " ")
	}
	return strings.Join(lines, "\n")
}

// replaceLastToken finds the last token in input matching pred and replaces it
// with replacement, preserving all surrounding whitespace (no space collapsing).
// Returns the result and whether a matching token was found.
func replaceLastToken(input string, pred func(string) bool, replacement string) (string, bool) {
	lastStart, lastEnd := -1, -1
	i := 0
	for i < len(input) {
		// skip whitespace
		for i < len(input) && (input[i] == ' ' || input[i] == '\t') {
			i++
		}
		if i >= len(input) {
			break
		}
		// find token end
		j := i
		for j < len(input) && input[j] != ' ' && input[j] != '\t' {
			j++
		}
		w := input[i:j]
		if pred(w) {
			lastStart, lastEnd = i, j
		}
		i = j
	}
	if lastStart < 0 {
		return input, false
	}
	return input[:lastStart] + replacement + input[lastEnd:], true
}

// rebuildInlineHints populates tabHints passively while the user types an @-path
// or a /slash command, without advancing tab-cycling state.
// Cleared when the token no longer matches either prefix.
func (m *model) rebuildInlineHints() {
	if len(m.tabMatches) > 0 {
		// Tab cycling is active — don't overwrite its hints.
		return
	}
	fullInput := m.ta.Value()
	lines := strings.Split(fullInput, "\n")
	curLine := m.ta.Line()
	if curLine >= len(lines) {
		curLine = len(lines) - 1
	}
	li := m.ta.LineInfo()
	col := li.StartColumn + li.ColumnOffset
	runes := []rune(lines[curLine])
	if col > len(runes) {
		col = len(runes)
	}
	beforeCursor := string(runes[:col])
	words := strings.Fields(beforeCursor)
	if len(words) == 0 {
		m.tabHints = nil
		m.tabHintsBase = nil
		m.hintIdx = -1
		return
	}
	last := words[len(words)-1]

	if strings.HasPrefix(last, "@") {
		// In-memory filter over the cached candidate index (fileindex.go): no
		// directory walk, no git process on this path. While the very first
		// build is still running a flat listing of the typed directory is
		// shown, and fileIndexReadyMsg redraws the hints when it finishes.
		limit := m.viewportHeight() / 2
		if limit < 1 {
			limit = 1
		}
		matches := atPathMatches(last[1:], m.st.cwd, limit, 0)
		if len(matches) == 0 {
			m.setHints(nil)
			return
		}
		raw := make([]string, len(matches))
		for i, p := range matches {
			raw[i] = " " + dim("@"+p)
		}
		m.setHints(m.capHints(raw))
		return
	}

	if last == "/" || isSlashCmdToken(last) {
		var hints []string
		for _, cmd := range slashCommands {
			if last != "/" && !strings.HasPrefix(strings.ToLower(cmd), strings.ToLower(last)) {
				continue
			}
			vs := cmdVariants[cmd]
			if len(vs) == 0 {
				hints = append(hints, " "+dim(cmd))
				continue
			}
			for _, v := range vs {
				hints = append(hints, " "+dim(v.sig)+"  "+dim(v.desc))
			}
		}
		m.setHints(m.capHints(hints))
		return
	}

	// Passive subcommand hints: cursor is on a non-slash word and the preceding
	// token is a known command with variants. e.g. "/memory sh" while typing.
	if len(words) >= 2 {
		prev := words[len(words)-2]
		if isSlashCmdToken(prev) {
			if vs := cmdVariants[prev]; len(vs) > 0 {
				lower := strings.ToLower(last)
				var hints []string
				for _, v := range vs {
					sigWords := strings.Fields(v.sig)
					if len(sigWords) >= 2 && strings.HasPrefix(strings.ToLower(sigWords[1]), lower) {
						hints = append(hints, " "+dim(v.sig)+"  "+dim(v.desc))
					}
				}
				m.setHints(m.capHints(hints))
				return
			}
		}
		// Compound command: e.g. "/agent tool l" → base="/agent", mid="tool", typing="l"
		if len(words) >= 3 {
			base := words[len(words)-3]
			mid := words[len(words)-2]
			if isSlashCmdToken(base) && !isSlashCmdToken(mid) {
				if vs := cmdVariants[base]; len(vs) > 0 {
					lower := strings.ToLower(last)
					prefix := base + " " + mid + " "
					var hints []string
					for _, v := range vs {
						if !strings.HasPrefix(v.sig+" ", prefix) {
							continue
						}
						sigWords := strings.Fields(v.sig)
						if len(sigWords) >= 3 && strings.HasPrefix(strings.ToLower(sigWords[2]), lower) {
							hints = append(hints, " "+dim(v.sig)+"  "+dim(v.desc))
						}
					}
					if len(hints) > 0 {
						m.setHints(m.capHints(hints))
						return
					}
				}
			}
		}
	}

	// Trailing space after a known slash command: show all its subcommand hints.
	if len(beforeCursor) > 0 && (beforeCursor[len(beforeCursor)-1] == ' ' || beforeCursor[len(beforeCursor)-1] == '\t') {
		if len(words) >= 1 {
			prev := words[len(words)-1]
			if isSlashCmdToken(prev) {
				if vs := cmdVariants[prev]; len(vs) > 0 {
					var hints []string
					for _, v := range vs {
						hints = append(hints, " "+dim(v.sig)+"  "+dim(v.desc))
					}
					m.setHints(m.capHints(hints))
					return
				}
			}
			// Compound trailing space: e.g. "/agent tool " → base="/agent", sub="tool"
			if len(words) >= 2 {
				base := words[len(words)-2]
				sub := words[len(words)-1]
				if isSlashCmdToken(base) && !isSlashCmdToken(sub) {
					if vs := cmdVariants[base]; len(vs) > 0 {
						prefix := base + " " + sub + " "
						var hints []string
						for _, v := range vs {
							if strings.HasPrefix(v.sig+" ", prefix) {
								hints = append(hints, " "+dim(v.sig)+"  "+dim(v.desc))
							}
						}
						if len(hints) > 0 {
							m.setHints(m.capHints(hints))
							return
						}
					}
				}
			}
		}
	}

	m.setHints(nil)
}

const hintDebounceDelay = 120 * time.Millisecond

// scheduleHintRebuild increments the debounce generation counter and returns a
// tea.Cmd that fires after hintDebounceDelay. Only the most-recent firing
// (matching hintDebounceGen) triggers an actual rebuild; superseded ones drop.
func (m *model) scheduleHintRebuild() tea.Cmd {
	m.hintDebounceGen++
	gen := m.hintDebounceGen
	return tea.Tick(hintDebounceDelay, func(time.Time) tea.Msg {
		return hintDebounceMsg{gen: gen}
	})
}

// setHints replaces the hint list and resets the highlight. Both tabHints and
// tabHintsBase are updated together so highlightHint always has the styled
// base to restore non-active entries from.
func (m *model) setHints(hints []string) {
	changed := !stringSlicesEqual(m.tabHintsBase, hints)
	m.tabHintsBase = hints
	if len(hints) == 0 {
		m.tabHints = nil
		m.hintIdx = -1
		return
	}
	m.tabHints = make([]string, len(hints))
	copy(m.tabHints, hints)
	if changed {
		m.hintIdx = -1
	}
	m.highlightHint()
}

// highlightHint updates tabHints to visually highlight the entry at hintIdx
// without touching the textarea. Non-active entries are restored from
// tabHintsBase so their original styling (yellow, dim, etc.) is preserved.
func (m *model) highlightHint() {
	if m.hintIdx < 0 {
		// No selection — restore all entries from base.
		copy(m.tabHints, m.tabHintsBase)
		return
	}
	for i := range m.tabHints {
		if i == m.hintIdx {
			const bg = "\033[48;5;238m"
			if len(m.tabMatches) > 0 {
				// Tab-cycling: base already has yellow/boldYellow/dim styling — preserve
				// it and inject bg, re-applying after any full-reset mid-line.
				styled := strings.ReplaceAll(m.tabHintsBase[i], "\033[0m", "\033[0m"+bg)
				m.tabHints[i] = bg + styled + "\033[0m"
			} else {
				// Passive hints: base is dim — render selected entry as yellow on dark-grey.
				plain := ansi.Strip(m.tabHintsBase[i])
				m.tabHints[i] = bg + yellow(plain) + "\033[0m"
			}
		} else {
			m.tabHints[i] = m.tabHintsBase[i]
		}
	}
}

// commitHintSelection writes the currently highlighted inline hint into the
// textarea, replacing the @-path or /command token the user is typing.
// Returns true when a hint was committed so the caller can skip normal Tab/Enter
// handling.
func (m *model) commitHintSelection() bool {
	if m.hintIdx < 0 || m.hintIdx >= len(m.tabHints) {
		return false
	}
	full := strings.TrimSpace(ansi.Strip(m.tabHints[m.hintIdx]))
	// Slash-command hints are "sig  desc" — keep only the sig part.
	raw, _, _ := strings.Cut(full, "  ")
	raw = strings.TrimSpace(raw)

	fullInput := m.ta.Value()
	lines := strings.Split(fullInput, "\n")
	curLine := m.ta.Line()
	if curLine >= len(lines) {
		curLine = len(lines) - 1
	}
	li := m.ta.LineInfo()
	col := li.StartColumn + li.ColumnOffset // rune offset within logical line
	runes := []rune(lines[curLine])
	if col > len(runes) {
		col = len(runes)
	}
	beforeCursor := string(runes[:col])
	afterCursor := string(runes[col:])

	completed := applyTabCompletion(beforeCursor, raw)
	lines[curLine] = completed + afterCursor
	m.ta.SetValue(strings.Join(lines, "\n"))
	precedingLen := 0
	if curLine > 0 {
		precedingLen = len([]rune(strings.Join(lines[:curLine], "\n"))) + 1
	}
	m.ta.SetCursorColumn(precedingLen + len([]rune(completed)))

	m.tabHints = nil
	m.tabHintsBase = nil
	m.hintIdx = -1
	return true
}

// acceptActiveCompletion applies Enter's autocompletion semantics: accept the
// tab-cycled completion (tabMatches), or commit the arrow-key hint selection
// (hintIdx), then clear the first <param>/[opt] placeholder and park the cursor
// there so the user can type the value directly. Returns false when no
// completion is active (or the hint commit failed) so the caller falls through
// to normal Enter handling — submission on the idle path, the busy-state trap
// while an agent turn is running. Shared by handleKey and handleBusyKey so
// Enter accepts completions identically in both states: while busy, only
// Enter-as-submit is trapped, never Enter-as-accept-completion.
func (m model) acceptActiveCompletion() (model, bool) {
	if len(m.tabMatches) > 0 {
		// Tab cycling is active: accept the already-inserted completion.
		current := m.ta.Value()
		m.tabMatches = nil
		m.tabIdx = -1
		m.tabCmdIdx = 0
		m.tabVarIdx = 0
		m.tabBeforeCursor = ""
		m.tabAfterCursor = ""
		m.tabPrefix = ""
		m.tabSubcmdMode = false
		m.tabValueMode = false
		m.tabNsLabel = ""
		m.tabSegPrefix = ""
		m.tabHints = nil
		m.tabHintsBase = nil
		m.hintIdx = -1
		if cleared, pos := clearFirstPlaceholder(current); pos >= 0 {
			m.ta.SetValue(cleared)
			m.ta.SetCursorColumn(pos)
			m.rebuildInlineHints()
		}
		m.syncLayout()
		return m, true
	}
	if m.hintIdx >= 0 {
		if m.commitHintSelection() {
			// Same: position cursor at first placeholder if present.
			if cleared, pos := clearFirstPlaceholder(m.ta.Value()); pos >= 0 {
				m.ta.SetValue(cleared)
				m.ta.SetCursorColumn(pos)
				m.rebuildInlineHints()
			}
			m.syncLayout()
			return m, true
		}
		m.hintIdx = -1
	}
	return m, false
}

// handleTabKey applies Tab (dir=+1) / Shift+Tab (dir=-1) semantics: commit the
// highlighted hint selection when one is active (forward Tab only), otherwise
// cycle through completions. Shared by handleKey and handleBusyKey — tab
// completion only edits the input buffer, so it works while an agent turn is
// running too (only Enter-as-submit is trapped there).
func (m model) handleTabKey(dir int) model {
	if dir > 0 && m.hintIdx >= 0 && len(m.tabMatches) == 0 {
		m.commitHintSelection()
		m.syncLayout()
		return m
	}
	m = m.handleTab(dir)
	m.syncLayout()
	return m
}

// clearFirstPlaceholder finds the first <param> or [opt] token that follows a
// slash-command word in s, strips it and everything after it on that line, and
// returns (result, runeOffset) where runeOffset is the cursor position to set.
// Returns ("", -1) when no placeholder is found so callers can skip SetValue.
func clearFirstPlaceholder(s string) (string, int) {
	lines := strings.Split(s, "\n")
	for li, line := range lines {
		foundCmd := false
		i := 0
		for i < len(line) {
			// skip whitespace
			j := i
			for j < len(line) && (line[j] == ' ' || line[j] == '\t') {
				j++
			}
			if j >= len(line) {
				break
			}
			// find token end
			k := j
			for k < len(line) && line[k] != ' ' && line[k] != '\t' {
				k++
			}
			token := line[j:k]
			if strings.HasPrefix(token, "/") {
				foundCmd = true
			} else if foundCmd && (strings.HasPrefix(token, "<") || strings.HasPrefix(token, "[")) {
				// Truncate this line just before the placeholder, keep a trailing space.
				prefix := strings.TrimRight(line[:j], " \t") + " "
				lines[li] = prefix
				runesBefore := 0
				for _, l := range lines[:li] {
					runesBefore += len([]rune(l)) + 1 // +1 for '\n'
				}
				return strings.Join(lines, "\n"), runesBefore + len([]rune(prefix))
			}
			i = k
		}
	}
	return "", -1
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// capHints trims hints to at most half the current viewport height.
func (m *model) capHints(hints []string) []string {
	max := m.viewportHeight() / 2
	if max < 1 {
		max = 1
	}
	if len(hints) > max {
		return hints[:max]
	}
	return hints
}

// tabBuild is the result of buildTabMatches: what to complete, and how.
type tabBuild struct {
	matches     []string // candidate completions (sigs, values, or commands)
	replaceBase string   // non-empty → subcommand mode: replace this slash token with the full sig
	valueMode   bool     // matches are concrete parameter values (#166): replace/append last word
	nsLabel     string   // name-space shown beside value hints (value mode only)
	segPrefix   string   // multi-value (#165): keep this prefix (through the last comma) when inserting a value
}

// paramLookup resolves a parameter name-space to its values (see
// namespaces.go); nil means namespace completion is unavailable.
type paramLookup func(ns string) []string

// buildTabMatches returns the completion candidates for input (the current
// line up to the cursor) plus how to apply them.
// replaceBase is the portion of input that should be used as the snapshot for
// applyTabCompletion: normally "" (caller uses beforeCursor as-is), but set to
// the slash-command token (e.g. "/memory") for subcommand completions so that
// the whole "/cmd sub…" sequence is replaced in one step. valueMode marks
// concrete parameter-value completions (namespaces.go).
func buildTabMatches(input, cwd string, lookup paramLookup) tabBuild {
	words := strings.Fields(input)
	if len(words) == 0 {
		return tabBuild{}
	}

	// Trailing whitespace after a known slash command → subcommand listing.
	// e.g. "/memory " → list all /memory variants.
	trailingSpace := input != "" && (input[len(input)-1] == ' ' || input[len(input)-1] == '\t')
	if trailingSpace {
		last := words[len(words)-1]
		if isSlashCmdToken(last) {
			if vs := cmdVariants[last]; len(vs) > 0 {
				sigs := make([]string, len(vs))
				for i, v := range vs {
					sigs[i] = v.sig
				}
				return tabBuild{matches: sigs, replaceBase: last}
			}
		}
		// Trailing space after completed words → parameter values at the next
		// position, e.g. "/agent switch " completes agent names.
		return buildParamMatches(words, true, lookup)
	}

	// Only complete the last word — the token the cursor is actively on.
	last := words[len(words)-1]
	if strings.HasPrefix(last, "@") {
		const tabCompletionLimit = 50
		// A Tab press may wait briefly for the first index build, so Tab right
		// after startup still completes from the full candidate list.
		matches := atPathMatches(last[1:], cwd, tabCompletionLimit, fileIndexTabWait)
		atMatches := make([]string, len(matches))
		for j, p := range matches {
			atMatches[j] = "@" + p
		}
		return tabBuild{matches: atMatches}
	}

	// Partial subcommand: cursor is on a non-slash token and the preceding word
	// is a known slash command with variants. e.g. "/memory sh" → filter to sigs
	// whose subcommand portion starts with "sh".
	if !isSlashCmdToken(last) {
		if len(words) >= 2 {
			prev := words[len(words)-2]
			if isSlashCmdToken(prev) {
				if vs := cmdVariants[prev]; len(vs) > 0 {
					var sigs []string
					lower := strings.ToLower(last)
					for _, v := range vs {
						// v.sig is "/cmd sub …" — compare the word after the command.
						sigWords := strings.Fields(v.sig)
						if len(sigWords) >= 2 && strings.HasPrefix(strings.ToLower(sigWords[1]), lower) {
							sigs = append(sigs, v.sig)
						}
					}
					if len(sigs) > 0 {
						return tabBuild{matches: sigs, replaceBase: prev}
					}
				}
			}
		}
		// Deeper positions and parameter placeholders: complete concrete
		// values from the name-space index (#166), e.g. "/agent tool l" →
		// "list", "/agent switch cl" → an agent name.
		return buildParamMatches(words, false, lookup)
	}

	// Top-level slash command prefix completion. e.g. "/mem" → ["/memory", …]
	var matches []string
	for _, cmd := range slashCommands {
		if strings.HasPrefix(strings.ToLower(cmd), strings.ToLower(last)) {
			matches = append(matches, cmd)
		}
	}
	return tabBuild{matches: matches}
}
