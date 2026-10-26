package local

// project_map (issue #324): a token-cheap structural map of a codebase —
// files grouped by directory with their symbols and signatures, never
// bodies. Go is outlined with go/ast (stdlib); JS/TS, Python, Java and Rust
// with heuristic scanners (project_map_langs.go); every other file is listed
// by name (file-tree degradation). A short non-code framing header names the
// project and its docs/build landmarks before the symbols.
//
// Budget first: the outline degrades gracefully to fit max_chars —
// signatures → names-only → reference-ranked names (Aider-style: the
// least-referenced symbols go first, see ADR-0081) → bare file tree — and
// textbudget is the hard backstop. Per-file summaries are cached in-process
// and on disk (~/.milk/outline-cache/) keyed by mtime+size, so repeated
// calls — in this session or the next — don't re-parse the tree.

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/textbudget"
)

const (
	projectMapDefaultMaxFiles = 200
	projectMapDefaultMaxChars = 6000
	projectMapFilesCap        = 1000  // hard cap regardless of max_files
	projectMapCharsCap        = 50000 // hard cap regardless of max_chars
	projectMapCacheMax        = 20000 // cached files before a wholesale reset
	projectMapIndexVersion    = 1
)

// projectMapBudget is a set of project_map default caps (files, chars) with
// zeros meaning "unwired — use the package constants".
type projectMapBudget struct{ files, chars int }

type projectMapBudgetKey struct{}

// withProjectMapBudget carries the run's derived project_map defaults on the
// ctx so dispatchTool's runProjectMap and the pushed projectMapContextBlock
// see the same numbers without threading them through schemas()' or
// dispatchTool's signatures (the WithImageParts precedent). A zero budget is
// a no-op: the ctx stays unwired and every consumer falls back to the
// constants.
func withProjectMapBudget(ctx context.Context, b projectMapBudget) context.Context {
	if b.files <= 0 && b.chars <= 0 {
		return ctx
	}
	return context.WithValue(ctx, projectMapBudgetKey{}, b)
}

// projectMapBudgetFrom returns the budget carried by ctx, if any.
func projectMapBudgetFrom(ctx context.Context) (projectMapBudget, bool) {
	b, ok := ctx.Value(projectMapBudgetKey{}).(projectMapBudget)
	return b, ok && b.files > 0 && b.chars > 0
}

// projectMapDefaultsFrom is the run's default caps: the ctx-derived budget
// when wired, the historical constants otherwise (bare New(), eval adapters
// and any Run path that never called WithProjectMapBudget).
func projectMapDefaultsFrom(ctx context.Context) (files, chars int) {
	if b, ok := projectMapBudgetFrom(ctx); ok {
		return b.files, b.chars
	}
	return projectMapDefaultMaxFiles, projectMapDefaultMaxChars
}

// projectMapDeriveBudget derives project_map's default caps from the agent's
// resolved context window (config.Config.AgentContextWindowTokens — explicit
// context_window_tokens, else the models.dev lookup). Growth-only: floored
// at the historical defaults so unknown and small windows behave exactly as
// before this derivation existed, capped at the tool's hard caps:
//
//	files = clamp(window/640, 200,  1000)
//	chars = clamp(window/6,   6000, 50000)
//
// The char floor (6000 ≈ 1.5k tokens) keeps tiny windows usable and the
// growth stays small relative to any window it derives from (~4% of window
// in tokens at a 128k window). window <= 0 yields the zero budget = unwired.
func projectMapDeriveBudget(window int) projectMapBudget {
	if window <= 0 {
		return projectMapBudget{}
	}
	return projectMapBudget{
		files: clampInt(window/640, projectMapDefaultMaxFiles, projectMapFilesCap),
		chars: clampInt(window/6, projectMapDefaultMaxChars, projectMapCharsCap),
	}
}

