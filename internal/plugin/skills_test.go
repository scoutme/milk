package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSkill(t *testing.T) {
	sk, err := ParseSkill([]byte("---\nname: pdf-processing\ndescription: Extract PDF text, fill forms. Use when handling PDFs.\nlicense: Apache-2.0\ncompatibility: Needs pdftotext\nmetadata:\n  author: example\nallowed-tools: Bash(git:*) Read\n---\n# Instructions\n\nDo the thing.\n"))
	if err != nil {
		t.Fatal(err)
	}
	if sk.Name != "pdf-processing" || sk.License != "Apache-2.0" || sk.AllowedTools != "Bash(git:*) Read" {
		t.Fatalf("got %+v", sk)
	}
	if sk.Metadata["author"] != "example" {
		t.Fatalf("metadata: %v", sk.Metadata)
	}
	if sk.Body != "# Instructions\n\nDo the thing.\n" {
		t.Fatalf("body: %q", sk.Body)
	}
}

func TestParseSkill_Rejections(t *testing.T) {
	cases := map[string]string{
		"no frontmatter":      "just text",
		"missing name":        "---\ndescription: d\n---\n",
		"missing description": "---\nname: n\n---\n",
		"empty description":   "---\nname: n\ndescription: \"\"\n---\n",
		"name uppercase":      "---\nname: PDF\ndescription: d\n---\n",
		"name leading hyphen": "---\nname: -pdf\ndescription: d\n---\n",
		"name double hyphen":  "---\nname: pdf--x\ndescription: d\n---\n",
		"name trailing hyp":   "---\nname: pdf-\ndescription: d\n---\n",
		"name too long":       "---\nname: " + strings.Repeat("a", 65) + "\ndescription: d\n---\n",
		"desc too long":       "---\nname: n\ndescription: " + strings.Repeat("d", 1025) + "\n---\n",
		"compat too long":     "---\nname: n\ndescription: d\ncompatibility: " + strings.Repeat("c", 501) + "\n---\n",
	}
	for label, raw := range cases {
		if _, err := ParseSkill([]byte(raw)); err == nil {
			t.Errorf("%s: expected rejection", label)
		}
	}
}

func TestParseSkill_UnknownFrontmatterTolerated(t *testing.T) {
	sk, err := ParseSkill([]byte("---\nname: n\ndescription: d\nfuture-field: x\n---\nbody"))
	if err != nil {
		t.Fatal(err)
	}
	if sk.Name != "n" || sk.Body != "body" {
		t.Fatalf("got %+v", sk)
	}
}

func TestLoadSkills_DiscoveryRules(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, "plugin.json", minManifest)
	writePlugin(t, root, "skills/one/SKILL.md", "---\nname: one\ndescription: first skill\n---\nbody one")
	writePlugin(t, root, "skills/two/SKILL.md", "---\nname: two\ndescription: second skill\n---\nbody two")
	// Not a skill: no SKILL.md.
	writePlugin(t, root, "skills/nope/README.md", "hi")
	// Invalid skill: skipped with warning, sibling unaffected.
	writePlugin(t, root, "skills/bad/SKILL.md", "no frontmatter here")
	// Deeper descendants are NOT discovered (§7.1: no recursion).
	writePlugin(t, root, "skills/one/nested/SKILL.md", "---\nname: nested\ndescription: hidden\n---\nx")

	p, err := Load(root, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, s := range p.Skills {
		names[s.Name] = true
	}
	if !names["one"] || !names["two"] || len(p.Skills) != 2 {
		t.Fatalf("want one+two only, got %+v", p.Skills)
	}
	if names["nested"] {
		t.Fatal("nested skills must not be discovered")
	}
	if !hasWarning(p, `"bad"`) {
		t.Fatalf("want invalid-skill warning, got %v", p.Warnings)
	}
	for _, s := range p.Skills {
		if s.Plugin != "my-plugin" {
			t.Fatalf("skill plugin backref: %+v", s)
		}
	}
}

func TestLoadSkills_SymlinkEscapeSkipped(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writePlugin(t, root, "plugin.json", minManifest)
	writePlugin(t, outside, "SKILL.md", "---\nname: evil\ndescription: escapes\n---\nx")
	if err := os.MkdirAll(filepath.Join(root, "skills", "evil"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "SKILL.md"), filepath.Join(root, "skills", "evil", "SKILL.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	p, err := Load(root, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Skills) != 0 {
		t.Fatalf("escaping skill must be skipped: %+v", p.Skills)
	}
	if !hasWarning(p, `"evil"`) {
		t.Fatalf("want containment warning, got %v", p.Warnings)
	}
}
