package showcase

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMP4Path(t *testing.T) {
	for in, want := range map[string]string{
		"docs/assets/showcase-fix-typo.gif": "docs/assets/showcase-fix-typo.mp4",
		"out.mp4":                           "out.mp4",
		"noext":                             "noext.mp4",
	} {
		if got := MP4Path(in); got != want {
			t.Errorf("MP4Path(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestGIFTwoPass: the derived gif must be a real, non-empty artifact — the
// size check is the whole point (vhs exiting 0 on a dead ffmpeg is how
// 0-byte gifs shipped). Uses ffmpeg on a tiny synthetic mp4; skipped where
// ffmpeg is absent (the recorder needs it, the library does not).
func TestGIFTwoPass(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	mp4 := filepath.Join(dir, "src.mp4")
	gif := filepath.Join(dir, "out.gif")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// 1s of solid color: frames fast enough for a unit test.
	src := exec.CommandContext(ctx, ff, "-y", "-f", "lavfi", "-i", "color=red:s=64x64:d=1:r=5", mp4)
	if out, err := src.CombinedOutput(); err != nil {
		t.Fatalf("seeding mp4: %v\n%s", err, out)
	}

	if err := GIFTwoPass(ctx, ff, mp4, gif); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(gif)
	if err != nil || len(data) == 0 {
		t.Fatalf("derived gif unreadable: %v", err)
	}
	if !strings.HasPrefix(string(data[:6]), "GIF8") {
		t.Errorf("derived file is not a GIF (magic %q)", data[:6])
	}
}

func TestCompressedPath(t *testing.T) {
	for in, want := range map[string]string{
		"docs/assets/milk-tui.gif":     "docs/assets/milk-tui-compressed.gif",
		"docs/assets/showcase-a.gif":   "docs/assets/showcase-a-compressed.gif",
		"already.mp4":                  "already.mp4-compressed.gif",
		"docs/assets/x-compressed.gif": "docs/assets/x-compressed-compressed.gif",
	} {
		if got := CompressedPath(in); got != want {
			t.Errorf("CompressedPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCompressGIF: the committable variant must exist and be a real GIF —
// the site hero ships as the compressed sibling, and a silent empty output
// is exactly the class of bug (0-byte artifacts) the recorder already
// guards against. Skipped where gifsicle is absent (the recorder soft-skips
// the same way).
func TestCompressGIF(t *testing.T) {
	gs, err := exec.LookPath("gifsicle")
	if err != nil {
		t.Skip("gifsicle not installed")
	}
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	mp4 := filepath.Join(dir, "src.mp4")
	src := filepath.Join(dir, "raw.gif")
	dst := filepath.Join(dir, "raw-compressed.gif")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, ff, "-y", "-f", "lavfi", "-i", "smptebars=s=128x128:d=1:r=10", mp4).CombinedOutput(); err != nil {
		t.Fatalf("seeding mp4: %v\n%s", err, out)
	}
	if err := GIFTwoPass(ctx, ff, mp4, src); err != nil {
		t.Fatal(err)
	}
	got, err := CompressGIF(gs, src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if got != dst {
		t.Errorf("CompressGIF returned %q, want %q", got, dst)
	}
	data, err := os.ReadFile(dst)
	if err != nil || len(data) == 0 {
		t.Fatalf("compressed gif unreadable: %v", err)
	}
	if !strings.HasPrefix(string(data[:6]), "GIF8") {
		t.Errorf("compressed file is not a GIF (magic %q)", data[:6])
	}
	if _, err := CompressGIF("", dst, filepath.Join(dir, "never.gif")); err != nil {
		t.Errorf("empty gifsicle path must soft-skip, got %v", err)
	}
}
