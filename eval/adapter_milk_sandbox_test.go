package eval

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// stubMilkBin returns a harmless executable to stand in for the milk binary
// where a test only needs Start to exec something ("true" lives at different
// paths across platforms and is absent on some, e.g. macOS has no /bin/true).
func stubMilkBin(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("true")
	if err != nil {
		t.Skip("no 'true' executable on PATH")
	}
	return p
}

func TestMilkAdapter_SetArgs_PseudoFlags(t *testing.T) {
	a := &milkAdapter{}
	a.SetArgs([]string{"--sandbox", "--milk-bin", "/bin/true", "--sandbox-seed", "/tmp/seed.json", "--agent", "mimo-local"})
	if !a.sandbox.Enabled {
		t.Error("--sandbox should enable the sandbox")
	}
	if a.sandbox.MilkBinary != "/bin/true" {
		t.Errorf("MilkBinary = %q", a.sandbox.MilkBinary)
	}
	if a.sandbox.SeedConfigPath != "/tmp/seed.json" {
		t.Errorf("SeedConfigPath = %q", a.sandbox.SeedConfigPath)
	}
	if len(a.extraArgs) != 2 || a.extraArgs[0] != "--agent" || a.extraArgs[1] != "mimo-local" {
		t.Errorf("extraArgs = %v, pseudo-flags must not reach milk", a.extraArgs)
	}
}

func TestMilkAdapter_SetSandbox_PseudoFlagsWin(t *testing.T) {
	a := &milkAdapter{}
	a.SetArgs([]string{"--milk-bin", "/bin/true"})
	a.SetSandbox(SandboxConfig{Enabled: true, MilkBinary: "/other/milk", SeedConfigPath: "/seed.json"})
	if a.sandbox.MilkBinary != "/bin/true" {
		t.Errorf("explicit --milk-bin must win over the experiment: %q", a.sandbox.MilkBinary)
	}
	if a.sandbox.SeedConfigPath != "/seed.json" {
		t.Errorf("experiment seed should fill the unset field: %q", a.sandbox.SeedConfigPath)
	}
	if !a.sandbox.Enabled {
		t.Error("experiment should enable the sandbox")
	}
}

func TestMilkAdapter_SandboxFreshRootSeededAndCleaned(t *testing.T) {
	seed := filepath.Join(t.TempDir(), "config.json")
	seedBody := `{"agents":[{"name":"mimo-local","provider":"local"}]}`
	if err := os.WriteFile(seed, []byte(seedBody), 0644); err != nil {
		t.Fatal(err)
	}

	a := &milkAdapter{milkBin: stubMilkBin(t)}
	a.SetArgs([]string{"--sandbox", "--sandbox-seed", seed})
	if err := a.Start(context.Background(), t.TempDir()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if a.sandboxDir == "" {
		t.Fatal("sandboxDir not created")
	}
	got, err := os.ReadFile(filepath.Join(a.sandboxDir, "config.json"))
	if err != nil {
		t.Fatalf("seeded config: %v", err)
	}
	if string(got) != seedBody {
		t.Errorf("seeded config = %s, want the current config copied verbatim", got)
	}

	// A second Start must get a different, again-fresh root.
	first := a.sandboxDir
	if err := a.Start(context.Background(), t.TempDir()); err != nil {
		t.Fatalf("Start #2: %v", err)
	}
	if a.sandboxDir == first {
		t.Error("sandbox root must be fresh per Start")
	}

	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := os.Stat(a.sandboxDir); !os.IsNotExist(err) {
		t.Errorf("sandbox root should be removed by Stop (stat err: %v)", err)
	}
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Errorf("first sandbox root should be removed too (stat err: %v)", err)
	}
}

func TestAllowToolsInConfig(t *testing.T) {
	cfg := []byte(`{"agent":"mimo-local","agents":[{"name":"a","model":"m"},{"name":"b"}],"agents_extra":12345678901234567890}`)
	out, err := allowToolsInConfig(cfg)
	if err != nil {
		t.Fatalf("allowToolsInConfig: %v", err)
	}
	var doc struct {
		Agents []map[string]any `json:"agents"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("re-encoded config invalid: %v", err)
	}
	for i, agent := range doc.Agents {
		if agent["dangerously_skip_permissions"] != true {
			t.Errorf("agents[%d] = %+v, want dangerously_skip_permissions", i, agent)
		}
	}

	// Shapes without an agents array pass through untouched.
	orig := []byte(`{"hello":"world"}`)
	out, err = allowToolsInConfig(orig)
	if err != nil || string(out) != string(orig) {
		t.Errorf("non-agent config = %s, %v; want passthrough", out, err)
	}
}

func TestMilkAdapter_SandboxAllowTools(t *testing.T) {
	seed := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(seed, []byte(`{"agents":[{"name":"mimo-local"}]}`), 0644); err != nil {
		t.Fatal(err)
	}

	a := &milkAdapter{milkBin: stubMilkBin(t)}
	a.SetArgs([]string{"--sandbox", "--allow-tools", "--sandbox-seed", seed})
	if err := a.Start(context.Background(), t.TempDir()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer a.Stop()

	got, err := os.ReadFile(filepath.Join(a.sandboxDir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"dangerously_skip_permissions":true`) {
		t.Errorf("seeded config missing skip-permissions: %s", got)
	}
	// The live config is never modified.
	live, _ := os.ReadFile(seed)
	if strings.Contains(string(live), "dangerously_skip_permissions") {
		t.Error("seed source must not be modified")
	}
}

