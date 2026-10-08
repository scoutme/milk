package acp

import (
	"fmt"
	"strings"
	"sync"
)

// available_commands_update + session/set_config_option (with the
// config_option_update mirror) — milk's slash-command surface and its three
// session-configurable knobs become native editor input completion and
// settings UI (design §7.1: "/think on|off, /agent switch, /model as
// SessionConfigOptions (v2 replaced v1's session/set_mode)").

// AvailableCommandInput hints: "text" means all text typed after the command
// name is provided as input; the hint shows while input is empty
// (TextCommandInput.hint).
const CommandInputText = "text"

// AvailableCommandInput is the "text" variant of the AvailableCommandInput
// union (TextCommandInput).
type AvailableCommandInput struct {
	Type string `json:"type"` // "text"
	Hint string `json:"hint"`
}

// AvailableCommand is one advertised slash command.
type AvailableCommand struct {
	Name        string                 `json:"name"`        // without the leading "/"; clients prepend it
	Description string                 `json:"description"` // required by the spec
	Input       *AvailableCommandInput `json:"input,omitempty"`
}

// Command builds an available command. A leading "/" on name is stripped (the
// wire form has none). Empty hint → no structured input.
func Command(name, description, hint string) AvailableCommand {
	name = strings.TrimPrefix(name, "/")
	if hint == "" {
		return AvailableCommand{Name: name, Description: description}
	}
	return AvailableCommand{Name: name, Description: description, Input: &AvailableCommandInput{Type: CommandInputText, Hint: hint}}
}

// AvailableCommandsUpdate is the available_commands_update session update.
// Later updates replace the whole list.
type AvailableCommandsUpdate struct {
	SessionUpdate     string             `json:"sessionUpdate"` // "available_commands_update"
	AvailableCommands []AvailableCommand `json:"availableCommands"`
	Meta              map[string]any     `json:"_meta,omitempty"`
}

func (AvailableCommandsUpdate) isSessionUpdate() {}

// NewAvailableCommandsUpdate wraps the advertised commands.
func NewAvailableCommandsUpdate(cmds []AvailableCommand) AvailableCommandsUpdate {
	return AvailableCommandsUpdate{SessionUpdate: "available_commands_update", AvailableCommands: cmds}
}

// --- session config options (/think, /agent switch, /model) ----------------

// Session config option IDs. Each maps onto a slash command the TUI already
// has (or, for model, the /agent-switch sibling config surface):
//
//	think → /think on|off
//	agent → /agent switch <name>
//	model → /model <id>
//	routing → /escalate, /primary (pin all turns) / neither (router decides)
const (
	ConfigIDThink   SessionConfigID = "think"
	ConfigIDAgent   SessionConfigID = "agent"
	ConfigIDModel   SessionConfigID = "model"
	ConfigIDRouting SessionConfigID = "routing"
)

// Routing option values: which agent handles turns.
const (
	RoutingAuto       = "auto"       // the router decides each turn (no pin)
	RoutingPrimary    = "primary"    // /primary — pinned to the primary agent
	RoutingEscalation = "escalation" // /escalate — pinned to the escalation agent
)

// SessionConfigOptionCategory values used by milk's options (the upstream
// enum is open-set).
const (
	ConfigCategoryThoughtLevel = "thought_level" // /think
	ConfigCategoryMode         = "mode"          // /agent switch (v1's session/set_mode replacement)
	ConfigCategoryModel        = "model"         // /model
)

// Session config option type discriminators (SessionConfigOption union).
const (
	ConfigTypeSelect  = "select"
	ConfigTypeBoolean = "boolean"
)

// SessionConfigSelectOption is one selectable value.
type SessionConfigSelectOption struct {
	Value SessionConfigValueID `json:"value"`
	Name  string               `json:"name"`
}

// SessionConfigOption is one session configuration option
// (SessionConfigOption union: select or boolean; currentValue carries the
// matching variant's value).
type SessionConfigOption struct {
	ConfigID     SessionConfigID             `json:"configId"`
	Name         string                      `json:"name"`
	Category     string                      `json:"category,omitempty"`
	Type         string                      `json:"type"` // "select" | "boolean" (open-set)
	CurrentValue any                         `json:"currentValue"`
	Options      []SessionConfigSelectOption `json:"options,omitempty"`
}

// BoolValue returns the current value of a boolean option.
func (o SessionConfigOption) BoolValue() (bool, bool) {
	v, ok := o.CurrentValue.(bool)
	return v, ok
}

// SelectValue returns the current value of a select option.
func (o SessionConfigOption) SelectValue() (SessionConfigValueID, bool) {
	switch v := o.CurrentValue.(type) {
	case string:
		return SessionConfigValueID(v), true
	case SessionConfigValueID:
		return v, true
	}
	return "", false
}

