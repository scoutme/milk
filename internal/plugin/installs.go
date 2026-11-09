package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// InstallRecord is one bookkeeping row in plugin-installs.json (ADR-0104
// Part II): what was installed, from where, at which pin. The source fields
// are stored structured (never re-parsed from a display string) so `update`
// can refetch exactly what `add` fetched.
type InstallRecord struct {
	Name        string    `json:"name"`
	Origin      string    `json:"origin"` // what the user typed or the marketplace entry
	Kind        string    `json:"kind"`
	Repo        string    `json:"repo,omitempty"`
	URL         string    `json:"url,omitempty"`
	Path        string    `json:"path,omitempty"`
	Package     string    `json:"package,omitempty"`
	Ref         string    `json:"ref,omitempty"`
	SHA         string    `json:"sha,omitempty"`
	Version     string    `json:"version,omitempty"`
	Shimmed     bool      `json:"shimmed,omitempty"`
	InstalledAt time.Time `json:"installed_at"`
}

// Installs is the install bookkeeping file (one JSON map, atomic writes).
type Installs struct {
	Path    string
	Records map[string]InstallRecord
}

func LoadInstalls(path string) (*Installs, error) {
	in := &Installs{Path: path, Records: map[string]InstallRecord{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return in, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(raw, &in.Records); err != nil {
		return nil, fmt.Errorf("plugin-installs.json: %w", err)
	}
	return in, nil
}

// Save writes atomically (tmp + rename) — the same write safety as config.
func (in *Installs) Save() error {
	raw, err := json.MarshalIndent(in.Records, "", "  ")
	if err != nil {
		return err
	}
	tmp := in.Path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, in.Path)
}

func (in *Installs) record(src Source, origin, name, version string, shimmed bool) InstallRecord {
	r := InstallRecord{
		Name:        name,
		Origin:      origin,
		Kind:        src.Kind.String(),
		Repo:        src.Repo,
		URL:         src.URL,
		Path:        src.Path,
		Package:     src.Package,
		Ref:         src.Ref,
		SHA:         src.SHA,
		Version:     version,
		Shimmed:     shimmed,
		InstalledAt: time.Now().UTC(),
	}
	in.Records[name] = r
	return r
}

func (r InstallRecord) source() Source {
	kind := SourceRelative
	switch r.Kind {
	case "github":
		kind = SourceGitHub
	case "git-subdir":
		kind = SourceGitSubdir
	case "url":
		kind = SourceURL
	case "npm":
		kind = SourceNPM
	}
	return Source{Kind: kind, Repo: r.Repo, URL: r.URL, Path: r.Path, Package: r.Package, Ref: r.Ref, SHA: r.SHA}
}

// InstallPlugin fetches a plugin from its distribution source, shims it when
// the package predates the portable layout, validates it through Load, and
// installs it atomically under pluginsDir/<manifest-name> (an existing
// install of the same name is replaced — that is `update`). Returns the
// freshly loaded plugin plus the fetch/shim report.
func InstallPlugin(ctx context.Context, client *http.Client, src Source, origin, pluginsDir, dataRoot string, in *Installs) (*Plugin, []string, error) {
	if err := os.MkdirAll(pluginsDir, 0o755); err != nil {
		return nil, nil, err
	}
	staging := filepath.Join(pluginsDir, fmt.Sprintf(".staging-%d", os.Getpid()))
	_ = os.RemoveAll(staging)
	defer os.RemoveAll(staging)

	warns, err := FetchTo(ctx, client, src, staging)
	if err != nil {
		return nil, warns, err
	}
	p, w2, err := finishStaged(staging, origin, src, pluginsDir, dataRoot, in)
	warns = append(warns, w2...)
	return p, warns, err
}

// InstallFromDir installs a plugin already on disk (marketplace relative
// sources, manual staging): copied into staging then run through the exact
// shim/validate/swap tail of InstallPlugin.
func InstallFromDir(srcDir, origin string, src Source, pluginsDir, dataRoot string, in *Installs) (*Plugin, []string, error) {
	if err := os.MkdirAll(pluginsDir, 0o755); err != nil {
		return nil, nil, err
	}
	staging := filepath.Join(pluginsDir, fmt.Sprintf(".staging-%d", os.Getpid()))
	_ = os.RemoveAll(staging)
	defer os.RemoveAll(staging)
	if err := copyTree(srcDir, staging); err != nil {
		return nil, nil, err
	}
	p, w2, err := finishStaged(staging, origin, src, pluginsDir, dataRoot, in)
	return p, w2, err
}

func finishStaged(staging, origin string, src Source, pluginsDir, dataRoot string, in *Installs) (*Plugin, []string, error) {
	var warns []string
	shimmed := false
	if NeedsShim(staging) {
		created, shimWarns, err := WriteShim(staging)
		warns = append(warns, shimWarns...)
		if err != nil {
			return nil, warns, err
		}
		if len(created) > 0 {
			shimmed = true
			warns = append(warns, fmt.Sprintf("shimmed portable manifest(s) from .claude-plugin/: %v", created))
		}
	}
	// Load validates the package before touching any existing install; its
	// manifest name decides the install directory.
	p, err := Load(staging, dataRoot)
	if err != nil {
		return nil, warns, fmt.Errorf("plugin does not load: %w", err)
	}
	name := p.Manifest.Name
	target := filepath.Join(pluginsDir, name)
	// Rename-aside swap (review W4): never delete-then-rename — a failed
	// rename must leave the previous install in place, not destroy it.
	trash := target + fmt.Sprintf(".old-%d", os.Getpid())
	_ = os.RemoveAll(trash)
	if err := os.Rename(target, trash); err != nil && !os.IsNotExist(err) {
		return nil, warns, err
	}
	if err := os.Rename(staging, target); err != nil {
		_ = os.Rename(trash, target) // restore the previous install
		return nil, warns, err
	}
	_ = os.RemoveAll(trash)
	if in != nil {
		in.record(src, origin, name, p.Manifest.Version, shimmed)
		if err := in.Save(); err != nil {
			// The plugin half succeeded: say which half failed. The
			// install stays (it is on disk and will load next start); only
			// its bookkeeping row is missing.
			return p, warns, fmt.Errorf("installed %s %s, but bookkeeping failed: %w", name, p.Manifest.Version, err)
		}
	}
	// Reload from the final location (paths baked into the first load point
	// at staging).
	p2, err := Load(target, dataRoot)
	return p2, warns, err
}

// UpdatePlugin refetches a recorded install from its source (re-shimming as
// needed). A missing record is an error — plugins installed by hand are
// updated by hand.
func UpdatePlugin(ctx context.Context, client *http.Client, name, pluginsDir, dataRoot string, in *Installs) (*Plugin, []string, error) {
	rec, ok := in.Records[name]
	if !ok {
		return nil, nil, fmt.Errorf("no install record for %q (installed by hand?)", name)
	}
	return InstallPlugin(ctx, client, rec.source(), rec.Origin, pluginsDir, dataRoot, in)
}

// RemovePlugin deletes an installed plugin's directory and its record.
// PLUGIN_DATA (the state dir) is deliberately preserved — per §9.1 it is
// data, not install.
func RemovePlugin(name, pluginsDir string, in *Installs) error {
	if _, ok := in.Records[name]; !ok {
		return fmt.Errorf("no install record for %q", name)
	}
	if err := os.RemoveAll(filepath.Join(pluginsDir, name)); err != nil {
		return err
	}
	delete(in.Records, name)
	return in.Save()
}

// copyTree copies a directory tree (marketplace relative sources), skipping
// symlinks — install copies are flat files, containment is the loader's job.
func copyTree(src, dest string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dest, 0o755)
		}
		if info.IsDir() {
			return os.MkdirAll(filepath.Join(dest, rel), 0o755)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dest, rel)), 0o755); err != nil {
			return err
		}
		out, err := os.Create(filepath.Join(dest, rel))
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	})
}
