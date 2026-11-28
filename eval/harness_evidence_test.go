package eval

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDiffSnapshots_DeletionRecorded: a deleted file must be recorded with an
// explicit Deleted marker — the judge template keys on the flags, and before
// this flag existed deletions matched neither the IsNew nor the Modified
// branch and were rendered as nothing at all (the judges literally scored
// "no deletion" because the evidence section omitted it).
func TestDiffSnapshots_DeletionRecorded(t *testing.T) {
	before := map[string]string{
		"gone.txt":     "stale content\n",
		"changed.txt":  "old\n",
		"untouched.go": "same\n",
	}
	after := map[string]string{
		"changed.txt":  "new\n",
		"untouched.go": "same\n",
		"fresh.txt":    "hello\n",
	}
	changes := diffSnapshots(before, after)
	byPath := map[string]FileChange{}
	for _, c := range changes {
		byPath[c.Path] = c
	}

	d, ok := byPath["gone.txt"]
	if !ok {
		t.Fatalf("deleted file not recorded: %+v", changes)
	}
	if !d.Deleted || d.IsNew || d.Modified {
		t.Errorf("deletion flags = deleted:%v new:%v mod:%v, want deleted only", d.Deleted, d.IsNew, d.Modified)
	}
	if d.Before != "stale content\n" || d.After != "" {
		t.Errorf("deletion content = before %q after %q, want before kept and after empty", d.Before, d.After)
	}
	if n := byPath["fresh.txt"]; !n.IsNew || n.Deleted {
		t.Errorf("new file flags = %+v, want IsNew only", n)
	}
	if m := byPath["changed.txt"]; !m.Modified || m.Deleted {
		t.Errorf("modified flags = %+v, want Modified only", m)
	}
}

// TestJudgePrompt_ShowsDeletions: the judge evidence section must render
// deletions as explicit **DELETED** entries carrying the previous content.
// The old template matched neither flag branch and rendered nothing, so
// judges docked "files_correct" against an empty evidence section.
func TestJudgePrompt_ShowsDeletions(t *testing.T) {
	var buf bytes.Buffer
	err := JudgePrompt.Execute(&buf, judgeInput{
		Description: "d",
		Prompt:      "p",
		Response:    "r",
		FileChanges: []FileChange{
			{Path: "reports/draft.md", Before: "draft body\n", Deleted: true},
			{Path: "reports/final.md", After: "final body\n", IsNew: true},
			{Path: "docs/CHANGELOG.md", Before: "- a\n", After: "- b\n", Modified: true},
		},
		Criteria: []RubricCriterion{{Criterion: "c", Scoring: "binary", Weight: 1, Description: "d"}},
	})
	if err != nil {
		t.Fatalf("execute judge template: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"**DELETED: reports/draft.md**",
		"draft body\n",
		"**NEW FILE: reports/final.md**",
		"**MODIFIED: docs/CHANGELOG.md**",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("judge prompt missing %q\n---\n%s", want, out)
		}
	}

	// A deletion-only diff must still produce a non-empty evidence section.
	buf.Reset()
	if err := JudgePrompt.Execute(&buf, judgeInput{
		FileChanges: []FileChange{{Path: "tmp/empty.log", Before: "x\n", Deleted: true}},
		Criteria:    []RubricCriterion{{Criterion: "c", Scoring: "binary", Weight: 1, Description: "d"}},
	}); err != nil {
		t.Fatalf("execute judge template (deletion-only): %v", err)
	}
	if !strings.Contains(buf.String(), "**DELETED: tmp/empty.log**") {
		t.Errorf("deletion-only diff rendered without a DELETED entry:\n%s", buf.String())
	}
}

// TestWriteResults_BacksUpPreviousRun: re-running the same experiment must not
// destroy the earlier dataset — writeResults preserves the previous results.json
// next to the fresh one (timestamped, collision-safe).
func TestWriteResults_BacksUpPreviousRun(t *testing.T) {
	dir := t.TempDir()

	write := func(name string) {
		t.Helper()
		if err := writeResults(dir, []ScenarioResult{{
			ScenarioName: name,
			AgentResults: map[string]AgentResult{"a": {}},
		}}); err != nil {
			t.Fatalf("writeResults(%s): %v", name, err)
		}
	}
	write("first-run")
	write("second-run")

	raw, err := os.ReadFile(filepath.Join(dir, "results.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "second-run") {
		t.Errorf("results.json should hold the fresh run, got: %.200s", raw)
	}

	backups, err := filepath.Glob(filepath.Join(dir, "results.*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 1 {
		t.Fatalf("want exactly 1 backup, got %v", backups)
	}
	bak, err := os.ReadFile(backups[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bak), "first-run") {
		t.Errorf("backup %s must contain the first run, got: %.200s", backups[0], bak)
	}

	// A third run must not clobber the first backup (same-second collision).
	write("third-run")
	backups, _ = filepath.Glob(filepath.Join(dir, "results.*.json"))
	if len(backups) != 2 {
		t.Fatalf("want 2 distinct backups after a third run, got %v", backups)
	}
}
