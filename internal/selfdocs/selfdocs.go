// Package selfdocs indexes milk's own embedded reference docs (docs/spec.md,
// docs/providers.md) by heading, so any agent — primary or escalation, on
// any backend — can look up how to manage milk's own configuration without
// that content being injected into every turn's context. See Lookup.
package selfdocs

import (
	"sort"
	"strings"

	"github.com/scoutme/milk/docs"
)

// section is one heading's title and the markdown body up to the next
// heading of the same or shallower level.
type section struct {
	title string
	body  string
}

// index maps a slugified heading to its section, built once at package init
// from the embedded docs. aliases maps slash-command-shaped topic names (the
// way an agent is likely to phrase a lookup) to the same slugs, since doc
// headings don't always read like command names.
var (
	index   map[string]section
	aliases = map[string]string{
		"mcp":            "mcp_servers-field",
		"mcp add":        "mcp_servers-field",
		"mcp assign":     "mcp_servers-field",
		"mcp unassign":   "mcp_servers-field",
		"mcp servers":    "mcp_servers-field",
		"mcp auth":       "mcp-servers-with-oauth",
		"mcp oauth":      "mcp-servers-with-oauth",
		"agent add":      "agents-entry-fields",
		"agent":          "agents-entry-fields",
		"agents":         "agents-entry-fields",
		"agent tool":     "agent_tools-field",
		"agent tools":    "agent_tools-field",
		"escalation":     "escalation-agent",
		"primary":        "primary-agent",
		"memory":         "memory-configuration",
		"context budget": "context-budget-configuration",
		"oversight":      "remote-oversight-telegram",
		"telegram":       "remote-oversight-telegram",
		"attachments":    "file-and-image-attachments",
	}
)

func init() {
	index = map[string]section{}
	for _, name := range []string{"spec.md", "providers.md"} {
		data, err := docs.FS.ReadFile(name)
		if err != nil {
			continue
		}
		for slug, sec := range parseSections(string(data)) {
			// First doc wins on a slug collision — spec.md is indexed before
			// providers.md above, and is the more authoritative schema doc.
			if _, exists := index[slug]; !exists {
				index[slug] = sec
			}
		}
	}

	// aliases' keys are written for readability ("mcp add"), not as slugs —
	// normalise them the same way a query is normalised so Lookup can match
	// on a single code path.
	resolvedAliases := make(map[string]string, len(aliases))
	for rawKey, slug := range aliases {
		resolvedAliases[slugify(rawKey)] = slug
	}
	aliases = resolvedAliases
}

// Topics returns every known lookup key (headings and their aliases),
// sorted, for a "what can I look up" listing.
func Topics() []string {
	seen := make(map[string]bool, len(index)+len(aliases))
	for slug := range index {
		seen[slug] = true
	}
	for alias := range aliases {
		seen[alias] = true
	}
	topics := make([]string, 0, len(seen))
	for t := range seen {
		topics = append(topics, t)
	}
	sort.Strings(topics)
	return topics
}

// Lookup returns the doc section for topic — matched first against known
// aliases, then against slugified doc headings — or ok=false when nothing
// matches. Callers should suggest Topics() on a miss.
func Lookup(topic string) (body string, ok bool) {
	key := slugify(topic)
	if key == "" {
		return "", false
	}
	if aliasSlug, isAlias := aliases[key]; isAlias {
		key = aliasSlug
	}
	sec, found := index[key]
	if !found {
		return "", false
	}
	return sec.title + "\n\n" + sec.body, true
}

// parseSections splits markdown into a slug->section map. A section's body
// runs from just after its heading to the next heading whose level is <= its
// own — i.e. it includes any nested subsections, the same way a reader
// scanning the rendered doc would see them grouped under that heading.
func parseSections(md string) map[string]section {
	lines := strings.Split(md, "\n")
	type headingLine struct {
		level int
		title string
		line  int
	}
	var headings []headingLine
	for i, l := range lines {
		trimmed := strings.TrimRight(l, " \t")
		level := 0
		for level < len(trimmed) && trimmed[level] == '#' {
			level++
		}
		if level == 0 || level > 6 || level >= len(trimmed) || trimmed[level] != ' ' {
			continue
		}
		title := strings.Trim(strings.TrimSpace(trimmed[level:]), "`")
		if title == "" {
			continue
		}
		headings = append(headings, headingLine{level: level, title: title, line: i})
	}

	out := make(map[string]section, len(headings))
	for i, h := range headings {
		end := len(lines)
		for j := i + 1; j < len(headings); j++ {
			if headings[j].level <= h.level {
				end = headings[j].line
				break
			}
		}
		body := strings.TrimSpace(strings.Join(lines[h.line+1:end], "\n"))
		slug := slugify(h.title)
		if slug == "" {
			continue
		}
		out[slug] = section{title: h.title, body: body}
	}
	return out
}

// slugify lowercases s and replaces runs of non-alphanumeric/underscore
// characters with a single hyphen, e.g. "`mcp_servers` field" -> "mcp_servers-field".
func slugify(s string) string {
	var b strings.Builder
	lastDash := true // avoid a leading hyphen
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}
