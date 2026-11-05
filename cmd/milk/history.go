package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/scoutme/milk/internal/config"
)

const maxPersistedHistory = 500

func globalHistoryPath() (string, error) {
	base, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "input_history"), nil
}

func sessionHistoryPath(sessID string) (string, error) {
	base, err := config.Dir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, sessID+".history"), nil
}

func readHistoryFile(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20) // 1 MiB — accommodate long multi-line entries
	for sc.Scan() {
		if line := sc.Text(); line != "" {
			lines = append(lines, strings.ReplaceAll(line, `\n`, "\n"))
		}
	}
	return lines
}

func writeHistoryFile(path string, history []string) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	start := 0
	if len(history) > maxPersistedHistory {
		start = len(history) - maxPersistedHistory
	}
	for _, entry := range history[start:] {
		fmt.Fprintln(w, strings.ReplaceAll(entry, "\n", `\n`))
	}
	w.Flush() //nolint:errcheck
}

// activeHistory returns the slice used for navigation (session or global).
// The corpus follows the input mode: bang-mode ("!") input is shell input,
// so it navigates and searches the shell's history (plus milk's record of
// its own executed commands) instead of milk's input history.
func (m *model) activeHistory() []string {
	if m.bangMode {
		return m.shellHistoryLines()
	}
	if m.useGlobalHistory {
		return m.globalHistory
	}
	return m.sessionHistory
}

// historySearchLabel renders the Ctrl+R/Ctrl+S prompt label. In bang mode it
// names the shell corpus so the source of a match is never ambiguous.
func historySearchLabel(bangMode, forward bool) string {
	dir := "r"
	if forward {
		dir = "f"
	}
	if bangMode {
		return yellow("("+dir+"-search:shell)") + " ❯ "
	}
	return yellow("("+dir+"-search)") + " ❯ "
}

func appendDeduped(h []string, entry string, max int) []string {
	if len(h) > 0 && h[len(h)-1] == entry {
		return h
	}
	h = append(h, entry)
	if len(h) > max {
		h = h[1:]
	}
	return h
}

func (m model) historyBack() model {
	h := m.activeHistory()
	if len(h) == 0 {
		return m
	}
	if m.histIdx == -1 {
		m.saved = m.ta.Value()
		m.savedLeadingPasted = m.leadingPasted
		m.histIdx = len(h) - 1
	} else if m.histIdx > 0 {
		m.histIdx--
	}
	m.ta.SetValue(h[m.histIdx])
	m.leadingPasted = false
	m.hintIdx = -1
	m.tabHints = nil
	m.tabHintsBase = nil
	m.taClearSel()
	return m
}

func (m model) historyForward() model {
	h := m.activeHistory()
	if m.histIdx == -1 {
		return m
	}
	m.histIdx++
	if m.histIdx >= len(h) {
		m.histIdx = -1
		m.ta.SetValue(m.saved)
		m.leadingPasted = m.savedLeadingPasted
		m.hintIdx = -1
		m.tabHints = nil
		m.tabHintsBase = nil
		m.taClearSel()
		return m
	}
	m.ta.SetValue(h[m.histIdx])
	m.leadingPasted = false
	m.hintIdx = -1
	m.tabHints = nil
	m.tabHintsBase = nil
	m.taClearSel()
	return m
}

// searchBack returns the index of the nearest match for q in h, searching
// backwards from start (exclusive). Returns -1 if no match found.
func searchBack(h []string, q string, start int) int {
	if start < 0 {
		start = len(h)
	}
	for i := start - 1; i >= 0; i-- {
		if strings.Contains(h[i], q) {
			return i
		}
	}
	return -1
}

// searchForward returns the index of the nearest match for q in h, searching
// forwards from start (exclusive). Returns -1 if no match found.
func searchForward(h []string, q string, start int) int {
	from := start + 1
	if start < 0 {
		from = 0
	}
	for i := from; i < len(h); i++ {
		if strings.Contains(h[i], q) {
			return i
		}
	}
	return -1
}

// historySearchBack finds the most recent entry in activeHistory() that contains
// searchQuery, starting from searchIdx-1 (or end if searchIdx==-1).
func (m model) historySearchBack() model {
	h := m.activeHistory()
	if len(h) == 0 || m.searchQuery.Len() == 0 {
		return m
	}
	if idx := searchBack(h, m.searchQuery.String(), m.searchIdx); idx >= 0 {
		m.searchIdx = idx
		m.ta.SetValue(h[idx])
		m.leadingPasted = false
	}
	return m
}

// historySearchForward finds the next (newer) entry in activeHistory() that
// contains searchQuery, starting from searchIdx+1.
func (m model) historySearchForward() model {
	h := m.activeHistory()
	if len(h) == 0 || m.searchQuery.Len() == 0 {
		return m
	}
	if idx := searchForward(h, m.searchQuery.String(), m.searchIdx); idx >= 0 {
		m.searchIdx = idx
		m.ta.SetValue(h[idx])
		m.leadingPasted = false
	}
	return m
}
