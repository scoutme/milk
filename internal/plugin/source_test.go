package plugin

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseAddRef(t *testing.T) {
	cases := []struct {
		in   string
		kind SourceKind
		want string // canonical body part
		err  bool
	}{
		{"lliu56/minicut-skills", SourceGitHub, "lliu56/minicut-skills", false},
		{"https://github.com/lliu56/minicut-skills", SourceGitHub, "lliu56/minicut-skills", false},
		{"https://github.com/lliu56/minicut-skills.git", SourceGitHub, "lliu56/minicut-skills", false},
		{"npm:@minicut/mcp", SourceNPM, "@minicut/mcp", false},
		{"https://example.com/x/plugin.zip", SourceURL, "https://example.com/x/plugin.zip", false},
		{"", 0, "", true},
		{"npm:", 0, "", true},
		{"just words", 0, "", true},
	}
	for _, c := range cases {
		src, err := ParseAddRef(c.in)
		if c.err {
			if err == nil {
				t.Errorf("ParseAddRef(%q): want error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseAddRef(%q): %v", c.in, err)
			continue
		}
		if src.Kind != c.kind {
			t.Errorf("ParseAddRef(%q): kind %v want %v", c.in, src.Kind, c.kind)
		}
		body := src.Canonical()
		if !strings.Contains(body, c.want) {
			t.Errorf("ParseAddRef(%q): canonical %q missing %q", c.in, body, c.want)
		}
	}
}

func TestParseMarketplaceSource(t *testing.T) {
	cases := []struct {
		raw  string
		kind SourceKind
		body string
		err  string
	}{
		{`"./"`, SourceRelative, "./", ""},
		{`"./plugins/foo"`, SourceRelative, "./plugins/foo", ""},
		{`"owner/repo"`, SourceGitHub, "owner/repo", ""},
		{`"https://github.com/a/b"`, SourceGitHub, "a/b", ""},
		{`{"source":"github","repo":"a/b"}`, SourceGitHub, "a/b", ""},
		{`{"source":"github","owner":"a","name":"b"}`, SourceGitHub, "a/b", ""},
		{`{"source":"git-subdir","url":"https://gitlab.com/g/m.git","path":"plugins/x"}`, SourceGitSubdir, "plugins/x", ""},
		{`{"source":"git-subdir","url":"https://gitlab.com/g/m","path":"../evil"}`, 0, "", "must be relative"},
		{`{"source":"url","url":"https://e.com/p.zip"}`, SourceURL, "https://e.com/p.zip", ""},
		{`{"source":"archive","url":"https://e.com/p.tgz"}`, SourceURL, "https://e.com/p.tgz", ""},
		{`{"source":"npm","package":"@x/y"}`, SourceNPM, "@x/y", ""},
		{`{"source":"command","command":"rm"}`, 0, "", "rejected"},
		{`{"source":"wat"}`, 0, "", "unknown kind"},
		{`{"source":"github"}`, 0, "", "needs repo"},
	}
	for _, c := range cases {
		src, err := ParseMarketplaceSource(json.RawMessage(c.raw))
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("ParseMarketplaceSource(%s): err=%v want contains %q", c.raw, err, c.err)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseMarketplaceSource(%s): %v", c.raw, err)
			continue
		}
		if src.Kind != c.kind {
			t.Errorf("ParseMarketplaceSource(%s): kind %v want %v", c.raw, src.Kind, c.kind)
		}
		if !strings.Contains(src.Canonical(), c.body) {
			t.Errorf("ParseMarketplaceSource(%s): canonical %q missing %q", c.raw, src.Canonical(), c.body)
		}
	}
}

func TestSource_CanonicalPin(t *testing.T) {
	s := Source{Kind: SourceGitHub, Repo: "a/b", Ref: "main"}
	if got := s.Canonical(); got != "github:a/b@main" {
		t.Errorf("canonical = %q", got)
	}
	s.SHA = "deadbeef"
	if got := s.Canonical(); got != "github:a/b@deadbeef" {
		t.Errorf("canonical with sha = %q", got)
	}
}

// TestParseMarketplaceSource_RelativePathContainment (review C1): a
// marketplace-relative source must stay inside the marketplace root —
// ".."-anywhere and absolute paths are refused at parse time, the same rule
// the git-subdir object form already enforced (source.go). Before the fix
// these parsed as plain Relative sources and the install joined them onto
// the snapshot dir uncontained — a crafted marketplace.json could copy
// arbitrary host directories into the plugin tree.
func TestParseMarketplaceSource_RelativePathContainment(t *testing.T) {
	for _, tc := range []struct{ raw, why string }{
		{`"../escape"`, "parent segment"},
		{`"./x/../../y"`, "mid-path parent"},
		{`"x/../y"`, "cleanable parent"},
		{`"/etc"`, "absolute unix"},
		{`"a..b/c"`, "dot-dot substring (same rule as git-subdir)"},
	} {
		if _, err := ParseMarketplaceSource(json.RawMessage(tc.raw)); err == nil {
			t.Errorf("source %s (%s) must be refused", tc.raw, tc.why)
		}
	}
	if _, err := ParseMarketplaceSource(json.RawMessage(`"./plugins/x"`)); err != nil {
		t.Errorf("plain relative must parse: %v", err)
	}
	if _, err := ParseMarketplaceSource(json.RawMessage(`"plugins/x"`)); err != nil {
		t.Errorf("bare relative must parse: %v", err)
	}
}
