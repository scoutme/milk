package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"unicode/utf8"
)

const commandFixture = `---
allowed-tools: Bash(git diff:*), Bash(git log:*), Bash(false:*)
description: Perform a comprehensive code review of recent changes
argument-hint: [scope]
---

## Context

- Changes: !` + "`git diff`" + `
- Log: !` + "`git log --oneline -5`" + `
- Meta: !` + "`git diff; echo pwned`" + `
- Foreign: !` + "`git status`" + `
- Failing: !` + "`false`" + `

Review $ARGUMENTS now.
`

func TestParseCommand_Fields(t *testing.T) {
	c, ok := parseCommand(commandFixture, "code-review")
	if !ok {
		t.Fatal("frontmatter fixture must parse")
	}
	if c.Name != "code-review" {
		t.Errorf("name = %q", c.Name)
	}
	if c.Description != "Perform a comprehensive code review of recent changes" {
		t.Errorf("description = %q", c.Description)
	}
	if c.ArgumentHint != "[scope]" {
		t.Errorf("argument-hint = %q", c.ArgumentHint)
	}
	if len(c.AllowedTools) != 3 || c.AllowedTools[0] != "Bash(git diff:*)" {
		t.Errorf("allowed-tools = %v", c.AllowedTools)
	}
	if !strings.Contains(c.Body, "!`git diff`") || !strings.Contains(c.Body, "Review $ARGUMENTS now.") {
		t.Errorf("body lost template content: %q", c.Body)
	}
	if strings.Contains(c.Body, "allowed-tools:") {
		t.Error("frontmatter leaked into the body")
	}
}

func TestParseCommand_NoFrontmatterIsProse(t *testing.T) {
	if _, ok := parseCommand("# README\n\nJust prose.", "README"); ok {
		t.Error("a file without frontmatter is prose, not a command")
	}
}