// applyProjectMapSchemaBudget rewrites project_map's parameter descriptions
// to state the defaults this run actually applies. schemas() is built without
// agent context, so the window-derived numbers are stamped on here when a
// budget is wired; unwired keeps the static text, which documents the same
// constants the run will use.
func applyProjectMapSchemaBudget(tools []map[string]any, b projectMapBudget) {
	if b.files <= 0 || b.chars <= 0 {
		return
	}
	for _, t := range tools {
		fn, _ := t["function"].(map[string]any)
		if name, _ := fn["name"].(string); name != "project_map" {
			continue
		}
		params, _ := fn["parameters"].(map[string]any)
		props, _ := params["properties"].(map[string]any)
		if pf, ok := props["max_files"].(map[string]any); ok {
			pf["description"] = fmt.Sprintf("Max files to include (default %d — scales with this model's context window; hard cap %d)", b.files, projectMapFilesCap)
		}
		if pc, ok := props["max_chars"].(map[string]any); ok {
			pc["description"] = fmt.Sprintf("Output budget in characters (default %d — scales with this model's context window; hard cap %d); the outline degrades to fit", b.chars, projectMapCharsCap)
		}
		return
	}
}

// projectMapSym is one outlined declaration: Name is the short identifier
// ("(*Store).Get"), Full the signature line ("func (s *Store) Get(key string)
// (string, error)"). Bodies never appear anywhere in either.
type projectMapSym struct {
	Name string `json:"n"`
	Full string `json:"f"`
}

// projectMapEntry is one cached per-file summary, keyed by mtime + size.
type projectMapEntry struct {
	modTime time.Time
	size    int64
	syms    []projectMapSym
}

// projectMapIndex is the on-disk cache (one JSON file per project root).
type projectMapIndex struct {
	Version int                          `json:"version"`
	Root    string                       `json:"root"`
	Files   map[string]projectMapIdxFile `json:"files"`
	dirty   bool                         `json:"-"`
}

type projectMapIdxFile struct {
	MTime int64           `json:"mtime"`
	Size  int64           `json:"size"`
	Syms  []projectMapSym `json:"syms,omitempty"`
}

var projectMapCache = struct {
	sync.Mutex
	m          map[string]projectMapEntry  // abs path -> entry
	roots      map[string]*projectMapIndex // project root -> disk index (loaded once)
	parseCalls int                         // test hook: how many files were parsed
}{m: map[string]projectMapEntry{}, roots: map[string]*projectMapIndex{}}

// projectMapFile is one file as rendered: rel path plus its symbols (empty
// for files no extractor handles) and how many symbols budget pruning
// dropped from it.
type projectMapFile struct {
	rel     string
	syms    []projectMapSym
	dropped int
}

func runProjectMap(ctx context.Context, args map[string]any, cwd string) (string, bool) {
	path, _ := args["path"].(string)
	if path == "" {
		path = "."
	}
	root := resolvePath(path, cwd)
	info, err := os.Stat(root)
	if err != nil {
		return toolResult{Error: err.Error()}.String(), false
	}

	// A single file is allowed: map just it, rooted at its directory.
	scopeFile := ""
	if !info.IsDir() {
		scopeFile = filepath.Base(root)
		root = filepath.Dir(root)
	}

	defFiles, defChars := projectMapDefaultsFrom(ctx)
	maxFiles := projectMapIntArg(args, "max_files", defFiles, projectMapFilesCap)
	maxChars := projectMapIntArg(args, "max_chars", defChars, projectMapCharsCap)

	out, ok := projectMapBuild(ctx, root, cwd, scopeFile, maxFiles, maxChars)
	if !ok {
		return toolResult{Output: "no files under " + root}.String(), false
	}
	return toolResult{Output: out}.String(), false
}