func TestMilkAdapter_SandboxEnvAndBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub binary is a sh script")
	}
	goldenAbs, err := filepath.Abs(filepath.Join(streamJSONEventsDir, "tool_use_turn.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	envLog := filepath.Join(tmp, "env.txt")
	argvLog := filepath.Join(tmp, "argv.txt")
	stub := filepath.Join(tmp, "milk-stub")
	script := "#!/bin/sh\n" +
		"printf '%s' \"$MILK_CONFIG_ROOT\" > " + strconv.Quote(envLog) + "\n" +
		"printf '%s\\n' \"$@\" > " + strconv.Quote(argvLog) + "\n" +
		"cat " + strconv.Quote(goldenAbs) + "\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	a := &milkAdapter{}
	a.SetArgs([]string{"--milk-bin", stub, "--sandbox", "--agent", "mimo-local"})
	if err := a.Start(context.Background(), t.TempDir()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer a.Stop()

	if _, err := a.RunPrompt(context.Background(), "hi"); err != nil {
		t.Fatalf("RunPrompt: %v", err)
	}

	env, err := os.ReadFile(envLog)
	if err != nil {
		t.Fatalf("reading env log: %v", err)
	}
	if string(env) != a.sandboxDir {
		t.Errorf("MILK_CONFIG_ROOT = %q, want sandbox root %q", env, a.sandboxDir)
	}

	argv, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("reading argv log: %v", err)
	}
	if strings.Contains(string(argv), "--milk-bin") || strings.Contains(string(argv), "--sandbox") {
		t.Errorf("pseudo-flags leaked into milk argv:\n%s", argv)
	}
	if !strings.Contains(string(argv), "--agent") {
		t.Errorf("forwarded flags missing from milk argv:\n%s", argv)
	}
}

// TestMilkAdapter_RunPromptKeepsPartialOnFailure: a batch run that dies
// (turn deadline, non-zero exit) must return everything parsed before the
// failure alongside the error. The harness records partial results next to
// the error; returning an empty result scored a ~10-minute killed turn as 0.
func TestMilkAdapter_RunPromptKeepsPartialOnFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub binary is a sh script")
	}
	for _, tc := range []struct {
		name         string
		events       []string
		wantErr      string
		wantResponse string
	}{
		{
			name: "stream ends without terminal result",
			events: []string{
				`{"type":"system","subtype":"init","session_id":"s","seq":0,"session_state":"idle","capabilities":["stream_v1"]}`,
				`{"type":"assistant","session_id":"s","seq":1,"agent":"primary","message":{"id":"m","role":"assistant","content":[{"type":"text","text":"partial work before the kill"}]}}`,
			},
			wantErr:      "no terminal result event",
			wantResponse: "partial work before the kill",
		},
		{
			name: "terminal error result then exit 1",
			events: []string{
				`{"type":"system","subtype":"init","session_id":"s","seq":0,"session_state":"idle","capabilities":["stream_v1"]}`,
				`{"type":"assistant","session_id":"s","seq":1,"agent":"primary","message":{"id":"m","role":"assistant","content":[{"type":"text","text":"working"}]}}`,
				`{"type":"result","subtype":"error","session_id":"s","seq":2,"is_error":true,"num_turns":1,"duration_ms":9000,"result":"one-shot budget exhausted","stop_reason":"deadline","usage":{"input_tokens":10,"output_tokens":5}}`,
			},
			wantErr:      "exit status",
			wantResponse: "one-shot budget exhausted",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			stub := filepath.Join(tmp, "milk-stub")
			script := "#!/bin/sh\n"
			for _, ev := range tc.events {
				script += "echo " + strconv.Quote(ev) + "\n"
			}
			script += "exit 1\n"
			if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}

			a := &milkAdapter{}
			a.SetArgs([]string{"--milk-bin", stub})
			if err := a.Start(context.Background(), tmp); err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer a.Stop()

			res, err := a.RunPrompt(context.Background(), "hi")
			if err == nil {
				t.Fatal("expected an error from the exiting stub, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not mention %q", err, tc.wantErr)
			}
			if res.Response != tc.wantResponse {
				t.Errorf("partial Response = %q, want %q", res.Response, tc.wantResponse)
			}
			if res.Duration <= 0 {
				t.Errorf("partial Duration = %v, want > 0", res.Duration)
			}
		})
	}
}
