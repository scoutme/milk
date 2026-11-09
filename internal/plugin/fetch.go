package plugin

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// FetchTo downloads a plugin from its distribution source and extracts the
// portable plugin surface into dest (created fresh) — no git involved
// (ADR-0104 Part II: HTTPS archive endpoints + extraction filter).
//
// Kept in the extraction: plugin.json, mcp.json, skills/**, mcp/**,
// scripts/**, LICENSE*, and .claude-plugin/** (shim input for packages that
// predate the portable layout). Everything else (media, other-client layouts,
// repo scaffolding) is skipped with an aggregated report — that is the filter
// that keeps a 500 MB repo from becoming a 500 MB install.
//
// Extraction is two-pass over the in-memory archive: pass one normalizes
// entry names (clamping any ".." at the root) and decides the single wrapper
// directory to strip (codeload: owner-repo-sha/; npm: package/), pass two
// extracts through the filter. A contains() guard in keep() remains as
// defense in depth behind the normalization.
func FetchTo(ctx context.Context, client *http.Client, src Source, dest string) ([]string, error) {
	if src.Kind == SourceRelative {
		return nil, fmt.Errorf("relative sources are resolved against a marketplace root, not fetched")
	}
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	data, err := downloadSource(ctx, client, src)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return nil, err
	}
	return extractFiltered(data, dest)
}

func downloadSource(ctx context.Context, client *http.Client, src Source) ([]byte, error) {
	switch src.Kind {
	case SourceGitHub:
		ref := src.SHA
		if ref == "" {
			ref = src.Ref
		}
		url := "https://api.github.com/repos/" + src.Repo + "/tarball"
		if ref != "" {
			url += "/" + ref
		}
		return httpGet(ctx, client, url, "application/vnd.github+json")
	case SourceNPM:
		metaRaw, err := httpGet(ctx, client, "https://registry.npmjs.org/"+src.Package+"/latest", "application/json")
		if err != nil {
			return nil, fmt.Errorf("npm metadata: %w", err)
		}
		var meta struct {
			Dist struct {
				Tarball string `json:"tarball"`
			} `json:"dist"`
		}
		if err := json.Unmarshal(metaRaw, &meta); err != nil || meta.Dist.Tarball == "" {
			return nil, fmt.Errorf("npm metadata for %s: no dist.tarball", src.Package)
		}
		return httpGet(ctx, client, meta.Dist.Tarball, "application/octet-stream")
	case SourceURL:
		return httpGet(ctx, client, src.URL, "application/octet-stream")
	case SourceGitSubdir:
		url, err := archiveEndpoint(src)
		if err != nil {
			return nil, err
		}
		return httpGet(ctx, client, url, "application/octet-stream")
	}
	return nil, fmt.Errorf("unsupported source kind %s", src.Kind)
}

// archiveEndpoint maps a git URL to a host archive endpoint (the no-clone
// divergence). Only hosts with a known endpoint are supported; anything else
// reports loudly instead of falling back to git.
func archiveEndpoint(src Source) (string, error) {
	ref := src.SHA
	if ref == "" {
		ref = src.Ref
	}
	if ref == "" {
		ref = "HEAD"
	}
	switch {
	case strings.Contains(src.URL, "github.com/"):
		repo, ok := githubRepoFromURL(strings.TrimSuffix(src.URL, ".git"))
		if !ok {
			return "", fmt.Errorf("git-subdir: cannot read owner/repo from %q", src.URL)
		}
		return "https://api.github.com/repos/" + repo + "/tarball/" + ref, nil
	case strings.Contains(src.URL, "gitlab.com/"):
		// GitLab's universal pattern: /-/archive/<ref>/<name>-<ref>.tar.gz
		trimmed := strings.TrimSuffix(strings.TrimPrefix(src.URL, "https://"), ".git")
		trimmed = strings.TrimSuffix(trimmed, "/")
		name := path.Base(trimmed)
		return "https://" + trimmed + "/-/archive/" + ref + "/" + name + "-" + ref + ".tar.gz", nil
	}
	return "", fmt.Errorf("no archive endpoint known for %q — clone it into ~/.milk/plugins manually (ADR-0104: milk never runs git)", src.URL)
}

func httpGet(ctx context.Context, client *http.Client, url, accept string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "milk-plugin-installer")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: HTTP %d", url, resp.StatusCode)
	}
	// Size ceiling: a package surface is text; 256 MiB is far beyond any real
	// plugin and stops a runaway archive before it becomes disk.
	const maxArchive = 256 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxArchive+1))
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	if len(data) > maxArchive {
		return nil, fmt.Errorf("fetch %s: archive exceeds %d MiB", url, maxArchive>>20)
	}
	return data, nil
}

// keepSurface decides extraction. .claude-plugin/** is kept as shim input;
// agents/** is the de-facto subagent directory (kept like skills); marketplace.json and plugins/** are the marketplace catalog's surface
// (a catalog repo bundles its plugin dirs); the other per-client layouts and
// asset trees are the skip set.
func keepSurface(rel string) bool {
	switch {
	case rel == "plugin.json" || rel == "mcp.json" || rel == "marketplace.json":
		return true
	case strings.HasPrefix(rel, "skills/"), strings.HasPrefix(rel, "mcp/"),
		strings.HasPrefix(rel, "scripts/"), strings.HasPrefix(rel, "agents/"),
		strings.HasPrefix(rel, "commands/"),
		strings.HasPrefix(rel, ".claude-plugin/"),
		strings.HasPrefix(rel, "plugins/"):
		return true
	case strings.HasPrefix(rel, "LICENSE"):
		return true
	}
	return false
}

