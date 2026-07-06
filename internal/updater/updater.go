// Package updater provides update-check and self-update logic for the milk CLI.
package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// ErrWindowsManual is returned by Apply on Windows because the running binary
// cannot be replaced in-place; the caller should print the temp file path.
var ErrWindowsManual = errors.New("cannot replace running binary on Windows; download saved to temp path")

// Release holds the fields of a GitHub release that the updater cares about.
type Release struct {
	Tag          string `json:"tag_name"`
	Name         string `json:"name"`
	HTMLURL      string `json:"html_url"`
	AssetURL     string `json:"-"` // resolved from assets array
	ChecksumURL  string `json:"-"` // resolved from assets array
	IsPrerelease bool   `json:"prerelease"`
}

// ghRelease is the raw GitHub API shape used only for JSON unmarshalling.
type ghRelease struct {
	TagName    string    `json:"tag_name"`
	Name       string    `json:"name"`
	HTMLURL    string    `json:"html_url"`
	Prerelease bool      `json:"prerelease"`
	Assets     []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// assetName returns the expected binary asset name for the current platform.
func assetName() string {
	if runtime.GOOS == "windows" {
		return "milk-windows-amd64.exe"
	}
	return fmt.Sprintf("milk-%s-%s", runtime.GOOS, runtime.GOARCH)
}

// checksumAssetName returns the expected checksum asset name for the current
// platform.
func checksumAssetName() string {
	return assetName() + ".sha256"
}

// CheckLatest fetches the list of releases from GitHub and returns the newest
// release that is newer than currentVersion, or nil if already up to date.
//
// When includePrerelease is false, pre-release entries are skipped.
// If no binary asset matching the current platform is found for a release,
// that release is skipped.
// The string "dev" is treated as "0.0.0" so any real release appears newer.
func CheckLatest(ctx context.Context, currentVersion string, includePrerelease bool) (*Release, error) {
	url := "https://api.github.com/repos/scoutme/milk/releases"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("updater: build request: %w", err)
	}
	req.Header.Set("User-Agent", "milk-updater/"+currentVersion)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("updater: fetch releases: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("updater: GitHub API returned %d", resp.StatusCode)
	}

	var raw []ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("updater: decode releases: %w", err)
	}

	want := assetName()
	wantCS := checksumAssetName()

	for _, r := range raw {
		if r.Prerelease && !includePrerelease {
			continue
		}

		var binaryURL, csURL string
		for _, a := range r.Assets {
			switch a.Name {
			case want:
				binaryURL = a.BrowserDownloadURL
			case wantCS:
				csURL = a.BrowserDownloadURL
			}
		}
		if binaryURL == "" {
			// No matching binary for this platform; skip.
			continue
		}

		if !newerThan(r.TagName, currentVersion) {
			// Already at this version or newer; because releases are ordered
			// newest-first, no point checking further.
			return nil, nil
		}

		return &Release{
			Tag:          r.TagName,
			Name:         r.Name,
			HTMLURL:      r.HTMLURL,
			AssetURL:     binaryURL,
			ChecksumURL:  csURL,
			IsPrerelease: r.Prerelease,
		}, nil
	}

	return nil, nil
}

// Apply downloads and installs the release binary over dest.
//
// progress is called with (bytesDownloaded, totalBytes); the first call is
// always (0, contentLength). If contentLength is unknown, totalBytes is -1.
//
// On Windows, Apply writes the download to a temp file and returns
// ErrWindowsManual with the temp path embedded in the error — callers should
// print it to the user.
//
// On all other platforms, the temp file is atomically renamed over dest.
func Apply(ctx context.Context, r *Release, dest string, progress func(done, total int64)) error {
	dir := filepath.Dir(dest)

	// Create temp file in the same directory so Rename is atomic.
	tmp, err := os.CreateTemp(dir, ".milk-update-*")
	if err != nil {
		return fmt.Errorf("updater: create temp file: %w", err)
	}
	tmpName := tmp.Name()

	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}

	// Download the binary.
	if err := downloadTo(ctx, r.AssetURL, tmp, progress); err != nil {
		cleanup()
		return fmt.Errorf("updater: download binary: %w", err)
	}

	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("updater: close temp file: %w", err)
	}

	// Verify checksum if available.
	if r.ChecksumURL != "" {
		if err := verifyChecksum(ctx, tmpName, r.ChecksumURL); err != nil {
			os.Remove(tmpName)
			return fmt.Errorf("updater: checksum mismatch: %w", err)
		}
	}

	if runtime.GOOS == "windows" {
		// Cannot replace the running binary on Windows.
		return fmt.Errorf("%w: %s", ErrWindowsManual, tmpName)
	}

	// chmod +x before replacing.
	if err := os.Chmod(tmpName, 0755); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("updater: chmod: %w", err)
	}

	if err := os.Rename(tmpName, dest); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("updater: rename into place: %w", err)
	}

	return nil
}

// CurrentBinaryPath returns the path of the running executable, resolving
// any symlinks.
func CurrentBinaryPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("updater: resolve executable: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", fmt.Errorf("updater: eval symlinks: %w", err)
	}
	return resolved, nil
}

// downloadTo streams the resource at url into dst, calling progress as data
// arrives.
func downloadTo(ctx context.Context, url string, dst io.Writer, progress func(done, total int64)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned %d", resp.StatusCode)
	}

	total := resp.ContentLength // -1 if unknown
	if progress != nil {
		progress(0, total)
	}

	buf := make([]byte, 32*1024)
	var done int64
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
			done += int64(n)
			if progress != nil {
				progress(done, total)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// verifyChecksum downloads the checksum file at csURL, parses the first
// hex-encoded SHA256 it finds, and compares it against the file at path.
func verifyChecksum(ctx context.Context, path, csURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, csURL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("checksum server returned %d", resp.StatusCode)
	}

	csBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	// The file is typically "<hex>  filename" or just "<hex>".
	fields := strings.Fields(string(csBytes))
	if len(fields) == 0 {
		return fmt.Errorf("empty checksum file")
	}
	expected := strings.ToLower(fields[0])

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))

	if got != expected {
		return fmt.Errorf("expected %s, got %s", expected, got)
	}
	return nil
}

// newerThan reports whether version a is strictly newer than version b.
// Both strings may have a leading "v". "dev" is treated as "0.0.0".
func newerThan(a, b string) bool {
	return versionCmp(normalizeVersion(a), normalizeVersion(b)) > 0
}

// normalizeVersion strips a leading "v" and maps "dev" to "0.0.0".
func normalizeVersion(v string) string {
	v = strings.TrimPrefix(v, "v")
	if v == "dev" || v == "" {
		return "0.0.0"
	}
	return v
}

// versionCmp compares two dot-separated integer version strings.
// Returns -1, 0, or 1.
func versionCmp(a, b string) int {
	partsA := strings.Split(a, ".")
	partsB := strings.Split(b, ".")

	maxLen := len(partsA)
	if len(partsB) > maxLen {
		maxLen = len(partsB)
	}

	for i := 0; i < maxLen; i++ {
		var ia, ib int
		if i < len(partsA) {
			ia, _ = strconv.Atoi(partsA[i])
		}
		if i < len(partsB) {
			ib, _ = strconv.Atoi(partsB[i])
		}
		if ia < ib {
			return -1
		}
		if ia > ib {
			return 1
		}
	}
	return 0
}
