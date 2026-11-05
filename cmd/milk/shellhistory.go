package main

// Shell history for bang-mode input ("!" direct-execution).
//
// The corpus rule: the history corpus follows the input mode. While the input
// area is in bang mode, Up/Down and Ctrl+R/Ctrl+S navigate and search the
// user's *shell* history instead of milk's input history — a shell command
// deserves shell completion semantics. All histfile format knowledge (zsh's
// extended ": <start>:<elapsed>;" entries, bash timestamp markers, fish's
// YAML-ish file) stays inside the shells themselves: milk runs the shell's
// own load-then-list incantation (shellHistoryCmd) and normalizes only the
// listing output. Exotic setups (atuin, nushell, custom HISTFILE) plug in
// through the shell_history_cmd config override instead of a new parser.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/scoutme/milk/internal/config"
)

// shellHistoryCmd resolves the history-dump incantation for the user's
// shell. The templates deliberately run rc-free (bash -c, zsh -f): user rc
// files cannot alias the output or slow the dump down, and the listing shape
// stays deterministic. The listing must enumerate the shell's WHOLE loaded
// window: `fc -l`/`history` without an explicit range only print the last
// ~16-17 events, and zsh additionally loads at most HISTSIZE events
// (default 30) from the histfile. Without HISTSIZE + a range, the corpus
// silently truncated to the newest handful — a 23k-line histfile came back
// as 17 lines, which is why bang-mode Ctrl+R had "almost nothing to search"
// (small test fixtures sat under both caps and hid it). HISTTIMEFORMAT=
// neutralizes an exported timestamp format that would otherwise corrupt the
// listing; the zsh listing tolerates an empty histfile (`fc -l -n 1` errors
// rc=1 with no events — hence `|| true`, so "no history yet" is an empty
// corpus, not a dump failure). Each template prints bare commands, one per
// line, on stdout (modulo the prefix normalizeShellHistory strips).
func shellHistoryCmd(shellPath, override string) (string, error) {
	if strings.TrimSpace(override) != "" {
		return override, nil
	}
	switch filepath.Base(shellPath) {
	case "bash":
		// HISTSIZE= must be set before history -r (it caps how many events
		// load); the range then lists them all (out-of-range upper bound
		// clamps, rc=0 even on an empty file). Flat line-per-entry output
		// (bash's default cmdhist joins multi-line commands at entry time).
		return `HISTSIZE=100000 HISTTIMEFORMAT= bash -c 'history -r "$HISTFILE"; fc -l -n 1 999999999'`, nil
	case "zsh":
		// HISTSIZE= overrides zsh's 30-event load default. The listing is a
		// VALUE stream, not a line listing: every event is printed as its
		// exact value followed by a 0x1e byte — one record per history
		// event with multi-line commands intact. A flat `fc -l` listing
		// explodes multi-line entries into per-line rows, and line-based
		// normalization then shows each continuation line as a standalone
		// "command" prefixed with a tab/space (the fragment garbage seen
		// in bang-mode search). Events are enumerated by SUBSCRIPT over
		// 1..lines(fc -l) — the keys enumeration ${(onk)history} proved
		// to drop events (a 40-event fixture listed 39 keys while
		// $history[40] resolved), and fc -l's line count is a guaranteed
		// superset of the event count (every event spans ≥1 line). Fresh
		// loads number events contiguously from 1, so direct subscripts
		// cover them. The '\'' dance embeds $'\x1e' inside the sh-quoted
		// zsh -c string; rc stays 0 on an empty histfile.
		return `HISTSIZE=100000 zsh -f -c 'fc -R $HISTFILE 2>/dev/null; n=$(fc -l -n 1 2>/dev/null | wc -l); for ((i = 1; i <= n; i ++)) do v=$history[$i]; [ -n "$v" ] && print -r -- "$v"$'\''\x1e'\''; done'`, nil
	case "fish":
		return `fish -c history`, nil
	}
	return "", fmt.Errorf("no history incantation for shell %q (set shell_history_cmd)", filepath.Base(shellPath))
}

