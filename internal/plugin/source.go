package plugin

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Source is one resolved plugin distribution source (ADR-0104 Part II).
// The vocabulary mirrors the ecosystem's marketplace.json source kinds
// (github, git-subdir, url, archive, npm + relative marketplace paths); the
// "command" kind is deliberately absent — arbitrary code at install time is
// rejected at parse.
type Source struct {
	Kind SourceKind

	Repo    string // github: "owner/repo"
	URL     string // url/archive: download URL; git-subdir: the git URL
	Path    string // git-subdir: subdirectory; relative: marketplace path
	Package string // npm: package name
	Ref     string // branch or tag (optional)
	SHA     string // pin (optional, wins over Ref)
}

type SourceKind int

const (
	SourceRelative SourceKind = iota // inside a marketplace repo
	SourceGitHub
	SourceGitSubdir
	SourceURL // any https URL to a zip/tarball
	SourceNPM
)

func (k SourceKind) String() string {
	switch k {
	case SourceRelative:
		return "relative"
	case SourceGitHub:
		return "github"
	case SourceGitSubdir:
		return "git-subdir"
	case SourceURL:
		return "url"
	case SourceNPM:
		return "npm"
	}
	return "unknown"
}

// Canonical returns the stable install-record form, e.g. "github:lliu56/minicut-skills@main".
func (s Source) Canonical() string {
	base := s.Kind.String() + ":"
	switch s.Kind {
	case SourceGitHub:
		base += s.Repo
	case SourceGitSubdir:
		base += s.URL + "#" + s.Path
	case SourceURL:
		base += s.URL
	case SourceNPM:
		base += s.Package
	case SourceRelative:
		base += s.Path
	}
	switch {
	case s.SHA != "":
		base += "@" + s.SHA
	case s.Ref != "":
		base += "@" + s.Ref
	}
	return base
}

var repoShorthand = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)

// ParseAddRef parses a user-facing install reference (the "paste a slug"
// gesture): "owner/repo", a GitHub URL, "npm:pkg", or an https URL to a
// zip/tarball. --ref/--sha pins are applied by the caller.
func ParseAddRef(s string) (Source, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Source{}, fmt.Errorf("empty plugin reference")
	}
	if pkg, ok := strings.CutPrefix(s, "npm:"); ok {
		if pkg == "" {
			return Source{}, fmt.Errorf("npm: needs a package name")
		}
		return Source{Kind: SourceNPM, Package: pkg}, nil
	}
	if repo, ok := githubRepoFromURL(s); ok {
		return Source{Kind: SourceGitHub, Repo: repo}, nil
	}
	if repoShorthand.MatchString(s) && !strings.Contains(s, "://") {
		return Source{Kind: SourceGitHub, Repo: s}, nil
	}
	if strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://") {
		return Source{Kind: SourceURL, URL: s}, nil
	}
	return Source{}, fmt.Errorf("unrecognized plugin reference %q (want owner/repo, https://…, or npm:pkg)", s)
}

func githubRepoFromURL(u string) (string, bool) {
	rest, ok := strings.CutPrefix(u, "https://github.com/")
	if !ok {
		rest, ok = strings.CutPrefix(u, "http://github.com/")
		if !ok {
			return "", false
		}
	}
	rest = strings.TrimSuffix(rest, "/")
	rest = strings.TrimSuffix(rest, ".git")
	parts := strings.Split(rest, "/")
	if len(parts) == 2 && repoShorthand.MatchString(parts[0]+"/"+parts[1]) {
		return parts[0] + "/" + parts[1], true
	}
	return "", false
}

// ParseMarketplaceSource parses a marketplace.json entry's "source": either a
// string (relative path, "owner/repo" shorthand, or URL) or the object form
// {"source": kind, …}. The "command" kind is rejected with a clear error.
func ParseMarketplaceSource(raw json.RawMessage) (Source, error) {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return Source{}, fmt.Errorf("source: %w", err)
		}
		switch {
		case s == "":
			return Source{}, fmt.Errorf("source: empty string")
		case strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../"):
			return Source{Kind: SourceRelative, Path: s}, nil
		case strings.Contains(s, "://"):
			return ParseAddRef(s)
		case repoShorthand.MatchString(s):
			return Source{Kind: SourceGitHub, Repo: s}, nil
		default:
			return Source{Kind: SourceRelative, Path: s}, nil
		}
	}
	obj, err := decodeObject(raw, "source")
	if err != nil {
		return Source{}, err
	}
	kind, err := requireString(obj, "source")
	if err != nil {
		return Source{}, err
	}
	out := Source{}
	opt := func(key string) string {
		var v string
		if raw, ok := obj[key]; ok {
			_ = json.Unmarshal(raw, &v)
		}
		return v
	}
	switch kind {
	case "github":
		out.Kind = SourceGitHub
		repo := opt("repo")
		if repo == "" {
			owner := opt("owner")
			name := opt("name")
			if owner != "" && name != "" {
				repo = owner + "/" + name
			}
		}
		if !repoShorthand.MatchString(repo) {
			return Source{}, fmt.Errorf("source: github needs repo %q in owner/repo form", repo)
		}
		out.Repo = repo
	case "git-subdir":
		out.Kind = SourceGitSubdir
		out.URL = opt("url")
		out.Path = opt("path")
		if out.URL == "" || out.Path == "" {
			return Source{}, fmt.Errorf("source: git-subdir needs url and path")
		}
		if strings.HasPrefix(out.Path, "/") || strings.Contains(out.Path, "..") {
			return Source{}, fmt.Errorf("source: git-subdir path %q must be relative without ..", out.Path)
		}
	case "url", "archive":
		out.Kind = SourceURL
		out.URL = opt("url")
		if out.URL == "" {
			return Source{}, fmt.Errorf("source: %s needs url", kind)
		}
	case "npm":
		out.Kind = SourceNPM
		out.Package = opt("package")
		if out.Package == "" {
			return Source{}, fmt.Errorf("source: npm needs package")
		}
	case "command":
		return Source{}, fmt.Errorf("source: kind %q is rejected — arbitrary code at install time is not run (ADR-0104)", kind)
	default:
		return Source{}, fmt.Errorf("source: unknown kind %q", kind)
	}
	out.Ref = opt("ref")
	out.SHA = opt("sha")
	return out, nil
}