// ThinkOption builds the /think on|off boolean option.
func ThinkOption(on bool) SessionConfigOption {
	return SessionConfigOption{
		ConfigID:     ConfigIDThink,
		Name:         "Reasoning visibility (/think)",
		Category:     ConfigCategoryThoughtLevel,
		Type:         ConfigTypeBoolean,
		CurrentValue: on,
	}
}

// AgentOption builds the /agent switch select option over the configured
// agent names (current is the active primary agent).
func AgentOption(agents []string, current string) SessionConfigOption {
	opts := make([]SessionConfigSelectOption, 0, len(agents))
	for _, a := range agents {
		opts = append(opts, SessionConfigSelectOption{Value: SessionConfigValueID(a), Name: a})
	}
	return SessionConfigOption{
		ConfigID:     ConfigIDAgent,
		Name:         "Active agent (/agent switch)",
		Category:     ConfigCategoryMode,
		Type:         ConfigTypeSelect,
		CurrentValue: current,
		Options:      opts,
	}
}

// ModelOption builds the /model select option over the known model IDs.
func ModelOption(models []string, current string) SessionConfigOption {
	opts := make([]SessionConfigSelectOption, 0, len(models))
	for _, m := range models {
		opts = append(opts, SessionConfigSelectOption{Value: SessionConfigValueID(m), Name: m})
	}
	return SessionConfigOption{
		ConfigID:     ConfigIDModel,
		Name:         "Model (/model)",
		Category:     ConfigCategoryModel,
		Type:         ConfigTypeSelect,
		CurrentValue: SessionConfigValueID(current),
		Options:      opts,
	}
}

// RoutingOption builds the routing select option (current is one of the
// Routing* values).
func RoutingOption(current string) SessionConfigOption {
	return SessionConfigOption{
		ConfigID: ConfigIDRouting,
		Name:     "Routing (/escalate, /primary)",
		Category: ConfigCategoryMode,
		Type:     ConfigTypeSelect,
		Options: []SessionConfigSelectOption{
			{Value: RoutingAuto, Name: "Auto (router decides)"},
			{Value: RoutingPrimary, Name: "Primary agent (pinned)"},
			{Value: RoutingEscalation, Name: "Escalation agent (pinned)"},
		},
		CurrentValue: current,
	}
}

// MilkConfigOptions builds milk's full session-config surface.
func MilkConfigOptions(thinkOn bool, agents []string, activeAgent string, models []string, activeModel string) []SessionConfigOption {
	return []SessionConfigOption{
		ThinkOption(thinkOn),
		AgentOption(agents, activeAgent),
		ModelOption(models, activeModel),
	}
}

// SetSessionConfigOptionRequest is the session/set_config_option params
// (client→agent): the value variant is discriminated by Type ("id" for
// select values, "boolean" for booleans).
type SetSessionConfigOptionRequest struct {
	SessionID SessionID       `json:"sessionId"`
	ConfigID  SessionConfigID `json:"configId"`
	Type      string          `json:"type"` // "id" | "boolean" (open-set)
	Value     any             `json:"value"`
}

// SetSessionConfigOptionResponse is the session/set_config_option result: the
// full set of options with their new current values.
type SetSessionConfigOptionResponse struct {
	ConfigOptions []SessionConfigOption `json:"configOptions"`
	Meta          map[string]any        `json:"_meta,omitempty"`
}

// ConfigOptionUpdate is the config_option_update session update (the mirror
// the agent pushes proactively; the response to set_config_option carries the
// same shape).
type ConfigOptionUpdate struct {
	SessionUpdate string                `json:"sessionUpdate"` // "config_option_update"
	ConfigOptions []SessionConfigOption `json:"configOptions"`
	Meta          map[string]any        `json:"_meta,omitempty"`
}

func (ConfigOptionUpdate) isSessionUpdate() {}

// ConfigSetter applies one option's new value (e.g. flip think visibility,
// switch the active agent, switch model). Returning an error rejects the
// change and rolls the option back.
type ConfigSetter func(value any) error

// ConfigState owns the session's config options and serves
// session/set_config_option requests against them. Setters are how the
// slash-command side effects (/think, /agent switch, /model) stay in sync
// with the option surface.
type ConfigState struct {
	mu      sync.Mutex
	options []SessionConfigOption
	setters map[SessionConfigID]ConfigSetter
}

// NewConfigState seeds the option surface and its setters. Options without a
// setter still validate and update (pure state), just with no side effect.
func NewConfigState(opts []SessionConfigOption, setters map[SessionConfigID]ConfigSetter) *ConfigState {
	c := &ConfigState{options: append([]SessionConfigOption(nil), opts...), setters: map[SessionConfigID]ConfigSetter{}}
	for id, fn := range setters {
		c.setters[id] = fn
	}
	return c
}