// projectMapBuild assembles the map text for root: enumerate (git first,
// walk fallback), optional scope filter (a base filename — single-file
// maps), framing over the full enumeration, max_files truncation, symbol
// resolution through the persistent index, the full → names →
// reference-ranked → tree degradation ladder, and the textbudget backstop.
// ok is false when nothing maps. Shared by the tool and the pushed
// project_map_context block (ADR-0081).
func projectMapBuild(ctx context.Context, root, cwd, scope string, maxFiles, maxChars int) (string, bool) {
	relFiles, ok := gitEnumerate(ctx, root)
	if !ok {
		relFiles = walkEnumerate(root)
	}
	if scope != "" {
		var one []string
		for _, rel := range relFiles {
			if rel == scope {
				one = append(one, rel)
				break
			}
		}
		relFiles = one
	}
	sort.Strings(relFiles)

	// Framing is computed over the full enumeration so it stays stable as
	// max_files prunes the symbol map below it.
	framing := projectMapFraming(root, relFiles)

	truncNote := ""
	if len(relFiles) > maxFiles {
		n := len(relFiles) - maxFiles
		word := "files"
		if n == 1 {
			word = "file"
		}
		truncNote = fmt.Sprintf("\n[%d more %s not shown — narrow with path= or raise max_files]", n, word)
		relFiles = relFiles[:maxFiles]
	}

	files := make([]projectMapFile, 0, len(relFiles))
	for _, rel := range relFiles {
		files = append(files, projectMapFile{rel: rel, syms: projectMapFileSymbols(root, rel)})
	}
	projectMapFlushIndex(root)

	if len(files) == 0 {
		return "", false
	}

	// Degrade to fit the budget; textbudget is the hard backstop.
	out := framing + projectMapRender(root, cwd, files, "full") + truncNote
	if len(out) > maxChars {
		out = framing + projectMapRender(root, cwd, files, "names") + truncNote
	}
	if len(out) > maxChars {
		// Aider-style ranking: drop the least-referenced symbols first
		// instead of truncating the file list, then fit.
		ranked := framing + projectMapRenderRanked(root, cwd, files, maxChars-len(framing)-len(truncNote))
		if len(ranked) <= maxChars {
			out = ranked
		} else {
			out = framing + projectMapRender(root, cwd, files, "tree") + truncNote
		}
	}
	out = textbudget.SummarizeLong(out, maxChars)
	return out, true
}

// projectMapIntArg reads a positive integer tool argument (JSON numbers
// arrive as float64), falling back to def and clamped to max.
func projectMapIntArg(args map[string]any, key string, def, max int) int {
	v := def
	switch n := args[key].(type) {
	case float64:
		if n > 0 {
			v = int(n)
		}
	case int:
		if n > 0 {
			v = n
		}
	}
	if v > max {
		return max
	}
	return v
}

