package main

import (
	"encoding/json"
	"fmt"

	"github.com/scoutme/milk/internal/transport/acp"
)

// ACP session config options (session/set_config_option): the editor-native
// settings surface for the knobs that also have slash commands. The commands
// and the options are two views of one state, never two copies of it:
//
//	think   → as.showThinking            (/think on|off)
//	routing → st.stickyEscalate/Primary  (/escalate, /primary, Ctrl+C unpin)
//
// Setting an option runs the same state change the command does; running a
// command re-reads the state and pushes config_option_update when the option
// moved (pushConfigOptions). /agent switch and /model have no ACP command
// (acpAgent is list-only), so they are not advertised here either.

// newACPConfigState builds the session's option surface with its setters.
func (as *acpSession) newACPConfigState() *acp.ConfigState {
	return acp.NewConfigState(
		[]acp.SessionConfigOption{
			acp.ThinkOption(as.showThinking.Load()),
			acp.RoutingOption(as.routingValue()),
		},
		map[acp.SessionConfigID]acp.ConfigSetter{
			acp.ConfigIDThink: func(v any) error {
				on, _ := v.(bool)
				as.showThinking.Store(on)
				return nil
			},
			acp.ConfigIDRouting: func(v any) error {
				value, _ := v.(string)
				return as.setRouting(value)
			},
		},
	)
}

// routingValue derives the routing option from the pins the router reads. An
// auto-sticky escalation is the router's own choice, not a pin, so it still
// reads as "auto".
func (as *acpSession) routingValue() string {
	as.loopMu.Lock()
	defer as.loopMu.Unlock()
	switch {
	case as.st.stickyEscalate:
		return acp.RoutingEscalation
	case as.st.stickyPrimary:
		return acp.RoutingPrimary
	}
	return acp.RoutingAuto
}

// setRouting applies a routing value through the /escalate and /primary
// executors themselves (pins, ESCALATION_WAITING reset, repetition baseline).
func (as *acpSession) setRouting(value string) error {
	as.loopMu.Lock()
	defer as.loopMu.Unlock()
	switch value {
	case acp.RoutingEscalation:
		handleSlashCommand(cmdEscalate, "", as.st)
	case acp.RoutingPrimary:
		handleSlashCommand(cmdPrimary, "", as.st)
	case acp.RoutingAuto:
		clearRoutingPins(as.st)
	default:
		return fmt.Errorf("unknown routing value %q", value)
	}
	return nil
}

// pushConfigOptions re-reads the state behind each option after a slash
// command and, if any moved, tells the client with config_option_update.
func (as *acpSession) pushConfigOptions() {
	if as.config == nil {
		return
	}
	changed := as.config.Sync(acp.ConfigIDThink, as.showThinking.Load())
	if as.config.Sync(acp.ConfigIDRouting, as.routingValue()) {
		changed = true
	}
	if changed {
		as.notify(as.config.Update())
	}
}

// handleSetConfigOption implements session/set_config_option.
func (s *acpServer) handleSetConfigOption(params json.RawMessage) (any, error) {
	var req acp.SetSessionConfigOptionRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, invalidParams("session/set_config_option", err)
	}
	as := s.session(req.SessionID)
	if as == nil {
		return nil, &acp.CodedError{Code: acp.CodeResourceNotFound,
			Message: fmt.Sprintf("session/set_config_option: unknown session %q", req.SessionID)}
	}
	resp, err := as.config.Set(req)
	if err != nil {
		return nil, invalidParams("session/set_config_option", err)
	}
	return resp, nil
}
