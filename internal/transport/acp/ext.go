package acp

// Ext* extension notifications — the sanctioned ACP escape hatch for
// milk-specific payloads ("never a forked schema", ADR-0049 rule 1). Each is
// a JSON-RPC notification whose method is one of the milk/* names below;
// clients that ignore extensions lose only turn-unrelated notices. Payload
// fields use snake_case per the locked §8.3 conventions (standard ACP fields
// stay camelCase; these are milk's own keys).

// Milk extension notification methods (ExtNotification).
const (
	// ExtMethodNotification carries one ADR-0048 toast (id, severity,
	// command_hint, body) — the notification toasts + /notifications history
	// ring, minus the client chrome.
	ExtMethodNotification = "milk/notification"
	// ExtMethodWarning carries loop-detection / consumption signals
	// (Signal.Category, IsConsumption(), count/limit facts — issue #173
	// wording: "[⚠ loop detected: …]" vs "[⚠ consumption: …]").
	ExtMethodWarning = "milk/warning"
	// ExtMethodMemory carries percept/current-need records written during
	// the turn — the F1 memory panel feed.
	ExtMethodMemory = "milk/memory"
	// ExtMethodRoute carries routing decisions and self-escalation agent
	// switches (router.Decision, dispatch hand-off) — the status-bar
	// route/role indicator.
	ExtMethodRoute = "milk/route"
)

// ExtNotification is one milk/* extension notification: an ACP Ext*
// notification whose Method is one of the ExtMethod* names and whose Params
// carry the kind-specific payload below. It is also the Host.Notify carrier —
// the TUI host renders the same payloads as toasts/status-bar badges.
type ExtNotification struct {
	Method string `json:"method"`
	Params any    `json:"params,omitempty"`
}

// NotificationPayload is the milk/notification payload — one toast
// (ADR-0048: turn-unrelated informational event with a command hint).
type NotificationPayload struct {
	ID          string `json:"id"`
	Severity    string `json:"severity"` // "info" | "warning" | open-set
	CommandHint string `json:"command_hint,omitempty"`
	Body        string `json:"body"`
}

// WarningPayload is the milk/warning payload — one loop/consumption signal
// (issue #173: a consumption threshold crossing is not loop evidence, so the
// category and the consumption flag must both survive to the renderer).
type WarningPayload struct {
	Category    string `json:"category"` // loop.Signal.String()
	Consumption bool   `json:"consumption"`
	Count       int    `json:"count,omitempty"`
	Limit       int    `json:"limit,omitempty"`
	Message     string `json:"message"`
}

// MemoryPayload is the milk/memory payload — one memory-panel feed entry.
type MemoryPayload struct {
	Op        string `json:"op"` // "record" | "get" | "list" | "forget" (open-set)
	PerceptID string `json:"percept_id,omitempty"`
	Subject   string `json:"subject,omitempty"`
}

// RoutePayload is the milk/route payload — a routing decision and/or a
// self-escalation agent switch. Target is router.Target ("primary" or
// "escalation"); From/To are set only for agent switches.
type RoutePayload struct {
	Target     string `json:"target,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Conclusive bool   `json:"conclusive,omitempty"`
	From       string `json:"from,omitempty"`
	To         string `json:"to,omitempty"`
}

// NotificationExt builds a milk/notification extension message.
func NotificationExt(p NotificationPayload) ExtNotification {
	return ExtNotification{Method: ExtMethodNotification, Params: p}
}

// WarningExt builds a milk/warning extension message.
func WarningExt(p WarningPayload) ExtNotification {
	return ExtNotification{Method: ExtMethodWarning, Params: p}
}

// MemoryExt builds a milk/memory extension message.
func MemoryExt(p MemoryPayload) ExtNotification {
	return ExtNotification{Method: ExtMethodMemory, Params: p}
}

// RouteExt builds a milk/route extension message.
func RouteExt(p RoutePayload) ExtNotification {
	return ExtNotification{Method: ExtMethodRoute, Params: p}
}
