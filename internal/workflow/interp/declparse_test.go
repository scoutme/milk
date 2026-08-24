package interp

import (
	"strings"
	"testing"
)

func TestParseDeclarations_Scalars(t *testing.T) {
	doc := `## Limits
max_passes: 4
max_sprints: 2
`
	d := ParseDeclarations(doc)
	if d.Scalars["max_passes"] != 4 {
		t.Errorf("max_passes = %d, want 4", d.Scalars["max_passes"])
	}
	if d.Scalars["max_sprints"] != 2 {
		t.Errorf("max_sprints = %d, want 2", d.Scalars["max_sprints"])
	}
}

func TestParseDeclarations_Sections(t *testing.T) {
	doc := `## Sprint 1
Do the first thing.

## Sprint 2
Do the second thing.
`
	d := ParseDeclarations(doc)
	sprints := d.SectionsFor("Sprint")
	if len(sprints) != 2 {
		t.Fatalf("got %d sprints, want 2", len(sprints))
	}
	if sprints[0].Index != 1 || sprints[1].Index != 2 {
		t.Errorf("indices = %d, %d, want 1, 2", sprints[0].Index, sprints[1].Index)
	}
	if sprints[0].Body == "" || sprints[1].Body == "" {
		t.Error("expected non-empty section bodies")
	}
	// The second section's body must not bleed into the first's.
	if strings.Contains(sprints[0].Body, "second thing") {
		t.Error("sprint 1's body leaked sprint 2's content")
	}
}

func TestParseDeclarations_SectionsForCaseInsensitive(t *testing.T) {
	doc := "## Task 1\nfoo\n"
	d := ParseDeclarations(doc)
	if len(d.SectionsFor("task")) != 1 {
		t.Error("expected case-insensitive label match")
	}
}

func TestParseDeclarations_DependsOn(t *testing.T) {
	doc := `## Task 1
independent

## Task 2 (depends_on: 1)
depends on task 1

## Task 3 (depends_on: 1, 2)
depends on both
`
	d := ParseDeclarations(doc)
	tasks := d.SectionsFor("Task")
	if len(tasks) != 3 {
		t.Fatalf("got %d tasks, want 3", len(tasks))
	}
	if len(tasks[0].DependsOn) != 0 {
		t.Errorf("task 1 DependsOn = %v, want empty", tasks[0].DependsOn)
	}
	if len(tasks[1].DependsOn) != 1 || tasks[1].DependsOn[0] != 1 {
		t.Errorf("task 2 DependsOn = %v, want [1]", tasks[1].DependsOn)
	}
	if len(tasks[2].DependsOn) != 2 || tasks[2].DependsOn[0] != 1 || tasks[2].DependsOn[1] != 2 {
		t.Errorf("task 3 DependsOn = %v, want [1 2]", tasks[2].DependsOn)
	}
}

func TestParseDeclarations_NoSections(t *testing.T) {
	d := ParseDeclarations("just prose, no headings")
	if len(d.Sections) != 0 {
		t.Errorf("expected no sections, got %d", len(d.Sections))
	}
}
