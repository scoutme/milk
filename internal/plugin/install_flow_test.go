package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const claudeLayout = `{
  "name": "minicut",
  "version": "0.1.3",
  "description": "AI film studio",
  "author": "lliu56",
  "keywords": ["media"],
  "mcpServers": {
    "minicut": {"command": "node", "args": ["${PLUGIN_ROOT}/mcp/index.js"], "env": {"MODE": "x"}},
    "remote": {"type": "http", "url": "https://m.example.com/rpc", "headers": {"Authorization": "Bearer t"}}
  }
}`

func TestWriteShim_FromClaudeManifest(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, filepath.Join(".claude-plugin", "plugin.json"), claudeLayout)
	if !NeedsShim(dir) {
		t.Fatal("claude layout needs a shim")
	}
	created, warns, err := WriteShim(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 0 {
		t.Errorf("warns = %v", warns)
	}
	if len(created) != 2 {
		t.Errorf("created = %v want plugin.json + mcp.json", created)
	}
	if NeedsShim(dir) {
		t.Error("shimmed dir must not need another shim")
	}

	raw, err := os.ReadFile(filepath.Join(dir, "plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	m, _, err := ParseManifest(raw)
	if err != nil {
		t.Fatalf("shimmed plugin.json does not parse: %v", err)
	}
	if m.Name != "minicut" || m.Version != "0.1.3" || m.Author.Name != "lliu56" {
		t.Errorf("manifest = %+v", m)
	}

	mcpRaw, err := os.ReadFile(filepath.Join(dir, "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var mcp map[string]any
	if err := json.Unmarshal(mcpRaw, &mcp); err != nil {
		t.Fatal(err)
	}
	servers := mcp["mcpServers"].(map[string]any)
	st := servers["minicut"].(map[string]any)
	if st["type"] != "stdio" || st["command"] != "node" {
		t.Errorf("stdio entry = %v", st)
	}
	rm := servers["remote"].(map[string]any)
	if rm["type"] != "streamable-http" {
		t.Errorf("remote entry = %v", rm)
	}
}

func TestWriteShim_CreateOnly(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, filepath.Join(".claude-plugin", "plugin.json"), claudeLayout)
	writePlugin(t, dir, "plugin.json", minManifest)
	if NeedsShim(dir) {
		t.Error("portable dir must not need a shim")
	}
	created, _, err := WriteShim(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 0 {
		t.Errorf("created = %v want none (create-only)", created)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "plugin.json"))
	if string(raw) != minManifest {
		t.Error("existing plugin.json must be untouched")
	}
}

func TestWriteShim_ClaudeMCPFile(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, filepath.Join(".claude-plugin", "plugin.json"), `{"name":"x","version":"1"}`)
	writePlugin(t, dir, filepath.Join(".claude-plugin", "mcp.json"),
		`{"mcpServers":{"srv":{"command":"./run"}}}`)
	created, _, err := WriteShim(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 2 {
		t.Fatalf("created = %v", created)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "mcp.json"))
	if !strings.Contains(string(raw), `"command": "./run"`) {
		t.Errorf("mcp.json = %s", raw)
	}
}

func TestParseMarketplace_RealShape(t *testing.T) {
	const market = `{
  "name": "minicut",
  "owner": {"name": "lliu56"},
  "plugins": [
    {"name": "minicut-skills", "source": "./", "description": "d", "version": "0.1.3", "category": "media", "keywords": ["video"]},
    {"name": "other", "source": {"source": "github", "repo": "a/b", "ref": "main"}},
    {"name": "bad", "source": {"source": "command", "command": "x"}},
    {"name": "nosrc"},
    {"source": "./orphan"}
  ]
}`
	mp, warns, err := ParseMarketplace([]byte(market))
	if err != nil {
		t.Fatal(err)
	}
	if mp.Name != "minicut" || mp.Owner != "lliu56" || len(mp.Plugins) != 2 {
		t.Fatalf("mp = %+v warns=%v", mp, warns)
	}
	if len(warns) != 3 {
		t.Errorf("warns = %v want 3 skipped entries (command, missing source, missing name)", warns)
	}
	ep, ok := mp.Entry("minicut-skills")
	if !ok || ep.Source.Kind != SourceRelative || ep.Version != "0.1.3" {
		t.Errorf("entry = %+v ok=%v", ep, ok)
	}
	ep2, ok := mp.Entry("other")
	if !ok || ep2.Source.Kind != SourceGitHub || ep2.Source.Ref != "main" {
		t.Errorf("entry = %+v ok=%v", ep2, ok)
	}
	if _, ok := mp.Entry("bad"); ok {
		t.Error("command-source entry must be dropped")
	}
}

func TestLoadMarketplaceDir(t *testing.T) {
	for _, rel := range []string{filepath.Join(".claude-plugin", "marketplace.json"), "marketplace.json"} {
		dir := t.TempDir()
		writePlugin(t, dir, rel, `{"name":"m","plugins":[{"name":"p","source":"./x"}]}`)
		mp, _, err := LoadMarketplaceDir(dir)
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		if mp.Name != "m" || len(mp.Plugins) != 1 {
			t.Errorf("%s: mp = %+v", rel, mp)
		}
	}
	if _, _, err := LoadMarketplaceDir(t.TempDir()); err == nil {
		t.Error("missing marketplace.json must error")
	}
}

// claudePackage is a codeload-wrapped repo holding a pre-portable layout.
var claudePackage = map[string]string{
	"pkg-abc123/.claude-plugin/plugin.json": claudeLayout,
	"pkg-abc123/skills/film/SKILL.md":       "---\nname: film\ndescription: d\n---\nbody",
	"pkg-abc123/assets/big.mp4":             "junk",
}

func TestInstallPlugin_EndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(makeTarGz(t, claudePackage))
	}))
	defer srv.Close()
	client := &http.Client{Transport: rewriteTransport{srv.Listener.Addr().String()}}

	pluginsDir := filepath.Join(t.TempDir(), "plugins")
	dataRoot := filepath.Join(t.TempDir(), "plugin-data")
	in, err := LoadInstalls(filepath.Join(t.TempDir(), "plugin-installs.json"))
	if err != nil {
		t.Fatal(err)
	}
	src := Source{Kind: SourceGitHub, Repo: "lliu56/minicut-skills", Ref: "main"}
	p, warns, err := InstallPlugin(context.Background(), client, src, "lliu56/minicut-skills", pluginsDir, dataRoot, in)
	if err != nil {
		t.Fatal(err)
	}
	if p.Manifest.Name != "minicut" {
		t.Errorf("installed name = %q", p.Manifest.Name)
	}
	if len(p.Skills) != 1 {
		t.Errorf("skills = %d", len(p.Skills))
	}
	if !strings.Contains(strings.Join(warns, "\n"), "skipped 1") {
		t.Errorf("warns = %v want asset skip reported", warns)
	}

	rec := in.Records["minicut"]
	if !rec.Shimmed || rec.Kind != "github" || rec.Ref != "main" || rec.Version != "0.1.3" {
		t.Errorf("record = %+v", rec)
	}
	// The bookkeeping file landed on disk.
	if _, err := os.Stat(in.Path); err != nil {
		t.Errorf("plugin-installs.json missing: %v", err)
	}
	// Shim output installed, asset filtered.
	if _, err := os.Stat(filepath.Join(pluginsDir, "minicut", "plugin.json")); err != nil {
		t.Errorf("shimmed manifest missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(pluginsDir, "minicut", "assets")); err == nil {
		t.Error("asset dir must not install")
	}

	// PLUGIN_DATA survives removal (§9.1: data, not install).
	writePlugin(t, dataRoot, filepath.Join("minicut", "state.json"), "{}")
	if err := RemovePlugin("minicut", pluginsDir, in); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(pluginsDir, "minicut")); err == nil {
		t.Error("plugin dir must be gone")
	}
	if _, err := os.Stat(filepath.Join(dataRoot, "minicut", "state.json")); err != nil {
		t.Errorf("PLUGIN_DATA must survive removal: %v", err)
	}
	if _, ok := in.Records["minicut"]; ok {
		t.Error("record must be gone")
	}
	if err := RemovePlugin("minicut", pluginsDir, in); err == nil {
		t.Error("removing an unrecorded plugin must error")
	}
}

func TestInstallFromDir_RelativeMarketplaceSource(t *testing.T) {
	mkt := t.TempDir()
	writePlugin(t, mkt, filepath.Join("plugins", "foo", "plugin.json"), minManifest)
	writePlugin(t, mkt, filepath.Join("plugins", "foo", "skills", "s", "SKILL.md"), "---\nname: s\ndescription: d\n---\nb")

	pluginsDir := filepath.Join(t.TempDir(), "plugins")
	in, _ := LoadInstalls(filepath.Join(t.TempDir(), "installs.json"))
	src := Source{Kind: SourceRelative, Path: "./plugins/foo"}
	p, _, err := InstallFromDir(filepath.Join(mkt, "plugins", "foo"), "minicut-market:foo", src, pluginsDir, t.TempDir(), in)
	if err != nil {
		t.Fatal(err)
	}
	if p.Manifest.Name != "my-plugin" || len(p.Skills) != 1 {
		t.Errorf("plugin = %s skills=%d", p.Manifest.Name, len(p.Skills))
	}
	if in.Records["my-plugin"].Kind != "relative" {
		t.Errorf("record = %+v", in.Records["my-plugin"])
	}
}
