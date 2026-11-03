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

// TestCalibrate_LiveSlashBeat is the honest end-to-end test: drive a real
// milk TUI in tmux, run one purely local slash beat (/learn needs no
// model), and prove launch-wait, step-waits and timing all work against a
// live pane. Gated on tmux + a built milk + a configured state root — on CI
// (none of the three) it skips, because the live pane is the thing only a
// developer machine can honestly provide. The model-dependent beats are
// exercised by `task showcase:record`, whose calibration IS the test.
func TestCalibrate_LiveSlashBeat(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	milk, err := exec.LookPath("milk")
	if err != nil {
		t.Skip("milk not on PATH (run task build)")
	}
	milk, _ = filepath.EvalSymlinks(milk)
	dir, err := os.UserHomeDir()
	if err != nil || dir == "" {
		t.Skip("no home")
	}
	if _, err := os.Stat(filepath.Join(dir, ".milk", "config.json")); err != nil {
		t.Skip("no milk config (CI): the live TUI needs one")
	}

	m, err := Load(writeManifest(t, `
name: live
output: live.gif
launch: "--new"
timeout: 30s
terminal: {cols: 100, rows: 30, width: 1000, height: 640}
beats:
  - id: learn
    steps:
      - type: "/learn showcase integration probe"
      - key: Enter
        wait: "[milk] learned:"
`))
	if err != nil {
		t.Fatal(err)
	}

	// Clean root: the probe must not touch the real state either.
	root := t.TempDir()
	if _, err := WriteCleanConfig(filepath.Join(dir, ".milk", "config.json"), root, m.StripConfigKeys); err != nil {
		t.Skip("clean config: " + err.Error())
	}
	res, err := Calibrate(context.Background(), m, Options{
		Session:    "showcase-itest",
		Workdir:    t.TempDir(),
		Launch:     "MILK_CONFIG_ROOT='" + root + "' '" + milk + "' --new",
		ConfigRoot: root,
		Timeout:    30 * time.Second,
	})
	if err != nil {
		t.Fatalf("calibration: %v", err)
	}
	if res.LaunchElapsed <= 0 || len(res.Timings) != 1 || res.Timings[0].Skipped {
		t.Errorf("result: launch=%v timings=%+v", res.LaunchElapsed, res.Timings)
	}
	if res.Timings[0].Elapsed <= 0 {
		t.Error("beat measured zero elapsed")
	}
	// The tape for a calibrated run must contain the live path, not the
	// manifest's bare words.
	tape := RenderTape(m, res, RenderOpts{
		MilkPath: milk, ConfigRoot: root, Workdir: t.TempDir(),
	})
	for _, want := range []string{"MILK_CONFIG_ROOT=", "--new", `Type "/learn showcase integration probe"`, "Sleep "} {
		if !strings.Contains(tape, want) {
			t.Errorf("tape missing %q", want)
		}
	}
}