// projectMapRender draws the outline in one of three modes: "full"
// (signatures), "names" (identifiers only), "tree" (bare file names).
// Files with a dropped symbol count (pruning) show `…+N` after their names.
func projectMapRender(root, cwd string, files []projectMapFile, mode string) string {
	var b strings.Builder
	curDir := ""
	for _, f := range files {
		if dir := filepath.Dir(f.rel); dir != curDir {
			curDir = dir
			b.WriteString(projectMapDirLabel(root, cwd, dir) + "/\n")
		}
		b.WriteString("  " + filepath.Base(f.rel))
		summary := ""
		switch mode {
		case "full":
			summary = projectMapJoinFull(f.syms)
		case "names":
			summary = projectMapJoinNames(f.syms)
		}
		if summary != "" {
			b.WriteString("  " + summary)
		}
		if f.dropped > 0 && mode != "tree" {
			b.WriteString(fmt.Sprintf(" …+%d", f.dropped))
		}
		b.WriteString("\n")
	}
	switch mode {
	case "names":
		b.WriteString("[signatures omitted to fit the budget — narrow with path= or raise max_chars]\n")
	case "tree":
		b.WriteString("[symbols omitted to fit the budget — narrow with path= or raise max_chars]\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func projectMapJoinFull(syms []projectMapSym) string {
	parts := make([]string, 0, len(syms))
	for _, s := range syms {
		parts = append(parts, s.Full)
	}
	return strings.Join(parts, "; ")
}

func projectMapJoinNames(syms []projectMapSym) string {
	parts := make([]string, 0, len(syms))
	for _, s := range syms {
		parts = append(parts, s.Name)
	}
	return strings.Join(parts, ", ")
}

// projectMapRenderRanked is the names mode with least-referenced symbols
// pruned first (the Aider repo-map idea, reduced: per-symbol cross-file
// reference counts instead of a dependency-graph PageRank — see ADR-0081).
// Pruning proceeds in ascending reference order until the outline fits the
// budget; each file shows `…+N` for what it lost.
func projectMapRenderRanked(root, cwd string, files []projectMapFile, budget int) string {
	pruned := projectMapPrune(root, files, budget-len("[least-referenced symbols omitted to fit the budget — narrow with path= or raise max_chars]"))
	out := projectMapRender(root, cwd, pruned, "names")
	out = strings.TrimSuffix(out, "[signatures omitted to fit the budget — narrow with path= or raise max_chars]")
	out += "[least-referenced symbols omitted to fit the budget — narrow with path= or raise max_chars]"
	return out
}

// projectMapPrune drops symbols in ascending cross-file reference order
// (ties broken by file order then declaration order, so it is deterministic)
// until the names-mode outline fits budget. Files keep at least one symbol.
func projectMapPrune(root string, files []projectMapFile, budget int) []projectMapFile {
	refs := projectMapRefCounts(root, files)

	type cand struct{ file, sym int }
	var order []cand
	for i, f := range files {
		for j := range f.syms {
			order = append(order, cand{i, j})
		}
	}
	sort.SliceStable(order, func(a, b int) bool {
		ra, rb := refs[projectMapRankKey(files[order[a].file].syms[order[a].sym])], refs[projectMapRankKey(files[order[b].file].syms[order[b].sym])]
		if ra != rb {
			return ra < rb
		}
		if order[a].file != order[b].file {
			return order[a].file < order[b].file
		}
		return order[a].sym > order[b].sym // drop later decls first
	})

	out := make([]projectMapFile, len(files))
	copy(out, files)
	length := func() int {
		total := 0
		for _, f := range out {
			total += 2 + len(f.rel) + 1 + len(projectMapJoinNames(f.syms)) + 1
		}
		return total
	}
	for _, c := range order {
		if length() <= budget {
			break
		}
		f := &out[c.file]
		if len(f.syms) <= 1 {
			continue // keep one symbol per file — an empty file line is a loss
		}
		f.syms = append(f.syms[:c.sym], f.syms[c.sym+1:]...)
		f.dropped++
		// Indices above shift down by one; mark the tail so later picks
		// don't mis-drop. Recomputing keeps this honest and cheap enough.
		for k := range order {
			if order[k].file == c.file && order[k].sym > c.sym {
				order[k].sym--
			}
		}
	}
	return out
}

// projectMapRankKey reduces a symbol to the identifier whose references
// count: `(*Store).Get` → `Get`, `impl Foo` → `Foo`.
func projectMapRankKey(s projectMapSym) string {
	name := s.Name
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	if i := strings.LastIndex(name, " "); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// projectMapRefCounts counts, for every symbol identifier, how many *other*
// files in the map reference it as a word. Computed lazily — only when the
// budget forces ranked pruning — in one identifier-frequency pass per file
// (≤256 KB each), so the whole rank is O(bytes + symbols·files). It is a
// heuristic rank, not a call graph: good enough to decide what the map can
// afford to drop.
func projectMapRefCounts(root string, files []projectMapFile) map[string]int {
	const maxRead = 256 << 10
	identRe := regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)
	counts := make([]map[string]int, len(files))
	for i, f := range files {
		b, err := os.ReadFile(filepath.Join(root, f.rel))
		if err != nil || len(b) > maxRead {
			continue
		}
		m := map[string]int{}
		for _, id := range identRe.FindAll(b, -1) {
			m[string(id)]++
		}
		counts[i] = m
	}
	refs := map[string]int{}
	for i, f := range files {
		for _, s := range f.syms {
			key := projectMapRankKey(s)
			if key == "" {
				continue
			}
			n := 0
			for j := range counts {
				if j != i {
					n += counts[j][key]
				}
			}
			refs[key] = n
		}
	}
	return refs
}

// projectMapDirLabel renders a directory header: subdirs as their slash rel
// path; the scope root itself as the root path relative to the session cwd
// when that is shorter/clearer than the absolute path.
func projectMapDirLabel(root, cwd, dir string) string {
	if dir != "." {
		return filepath.ToSlash(dir)
	}
	label := root
	if cwd != "" {
		if rel, err := filepath.Rel(cwd, root); err == nil && !strings.HasPrefix(rel, "..") {
			label = rel
		}
	}
	return filepath.ToSlash(label)
}

// --- pushed context (project_map_context, ADR-0081) -----------------------

// projectMapMinimalBlockMaxChars bounds the minimal orientation block:
// a huge flat tree must not turn "minimal" into "full".
const projectMapMinimalBlockMaxChars = 1200

// projectMapContextBlock builds the pushed orientation block for the
// project_map_context mode (ADR-0081 amendment): "off" pushes nothing
// (tool-only), ""/"minimal" push the framing header + top-level directory
// shape, and "full" pushes the budgeted symbol map rebuilt per turn (à la
// Aider's repo map). toolRegistered controls the closing pointer at
// project_map — never advertise a tool the agent doesn't have (issue #172).
// The block is plain prompt content; it never carries symbol bodies.
func projectMapContextBlock(ctx context.Context, root, cwd string, mode config.ProjectMapMode, toolRegistered bool) string {
	switch mode {
	case config.ProjectMapOff:
		return ""
	case config.ProjectMapFull:
		defFiles, defChars := projectMapDefaultsFrom(ctx)
		out, ok := projectMapBuild(ctx, root, cwd, "", defFiles, defChars)
		if !ok {
			return ""
		}
		return "Project map (as of this turn's start; call project_map to narrow by path):\n" + out
	default: // "", minimal
		relFiles, ok := gitEnumerate(ctx, root)
		if !ok {
			relFiles = walkEnumerate(root)
		}
		if len(relFiles) == 0 {
			return ""
		}
		sort.Strings(relFiles)
		var b strings.Builder
		b.WriteString("Project orientation (as of this turn's start):\n")
		b.WriteString(projectMapFraming(root, relFiles))
		b.WriteString(projectMapTopTree(relFiles))
		if toolRegistered {
			b.WriteString("\nFor symbols and signatures call project_map (cheap, cached) — never read files just to learn what they define.")
		}
		return textbudget.SummarizeLong(b.String(), projectMapMinimalBlockMaxChars)
	}
}

// projectMapTopTree renders the top-level shape: one entry per top-level
// directory with its file count, then top-level files — capped so a flat
// 500-entry directory can't blow the block up.
func projectMapTopTree(relFiles []string) string {
	dirs := map[string]int{}
	var files []string
	for _, rel := range relFiles {
		if i := strings.IndexByte(rel, '/'); i >= 0 {
			dirs[rel[:i]]++
		} else {
			files = append(files, rel)
		}
	}
	items := make([]string, 0, len(dirs)+len(files))
	dnames := make([]string, 0, len(dirs))
	for d := range dirs {
		dnames = append(dnames, d)
	}
	sort.Strings(dnames)
	for _, d := range dnames {
		items = append(items, fmt.Sprintf("%s/ (%d files)", d, dirs[d]))
	}
	items = append(items, files...)
	const maxEntries = 24
	hidden := 0
	if len(items) > maxEntries {
		hidden = len(items) - maxEntries
		items = items[:maxEntries]
	}
	out := "top level: " + strings.Join(items, ", ")
	if hidden > 0 {
		out += fmt.Sprintf(", …+%d more", hidden)
	}
	return out + "\n"
}

// --- non-code framing ------------------------------------------------------

var (
	projectMapDocDirs     = map[string]bool{"docs": true, "doc": true, "documentation": true}
	projectMapConfigNames = map[string]bool{
		"go.mod": true, "go.sum": true, "package.json": true, "package-lock.json": true,
		"pyproject.toml": true, "cargo.toml": true, "cargo.lock": true,
		"pom.xml": true, "build.gradle": true, "build.gradle.kts": true, "settings.gradle": true,
		"makefile": true, "taskfile.yml": true, "taskfile.yaml": true, "justfile": true,
		"dockerfile": true, "docker-compose.yml": true, "docker-compose.yaml": true,
		"mkdocs.yml": true, "tsconfig.json": true, "jsconfig.json": true,
		"requirements.txt": true, "setup.py": true, "setup.cfg": true,
		"eslint.config.js": true, ".eslintrc.json": true, ".eslintrc.cjs": true,
		"vitest.config.ts": true, "jest.config.js": true, "flake.nix": true,
	}
)

// projectMapFraming is the map's orientation header: who the project is
// (manifest identity) and where its non-code landmarks live (docs, build and
// config files), so an agent sees the project's shape beyond symbols.
func projectMapFraming(root string, relFiles []string) string {
	var b strings.Builder
	if name, lang := projectMapIdentity(root); name != "" {
		b.WriteString("project: " + name)
		if lang != "" {
			b.WriteString(" (" + lang + ")")
		}
		b.WriteString("\n")
	}

	var docs, configs []string
	for _, rel := range relFiles {
		dir := filepath.Dir(rel)
		base := strings.ToLower(filepath.Base(rel))
		switch {
		case dir == "." && strings.HasSuffix(base, ".md"):
			docs = append(docs, rel)
		case projectMapDocDirs[filepath.ToSlash(dir)] || strings.HasPrefix(filepath.ToSlash(dir), "docs/"):
			docs = append(docs, rel)
		case projectMapConfigNames[base] || strings.HasPrefix(filepath.ToSlash(dir), ".github/"):
			configs = append(configs, rel)
		}
	}
	writeGroup := func(label string, paths []string) {
		if len(paths) == 0 {
			return
		}
		sort.Strings(paths)
		const max = 8
		shown := paths
		extra := ""
		if len(paths) > max {
			shown = paths[:max]
			extra = fmt.Sprintf(", …+%d", len(paths)-max)
		}
		b.WriteString(label + ": " + strings.Join(shown, ", ") + extra + "\n")
	}
	writeGroup("docs", docs)
	writeGroup("build/config", configs)
	return b.String()
}

// projectMapIdentity finds the project's name and language from the first
// known manifest at the scope root.
func projectMapIdentity(root string) (name, lang string) {
	type probe struct{ file, lang string }
	for _, p := range []probe{{"go.mod", "go"}, {"package.json", "js"}, {"Cargo.toml", "rust"}, {"pyproject.toml", "python"}} {
		b, err := os.ReadFile(filepath.Join(root, p.file))
		if err != nil || len(b) > 64<<10 {
			continue
		}
		switch p.file {
		case "go.mod":
			for _, line := range strings.Split(string(b), "\n") {
				if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
					return strings.TrimSpace(rest), p.lang
				}
			}
		case "package.json":
			var pj struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(b, &pj) == nil && pj.Name != "" {
				return pj.Name, p.lang
			}
		default:
			if m := regexp.MustCompile(`(?m)^\s*name\s*=\s*"([^"]+)"`).FindSubmatch(b); m != nil {
				return string(m[1]), p.lang
			}
		}
	}
	return "", ""
}

// --- per-file extraction + caching -----------------------------------------

// projectMapFileSymbols returns a file's symbol summary, from cache when the
// file is unchanged (mtime + size), else by parsing. The cache lives in
// memory and on disk, so a fresh session on an unchanged tree parses nothing.
func projectMapFileSymbols(root, rel string) []projectMapSym {
	abs := filepath.Join(root, rel)
	info, err := os.Stat(abs)
	if err != nil {
		return nil
	}
	projectMapCache.Lock()
	defer projectMapCache.Unlock()
	projectMapIndexLocked(root)
	if e, ok := projectMapCache.m[abs]; ok && e.modTime.Equal(info.ModTime()) && e.size == info.Size() {
		return e.syms
	}
	syms := projectMapParse(abs, filepath.Ext(rel))
	e := projectMapEntry{modTime: info.ModTime(), size: info.Size(), syms: syms}
	if len(projectMapCache.m) >= projectMapCacheMax {
		projectMapCache.m = map[string]projectMapEntry{}
	}
	projectMapCache.m[abs] = e
	if idx := projectMapCache.roots[root]; idx != nil {
		idx.Files[rel] = projectMapIdxFile{MTime: info.ModTime().UnixNano(), Size: info.Size(), Syms: syms}
		idx.dirty = true
	}
	projectMapCache.parseCalls++
	return syms
}

// projectMapParse extracts declaration summaries for one file. Go goes
// through go/ast; JS/TS, Python, Java and Rust through the heuristic
// scanners; anything else yields no symbols (file-tree listing). A parse
// failure degrades the file to a bare listing rather than failing the map.
func projectMapParse(abs, ext string) []projectMapSym {
	ext = strings.ToLower(ext)
	if ext != ".go" {
		src, err := os.ReadFile(abs)
		if err != nil {
			return nil
		}
		switch ext {
		case ".py":
			return projectMapExtractPython(src)
		case ".js", ".jsx", ".mjs", ".cjs", ".ts", ".tsx", ".mts", ".cts":
			return projectMapExtractJS(src)
		case ".java":
			return projectMapExtractJava(src)
		case ".rs":
			return projectMapExtractRust(src)
		}
		return nil
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, abs, nil, 0)
	if err != nil || f == nil {
		return nil
	}
	var out []projectMapSym
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			out = append(out, projectMapSym{Name: projectMapFuncShort(d), Full: projectMapFuncSig(d)})
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					out = append(out, projectMapSym{Name: s.Name.Name, Full: projectMapTypeSig(s)})
				case *ast.ValueSpec:
					var ns []string
					for _, n := range s.Names {
						if n.Name != "_" {
							ns = append(ns, n.Name)
						}
					}
					if len(ns) == 0 {
						continue
					}
					kind := "var"
					if d.Tok == token.CONST {
						kind = "const"
					}
					out = append(out, projectMapSym{Name: strings.Join(ns, ", "), Full: kind + " " + strings.Join(ns, ", ")})
				}
			}
		}
	}
	return out
}

