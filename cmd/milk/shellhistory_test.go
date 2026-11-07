package main

// Tests for the bang-mode shell-history corpus (corpus follows input mode).
// The real-incantation tests drive the actual shells where installed — the
// output-shape rules (bash's tab-space indent, bare zsh lines, timestamp
// marker leaks) are pinned against real binaries, not assumptions.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/scoutme/milk/internal/testutil"
)

func TestShellHistoryCmd_Resolution(t *testing.T) {
	tests := []struct {
		name         string
		shell        string
		override     string
		wantContains string
		wantErr      bool
	}{
		{"bash", "/bin/bash", "", "history -r", false},
		{"zsh", "/usr/bin/zsh", "", "fc -R", false},
		{"fish", "/usr/local/bin/fish", "", "fish -c history", false},
		{"unknown shell errors", "/bin/dash", "", "", true},
		{"empty shell errors", "", "", "", true},
		{"override wins verbatim", "/bin/dash", "atuin history list --cmd-only", "atuin history list", false},
		{"whitespace override falls through", "/bin/dash", "   ", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := shellHistoryCmd(tt.shell, tt.override)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.Contains(got, tt.wantContains) {
				t.Errorf("cmd %q does not contain %q", got, tt.wantContains)
			}
		})
	}
}

func TestDetectHistfile(t *testing.T) {
	home := testutil.SandboxHome(t)
	t.Setenv("HOME", home)
	t.Setenv("HISTFILE", "")
	t.Setenv("XDG_DATA_HOME", "")

	t.Run("HISTFILE env wins", func(t *testing.T) {
		t.Setenv("HISTFILE", "/x/custom_hist")
		if got := detectHistfile("/bin/zsh"); got != "/x/custom_hist" {
			t.Errorf("got %q, want /x/custom_hist", got)
		}
	})
	t.Run("zsh default", func(t *testing.T) {
		t.Setenv("HISTFILE", "")
		if got, want := detectHistfile("/bin/zsh"), filepath.Join(home, ".zsh_history"); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("bash default and fallback", func(t *testing.T) {
		t.Setenv("HISTFILE", "")
		want := filepath.Join(home, ".bash_history")
		if got := detectHistfile("/bin/bash"); got != want {
			t.Errorf("bash: got %q, want %q", got, want)
		}
		if got := detectHistfile("/bin/dash"); got != want {
			t.Errorf("fallback: got %q, want %q", got, want)
		}
	})
	t.Run("fish XDG", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", "/xdg")
		if got, want := detectHistfile("/usr/bin/fish"), "/xdg/fish/fish_history"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
		t.Setenv("XDG_DATA_HOME", "")
		if got, want := detectHistfile("/usr/bin/fish"), filepath.Join(home, ".local", "share", "fish", "fish_history"); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}

func TestNormalizeShellHistory(t *testing.T) {
	raw := "\t echo one\n" + // bash fc -l -n indent (tab+space)
		"\techo two\n" + // tab-only indent
		"bare cmd\n" +
		"#1700000001\n" + // leaked timestamp marker: metadata, never a command
		"42 do stuff\n" + // digit-leading command must survive verbatim
		"\n" + // empty lines dropped
		"windows\r\n" // CRLF tolerated
	want := []string{"echo one", "echo two", "bare cmd", "42 do stuff", "windows"}
	if got := normalizeShellHistory(raw); !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

// TestRunShellHistory_BashFixture drives the real bash incantation over
// fixture histfiles — the exact shapes probed when designing this feature:
// timestamped entries parse clean; a mixed file leaks the "#<epoch>" marker
// through bash's own reader (and normalizeShellHistory drops it).
func TestRunShellHistory_BashFixture(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
	t.Setenv("SHELL", "/bin/bash")
	t.Setenv("HISTFILE", filepath.Join(t.TempDir(), "hist"))
	t.Setenv("HISTTIMEFORMAT", "%F ") // exported leak: must be neutralized

	script, err := shellHistoryCmd("/bin/bash", "")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("timestamped histfile", func(t *testing.T) {
		hf := filepath.Join(t.TempDir(), "hist")
		writeFile(t, hf, "#1700000000\necho one\n#1700000001\necho two\necho three \"quoted\" args\n")
		got, err := runShellHistory(script, hf)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"echo one", "echo two", `echo three "quoted" args`}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %#v, want %#v", got, want)
		}
	})

	t.Run("mixed file leaks marker which normalize drops", func(t *testing.T) {
		hf := filepath.Join(t.TempDir(), "hist")
		writeFile(t, hf, "echo one\n#1700000001\necho multi\nline-two\n")
		got, err := runShellHistory(script, hf)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"echo one", "echo multi", "line-two"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %#v, want %#v", got, want)
		}
	})
}

func TestShellHistory_MergeOrder(t *testing.T) {
	c := &shellHistCache{lines: []string{"a", "b"}, milk: []string{"c"}}
	want := []string{"a", "b", "c"} // shell first (oldest→newest), milk's runs last
	if got := c.merged(); !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

func TestActiveHistory_FollowsInputMode(t *testing.T) {
	m := newTestModelForPanels(t)
	m.sessionHistory = []string{"sess-a"}
	m.globalHistory = []string{"glob-a"}
	m.shellHist = &shellHistCache{
		loaded: true, milkLoaded: true,
		lines: []string{"sh-1"}, milk: []string{"mk-1"},
	}

	m.bangMode = true
	if got := m.activeHistory(); !reflect.DeepEqual(got, []string{"sh-1", "mk-1"}) {
		t.Errorf("bang mode: got %#v", got)
	}
	m.bangMode = false
	if got := m.activeHistory(); !reflect.DeepEqual(got, []string{"sess-a"}) {
		t.Errorf("session mode: got %#v", got)
	}
	m.useGlobalHistory = true
	if got := m.activeHistory(); !reflect.DeepEqual(got, []string{"glob-a"}) {
		t.Errorf("global mode: got %#v", got)
	}
}

func TestBangModeHistory_NavAndSearchWalkShellCorpus(t *testing.T) {
	m := newTestModelForPanels(t)
	m.sessionHistory = []string{"sess-a"}
	m.shellHist = &shellHistCache{
		loaded: true, milkLoaded: true,
		lines: []string{"make test", "git push"}, milk: []string{"deploy now"},
	}
	m.bangMode = true
	m.histIdx = -1

	m = m.historyBack()
	if got := m.ta.Value(); got != "deploy now" {
		t.Fatalf("first back: got %q, want milk's most recent", got)
	}
	m = m.historyBack()
	if got := m.ta.Value(); got != "git push" {
		t.Fatalf("second back: got %q, want shell entry", got)
	}

	// Ctrl+R search hits the shell corpus too.
	m.searchIdx = -1
	m.searchQuery.Reset()
	m.searchQuery.WriteString("git")
	m = m.historySearchBack()
	if got := m.ta.Value(); got != "git push" {
		t.Fatalf("search: got %q, want git push", got)
	}

	// Out of bang mode the same keys walk milk's input history.
	m.bangMode = false
	m.histIdx = -1
	m = m.historyBack()
	if got := m.ta.Value(); got != "sess-a" {
		t.Fatalf("normal mode back: got %q, want sess-a", got)
	}
}

func TestRecordShellCommand_PersistsAndReloads(t *testing.T) {
	m := newTestModelForPanels(t)  // isolates HOME → shell_history lives in tmp
	t.Setenv("SHELL", "/bin/dash") // no incantation: deterministic, no subprocess

	m.recordShellCommand("make test")
	m.recordShellCommand("make test") // consecutive dedup
	m.recordShellCommand("deploy now")

	p, err := shellHistoryPath()
	if err != nil {
		t.Fatal(err)
	}
	if got := readHistoryFile(p); !reflect.DeepEqual(got, []string{"make test", "deploy now"}) {
		t.Fatalf("file: got %#v", got)
	}

	// A fresh cache (restart semantics) reads the commands back from disk.
	m.shellHist = &shellHistCache{}
	m.bangMode = true
	got := m.activeHistory()
	if !reflect.DeepEqual(got, []string{"make test", "deploy now"}) {
		t.Fatalf("reload: got %#v", got)
	}
}

func TestHistorySearchLabel_NamesShellCorpus(t *testing.T) {
	if got := historySearchLabel(true, false); !strings.Contains(got, "(r-search:shell)") {
		t.Errorf("bang reverse: got %q", got)
	}
	if got := historySearchLabel(true, true); !strings.Contains(got, "(f-search:shell)") {
		t.Errorf("bang forward: got %q", got)
	}
	if got := historySearchLabel(false, true); !strings.Contains(got, "(f-search)") || strings.Contains(got, ":shell") {
		t.Errorf("normal forward: got %q", got)
	}
}

// TestBangModeTransitions_ResetHistoryIndex pins the corpus-swap invariant:
// leaving bang mode (or entering it) invalidates the history index, so no
// index built against one corpus ever addresses the other.
func TestBangModeTransitions_ResetHistoryIndex(t *testing.T) {
	t.Run("submit runs and records the command", func(t *testing.T) {
		m := newTestModelForPanels(t)
		m.bangMode = true
		m.histIdx = 2
		m.ta.SetValue("echo hi")
		next, _ := m.handleEnter()
		nm := next.(model)
		t.Cleanup(func() {
			if nm.ptyPane != nil {
				_ = nm.ptyPane.cmd.Process.Kill()
				_, _ = nm.ptyPane.cmd.Process.Wait()
			}
		})
		if nm.bangMode {
			t.Error("bang mode should be off after submit")
		}
		if nm.histIdx != -1 {
			t.Errorf("histIdx = %d, want -1", nm.histIdx)
		}
		p, err := shellHistoryPath()
		if err != nil {
			t.Fatal(err)
		}
		if got := readHistoryFile(p); !reflect.DeepEqual(got, []string{"echo hi"}) {
			t.Errorf("recorded: got %#v", got)
		}
	})

	t.Run("backspace exit resets", func(t *testing.T) {
		m := newTestModelForPanels(t)
		m.bangMode = true
		m.histIdx = 3
		next, _ := m.handleKey(tea.KeyPressMsg{Code: tea.KeyBackspace})
		nm := next.(model)
		if nm.bangMode {
			t.Error("bang mode should be off")
		}
		if nm.histIdx != -1 {
			t.Errorf("histIdx = %d, want -1", nm.histIdx)
		}
	})
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRunShellHistory_FullWindow_BeyondLastN pins the load-bearing listing
// ranges. `fc -l`/`history` without an explicit range print only the last
// ~16-17 events, and zsh loads at most HISTSIZE events from the histfile
// (default 30) — under those caps the corpus silently truncated to the
// newest handful (a real 23k-line histfile came back as 17 lines, which is
// why bang-mode Ctrl+R had "almost nothing to search"). The original
// fixtures below sit at 3-5 entries — under every cap — which is exactly
// how this stayed invisible in CI. 40 crosses all three caps.
func TestRunShellHistory_FullWindow_BeyondLastN(t *testing.T) {
	const n = 40

	if _, err := exec.LookPath("bash"); err == nil {
		t.Run("bash", func(t *testing.T) {
			hf := filepath.Join(t.TempDir(), "hist")
			var b strings.Builder
			for i := 1; i <= n; i++ {
				fmt.Fprintf(&b, "cmd entry %02d\n", i)
			}
			if err := os.WriteFile(hf, []byte(b.String()), 0o600); err != nil {
				t.Fatal(err)
			}
			script, err := shellHistoryCmd("/bin/bash", "")
			if err != nil {
				t.Fatal(err)
			}
			got, err := runShellHistory(script, hf)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != n {
				t.Fatalf("got %d entries, want %d — listing fell back to the last-N default", len(got), n)
			}
			if got[0] != "cmd entry 01" || got[n-1] != "cmd entry 40" {
				t.Errorf("order wrong: first=%q last=%q", got[0], got[n-1])
			}
		})
	}

	if _, err := exec.LookPath("zsh"); err == nil {
		t.Run("zsh", func(t *testing.T) {
			hf := filepath.Join(t.TempDir(), "hist")
			var b strings.Builder
			for i := 1; i <= n; i++ {
				fmt.Fprintf(&b, ": %d:0;cmd entry %02d\n", 1700000000+i, i)
			}
			if err := os.WriteFile(hf, []byte(b.String()), 0o600); err != nil {
				t.Fatal(err)
			}
			script, err := shellHistoryCmd("/usr/bin/zsh", "")
			if err != nil {
				t.Fatal(err)
			}
			got, err := runShellHistory(script, hf)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != n {
				t.Fatalf("got %d entries, want %d — HISTSIZE/load or listing range regressed", len(got), n)
			}
			if got[0] != "cmd entry 01" || got[n-1] != "cmd entry 40" {
				t.Errorf("order wrong: first=%q last=%q", got[0], got[n-1])
			}
		})
	}
}

// TestRunShellHistory_EmptyHistfile — "no history yet" must surface as an
// empty corpus with no error (the hint says "nothing recorded"), never as a
// dump failure: zsh's `fc -l -n 1` exits 1 when no events exist, which the
// template's `|| true` absorbs; bash's clamped range already exits 0.
func TestRunShellHistory_EmptyHistfile(t *testing.T) {
	shells := []struct {
		name string
		path string
	}{
		{"bash", "/bin/bash"},
		{"zsh", "/usr/bin/zsh"},
	}
	for _, sh := range shells {
		if _, err := exec.LookPath(sh.path); err != nil {
			continue
		}
		t.Run(sh.name, func(t *testing.T) {
			hf := filepath.Join(t.TempDir(), "hist")
			if err := os.WriteFile(hf, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			script, err := shellHistoryCmd(sh.path, "")
			if err != nil {
				t.Fatal(err)
			}
			got, err := runShellHistory(script, hf)
			if err != nil {
				t.Fatalf("empty histfile must not be a dump failure: %v", err)
			}
			if len(got) != 0 {
				t.Errorf("got %d entries from an empty file, want 0: %q", len(got), got)
			}
		})
	}
}

// TestNormalizeHistoryRecords_ValueStream pins record-mode: a 0x1e-separated
// stream yields one record per history EVENT — multi-line values intact,
// genuine fragment events (values that start with whitespace, as zsh itself
// stores them) preserved verbatim, and no continuation line ever becoming a
// standalone "command".
func TestNormalizeHistoryRecords_ValueStream(t *testing.T) {
	raw := "git commit -m \"line one\nline two continued\"\x1e\nrun tool\x1e\n indented fragment\x1e\n"
	want := []string{
		"git commit -m \"line one\nline two continued\"",
		"run tool",
		" indented fragment",
	}
	got := normalizeShellHistory(raw)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("records = %#v, want %#v", got, want)
	}

	if got := normalizeHistoryRecords(""); len(got) != 0 {
		t.Errorf("empty stream → %#v, want empty", got)
	}
	if got := normalizeHistoryRecords("\x1e\n"); len(got) != 0 {
		t.Errorf("separator-only stream → %#v, want empty", got)
	}
	// No separator: line mode still applies (bash/fish/overrides).
	if got := normalizeShellHistory("a\nb"); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("line mode broke: %#v", got)
	}
}