// normEntry cleans an archive entry name into a root-relative path:
// "./x" → "x", absolute and ".."-escaping names clamp at the root (so an
// escape name can never resolve outside dest), directory markers → "".
func normEntry(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	cleaned := path.Clean("/" + strings.TrimPrefix(name, "./"))
	cleaned = strings.TrimPrefix(cleaned, "/")
	if cleaned == "" || cleaned == "." || strings.HasSuffix(name, "/") {
		return ""
	}
	return cleaned
}

// commonWrapper finds the single wrapper directory to strip (codeload:
// owner-repo-sha/…; npm: package/…). Only strips when every entry shares one
// top directory that is not the plugin surface itself. Order-independent —
// the decision looks at the whole archive, never at "the first entry".
func commonWrapper(names []string) string {
	if len(names) == 0 {
		return ""
	}
	first := ""
	for _, n := range names {
		if n == "plugin.json" || n == "mcp.json" || n == "marketplace.json" {
			return ""
		}
		i := strings.Index(n, "/")
		if i < 0 {
			return ""
		}
		top := n[:i]
		switch top {
		case "skills", "mcp", "scripts", "agents", "commands", ".claude-plugin", "plugins":
			return ""
		}
		if first == "" {
			first = top
		} else if first != top {
			return ""
		}
	}
	return first + "/"
}

// entry is one normalized archive file ready to keep.
type entry struct {
	rel  string
	open func() (io.ReadCloser, int64, error)
}

func extractFiltered(data []byte, dest string) ([]string, error) {
	var entries []entry
	if isZip(data) {
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, fmt.Errorf("archive: %w", err)
		}
		type zf struct {
			rel string
			f   *zip.File
		}
		var kept []zf
		var names []string
		for _, f := range zr.File {
			if f.FileInfo().IsDir() || f.Mode()&os.ModeSymlink != 0 {
				continue
			}
			rel := normEntry(f.Name)
			if rel == "" {
				continue
			}
			names = append(names, rel)
			kept = append(kept, zf{rel, f})
		}
		strip := commonWrapper(names)
		for _, k := range kept {
			f := k.f
			rel := strings.TrimPrefix(k.rel, strip)
			if rel == "" || rel == k.rel && strip != "" {
				continue
			}
			entries = append(entries, entry{rel: rel, open: func() (io.ReadCloser, int64, error) {
				rc, err := f.Open()
				return rc, int64(f.UncompressedSize64), err
			}})
		}
	} else {
		var kept []struct {
			rel string
			hdr *tar.Header
		}
		var names []string
		if err := eachTar(data, func(hdr *tar.Header, r io.Reader) error {
			rel := normEntry(hdr.Name)
			if rel == "" {
				return nil
			}
			names = append(names, rel)
			h := *hdr
			kept = append(kept, struct {
				rel string
				hdr *tar.Header
			}{rel, &h})
			return nil
		}); err != nil {
			return nil, err
		}
		strip := commonWrapper(names)
		for _, k := range kept {
			rel := strings.TrimPrefix(k.rel, strip)
			if rel == "" || (rel == k.rel && strip != "") {
				continue
			}
			hdr := k.hdr
			entries = append(entries, entry{rel: rel, open: func() (io.ReadCloser, int64, error) {
				pr, pw := io.Pipe()
				go func() {
					pw.CloseWithError(eachTarOne(data, hdr.Name, pw))
				}()
				return pr, hdr.Size, nil
			}})
		}
	}

	var warns []string
	skipped := 0
	for _, e := range entries {
		if !keepSurface(e.rel) {
			skipped++
			continue
		}
		rel := filepath.FromSlash(e.rel)
		target := filepath.Join(dest, rel)
		if !contains(dest, target) {
			return nil, fmt.Errorf("archive entry %q escapes the plugin root", e.rel)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil, err
		}
		rc, size, err := e.open()
		if err != nil {
			return nil, err
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			rc.Close()
			return nil, err
		}
		_, err = io.Copy(f, io.LimitReader(rc, size+1))
		rc.Close()
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, err
		}
	}
	if skipped > 0 {
		warns = append(warns, fmt.Sprintf("skipped %d archive entries outside the plugin surface (assets, other-client layouts, scaffolding)", skipped))
	}
	return warns, nil
}

// eachTar runs fn over every regular tar entry of the in-memory archive.
func eachTar(data []byte, fn func(hdr *tar.Header, r io.Reader) error) error {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("archive: not a zip or tar.gz (%v)", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			continue
		}
		if err := fn(hdr, tr); err != nil {
			return err
		}
	}
}

// eachTarOne streams exactly one entry's content into w (second-pass reads
// are per entry — the archive is in memory, so this is cheap and keeps the
// two-pass layout simple).
func eachTarOne(data []byte, name string, w io.Writer) error {
	return eachTar(data, func(hdr *tar.Header, r io.Reader) error {
		if hdr.Name != name {
			return nil
		}
		_, err := io.Copy(w, r)
		return err
	})
}

func isZip(data []byte) bool {
	return len(data) > 3 && data[0] == 'P' && data[1] == 'K' && data[2] == 3 && data[3] == 4
}