func TestLoadCommands_DiscoveryAndIsolation(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, "plugin.json", minManifest)
	writePlugin(t, root, "commands/review.md", commandFixture)
	writePlugin(t, root, "commands/README.md", "# not a command\n\nprose")
	// A nested command directory (Claude's /parent:child form) is skipped
	// with a report — immediate children only, same rule as skills.
	if err := os.MkdirAll(filepath.Join(root, "commands", "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writePlugin(t, root, "commands/git/commit.md", "---\ndescription: nested\n---\nbody")

	p, err := Load(root, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Commands) != 1 || p.Commands[0].Name != "review" {
		t.Fatalf("commands = %+v, want just [review]", p.Commands)
	}
	if !hasWarning(p, "nested command directories") {
		t.Error("nested directory must be reported")
	}
	// README.md is prose: silent, no warning (the skills precedent).
	for _, w := range p.Warnings {
		if strings.Contains(w, "README") {
			t.Errorf("prose file must be silent, got warning %q", w)
		}
	}
}

func TestCommand_DescriptionFallsBackToBodyLine(t *testing.T) {
	root := t.TempDir()
	writePlugin(t, root, "plugin.json", minManifest)
	writePlugin(t, root, "commands/bare.md", "---\nargument-hint: x\n---\n# Heading\n\nDo the thing.\n")
	p, err := Load(root, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Commands) != 1 {
		t.Fatalf("commands = %d", len(p.Commands))
	}
	if got := p.Commands[0].Description; got != "Do the thing." {
		t.Errorf("fallback description = %q, want the first body prose line", got)
	}
}

func TestCommandBuildPrompt_ArgumentsAndExpansion(t *testing.T) {
	c, _ := parseCommand(commandFixture, "code-review")

	// $ARGUMENTS substitution.
	if got := c.BuildPrompt("the auth layer"); !strings.Contains(got, "Review the auth layer now.") {
		t.Errorf("arguments not substituted: %q", got)
	}

	// Whitelisted + static + runnable → expanded (echo stands in for the
	// git queries: deterministic on every runner, git's success is not).
	e, _ := parseCommand("---\nallowed-tools: Bash(echo:*)\ndescription: d\n---\nctx: !`echo hi`", "e")
	if got := e.BuildPrompt(""); !strings.Contains(got, "ctx: hi") {
		t.Errorf("whitelisted static snippet not expanded: %q", got)
	}

	got := c.BuildPrompt("")
	// Whitelisted but failing (sh's `false`, deterministic on every runner)
	// → literal, never an error. (No git/no-repo cases are deliberately
	// avoided: the test process runs INSIDE the milk repo, so git snippets
	// legitimately succeed here.)
	if !strings.Contains(got, "- Failing: !`false`") {
		t.Errorf("failing snippet must stay literal: %q", got)
	}
	// Metacharacter smuggling past the prefix whitelist → literal.
	if !strings.Contains(got, "!`git diff; echo pwned`") {
		t.Errorf("metachar snippet must stay literal: %q", got)
	}
	// Not in allowed-tools at all → literal (the author never vouched).
	if !strings.Contains(got, "!`git status`") {
		t.Errorf("unwhitelisted snippet must stay literal: %q", got)
	}
	if strings.Contains(got, "pwned") && !strings.Contains(got, "!`") {
		t.Error("smuggling snippet executed")
	}
}

// TestCommandBuildPrompt_OnlyAllowedToolsRun pins the whitelist→exec cut
// explicitly: same body, two tools declared — only the declared one expands.
func TestCommandBuildPrompt_OnlyAllowedToolsRun(t *testing.T) {
	c, _ := parseCommand("---\nallowed-tools: Bash(echo ON*)\ndescription: d\n---\na: !`echo ONE` b: !`echo TWO`", "x")
	got := c.BuildPrompt("")
	if !strings.Contains(got, "a: ONE") {
		t.Errorf("allowed snippet not expanded: %q", got)
	}
	if !strings.Contains(got, "b: !`echo TWO`") {
		t.Errorf("undeclared snippet must stay literal: %q", got)
	}
}

// TestCommandExpand_WordBoundaryAndDangerousFlags (review W1): the colon
// form of Bash(...) authorizes a COMMAND word sequence — a prefix-adjacent
// token must not ride along ("git difffoo" under Bash(git diff:*)); and
// write/exec-class flags are refused even inside a genuine match, because
// "static + no metacharacters" does not make `git diff --output=<file>`
// harmless. Both cases must stay literal (the model runs them itself).
func TestCommandExpand_WordBoundaryAndDangerousFlags(t *testing.T) {
	c, _ := parseCommand("---\nallowed-tools: Bash(echo hi:*)\ndescription: d\n---\nx: !`echo hiX`", "x")
	if got := c.BuildPrompt(""); !strings.Contains(got, "x: !`echo hiX`") {
		t.Errorf("prefix-adjacent token must stay literal: %q", got)
	}

	c2, _ := parseCommand("---\nallowed-tools: Bash(echo:*)\ndescription: d\n---\nx: !`echo pwn --output=/tmp/x`", "x")
	if got := c2.BuildPrompt(""); !strings.Contains(got, "x: !`echo pwn --output=/tmp/x`") {
		t.Errorf("dangerous-flag snippet must stay literal: %q", got)
	}

	// Control: the boundary must not over-tighten the author's bare-* form —
	// Bash(echo ON*) is an explicit fuzzy stem and "echo ONE" still expands.
	c3, _ := parseCommand("---\nallowed-tools: Bash(echo ON*)\ndescription: d\n---\nx: !`echo ONE`", "x")
	if got := c3.BuildPrompt(""); !strings.Contains(got, "x: ONE") {
		t.Errorf("bare-* form must keep string-prefix semantics: %q", got)
	}
}

// TestParseCommand_YAMLListAllowedTools (review W3): the common Claude
// YAML-list spelling of allowed-tools must parse like the scalar form —
// before the fix the line-based parser read `key: value` only and a list
// silently produced zero allowed tools (no expansion, no report).
func TestParseCommand_YAMLListAllowedTools(t *testing.T) {
	c, ok := parseCommand("---\ndescription: d\nargument-hint: [scope]\nallowed-tools:\n  - Bash(echo ON*)\n  - Bash(false:*)\n---\nx: !`echo ONE`\n", "x")
	if !ok {
		t.Fatal("YAML-list frontmatter must parse as a command")
	}
	if len(c.AllowedTools) != 2 {
		t.Fatalf("allowed-tools = %v, want 2 entries", c.AllowedTools)
	}
	if got := c.BuildPrompt(""); !strings.Contains(got, "x: ONE") {
		t.Errorf("list-declared snippet must expand: %q", got)
	}
	if c.ArgumentHint != "[scope]" {
		t.Errorf("argument-hint = %q, want the literal [scope] round-trip", c.ArgumentHint)
	}

	// Invalid YAML with frontmatter present = not prose: refused (the
	// loader warns; silent acceptance would ship a half-parsed command).
	if _, ok := parseCommand("---\ndescription: [unterminated\n---\nbody\n", "x"); ok {
		t.Error("invalid YAML frontmatter must not parse as a command")
	}
}

// TestFirstBodyLine_RuneSafeTruncation (review S3): the description fallback
// truncates at ~70 bytes and must never split a UTF-8 rune (a derived
// description ending in an invalid byte renders as U+FFFD garbage).
func TestFirstBodyLine_RuneSafeTruncation(t *testing.T) {
	long := strings.Repeat("é", 50) // 100 bytes; byte 69 lands mid-rune
	got := firstBodyLine(long)
	if !utf8.ValidString(got) {
		t.Fatalf("truncation split a rune: %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated line should end in …, got %q", got)
	}
}
