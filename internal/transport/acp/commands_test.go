package acp

import (
	"context"
	"testing"
)

// available_commands_update + session/set_config_option (commands.go):
// slash commands become editor input completion; /think, /agent switch and
// /model become SessionConfigOptions.

func TestMilkCommandsShape(t *testing.T) {
	cmds := MilkCommands()
	byName := map[string]AvailableCommand{}
	for _, c := range cmds {
		byName[c.Name] = c
	}
	// The deliverable surface must include the slash commands named in the
	// sprint, with text-input hints where they take arguments.
	for _, name := range []string{"/think", "/agent", "/panel", "/bg"} {
		c, ok := byName[name]
		if !ok {
			t.Fatalf("missing command %q", name)
		}
		if c.Input == nil || c.Input.Type != CommandInputText || c.Input.Hint == "" {
			t.Fatalf("%s input = %+v", name, c.Input)
		}
	}
	if c := byName["/new"]; c.Input != nil {
		t.Fatalf("/new takes no input: %+v", c.Input)
	}

	upd := NewAvailableCommandsUpdate(cmds)
	if upd.SessionUpdate != "available_commands_update" {
		t.Fatalf("sessionUpdate = %q", upd.SessionUpdate)
	}
	body := jsonBody(t, upd)
	if body["sessionUpdate"] != "available_commands_update" {
		t.Fatalf("wire sessionUpdate = %v", body["sessionUpdate"])
	}
	list, ok := body["availableCommands"].([]any)
	if !ok || len(list) != len(cmds) {
		t.Fatalf("availableCommands = %v", body["availableCommands"])
	}
	first := list[0].(map[string]any)
	if _, ok := first["name"]; !ok {
		t.Fatalf("command missing name: %v", first)
	}
}

func TestConfigOptionKinds(t *testing.T) {
	opts := MilkConfigOptions(true, []string{"qwen-local", "claude"}, "qwen-local", []string{"qwen2.5-coder", "claude-sonnet-5"}, "qwen2.5-coder")
	if len(opts) != 3 {
		t.Fatalf("opts = %+v", opts)
	}
	think, agent, model := opts[0], opts[1], opts[2]

	if think.ConfigID != ConfigIDThink || think.Type != ConfigTypeBoolean || think.Category != ConfigCategoryThoughtLevel {
		t.Fatalf("think = %+v", think)
	}
	if v, ok := think.BoolValue(); !ok || !v {
		t.Fatalf("think value = %v %v", v, ok)
	}

	if agent.ConfigID != ConfigIDAgent || agent.Type != ConfigTypeSelect || agent.Category != ConfigCategoryMode {
		t.Fatalf("agent = %+v", agent)
	}
	if v, _ := agent.SelectValue(); v != "qwen-local" {
		t.Fatalf("agent value = %q", v)
	}
	if len(agent.Options) != 2 {
		t.Fatalf("agent options = %+v", agent.Options)
	}

	if model.ConfigID != ConfigIDModel || model.Category != ConfigCategoryModel {
		t.Fatalf("model = %+v", model)
	}

	// Wire shape: type discriminator + currentValue + options.
	body := jsonBody(t, agent)
	if body["type"] != "select" || body["configId"] != "agent" {
		t.Fatalf("agent wire = %v", body)
	}
	if _, ok := body["options"].([]any); !ok {
		t.Fatalf("agent options wire = %v", body)
	}
}

func TestConfigStateSet(t *testing.T) {
	var thinkOn, switched bool
	var model string
	cs := NewConfigState(
		MilkConfigOptions(false, []string{"a", "b"}, "a", []string{"m1", "m2"}, "m1"),
		map[SessionConfigID]ConfigSetter{
			ConfigIDThink: func(v any) error { thinkOn = v.(bool); return nil },
			ConfigIDAgent: func(v any) error { switched = true; return nil },
			ConfigIDModel: func(v any) error { model = v.(string); return nil },
		},
	)

	// /think on|off as a boolean option.
	resp, err := cs.Set(SetSessionConfigOptionRequest{ConfigID: ConfigIDThink, Type: "boolean", Value: true})
	if err != nil {
		t.Fatalf("set think: %v", err)
	}
	if !thinkOn || len(resp.ConfigOptions) != 3 {
		t.Fatalf("think resp = %+v", resp)
	}

	// /agent switch as a select id (string and value-id forms equivalent).
	if err := cs.SetAgent("b"); err != nil {
		t.Fatalf("set agent: %v", err)
	}
	if !switched {
		t.Fatal("agent setter not called")
	}
	opts := cs.Options()
	if v, _ := opts[1].SelectValue(); v != "b" {
		t.Fatalf("agent value = %q", v)
	}

	// /model as a select id.
	if err := cs.SetModel("m2"); err != nil {
		t.Fatalf("set model: %v", err)
	}
	if model != "m2" {
		t.Fatalf("model setter = %q", model)
	}

	// Validation: unknown id, wrong type, unknown value, setter rejection.
	if _, err := cs.Set(SetSessionConfigOptionRequest{ConfigID: "nope", Type: "boolean", Value: true}); err == nil {
		t.Fatal("unknown configId accepted")
	}
	if _, err := cs.Set(SetSessionConfigOptionRequest{ConfigID: ConfigIDThink, Type: "id", Value: "on"}); err == nil {
		t.Fatal("wrong value type accepted")
	}
	if _, err := cs.Set(SetSessionConfigOptionRequest{ConfigID: ConfigIDModel, Type: "id", Value: "m3"}); err == nil {
		t.Fatal("unknown select value accepted")
	}
	cs2 := NewConfigState(
		MilkConfigOptions(false, []string{"a"}, "a", []string{"m1"}, "m1"),
		map[SessionConfigID]ConfigSetter{
			ConfigIDModel: func(any) error { return context.DeadlineExceeded },
		},
	)
	if err := cs2.SetModel("m1"); err == nil {
		t.Fatal("setter rejection accepted")
	}
	if v, _ := cs2.Options()[2].SelectValue(); v != "m1" {
		t.Fatalf("rolled-back model = %q", v)
	}

	// config_option_update carries the same full set.
	upd := cs.Update()
	if upd.SessionUpdate != "config_option_update" || len(upd.ConfigOptions) != 3 {
		t.Fatalf("update = %+v", upd)
	}
	m := Mapper{Session: "sess-1"}
	n := m.ConfigOptions(cs.Options())
	if n.Method != MethodSessionUpdate {
		t.Fatalf("method = %q", n.Method)
	}
}
