package main

import (
	"strings"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/agent/local"
)

func TestBuildBackgroundPanelLines_Empty(t *testing.T) {
	lines := buildBackgroundPanelLines(nil, backgroundPanelInner)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "(none)") {
		t.Errorf("expected a (none) placeholder for an empty job list, got %q", joined)
	}
}

func TestBuildBackgroundPanelLines_ShowsEachJobWithinWidthBudget(t *testing.T) {
	jobs := []local.Job{
		{ID: "job_1", Label: "terrain & mesher deep dive", Status: local.JobRunning, StartedAt: time.Now().Add(-30 * time.Second)},
		{ID: "job_2", Label: "combat & AI deep dive", Status: local.JobCompleted, StartedAt: time.Now().Add(-90 * time.Second), EndedAt: time.Now().Add(-10 * time.Second)},
		{ID: "job_3", Label: "a needlessly long label that will not fit in the panel width at all", Status: local.JobFailed, StartedAt: time.Now().Add(-5 * time.Second), EndedAt: time.Now()},
	}
	lines := buildBackgroundPanelLines(jobs, backgroundPanelInner)
	joined := strings.Join(lines, "\n")

	for _, want := range []string{"terrain", "combat", "a needlessly long"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected panel content to mention %q, got:\n%s", want, joined)
		}
	}
	for _, line := range lines {
		if w := runeWidthNoANSI(line); w > backgroundPanelInner {
			t.Errorf("line exceeds panel width %d (got %d): %q", backgroundPanelInner, w, line)
		}
	}
}

func runeWidthNoANSI(s string) int {
	return len([]rune(stripANSI(s)))
}
