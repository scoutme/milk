package theme

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTheme(t *testing.T, dir, file, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryBuiltins(t *testing.T) {
	r := New()
	names := r.Names()
	if len(names) != 2 || names[0] != "off" || names[1] != "supermilk" {
		t.Fatalf("Names() = %v", names)
	}
	if _, ok := r.Get("SUPERMILK"); !ok {
		t.Fatal("Get must be case-insensitive")
	}
	if _, ok := r.Get(""); ok {
		t.Fatal("empty name must not resolve")
	}
	if src, _ := r.Source("off"); src != SourceBuiltin {
		t.Errorf("off source = %s", src)
	}
}

func TestRegistryLoadAndShadowing(t *testing.T) {
	user := t.TempDir()
	project := t.TempDir()
	writeTheme(t, user, "oatmilk.json", `{
		"name": "oatmilk",
		"extends": "supermilk",
		"colors": {"base": "#f6f2ea"}
	}`)
	writeTheme(t, user, "supermilk.json", `{"name": "supermilk", "colors": {"accent": "#123456"}}`)
	writeTheme(t, project, "oatmilk.json", `{"name": "oatmilk", "colors": {"base": "#202020", "text": "#f0f0f0"}}`)

	r := New()
	r.Load(user, project)

	// Project shadows user shadows builtin; positions stay stable.
	names := r.Names()
	want := []string{"off", "supermilk", "oatmilk"}
	if len(names) != 3 || names[0] != want[0] || names[1] != want[1] || names[2] != want[2] {
		t.Fatalf("Names() = %v, want %v", names, want)
	}
	if src, _ := r.Source("oatmilk"); src != SourceProject {
		t.Errorf("oatmilk source = %s, want project", src)
	}
	if src, _ := r.Source("supermilk"); src != SourceUser {
		t.Errorf("supermilk source = %s, want user (shadowed builtin)", src)
	}

	// The shadowed builtin is a full palette override on the user side...
	sm, ok := r.Get("supermilk")
	if !ok || sm.Colors.Accent != Fixed("#123456") {
		t.Fatalf("shadowed supermilk accent = %+v", sm.Colors.Accent)
	}
	// ...while oatmilk (project variant) is the project file's palette,
	// with derivations filling what it did not declare.
	oa, ok := r.Get("oatmilk")
	if !ok || oa.Colors.Base != Fixed("#202020") {
		t.Fatalf("project oatmilk base = %+v", oa.Colors.Base)
	}
	if oa.Colors.Raised.Absent() {
		t.Error("oatmilk raised must derive from its base")
	}
}

func TestRegistryExtendsChain(t *testing.T) {
	user := t.TempDir()
	writeTheme(t, user, "a.json", `{"name": "a", "extends": "supermilk", "tagline": "from a",
		"colors": {"base": {"dark": "#111111", "light": "#eeeeee"}}}`)
	r := New()
	r.Load(user, "")
	a, ok := r.Get("a")
	if !ok {
		t.Fatal("a must resolve")
	}
	if a.Tagline != "from a" || a.Colors.Text != Fixed("#3f2a26") {
		t.Errorf("chain merge: tagline=%q text=%+v", a.Tagline, a.Colors.Text)
	}
	if a.Terminal == nil || a.Terminal.Bg != "#f8f5ee" {
		t.Errorf("a must inherit supermilk's terminal block, got %+v", a.Terminal)
	}
}

func TestRegistrySkipsInvalidFiles(t *testing.T) {
	user := t.TempDir()
	writeTheme(t, user, "broken.json", `{"name": "broken", "colors": {"base": "#zzz"}}`)
	writeTheme(t, user, "typo.json", `{"name": "typo", "looks": "skin"}`) // unknown key
	writeTheme(t, user, "good.json", `{"name": "good", "colors": {"base": "#222222"}}`)
	r := New()
	r.Load(user, "")
	if _, ok := r.Get("broken"); ok {
		t.Error("invalid hex file must be skipped")
	}
	if _, ok := r.Get("typo"); ok {
		t.Error("unknown-key file must be skipped")
	}
	if _, ok := r.Get("good"); !ok {
		t.Error("valid file must load")
	}
	if w := r.Warnings(); len(w) != 2 {
		t.Errorf("warnings = %v, want 2 (broken + typo)", w)
	}
}

func TestRegistryNameFromBasename(t *testing.T) {
	user := t.TempDir()
	writeTheme(t, user, "from-file.json", `{"colors": {"base": "#222222"}}`)
	r := New()
	r.Load(user, "")
	if _, ok := r.Get("from-file"); !ok {
		t.Error("theme name must default to the file basename")
	}
}

func TestRegistryExtendsErrors(t *testing.T) {
	user := t.TempDir()
	writeTheme(t, user, "loop1.json", `{"name": "loop1", "extends": "loop2"}`)
	writeTheme(t, user, "loop2.json", `{"name": "loop2", "extends": "loop1"}`)
	writeTheme(t, user, "orphan.json", `{"name": "orphan", "extends": "nope"}`)
	r := New()
	r.Load(user, "")
	if _, ok := r.Get("loop1"); ok {
		t.Error("extends cycle must not resolve")
	}
	if _, ok := r.Get("orphan"); ok {
		t.Error("unknown parent must not resolve")
	}
}