// Options returns a copy of the current option set.
func (c *ConfigState) Options() []SessionConfigOption {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]SessionConfigOption(nil), c.options...)
}

// Update is the config_option_update for the current set.
func (c *ConfigState) Update() ConfigOptionUpdate {
	return ConfigOptionUpdate{SessionUpdate: "config_option_update", ConfigOptions: c.Options()}
}

// Sync records a value that changed outside session/set_config_option (a
// slash command flipped the same knob) without running the setter, which
// already happened. It reports whether the stored value actually changed, so
// the caller knows whether a config_option_update is due.
func (c *ConfigState) Sync(id SessionConfigID, value any) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.options {
		if c.options[i].ConfigID != id {
			continue
		}
		if c.options[i].CurrentValue == value {
			return false
		}
		c.options[i].CurrentValue = value
		return true
	}
	return false
}

// SetThink applies /think on|off through the option surface.
func (c *ConfigState) SetThink(on bool) error {
	_, err := c.Set(SetSessionConfigOptionRequest{ConfigID: ConfigIDThink, Type: "boolean", Value: on})
	return err
}

// SetAgent applies /agent switch <name> through the option surface.
func (c *ConfigState) SetAgent(name string) error {
	_, err := c.Set(SetSessionConfigOptionRequest{ConfigID: ConfigIDAgent, Type: "id", Value: SessionConfigValueID(name)})
	return err
}

// SetModel applies /model <id> through the option surface.
func (c *ConfigState) SetModel(id string) error {
	_, err := c.Set(SetSessionConfigOptionRequest{ConfigID: ConfigIDModel, Type: "id", Value: SessionConfigValueID(id)})
	return err
}

// Set validates and applies one session/set_config_option request, returning
// the full updated set (the schema's SetSessionConfigOptionResponse).
func (c *ConfigState) Set(req SetSessionConfigOptionRequest) (SetSessionConfigOptionResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.options {
		opt := &c.options[i]
		if opt.ConfigID != req.ConfigID {
			continue
		}
		if err := validateConfigValue(*opt, req); err != nil {
			return SetSessionConfigOptionResponse{ConfigOptions: c.snapshot()}, err
		}
		prev := opt.CurrentValue
		opt.CurrentValue = normalizeConfigValue(*opt, req)
		if setter := c.setters[req.ConfigID]; setter != nil {
			if err := setter(opt.CurrentValue); err != nil {
				opt.CurrentValue = prev // roll back — the change is rejected
				return SetSessionConfigOptionResponse{ConfigOptions: c.snapshot()}, err
			}
		}
		return SetSessionConfigOptionResponse{ConfigOptions: c.snapshot()}, nil
	}
	return SetSessionConfigOptionResponse{ConfigOptions: c.snapshot()},
		fmt.Errorf("acp: session/set_config_option: unknown configId %q", req.ConfigID)
}

// snapshot copies the current options (callers hold c.mu).
func (c *ConfigState) snapshot() []SessionConfigOption {
	return append([]SessionConfigOption(nil), c.options...)
}

// validateConfigValue checks a request value against the option's shape:
// booleans take type "boolean", selects take type "id" with a declared value.
func validateConfigValue(opt SessionConfigOption, req SetSessionConfigOptionRequest) error {
	switch opt.Type {
	case ConfigTypeBoolean:
		if req.Type != "boolean" {
			return fmt.Errorf("acp: session/set_config_option: %s takes a boolean value, got type %q", opt.ConfigID, req.Type)
		}
		if _, ok := req.Value.(bool); !ok {
			return fmt.Errorf("acp: session/set_config_option: %s value is not a boolean", opt.ConfigID)
		}
	case ConfigTypeSelect:
		if req.Type != "id" {
			return fmt.Errorf("acp: session/set_config_option: %s takes a value id, got type %q", opt.ConfigID, req.Type)
		}
		id := toValueID(req.Value)
		for _, o := range opt.Options {
			if o.Value == id {
				return nil
			}
		}
		return fmt.Errorf("acp: session/set_config_option: %s has no value %q", opt.ConfigID, id)
	}
	return nil
}

// normalizeConfigValue stores select values as their canonical value ID's
// string form (string and value-id request forms are equivalent; setters see
// the plain string).
func normalizeConfigValue(opt SessionConfigOption, req SetSessionConfigOptionRequest) any {
	if opt.Type == ConfigTypeSelect {
		return string(toValueID(req.Value))
	}
	return req.Value
}

func toValueID(v any) SessionConfigValueID {
	switch t := v.(type) {
	case string:
		return SessionConfigValueID(t)
	case SessionConfigValueID:
		return t
	default:
		return SessionConfigValueID(fmt.Sprint(t))
	}
}