// projectMapFuncSig renders a function/method signature without its body:
// `func (s *Store) Get(key string) (string, error)`.
func projectMapFuncSig(fn *ast.FuncDecl) string {
	var b strings.Builder
	b.WriteString("func ")
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		b.WriteString("(" + projectMapFieldList(fn.Recv) + ") ")
	}
	b.WriteString(fn.Name.Name)
	b.WriteString("(" + projectMapFieldList(fn.Type.Params) + ")")
	if fn.Type.Results != nil && len(fn.Type.Results.List) > 0 {
		res := projectMapFieldList(fn.Type.Results)
		if len(fn.Type.Results.List) > 1 || strings.Contains(res, ", ") {
			b.WriteString(" (" + res + ")")
		} else {
			b.WriteString(" " + res)
		}
	}
	return b.String()
}

// projectMapFuncShort is the names-mode form: `Load` or `(*Store).Get`.
func projectMapFuncShort(fn *ast.FuncDecl) string {
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		return "(" + types.ExprString(fn.Recv.List[0].Type) + ")." + fn.Name.Name
	}
	return fn.Name.Name
}

// projectMapFieldList renders a parameter/result list: types only where
// unnamed, `name Type` otherwise (names help orientation, bodies never).
func projectMapFieldList(fl *ast.FieldList) string {
	if fl == nil {
		return ""
	}
	var parts []string
	for _, f := range fl.List {
		t := types.ExprString(f.Type)
		if len(f.Names) == 0 {
			parts = append(parts, t)
			continue
		}
		var ns []string
		for _, n := range f.Names {
			ns = append(ns, n.Name)
		}
		parts = append(parts, strings.Join(ns, ", ")+" "+t)
	}
	return strings.Join(parts, ", ")
}

