package plugin

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Command is one de-facto commands/*.md slash-command component (Claude
// Code's custom-command layout; client behavior under §4.1, ADR-0104 Part IV
// — commands are not a portable component type, exactly like agents/).
//
// Name comes from the filename stem (Claude's rule: commands/foo.md → /foo).
// Nested command directories (commands/git/commit.md → /git:commit) are
// skipped with a report — the loader reads immediate children only, the same
// rule as skills/.
type Command struct {
	Name         string // filename stem, without the leading "/"
	Description  string
	ArgumentHint string
	AllowedTools []string // raw frontmatter values (e.g. "Bash(git diff:*)")
	Body         string   // prompt template
}

// loadCommands discovers commands/*.md. Failure isolation (§11.3): one
// malformed file skips that command only; a file without frontmatter is
// treated as prose (README-style) and skipped silently.
func (p *Plugin) loadCommands() {
	dir := filepath.Join(p.Root, "commands")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return // absent = no commands (not an error)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			p.warnf("commands/%s: nested command directories are not supported — skipped", name)
			continue
		}
		if !strings.HasSuffix(name, ".md") || strings.HasPrefix(name, ".") {
			continue
		}
		full := filepath.Join(dir, name)
		rel := "commands/" + name
		if !contains(p.Root, full) {
			p.warnf("%s: escapes the plugin root — skipped", rel)
			continue
		}
		raw, rerr := os.ReadFile(full)
		if rerr != nil || len(raw) > maxCommandBytes {
			p.warnf("%s: unreadable or oversized — skipped", rel)
			continue
		}
		cmd, ok := parseCommand(string(raw), strings.TrimSuffix(name, ".md"))
		if !ok {
			// No frontmatter → prose file, not a command: silent (skills
			// set the precedent). Frontmatter present but unparseable is
			// a half-shipped command — say so (review W3).
			if hasFrontmatter(raw) {
				p.warnf("%s: invalid YAML frontmatter — skipped", rel)
			}
			continue
		}
		if cmd.Description == "" {
			// Advertised commands always need a summary (the registry and
			// the ACP surface both reject empty descriptions): derive one
			// from the body's first prose line.
			cmd.Description = firstBodyLine(cmd.Body)
		}
		p.Commands = append(p.Commands, cmd)
	}
}

// firstBodyLine returns the first non-empty, non-heading body line (trimmed),
// or a fixed fallback — anything non-empty.
func firstBodyLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, ">") {
			continue
		}
		if len(t) > 72 {
			t = t[:69] + "…"
		}
		return t
	}
	return "plugin command"
}

// parseCommand splits frontmatter and body (shared splitFrontmatter) and
// reads the three supported fields through YAML (review W3 — the previous
// line parser silently produced zero allowed tools for the common
// `allowed-tools:\n  - …` list spelling). Unknown keys are ignored
// (report-and-continue is the frontmatter norm across components); invalid
// YAML returns false so the loader can warn (frontmatter present) or pass
// the file as prose (frontmatter absent).
func parseCommand(text, stem string) (Command, bool) {
	fm, body, ok := splitFrontmatter(text)
	if !ok {
		return Command{}, false
	}
	var fields map[string]any
	if err := yaml.Unmarshal([]byte(fm), &fields); err != nil {
		return Command{}, false
	}
	c := Command{Name: stem, Body: strings.TrimSpace(body)}
	for key, raw := range fields {
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "description":
			if s, isStr := raw.(string); isStr {
				c.Description = s
			}
		case "argument-hint":
			c.ArgumentHint = yamlHintString(raw)
		case "allowed-tools":
			c.AllowedTools = yamlToolsList(raw)
		}
	}
	return c, true
}

// yamlHintString normalizes argument-hint: a scalar stays verbatim; a YAML
// list re-renders bracketed (so the common unquoted `argument-hint: [scope]`
// round-trips to the literal "[scope]" the line parser produced).
func yamlHintString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		parts := make([]string, 0, len(t))
		for _, it := range t {
			if s, isStr := it.(string); isStr {
				parts = append(parts, s)
			}
		}
		if len(parts) > 0 {
			return "[" + strings.Join(parts, ", ") + "]"
		}
	}
	return ""
}

// yamlToolsList normalizes allowed-tools: the scalar form splits on commas
// (Claude's one-line spelling), the list form takes the items as-is.
func yamlToolsList(v any) []string {
	var out []string
	add := func(s string) {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	switch t := v.(type) {
	case string:
		for _, item := range strings.Split(t, ",") {
			add(item)
		}
	case []any:
		for _, it := range t {
			if s, isStr := it.(string); isStr {
				add(s)
			}
		}
	}
	return out
}

// maxCommandBytes caps one command file (the same 64 KB class as the skill
// tool's body cap — a slash command is a prompt, not a payload).
const maxCommandBytes = 64 << 10

// maxCommandOutput caps one expansion snippet's captured output.
const maxCommandOutput = 8 << 10 // 8 KB

// expansionTimeout bounds one !`…` snippet execution.
const expansionTimeout = 5 * time.Second

// buildPrompt renders the command for dispatch: arguments substituted
// ($ARGUMENTS, or appended when the template doesn't reference it), then the
// dynamic-context snippets expanded.
//
// Expansion safety (the deliberate design line, honest scope — see
// expandSnippet): a !`…` snippet runs only when (a) the command declares a
// Bash(...) allowed-tools entry it matches (the author's whitelist) and
// (b) the snippet is a single static command with no shell metacharacters
// (no ; | & ` $ ( ) < > newline) and no write/exec-class flag. Non-
// qualifying snippets stay in the prompt VERBATIM — the model runs them
// itself with its own tools — and are never executed. Failure (timeout,
// error) also leaves the snippet literal: context gathering never fails the
// command. A declared whitelist can still be destructive: installation is
// the consent boundary (ADR-0104 Part IV).
func (c Command) BuildPrompt(args string) string {
	body := c.Body
	if strings.Contains(body, "$ARGUMENTS") {
		body = strings.ReplaceAll(body, "$ARGUMENTS", args)
	} else if args != "" {
		body = body + "\n\n" + args
	}
	if !strings.Contains(body, "!`") {
		return body
	}
	var out strings.Builder
	rest := body
	for {
		start := strings.Index(rest, "!`")
		if start < 0 {
			out.WriteString(rest)
			break
		}
		end := strings.Index(rest[start+2:], "`")
		if end < 0 {
			out.WriteString(rest) // unterminated: leave as-is
			break
		}
		end += start + 2
		snippet := rest[start+2 : end]
		out.WriteString(rest[:start])
		out.WriteString(c.expandSnippet(snippet))
		rest = rest[end+1:]
	}
	return out.String()
}

// expandSnippet runs one qualified snippet or returns the literal form.
//
// Safety (review W1, honest scope): the metacharacter cut blocks command
// CHAINING, and dangerousFlags blocks the obvious write/exec flag shapes —
// but a declared whitelist can still be destructive (git config --global …
// fits Bash(git:*)). The consent boundary is the author's declared
// allowed-tools plus the user's decision to install the plugin (ADR-0104
// Part IV): milk does not sandbox snippets, and does not claim to.
func (c Command) expandSnippet(snippet string) string {
	literal := "!`" + snippet + "`"
	trimmed := strings.TrimSpace(snippet)
	if trimmed == "" || !c.snippetAllowed(trimmed) || hasShellMetachars(trimmed) || hasDangerousFlag(trimmed) {
		return literal
	}
	ctx, cancel := context.WithTimeout(context.Background(), expansionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", trimmed)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil || ctx.Err() != nil {
		return literal
	}
	out := strings.TrimRight(buf.String(), "\n")
	if len(out) > maxCommandOutput {
		out = out[:maxCommandOutput] + "\n… (truncated)"
	}
	return out
}

// snippetAllowed matches the snippet against the command's declared
// Bash(...) allowed-tools entries (review W1, grounded in Claude's two
// spellings):
//   - Bash(git diff:*)  — the ":*" (colon) form: the stem is a COMMAND word
//     sequence, so the snippet must be exactly the stem or the stem plus a
//     space ("git diff" ✓, "git diff HEAD~1" ✓, "git difffoo" ✗);
//   - Bash(echo ON*)    — the bare "*" form: the author wrote an explicit
//     fuzzy stem, so a plain string prefix matches ("echo ONE" ✓);
//   - Bash(tool)        — exact match only.
func (c Command) snippetAllowed(snippet string) bool {
	for _, tool := range c.AllowedTools {
		rest, ok := strings.CutPrefix(tool, "Bash(")
		if !ok {
			continue
		}
		pattern := strings.TrimSpace(strings.TrimSuffix(rest, ")"))
		switch {
		case strings.HasSuffix(pattern, ":*"):
			stem := strings.TrimSuffix(pattern, ":*")
			if stem != "" && (snippet == stem || strings.HasPrefix(snippet, stem+" ")) {
				return true
			}
		case strings.HasSuffix(pattern, "*"):
			stem := strings.TrimSuffix(pattern, "*")
			if stem != "" && strings.HasPrefix(snippet, stem) {
				return true
			}
		default:
			if pattern != "" && snippet == pattern {
				return true
			}
		}
	}
	return false
}

// dangerousFlags are flag shapes that turn an otherwise read-looking command
// into file writes or program execution even with no shell metacharacters
// (review W1): `git diff --output=<file>` writes a file; --exec /
// --upload-pack / --receive-pack / --separate-git-dir run or install
// programs. The list is BEST-EFFORT and says so: it stops the obvious
// smuggling shapes, it cannot make a declared whitelist non-destructive
// (git config --global … still fits Bash(git:*)). The real consent boundary
// is the author's declared whitelist plus the user's decision to install
// the plugin — documented at these exact words in ADR-0104 Part IV.
var dangerousFlags = []string{
	"--output", "--exec", "--upload-pack", "--receive-pack",
	"--separate-git-dir", "--template",
}

func hasDangerousFlag(s string) bool {
	for _, f := range dangerousFlags {
		if strings.Contains(s, f) {
			return true
		}
	}
	return false
}

// hasShellMetachars rejects snippets that could smuggle a second command
// past the prefix whitelist.
func hasShellMetachars(s string) bool {
	return strings.ContainsAny(s, ";|&`$()<>\n")
}
