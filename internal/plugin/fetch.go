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
	"net/url"
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
	u, perr := url.Parse(src.URL)
	if perr != nil || u.Host == "" {
		return "", fmt.Errorf("git-subdir: cannot read a host from %q", src.URL)
	}
	if u.Scheme != "https" {
		return "", fmt.Errorf("git-subdir: only https URLs are supported (got %q)", src.URL)
	}
	switch u.Host {
	case "github.com":
		repo, ok := githubRepoFromURL(strings.TrimSuffix(src.URL, ".git"))
		if !ok {
			return "", fmt.Errorf("git-subdir: cannot read owner/repo from %q", src.URL)
		}
		return "https://api.github.com/repos/" + repo + "/tarball/" + ref, nil
	case "gitlab.com":
		// GitLab's universal pattern: /-/archive/<ref>/<name>-<ref>.tar.gz
		trimmed := strings.TrimSuffix(strings.TrimPrefix(src.URL, "https://"), ".git")
		trimmed = strings.TrimSuffix(trimmed, "/")
		name := path.Base(trimmed)
		return "https://" + trimmed + "/-/archive/" + ref + "/" + name + "-" + ref + ".tar.gz", nil
	}
	return "", fmt.Errorf("git-subdir: unsupported host %q — no archive endpoint known (clone it into ~/.milk/plugins manually; ADR-0104: milk never runs git)", u.Host)
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

// entry is one normalized archive file kept into the install.
type entry struct {
	rel  string
	blob []byte // materialized payload, size-checked against its header
}

func extractFiltered(data []byte, dest string) ([]string, error) {
	var entries []entry
	skipped := 0
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
			rel := strings.TrimPrefix(k.rel, strip)
			if rel == "" || rel == k.rel && strip != "" {
				continue
			}
			if !keepSurface(rel) {
				skipped++
				continue
			}
			// Materialize only surface-kept entries (review S1): one read
			// per kept file — never the asset payload the filter exists to
			// skip — and never the k full-archive decompressions the lazy
			// tar path used to do.
			rc, err := k.f.Open()
			if err != nil {
				return nil, err
			}
			declared := int64(k.f.UncompressedSize64)
			blob, err := readBounded(rc, declared)
			rc.Close()
			if err != nil {
				return nil, fmt.Errorf("archive entry %q: %w", k.f.Name, err)
			}
			entries = append(entries, entry{rel: rel, blob: blob})
		}
	} else {
		// tar: pass 1 scans names/headers (one decompression), pass 2 is
		// ONE more scan materializing just the surface-kept payloads —
		// the previous lazy path re-decompressed the whole archive per
		// kept entry (k+1 passes, pathological exactly when the filter
		// exists: big archives with few kept files).
		type th struct {
			rel string
			hdr *tar.Header
		}
		var kept []th
		var names []string
		if err := eachTar(data, func(hdr *tar.Header, r io.Reader) error {
			rel := normEntry(hdr.Name)
			if rel == "" {
				return nil
			}
			names = append(names, rel)
			kept = append(kept, th{rel, hdr})
			return nil
		}); err != nil {
			return nil, err
		}
		strip := commonWrapper(names)
		want := map[string]string{} // archive entry name -> final rel
		for _, k := range kept {
			rel := strings.TrimPrefix(k.rel, strip)
			if rel == "" || (rel == k.rel && strip != "") {
				continue
			}
			if !keepSurface(rel) {
				skipped++
				continue
			}
			want[k.hdr.Name] = rel
		}
		if err := eachTar(data, func(hdr *tar.Header, r io.Reader) error {
			rel, ok := want[hdr.Name]
			if !ok {
				return nil
			}
			blob, err := readBounded(r, hdr.Size)
			if err != nil {
				return fmt.Errorf("archive entry %q: %w", hdr.Name, err)
			}
			entries = append(entries, entry{rel: rel, blob: blob})
			return nil
		}); err != nil {
			return nil, err
		}
	}

	var warns []string
	for _, e := range entries {
		rel := filepath.FromSlash(e.rel)
		target := filepath.Join(dest, rel)
		if !contains(dest, target) {
			return nil, fmt.Errorf("archive entry %q escapes the plugin root", e.rel)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(target, e.blob, 0o644); err != nil {
			return nil, err
		}
	}
	if skipped > 0 {
		warns = append(warns, fmt.Sprintf("skipped %d archive entries outside the plugin surface (assets, other-client layouts, scaffolding)", skipped))
	}
	return warns, nil
}

// readBounded reads one entry's payload capped at declared+1 bytes and
// refuses size lies (review S1): a header declaring fewer bytes than the
// payload carries — or a stream ending early — is an error, never a silent
// truncation.
func readBounded(r io.Reader, declared int64) ([]byte, error) {
	blob, err := io.ReadAll(io.LimitReader(r, declared+1))
	if err != nil {
		return nil, err
	}
	if int64(len(blob)) != declared {
		return nil, fmt.Errorf("size mismatch: header declares %d bytes, payload is %d", declared, len(blob))
	}
	return blob, nil
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

func isZip(data []byte) bool {
	return len(data) > 3 && data[0] == 'P' && data[1] == 'K' && data[2] == 3 && data[3] == 4
}