// projectMapTypeSig renders a type declaration: `type Config struct`,
// `type Handler interface`, `type Fn func(string) error`.
func projectMapTypeSig(ts *ast.TypeSpec) string {
	name := "type " + ts.Name.Name
	switch ts.Type.(type) {
	case *ast.StructType:
		return name + " struct"
	case *ast.InterfaceType:
		return name + " interface"
	default:
		return name + " " + types.ExprString(ts.Type)
	}
}

// --- persistent index ------------------------------------------------------

// projectMapIndexLocked loads (once) the on-disk index for root into the
// cache. Disk entries are trusted only when version and root match; any
// other error just means "start fresh" — the index is an optimization, and
// the mtime+size check re-verifies every file anyway.
func projectMapIndexLocked(root string) *projectMapIndex {
	if idx, ok := projectMapCache.roots[root]; ok {
		return idx
	}
	idx := &projectMapIndex{Version: projectMapIndexVersion, Root: root, Files: map[string]projectMapIdxFile{}}
	if b, err := os.ReadFile(projectMapIndexPath(root)); err == nil {
		var disk projectMapIndex
		if json.Unmarshal(b, &disk) == nil && disk.Version == projectMapIndexVersion && disk.Root == root {
			for rel, f := range disk.Files {
				idx.Files[rel] = f
				abs := filepath.Join(root, rel)
				if _, ok := projectMapCache.m[abs]; !ok {
					projectMapCache.m[abs] = projectMapEntry{
						modTime: time.Unix(0, f.MTime),
						size:    f.Size,
						syms:    f.Syms,
					}
				}
			}
		}
	}
	projectMapCache.roots[root] = idx
	return idx
}

// projectMapFlushIndex writes the index back if any file was (re)parsed
// this call. Atomic (temp + rename), like every other milk state file.
func projectMapFlushIndex(root string) {
	projectMapCache.Lock()
	defer projectMapCache.Unlock()
	idx := projectMapCache.roots[root]
	if idx == nil || !idx.dirty {
		return
	}
	idx.dirty = false
	b, err := json.Marshal(idx)
	if err != nil {
		return
	}
	p := projectMapIndexPath(root)
	if p == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, p)
}

// projectMapIndexPath names the per-project index file: the folder-bound
// `<base>-<hash>.json` convention (ADR-0078) under ~/.milk/outline-cache/.
// Returns "" when there is no state root — persistence then silently stays
// off and the in-memory cache still works.
func projectMapIndexPath(root string) string {
	dir, err := config.Dir()
	if err != nil {
		return ""
	}
	base := projectMapSanitizeName(filepath.Base(root))
	return filepath.Join(dir, "outline-cache", base+"-"+session.CWDHash(root)+".json")
}

// projectMapSanitizeName keeps a filesystem-safe lowercase stem.
func projectMapSanitizeName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "project"
	}
	if b.Len() > 32 {
		return b.String()[:32]
	}
	return b.String()
}
