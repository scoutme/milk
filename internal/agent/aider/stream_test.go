package aider

import (
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/agent/subprocess"
)

func TestIsAiderDecoration(t *testing.T) {
	cases := []struct {
		line string
		skip bool
	}{
		{"─────────────────", true},
		{"Aider v0.82.0", true},
		{"Aider version string with more text", true},
		{"normal output line", false},
		{"  indented line", false},
		{"", false},
	}
	for _, c := range cases {
		got := isAiderDecoration(c.line)
		if got != c.skip {
			t.Errorf("isAiderDecoration(%q) = %v, want %v", c.line, got, c.skip)
		}
	}
}

func TestParser_FiltersDecoration(t *testing.T) {
	input := "Aider v0.82.0\n─────\nreal output\n"
	var out strings.Builder
	p := &Parser{}
	res, err := p.Parse(strings.NewReader(input), &out, subprocess.ParseOpts{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(out.String(), "Aider v") {
		t.Errorf("decoration line leaked into output: %q", out.String())
	}
	if strings.Contains(out.String(), "─") {
		t.Errorf("separator line leaked into output: %q", out.String())
	}
	if !strings.Contains(res.Text, "real output") {
		t.Errorf("expected real output in Text, got %q", res.Text)
	}
}

func TestParser_DiffAnnotation(t *testing.T) {
	input := "+++ myfile.go\nsome diff content\n"
	var out strings.Builder
	p := &Parser{}
	_, err := p.Parse(strings.NewReader(input), &out, subprocess.ParseOpts{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The +++ line should produce a dim edit annotation, not be echoed raw.
	if strings.Contains(out.String(), "+++ myfile.go") {
		t.Errorf("raw diff header leaked into output: %q", out.String())
	}
	if !strings.Contains(out.String(), "myfile.go") {
		t.Errorf("expected filename in annotation output: %q", out.String())
	}
}
