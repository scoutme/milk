package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// feedOpts describes the single-release GitHub feed stubFeed serves.
type feedOpts struct {
	tag               string
	prerelease        bool
	binary            []byte
	checksumOK        bool
	omitPlatformAsset bool // release carries no binary asset for this platform
}

// stubFeed serves a one-release GitHub feed whose binary and checksum assets
// self-host on the same server, and points ReleasesURL at it.
func stubFeed(t *testing.T, o feedOpts) *httptest.Server {
	t.Helper()
	if o.binary == nil {
		o.binary = []byte("fake milk binary")
	}
	sum := sha256.Sum256(o.binary)
	sumHex := hex.EncodeToString(sum[:])
	if !o.checksumOK {
		sumHex = strings.Repeat("0", 64)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host
		switch {
		case strings.HasSuffix(r.URL.Path, ".sha256"):
			fmt.Fprintf(w, "%s  %s\n", sumHex, assetName())
		case strings.Contains(r.URL.Path, "/assets/"):
			_, _ = w.Write(o.binary)
		default:
			assets := ""
			if !o.omitPlatformAsset {
				assets = fmt.Sprintf(`,{"name":%q,"browser_download_url":%q},{"name":%q,"browser_download_url":%q}`,
					assetName(), base+"/assets/"+assetName(),
					checksumAssetName(), base+"/assets/"+checksumAssetName())
			} else {
				assets = `,{"name":"milk-some-other-platform","browser_download_url":"` + base + `/assets/x"}`
			}
			fmt.Fprintf(w, `[{"tag_name":%q,"name":"rel","html_url":%q,"prerelease":%v,"assets":[%s]}]`,
				strings.TrimPrefix(o.tag, ""), base+"/releases/"+o.tag, o.prerelease, strings.TrimPrefix(assets, ","))
		}
	}))
	t.Cleanup(srv.Close)
	old := ReleasesURL
	ReleasesURL = srv.URL + "/releases"
	t.Cleanup(func() { ReleasesURL = old })
	return srv
}

func TestCheckLatest_NewerReleaseMatched(t *testing.T) {
	stubFeed(t, feedOpts{tag: "v2.0.0", checksumOK: true})

	rel, err := CheckLatest(context.Background(), "v1.0.0", false)
	if err != nil {
		t.Fatalf("CheckLatest: %v", err)
	}
	if rel == nil {
		t.Fatal("release = nil, want v2.0.0")
	}
	if rel.Tag != "v2.0.0" {
		t.Errorf("Tag = %q, want v2.0.0", rel.Tag)
	}
	if rel.AssetURL == "" || rel.ChecksumURL == "" {
		t.Errorf("asset URLs not resolved: %+v", rel)
	}
}

func TestCheckLatest_UpToDateNil(t *testing.T) {
	stubFeed(t, feedOpts{tag: "v1.0.0", checksumOK: true})

	rel, err := CheckLatest(context.Background(), "v1.0.0", false)
	if err != nil {
		t.Fatalf("CheckLatest: %v", err)
	}
	if rel != nil {
		t.Errorf("release = %+v, want nil when already at the newest tag", rel)
	}
}

func TestCheckLatest_PrereleaseGated(t *testing.T) {
	stubFeed(t, feedOpts{tag: "v2.0.0", prerelease: true, checksumOK: true})

	rel, err := CheckLatest(context.Background(), "v1.0.0", false)
	if err != nil {
		t.Fatalf("CheckLatest: %v", err)
	}
	if rel != nil {
		t.Errorf("prerelease shown on the stable channel: %+v", rel)
	}

	rel, err = CheckLatest(context.Background(), "v1.0.0", true)
	if err != nil {
		t.Fatalf("CheckLatest (prerelease): %v", err)
	}
	if rel == nil || rel.Tag != "v2.0.0" {
		t.Errorf("release = %+v, want v2.0.0 on the prerelease channel", rel)
	}
}

func TestCheckLatest_NoPlatformAssetSkipped(t *testing.T) {
	stubFeed(t, feedOpts{tag: "v2.0.0", checksumOK: true, omitPlatformAsset: true})

	rel, err := CheckLatest(context.Background(), "v1.0.0", false)
	if err != nil {
		t.Fatalf("CheckLatest: %v", err)
	}
	if rel != nil {
		t.Errorf("release without a %s asset returned: %+v", assetName(), rel)
	}
}

func TestApply_VerifiesChecksumAndReplaces(t *testing.T) {
	if testing.Short() {
		t.Skip("replaces a file on disk")
	}
	binary := []byte("new milk binary")
	stubFeed(t, feedOpts{tag: "v2.0.0", binary: binary, checksumOK: true})
	rel, err := CheckLatest(context.Background(), "v1.0.0", false)
	if err != nil || rel == nil {
		t.Fatalf("CheckLatest = (%+v, %v)", rel, err)
	}

	dest := filepath.Join(t.TempDir(), "milk-bin")
	if err := Apply(context.Background(), rel, dest, nil); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if string(got) != string(binary) {
		t.Errorf("dest content = %q, want %q", got, binary)
	}
}

func TestApply_ChecksumMismatchAbortsWithoutTouchingDest(t *testing.T) {
	stubFeed(t, feedOpts{tag: "v2.0.0", checksumOK: false})
	rel, err := CheckLatest(context.Background(), "v1.0.0", false)
	if err != nil || rel == nil {
		t.Fatalf("CheckLatest = (%+v, %v)", rel, err)
	}

	dest := filepath.Join(t.TempDir(), "milk-bin")
	err = Apply(context.Background(), rel, dest, nil)
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("Apply error = %v, want a checksum mismatch", err)
	}
	if _, statErr := os.Stat(dest); statErr == nil {
		t.Error("dest exists after a failed checksum — the download must not land")
	}
}

func TestVersionCmpAndNewerThan(t *testing.T) {
	// versionCmp compares already-normalized numeric versions; newerThan is
	// the public entry point that strips "v" and maps "dev" first.
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"1.2.3", "1.2.3", 0},
		{"1.10.0", "1.9.0", 1},
		{"1.2", "1.2.0", 0},
		{"0.9.0", "0.10.0", -1},
	} {
		if got := versionCmp(tc.a, tc.b); got != tc.want {
			t.Errorf("versionCmp(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"v2.0.0", "1.9.9", true},
		{"v1.0.0", "v1.0.0", false},
		{"v0.0.1", "dev", true},
		{"dev", "v0.0.1", false},
	} {
		if got := newerThan(tc.a, tc.b); got != tc.want {
			t.Errorf("newerThan(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
