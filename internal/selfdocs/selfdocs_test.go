package selfdocs

import (
	"strings"
	"testing"
)

func TestParseSections_NestedHeadingIncludedInParentBody(t *testing.T) {
	md := "## Parent\n\nintro text\n\n### Child\n\nchild text\n\n## Sibling\n\nsibling text\n"
	sections := parseSections(md)

	parent, ok := sections["parent"]
	if !ok {
		t.Fatal("expected a \"parent\" section")
	}
	if !strings.Contains(parent.body, "intro text") || !strings.Contains(parent.body, "### Child") {
		t.Errorf("parent body should include its own text and the nested child heading, got: %q", parent.body)
	}
	if strings.Contains(parent.body, "sibling text") {
		t.Errorf("parent body should stop before the next same-level heading, got: %q", parent.body)
	}

	child, ok := sections["child"]
	if !ok {
		t.Fatal("expected a \"child\" section")
	}
	if !strings.Contains(child.body, "child text") {
		t.Errorf("child body = %q, want to contain %q", child.body, "child text")
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"`mcp_servers` field":    "mcp_servers-field",
		"MCP servers with OAuth": "mcp-servers-with-oauth",
		"  leading/trailing  ":   "leading-trailing",
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLookup_KnownAliasesResolve(t *testing.T) {
	for _, topic := range []string{"mcp add", "mcp assign", "agent add", "mcp auth"} {
		if _, ok := Lookup(topic); !ok {
			t.Errorf("Lookup(%q) = not found, want a resolved section from the real embedded docs", topic)
		}
	}
}

func TestLookup_UnknownTopicMisses(t *testing.T) {
	if _, ok := Lookup("something that does not exist anywhere"); ok {
		t.Error("expected a miss for a nonsense topic")
	}
	if _, ok := Lookup(""); ok {
		t.Error("expected a miss for an empty topic")
	}
}

func TestTopics_IncludesAliasesAndHeadings(t *testing.T) {
	topics := Topics()
	if len(topics) == 0 {
		t.Fatal("expected a non-empty topic list")
	}
	found := false
	for _, topic := range topics {
		if topic == "mcp-add" {
			found = true
		}
	}
	if !found {
		t.Error(`expected "mcp-add" alias in Topics()`)
	}
}
