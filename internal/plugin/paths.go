package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// resolveWithin resolves the spec-relative path rel ("./"-prefixed, §4.1 rule 4)
// against root and returns the filesystem-absolute path, enforcing containment:
// the result must stay inside the filesystem-resolved root even after symlink
// resolution (§4.1 rule 3). A path that is not "./"-prefixed or that escapes
// the root is an error. The target does not need to exist yet (a bundled
// executable or data dir may be created later); symlinks in its existing
// prefix are still resolved and checked.
func resolveWithin(root, rel string) (string, error) {
	if rel == "" || !strings.HasPrefix(rel, "./") {
		return "", fmt.Errorf("path %q must start with ./", rel)
	}
	joined := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(rel, "./")))
	clean := filepath.Clean(joined)
	if clean != root && !strings.HasPrefix(clean, root+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the plugin root", rel)
	}
	resolvedRoot, err := safeEvalSymlinks(root)
	if err != nil {
		return "", err
	}
	resolvedTarget, err := evalExisting(clean)
	if err != nil {
		return "", fmt.Errorf("path %q: %w", rel, err)
	}
	if resolvedTarget != resolvedRoot && !strings.HasPrefix(resolvedTarget, resolvedRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q resolves outside the plugin root", rel)
	}
	return resolvedTarget, nil
}

// ResolveRelative joins a marketplace-relative source path onto its snapshot
// root and returns the absolute path, refusing anything that escapes root —
// the install-path containment behind ParseMarketplaceSource's parse-time
// check (review C1): both catalog entries and bookkeeping records land here,
// so a poisoned path can never copy outside the snapshot. Symlink-aware via
// contains() (§4.1 rule 3 at install time).
func ResolveRelative(root, rel string) (string, error) {
	target := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(rel, "./")))
	if !contains(root, target) {
		return "", fmt.Errorf("relative source %q escapes the marketplace root", rel)
	}
	return target, nil
}

// contains reports whether candidate is root or inside root after symlink
// resolution. Used for free-form paths (e.g. an expanded cwd) that are not
// required to carry the "./" prefix. Neither side needs to exist yet (the
// PLUGIN_DATA dir is created before launching, §9.1); existing prefixes are
// symlink-resolved.
func contains(root, candidate string) bool {
	resolvedRoot, err := evalExisting(root)
	if err != nil {
		return false
	}
	resolvedTarget, err := evalExisting(candidate)
	if err != nil {
		return false
	}
	return resolvedTarget == resolvedRoot || strings.HasPrefix(resolvedTarget, resolvedRoot+string(filepath.Separator))
}

// evalExisting resolves symlinks in the deepest existing ancestor of p and
// re-appends the non-existent remainder, so paths that will exist later are
// still checked against their real location.
func evalExisting(p string) (string, error) {
	abs := filepath.Clean(p)
	if !filepath.IsAbs(abs) {
		var err error
		abs, err = filepath.Abs(abs)
		if err != nil {
			return "", err
		}
	}
	if _, err := os.Lstat(abs); err == nil {
		return filepath.EvalSymlinks(abs)
	}
	prefix := abs
	// The deepest non-existent segment is the first tail element; without it
	// the walk would silently drop the last path component.
	tail := []string{filepath.Base(abs)}
	for {
		parent := filepath.Dir(prefix)
		if parent == prefix {
			break
		}
		if _, err := os.Lstat(parent); err == nil {
			resolved, err := filepath.EvalSymlinks(parent)
			if err != nil {
				return "", err
			}
			return filepath.Join(resolved, filepath.Join(tail...)), nil
		}
		tail = append([]string{filepath.Base(parent)}, tail...)
		prefix = parent
	}
	return abs, nil
}

// safeEvalSymlinks is filepath.EvalSymlinks with the path absolutized first so
// relative inputs behave.
func safeEvalSymlinks(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}
