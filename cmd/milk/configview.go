package main

// Host-independent /config surface: the renderers for /config and /config
// show, plus the shared "open" action rules (which file "open" targets, and
// how a host with no editor of its own makes it appear). The TUI appends the
// returned text to its transcript; the ACP server (acp_commands.go) sends it
// as an agent_message_chunk — one source of truth for the output itself, per
// the TUI/ACP de-duplication direction in issue #188.

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/scoutme/milk/internal/config"
)

// renderConfigJSON returns the bare /config output: a header naming the config
// scope followed by the merged config as a fenced JSON block. The caller owns
// presentation details beyond that (the TUI forces a colorize, ACP strips the
// header's ANSI).
func renderConfigJSON() (string, error) {
	_, _, merged, hasLocal, err := config.LoadWithLocal()
	if err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshalling config: %w", err)
	}
	header := milkTag() + " ~/.milk/config.json"
	if hasLocal {
		header = milkTag() + " merged config (global + local overrides)"
	}
	return header + "\n```json\n" + string(data) + "\n```\n", nil
}

// renderConfigShow returns the /config show output: the merged config with a
// per-field [global]/[local]/[default] source annotation, plus the full merged
// JSON behind a details block.
func renderConfigShow() (string, error) {
	global, local, merged, hasLocal, err := config.LoadWithLocal()
	if err != nil {
		return "", err
	}

	if !hasLocal {
		data, _ := json.MarshalIndent(merged, "", "  ")
		return milkTag() + " no local config — all values from global\n```json\n" + string(data) + "\n```\n", nil
	}

	// Build source annotations per top-level field.
	globalRaw, _ := json.Marshal(global)
	localRaw, _ := json.Marshal(local)
	mergedRaw, _ := json.Marshal(merged)

	var gMap, lMap, mMap map[string]json.RawMessage
	json.Unmarshal(globalRaw, &gMap)
	json.Unmarshal(localRaw, &lMap)
	json.Unmarshal(mergedRaw, &mMap)

	var lines []string
	for key, val := range mMap {
		source := "default"
		if _, ok := lMap[key]; ok {
			source = "local"
		} else if _, ok := gMap[key]; ok {
			source = "global"
		}
		lines = append(lines, fmt.Sprintf("  /* [%s] */ %q: %s", source, key, string(val)))
	}
	// Deterministic order.
	for i := 0; i < len(lines); i++ {
		for j := i + 1; j < len(lines); j++ {
			if lines[j] < lines[i] {
				lines[i], lines[j] = lines[j], lines[i]
			}
		}
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%s merged config — [local] fields override [global]; unset = [default]\n", milkTag()))
	sb.WriteString("```json\n{\n")
	for i, l := range lines {
		sb.WriteString(l)
		if i < len(lines)-1 {
			sb.WriteString(",")
		}
		sb.WriteString("\n")
	}
	sb.WriteString("}\n```\n")

	fullData, _ := json.MarshalIndent(merged, "", "  ")
	sb.WriteString(fmt.Sprintf("\n<details><summary>Full merged JSON</summary>\n\n```json\n%s\n```\n</details>\n", string(fullData)))

	return sb.String(), nil
}

// configOpenTarget returns the config file the "open" action targets — the
// local config when one exists (its unset fields inherit from global), else
// the global config — plus whether the target is the local one. Shared by the
// TUI (commands.go), the CLI (main.go's runConfigOpen uses the same rule) and
// the ACP server (acp_commands.go) so the hosts never disagree on which file
// "open" means.
func configOpenTarget() (path string, local bool, err error) {
	if config.HasLocalConfig() {
		p, err := config.LocalConfigPath()
		return p, true, err
	}
	dir, err := config.Dir()
	if err != nil {
		return "", false, err
	}
	return filepath.Join(dir, "config.json"), false, nil
}

// openPathDetached makes path appear in the user's desktop environment by
// launching the platform file opener in its own process and returning without
// waiting for it ("xdg-open" on Linux, "open" on macOS, rundll32/shell start
// on Windows), returning the opener's command name. Headless hosts use it
// where the TUI suspends into $EDITOR: an ACP agent has no TTY to run an
// interactive editor in, but it can still hand the file to the desktop of the
// machine milk runs on. It is a variable so tests can stub the side effect.
var openPathDetached = func(path string) (string, error) {
	type opener struct {
		bin  string
		args []string
	}
	var candidates []opener
	switch runtime.GOOS {
	case "darwin":
		candidates = []opener{{"open", []string{path}}}
	case "windows":
		candidates = []opener{
			{"rundll32", []string{"url.dll,FileProtocolHandler", path}},
			{"cmd", []string{"/c", "start", "", path}},
		}
	default:
		candidates = []opener{{"xdg-open", []string{path}}}
	}
	var tried []string
	for _, c := range candidates {
		tried = append(tried, c.bin)
		if _, err := exec.LookPath(c.bin); err != nil {
			continue
		}
		cmd := exec.Command(c.bin, c.args...)
		if err := cmd.Start(); err != nil {
			continue
		}
		go cmd.Wait() // reap it — the opener outlives us but must not zombify
		return c.bin, nil
	}
	return "", fmt.Errorf("no usable file opener (tried %s)", strings.Join(tried, ", "))
}
