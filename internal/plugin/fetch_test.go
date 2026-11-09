package plugin

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rewriteTransport sends every request to a test server — the fetchers
// hardcode their real endpoints (api.github.com, registry.npmjs.org) and the
// tests speak those protocols against fixtures.
type rewriteTransport struct{ target string }

func (r rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = "http"
	req.URL.Host = r.target
	return http.DefaultTransport.RoundTrip(req)
}

func makeTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func makeZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// codeload-style payload: one wrapper dir over the surface plus junk.
var fetchPayload = map[string]string{
	"owner-repo-sha/plugin.json":                minManifest,
	"owner-repo-sha/mcp.json":                   `{"$schema":"` + SchemaMCP + `","mcpServers":{}}`,
	"owner-repo-sha/skills/x/SKILL.md":          "---\nname: x\ndescription: d\n---\nbody",
	"owner-repo-sha/.claude-plugin/plugin.json": `{"name":"my-plugin"}`,
	"owner-repo-sha/assets/video.mp4":           "BINARY",
	"owner-repo-sha/.codex-plugin/plugin.json":  `{"name":"my-plugin"}`,
	"owner-repo-sha/README.md":                  "hi",
}

func TestFetchTo_TarGzSurfaceFilter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(makeTarGz(t, fetchPayload))
	}))
	defer srv.Close()
	client := &http.Client{Transport: rewriteTransport{srv.Listener.Addr().String()}}

	dest := filepath.Join(t.TempDir(), "out")
	warns, err := FetchTo(context.Background(), client, Source{Kind: SourceGitHub, Repo: "a/b"}, dest)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"plugin.json", "mcp.json", filepath.Join("skills", "x", "SKILL.md")} {
		if _, err := os.Stat(filepath.Join(dest, want)); err != nil {
			t.Errorf("missing kept entry %s", want)
		}
	}
	for _, skip := range []string{filepath.Join("assets", "video.mp4"), filepath.Join(".codex-plugin", "plugin.json"), "README.md"} {
		if _, err := os.Stat(filepath.Join(dest, skip)); err == nil {
			t.Errorf("junk entry extracted: %s", skip)
		}
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "skipped 3") {
		t.Errorf("warns = %v want one aggregated skip report", warns)
	}
}

func TestFetchTo_Zip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(makeZip(t, fetchPayload))
	}))
	defer srv.Close()
	client := &http.Client{Transport: rewriteTransport{srv.Listener.Addr().String()}}

	dest := filepath.Join(t.TempDir(), "out")
	if _, err := FetchTo(context.Background(), client, Source{Kind: SourceURL, URL: "http://plugin.test/p.zip"}, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "plugin.json")); err != nil {
		t.Errorf("zip: wrapper not stripped: %v", err)
	}
}

func TestFetchTo_EscapeEntriesNeverExtracted(t *testing.T) {
	// Escape-named entries are neutralized before keep() even sees them:
	// keepSurface cleans the name and matches surface prefixes, so ".." paths
	// fall out of the filter; keep()'s contains() guard stays as defense in
	// depth for anything the filter might let through.
	// The escapes clamp to root-level names; the legit file is kept flat so
	// there is no wrapper to strip (order-independent wrapper detection).
	payload := map[string]string{"plugin.json": minManifest, "w/../evil.txt": "x", "skills/../../evil2.txt": "x"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(makeTarGz(t, payload))
	}))
	defer srv.Close()
	client := &http.Client{Transport: rewriteTransport{srv.Listener.Addr().String()}}

	base := t.TempDir()
	dest := filepath.Join(base, "out")
	warns, err := FetchTo(context.Background(), client, Source{Kind: SourceURL, URL: "http://plugin.test/p"}, dest)
	if err != nil {
		t.Fatal(err)
	}
	for _, evil := range []string{filepath.Join(base, "evil.txt"), filepath.Join(base, "evil2.txt"), filepath.Join(dest, "evil.txt")} {
		if _, err := os.Stat(evil); err == nil {
			t.Errorf("escape entry extracted at %s", evil)
		}
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "skipped 2") {
		t.Errorf("warns = %v want aggregated skip of the 2 escape entries", warns)
	}
}

func TestFetchTo_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	client := &http.Client{Transport: rewriteTransport{srv.Listener.Addr().String()}}
	if _, err := FetchTo(context.Background(), client, Source{Kind: SourceURL, URL: "http://plugin.test/p"}, t.TempDir()); err == nil {
		t.Fatal("HTTP 404 must error")
	}
}

func TestFetchTo_NPM(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/latest"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"dist": map[string]any{"tarball": "http://" + srvURL(r) + "/tarball.tgz"},
			})
		default:
			_, _ = w.Write(makeTarGz(t, map[string]string{
				"package/plugin.json":             minManifest,
				"package/skills/y/SKILL.md":       "---\nname: y\ndescription: d\n---\nb",
				"package/node_modules/z/index.js": "junk",
			}))
		}
	}))
	defer srv.Close()
	client := &http.Client{Transport: rewriteTransport{srv.Listener.Addr().String()}}

	dest := filepath.Join(t.TempDir(), "out")
	if _, err := FetchTo(context.Background(), client, Source{Kind: SourceNPM, Package: "@x/y"}, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "plugin.json")); err != nil {
		t.Errorf("npm: package/ wrapper not stripped: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "node_modules")); err == nil {
		t.Error("npm: node_modules must be filtered out")
	}
}

func srvURL(r *http.Request) string { return r.Host }

func TestArchiveEndpoints(t *testing.T) {
	got, err := archiveEndpoint(Source{Kind: SourceGitSubdir, URL: "https://github.com/a/b.git", Path: "p", Ref: "main"})
	if err != nil || got != "https://api.github.com/repos/a/b/tarball/main" {
		t.Errorf("github endpoint = %q, %v", got, err)
	}
	got, err = archiveEndpoint(Source{Kind: SourceGitSubdir, URL: "https://gitlab.com/g/m", Path: "p", SHA: "abc"})
	if err != nil || got != "https://gitlab.com/g/m/-/archive/abc/m-abc.tar.gz" {
		t.Errorf("gitlab endpoint = %q, %v", got, err)
	}
	if _, err := archiveEndpoint(Source{Kind: SourceGitSubdir, URL: "https://codeberg.org/a/b"}); err == nil {
		t.Error("unknown host must report loudly, never fall back to git")
	}
}