// detectHistfile resolves the interactive shell's history file: $HISTFILE
// wins when set, then a per-shell default, then bash's. Used for the
// incantation's HISTFILE env and for the mtime that gates cache refreshes.
func detectHistfile(shellPath string) string {
	if hf := os.Getenv("HISTFILE"); hf != "" {
		return hf
	}
	home, _ := os.UserHomeDir()
	switch filepath.Base(shellPath) {
	case "zsh":
		return filepath.Join(home, ".zsh_history")
	case "fish":
		if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
			return filepath.Join(xdg, "fish", "fish_history")
		}
		return filepath.Join(home, ".local", "share", "fish", "fish_history")
	}
	return filepath.Join(home, ".bash_history")
}

// shellHistTimestampLine matches bash timestamp markers ("#<epoch>") that
// some builds leak through a mixed histfile into the listing — metadata,
// never commands.
var shellHistTimestampLine = regexp.MustCompile(`^#\d+$`)

// normalizeShellHistory turns a history-dump listing into corpus records.
// Two stream shapes are recognized:
//
//   - 0x1e-separated value stream (the zsh template): one record per
//     history event — split on the separator, keep the value verbatim
//     (multi-line commands stay whole), strip only the newline print(1)
//     appends after each record. Line-splitting this stream would fabricate
//     standalone "commands" out of every multi-line entry's continuation
//     lines — the tab/space-prefixed fragment garbage seen in bang-mode
//     search. Values are kept exactly as zsh stores them (including the
//     rare fragment events zsh itself stores) — the shells stay the single
//     source of format truth.
//   - line-per-entry listing (bash, fish, shell_history_cmd overrides):
//     bash's fc -l -n indents every row ("\t "), some builds leak
//     "#<epoch>" timestamp markers — both are stripped/dropped. Number
//     prefixes are deliberately NOT stripped — "42 do" is a valid command —
//     so an override must emit bare lines (or 0x1e-separated records).
func normalizeShellHistory(raw string) []string {
	if strings.Contains(raw, "\x1e") {
		return normalizeHistoryRecords(raw)
	}
	parts := strings.Split(raw, "\n")
	out := make([]string, 0, len(parts))
	for _, line := range parts {
		line = strings.TrimSuffix(line, "\r")
		// Strip the listing indent first: bash fc -l -n indents every row, and
		// a leaked timestamp marker arrives as "\t #1700000001" — the marker
		// check below must see the bare line.
		line = strings.TrimPrefix(line, "\t ")
		line = strings.TrimPrefix(line, "\t")
		if shellHistTimestampLine.MatchString(line) {
			continue
		}
		if line == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}

// normalizeHistoryRecords splits a 0x1e-separated value stream into records:
// the dump prints value+0x1e+"\n" per event, so every record after the first
// carries the previous record's trailing newline as a one-char prefix.
// Interior newlines belong to the value and are kept.
func normalizeHistoryRecords(raw string) []string {
	recs := strings.Split(raw, "\x1e")
	out := make([]string, 0, len(recs))
	for i, rec := range recs {
		if i > 0 {
			rec = strings.TrimPrefix(rec, "\n")
		}
		rec = strings.TrimSuffix(rec, "\r")
		if rec == "" {
			continue
		}
		out = append(out, rec)
	}
	return out
}

// runShellHistory executes a history-dump incantation and normalizes its
// output. Bounded at 2s: this runs on the keystroke path (first history
// access in bang mode) and must never hang the input loop.
func runShellHistory(script, histfile string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	// Explicit, dedup'd env: a user-exported HISTFILE/HISTTIMEFORMAT must not
	// shadow ours — getenv semantics would hand the shell the stale first hit.
	env := make([]string, 0, len(os.Environ())+2)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "HISTFILE=") || strings.HasPrefix(kv, "HISTTIMEFORMAT=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "HISTFILE="+histfile, "HISTTIMEFORMAT=")
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("history dump: %w", err)
	}
	return normalizeShellHistory(string(out)), nil
}

