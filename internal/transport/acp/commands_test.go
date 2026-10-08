package acp

import (
	"context"
	"github.com/scoutme/milk/internal/livebuf"
	"github.com/scoutme/milk/internal/tasks"
	"testing"
)

// available_commands_update + session/set_config_option (commands.go):
// slash commands become editor input completion; /think, /agent switch and
// /model become SessionConfigOptions.

func TestAvailableCommandsWireShape(t *testing.T) {
	cmds := []AvailableCommand{
		Command("/think", "toggle reasoning", "on|off"),
		Command("/new", "fresh session", ""),
	}
	if cmds[0].Name != "think" || cmds[0].Input == nil || cmds[0].Input.Type != CommandInputText || cmds[0].Input.Hint != "on|off" {
		t.Fatalf("think = %+v", cmds[0])
	}
	if cmds[1].Input != nil {
		t.Fatalf("no-hint command has input: %+v", cmds[1].Input)
	}

	upd := NewAvailableCommandsUpdate(cmds)
	body := jsonBody(t, upd)
	if body["sessionUpdate"] != "available_commands_update" {
		t.Fatalf("wire sessionUpdate = %v", body["sessionUpdate"])
	}
	list, ok := body["availableCommands"].([]any)
	if !ok || len(list) != len(cmds) {
		t.Fatalf("availableCommands = %v", body["availableCommands"])
	}
	for _, raw := range list {
		c := raw.(map[string]any)
		name, _ := c["name"].(string)
		if name == "" || name[0] == '/' {
			t.Fatalf("command name must be non-empty and have no leading slash: %v", c)
		}
		if d, _ := c["description"].(string); d == "" {
			t.Fatalf("command %q missing required description", name)
		}
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

func TestPlanEntriesForTasksAndV1Shape(t *testing.T) {
	ts := []tasks.Task{
		{Title: "a", Status: tasks.StatusPending},
		{Title: "b", Status: tasks.StatusInProgress},
		{Title: "c", Status: tasks.StatusDone},
		{Title: "d", Status: tasks.StatusBlocked},
	}
	entries := PlanEntriesForTasks(ts)
	want := []PlanEntryStatus{PlanEntryPending, PlanEntryInProgress, PlanEntryCompleted, PlanEntryPending}
	for i, e := range entries {
		if e.Status != want[i] || e.Priority != PlanPriorityMedium {
			t.Errorf("entry %d = %+v, want status %s / medium", i, e, want[i])
		}
	}
	if entries[3].Content != "d (blocked)" {
		t.Errorf("blocked content = %q", entries[3].Content)
	}

	v2 := jsonBody(t, NewPlanUpdate("tasks-1", entries))
	if v2["sessionUpdate"] != "plan_update" {
		t.Errorf("v2 sessionUpdate = %v", v2["sessionUpdate"])
	}

	cancelled := append(entries, PlanEntry{Content: "x", Priority: PlanPriorityLow, Status: PlanEntryCancelled})
	v1 := jsonBody(t, NewPlanUpdate("tasks-1", cancelled).AsV1())
	if v1["sessionUpdate"] != "plan" {
		t.Errorf("v1 sessionUpdate = %v", v1["sessionUpdate"])
	}
	if _, has := v1["planId"]; has {
		t.Error("v1 plan must not carry planId")
	}
	last := v1["entries"].([]any)[4].(map[string]any)
	if last["status"] != "completed" || last["content"] != "x (cancelled)" {
		t.Errorf("v1 cancelled entry = %v, want completed with marker", last)
	}
}

func TestToolCallContentReplaceSendsFullContentOnlyWhenGrown(t *testing.T) {
	buf := livebuf.New(0)
	var cur BufCursor

	if got := ToolCallContentReplace("job:1", buf, &cur); got != nil {
		t.Fatalf("empty buffer produced %v", got)
	}
	buf.Append([]byte("hello "))
	got := ToolCallContentReplace("job:1", buf, &cur)
	if len(got) != 1 {
		t.Fatalf("got %d updates, want 1", len(got))
	}
	buf.Append([]byte("world"))
	got = ToolCallContentReplace("job:1", buf, &cur)
	upd, ok := got[0].(ToolCallUpdate)
	if !ok || len(upd.Content) != 1 || upd.Content[0].Content.Text != "hello world" {
		t.Fatalf("second update = %+v, want the full 'hello world'", got[0])
	}
	if got := ToolCallContentReplace("job:1", buf, &cur); got != nil {
		t.Fatalf("unchanged buffer re-sent content: %v", got)
	}
}

func TestConfigState_SyncAndRoutingOption(t *testing.T) {
	ran := 0
	c := NewConfigState(
		[]SessionConfigOption{ThinkOption(true), RoutingOption(RoutingAuto)},
		map[SessionConfigID]ConfigSetter{ConfigIDRouting: func(any) error { ran++; return nil }},
	)
	if !c.Sync(ConfigIDRouting, RoutingEscalation) {
		t.Fatal("Sync to a new value must report a change")
	}
	if c.Sync(ConfigIDRouting, RoutingEscalation) {
		t.Error("Sync to the same value must not report a change")
	}
	if c.Sync("nope", true) {
		t.Error("Sync of an unknown option must report no change")
	}
	if ran != 0 {
		t.Errorf("Sync ran the setter %d times; the command already applied the effect", ran)
	}
	if _, err := c.Set(SetSessionConfigOptionRequest{ConfigID: ConfigIDRouting, Type: "id", Value: "bogus"}); err == nil {
		t.Error("a routing value outside the declared set must be rejected")
	}
	if _, err := c.Set(SetSessionConfigOptionRequest{ConfigID: ConfigIDRouting, Type: "id", Value: RoutingPrimary}); err != nil || ran != 1 {
		t.Errorf("Set routing: err=%v setterRuns=%d", err, ran)
	}
}
