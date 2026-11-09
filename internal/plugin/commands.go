package plugin

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
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
			// set the precedent).
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
// reads the three supported fields; unknown frontmatter is tolerated
// (report-and-continue is the frontmatter norm across components).
func parseCommand(text, stem string) (Command, bool) {
	fm, body, ok := splitFrontmatter(text)
	if !ok {
		return Command{}, false
	}
	c := Command{Name: stem, Body: strings.TrimSpace(body)}
	for _, rawLine := range strings.Split(fm, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		val = strings.TrimSpace(val)
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "description":
			c.Description = val
		case "argument-hint":
			c.ArgumentHint = val
		case "allowed-tools":
			for _, item := range strings.Split(val, ",") {
				if item = strings.TrimSpace(item); item != "" {
					c.AllowedTools = append(c.AllowedTools, item)
				}
			}
		}
	}
	return c, true
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
// Expansion safety (the deliberate design line): a !`…` snippet runs only
// when (a) the command declares a Bash(...) allowed-tools entry whose prefix
// it matches (the author's own whitelist — installation is the user's
// consent) and (b) the snippet is a single static command with no shell
// metacharacters (no ; | & ` $ ( ) < > newline) — prefix matching a snippet
// like `git diff; rm -rf ~` against `git diff:*` would otherwise authorize
// arbitrary pipelines. Non-qualifying snippets stay in the prompt VERBATIM —
// the model runs them itself with its own tools — and are never executed.
// Failure (timeout, error) also leaves the snippet literal: context
// gathering never fails the command.
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
func (c Command) expandSnippet(snippet string) string {
	literal := "!`" + snippet + "`"
	trimmed := strings.TrimSpace(snippet)
	if trimmed == "" || !c.snippetAllowed(trimmed) || hasShellMetachars(trimmed) {
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
// Bash(...) allowed-tools prefixes (Claude's `Bash(git diff:*)` shape: the
// part before ":*"/":" is the authorized command prefix).
func (c Command) snippetAllowed(snippet string) bool {
	for _, tool := range c.AllowedTools {
		rest, ok := strings.CutPrefix(tool, "Bash(")
		if !ok {
			continue
		}
		pattern := strings.TrimSuffix(strings.TrimSuffix(rest, ")"), "*")
		pattern = strings.TrimSuffix(pattern, ":")
		pattern = strings.TrimSpace(pattern)
		if pattern != "" && strings.HasPrefix(snippet, pattern) {
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