// shellHistoryPath is milk's own record of the commands it executed on the
// user's behalf (bang-mode and allow-listed direct-bash runs). One-shot
// sh -c runs never land in the interactive shell's histfile — and milk must
// not write there either: appending would race the live shell's own
// rewrites and corrupt zsh's extended format.
func shellHistoryPath() (string, error) {
	base, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "shell_history"), nil
}

// shellHistCache caches the bang-mode history corpus. Accessed only from the
// Update goroutine; the model holds it as a pointer so the value-receiverkey
// handlers (historyBack and friends) share one cache across model copies.
type shellHistCache struct {
	loaded     bool
	mtime      time.Time
	mtimeOK    bool
	lines      []string // normalized dump of the user's shell history
	milk       []string // commands milk itself executed
	milkLoaded bool
	loadErr    string
	hinted     bool
}

// merged returns the bang-mode corpus: the user's shell history first
// (oldest→newest as the shell lists it), then milk's own executed commands —
// so reverse search (Ctrl+R) sees milk's most recent run first.
func (c *shellHistCache) merged() []string {
	out := make([]string, 0, len(c.lines)+len(c.milk))
	out = append(out, c.lines...)
	out = append(out, c.milk...)
	return out
}

// shellHistoryLines lazily loads and returns the bang-mode corpus,
// refreshing when the histfile's mtime changes (a live shell flushing new
// entries). An empty corpus surfaces one dim hint and then stays silent — a
// missing shell history must never block input.
func (m *model) shellHistoryLines() []string {
	c := m.shellHist
	if c == nil {
		// Safety net for models built without newModel: the cache pointer is
		// what makes mutations survive value-receiver copies.
		c = &shellHistCache{}
		m.shellHist = c
	}
	m.loadMilkShellHistory(c)
	shellPath := os.Getenv("SHELL")
	hf := detectHistfile(shellPath)
	if fi, err := os.Stat(hf); err == nil {
		if c.loaded && c.mtimeOK && fi.ModTime().Equal(c.mtime) {
			return c.merged()
		}
		c.mtime, c.mtimeOK = fi.ModTime(), true
	} else if c.loaded {
		return c.merged()
	}
	script, err := shellHistoryCmd(shellPath, m.st.cfg.ShellHistoryCmd)
	if err != nil {
		c.loaded, c.loadErr = true, err.Error()
	} else if lines, lerr := runShellHistory(script, hf); lerr != nil {
		c.loaded, c.loadErr = true, lerr.Error()
	} else {
		c.loaded, c.loadErr, c.lines = true, "", lines
	}
	merged := c.merged()
	if len(merged) == 0 && !c.hinted {
		c.hinted = true
		reason := "nothing recorded"
		if c.loadErr != "" {
			reason = c.loadErr
		}
		m.appendTranscript(milkTag() + dim(" no shell history: "+reason+"\n"))
	}
	return merged
}

// loadMilkShellHistory seeds the cache's milk-side entries from disk once.
func (m *model) loadMilkShellHistory(c *shellHistCache) {
	if c.milkLoaded {
		return
	}
	c.milkLoaded = true
	if p, err := shellHistoryPath(); err == nil {
		c.milk = readHistoryFile(p)
	}
}

// recordShellCommand appends a command milk executed on the user's behalfto milk's own shell history (file + in-memory corpus), so bang-mode
// Ctrl+R finds it later even though no interactive shell ever saw it.
func (m *model) recordShellCommand(shellCmd string) {
	shellCmd = strings.TrimSpace(shellCmd)
	if shellCmd == "" {
		return
	}
	c := m.shellHist
	if c == nil {
		c = &shellHistCache{}
		m.shellHist = c
	}
	m.loadMilkShellHistory(c)
	c.milk = appendDeduped(c.milk, shellCmd, maxPersistedHistory)
	if p, err := shellHistoryPath(); err == nil {
		writeHistoryFile(p, c.milk)
	}
}
