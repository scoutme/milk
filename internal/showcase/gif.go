package showcase

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The raw recording is mp4, the shipped artifact a palette GIF derived from
// it (RenderTape's Output line points at the mp4). The split exists because
// vhs's own gif pipeline — one ffmpeg pass with palettegen+paletteuse —
// buffers every frame until the palette is ready: O(duration × resolution)
// RAM, and on minutes-long full-width recordings the kernel OOM-kills ffmpeg
// mid-encode, vhs still exits 0, and the gif is left at 0 bytes (observed
// verbatim in dmesg: "Out of memory: Killed process … (ffmpeg)"). The mp4
// encoder streams frame-by-frame, and the two-pass palette below processes
// each frame once per pass and releases it — flat memory at any duration.

// MP4Path is the raw recording path for a manifest output: the output with
// its extension swapped for .mp4 (docs/assets/x.gif → docs/assets/x.mp4).
func MP4Path(output string) string {
	return strings.TrimSuffix(output, filepath.Ext(output)) + ".mp4"
}

// GIFTwoPass derives gifPath from the recorded mp4 in two streaming passes:
// pass 1 builds the palette (histogram over the video), pass 2 encodes
// against it. The result is verified non-empty — vhs exits 0 even when its
// ffmpeg dies, so an unverified artifact is exactly how the 0-byte gifs got
// shipped unnoticed.
func GIFTwoPass(ctx context.Context, ffmpeg, mp4Path, gifPath string) error {
	palette, err := os.CreateTemp("", "showcase-palette-*.png")
	if err != nil {
		return fmt.Errorf("palette temp: %w", err)
	}
	defer os.Remove(palette.Name())

	run := func(args ...string) error {
		cmd := exec.CommandContext(ctx, ffmpeg, args...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			msg := strings.TrimSpace(stderr.String())
			if len(msg) > 400 {
				msg = msg[len(msg)-400:]
			}
			return fmt.Errorf("ffmpeg %s: %w (%s)", args[1], err, msg)
		}
		return nil
	}
	if err := run("-y", "-i", mp4Path, "-vf", "palettegen", palette.Name()); err != nil {
		return err
	}
	if err := run("-y", "-i", mp4Path, "-i", palette.Name(), "-lavfi", "paletteuse", gifPath); err != nil {
		return err
	}
	fi, err := os.Stat(gifPath)
	if err != nil || fi.Size() == 0 {
		return fmt.Errorf("derived gif %s is empty — encoding did not produce an artifact", gifPath)
	}
	return nil
}

// CompressedPath is the committable variant of a recorded GIF: x.gif →
// x-compressed.gif (the repo convention — gitignore keeps the raw recordings
// and tracks only the compressed sibling; the site hero ships that way).
func CompressedPath(output string) string {
	if strings.HasSuffix(output, ".gif") {
		return strings.TrimSuffix(output, ".gif") + "-compressed.gif"
	}
	return output + "-compressed.gif"
}

// CompressGIF derives the committable compressed variant with gifsicle —
// the same `-O3 --lossy=80` that produced the original site hero. A missing
// gifsicle ("" path) is a soft skip: the raw recording stays valid and the
// caller notes the committed copy wasn't refreshed — never a failure, since
// the recorder itself doesn't need gifsicle. Returns the output path (or ""
// when skipped); errors only on a real failure or an empty artifact — the
// same loud rule as the raw recording (vhs once shipped 0-byte gifs).
func CompressGIF(gifsicle, src, dst string) (string, error) {
	if gifsicle == "" {
		return "", nil
	}
	out, err := exec.Command(gifsicle, "-O3", "--lossy=80", "-o", dst, src).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gifsicle: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	if fi, serr := os.Stat(dst); serr != nil || fi.Size() == 0 {
		return "", fmt.Errorf("gifsicle produced no output at %s", dst)
	}
	return dst, nil
}
