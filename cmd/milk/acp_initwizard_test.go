package main

// Tests for the /config init wizard over ACP's input surfaces: elicitation
// form dialogs (titled choices, pre-populated defaults) on form-capable
// clients, clickable choice prompts (session/request_permission options —
// "use default" as a button) elsewhere, typed chat input as the floor with
// the "default" answer word that replaces the empty turn ACP chat can't
// send, and the credential step's skip/type clicks.

import (
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/transport/acp"
)

// acpFormSession returns a session whose client advertised form-mode
// elicitation under the released wire name (`clientCapabilities`), with the
// conn scripted to answer every elicitation/create from responses in order
// (beyond the list: an accept with empty content = the untouched default).
// Recorded requests land in reqs.
func acpFormSession(t *testing.T, responses []acp.CreateElicitationResponse) (*acpServer, *fakeACPConn, acp.SessionID, *[]acp.CreateElicitationRequest) {
	t.Helper()
	server, conn := acpTestServer(t, "model reply")
	acpRequest(t, server, "initialize", map[string]any{
		"protocolVersion":    acp.ProtocolVersion,
		"clientCapabilities": map[string]any{"elicitation": map[string]any{"form": map[string]any{}}},
	})
	id := acpNewSession(t, server)

	var reqs []acp.CreateElicitationRequest
	var n int
	conn.respond = func(method string, params any) (any, error) {
		if method != acp.MethodElicitationCreate {
			return nil, nil
		}
		req := params.(acp.CreateElicitationRequest)
		reqs = append(reqs, req)
		resp := acp.CreateElicitationResponse{Action: acp.ElicitationAccept, Content: map[string]any{}}
		if n < len(responses) {
			resp = responses[n]
		}
		n++
		return resp, nil
	}
	return server, conn, id, &reqs
}

// acpElicitedNames returns the form-field key of each recorded request.
func acpElicitedNames(reqs []acp.CreateElicitationRequest) []string {
	var names []string
	for _, r := range reqs {
		for name := range r.RequestedSchema.Properties {
			names = append(names, name)
		}
	}
	return names
}

// TestACPInit_FormDialogsDriveWizard: with form elicitation the whole
// first-run flow completes inside the /init turn as form dialogs — no chat
// answers — ending with the config written and the wizard cleared.
func TestACPInit_FormDialogsDriveWizard(t *testing.T) {
	server, conn, id, reqs := acpFormSession(t, []acp.CreateElicitationResponse{
		{Action: acp.ElicitationAccept, Content: map[string]any{}}, // name → default "local"
		{Action: acp.ElicitationAccept, Content: map[string]any{"provider": "local"}},
		{Action: acp.ElicitationAccept, Content: map[string]any{"url": "http://localhost:8080"}},
		{Action: acp.ElicitationAccept, Content: map[string]any{}}, // run_cmd → skip
		{Action: acp.ElicitationAccept, Content: map[string]any{"model": "qwen2.5-coder"}},
		{Action: acp.ElicitationAccept, Content: map[string]any{}}, // limits → no catalog match
		{Action: acp.ElicitationAccept, Content: map[string]any{"escalation": "n"}},
	})
	as := server.session(id)

	acpPromptText(t, server, id, "/init")

	if as.pendingInit != nil {
		t.Fatal("the form flow must finish the wizard inside the /init turn")
	}
	names := acpElicitedNames(*reqs)
	want := []string{"agent_name", "provider", "url", "run_cmd", "model", "context_window_tokens", "escalation"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("elicited fields = %v, want %v", names, want)
	}

	// The choices and defaults surface is what makes this clickable: the
	// name field pre-populates its default and the provider field is a
	// titled select.
	first := (*reqs)[0].RequestedSchema
	if d := first.Properties["agent_name"].Default; d != "local" {
		t.Errorf("agent_name default = %v, want local (clients pre-populate it)", d)
	}
	prov := (*reqs)[1].RequestedSchema.Properties["provider"]
	if len(prov.OneOf) != 6 || prov.Default != "local" {
		t.Errorf("provider field = %+v, want a 6-choice select defaulting to local", prov)
	}

	// Every dialog round trip is closed out with elicitation/complete.
	completes := 0
	for _, n := range conn.sent() {
		if n.Method == acp.MethodElicitationComplete {
			completes++
		}
	}
	if completes != len(want) {
		t.Errorf("elicitation/complete notifications = %d, want one per dialog (%d)", completes, len(want))
	}

	chunks := acpChunks(conn)
	if len(chunks) != 1 {
		t.Fatalf("chunks = %q, want the single /init turn output", chunks)
	}
	if !strings.Contains(chunks[0], "config written to") || !strings.Contains(chunks[0], "qwen2.5-coder") {
		t.Errorf("chunk = %q, want the completion summary", chunks[0])
	}
	if strings.Contains(chunks[0], "model reply") {
		t.Errorf("wizard output must never reach the model: %q", chunks[0])
	}

	// And the committed config is live in the session.
	if got := as.st.cfg.ActiveAgent().URL; got != "http://localhost:8080" {
		t.Errorf("active agent URL = %q, want the wizard's URL", got)
	}
}

// TestACPInit_FormSecretStepInChatThenResumes: the credential step cannot go
// through a form (the spec forbids secrets in form mode) — it drops to chat
// once, then the remaining steps return to dialogs and the wizard finishes.
func TestACPInit_FormSecretStepInChatThenResumes(t *testing.T) {
	server, conn, id, reqs := acpFormSession(t, []acp.CreateElicitationResponse{
		{Action: acp.ElicitationAccept, Content: map[string]any{"agent_name": "orb"}},
		{Action: acp.ElicitationAccept, Content: map[string]any{"provider": "bearer"}},
		{Action: acp.ElicitationAccept, Content: map[string]any{"url": "https://openrouter.ai/api/v1"}},
		{Action: acp.ElicitationAccept, Content: map[string]any{}}, // chat_path → default
		{Action: acp.ElicitationAccept, Content: map[string]any{"model": "meta-llama/llama-3.1-8b-instruct"}},
		// (no response for the credential step — it is asked in chat)
		{Action: acp.ElicitationAccept, Content: map[string]any{}}, // limits
		{Action: acp.ElicitationAccept, Content: map[string]any{"escalation": "y"}},
	})
	as := server.session(id)

	acpPromptText(t, server, id, "/init")

	if as.pendingInit == nil {
		t.Fatal("the wizard must wait for the credential step in chat")
	}
	names := acpElicitedNames(*reqs)
	if strings.Join(names, ",") != "agent_name,provider,url,chat_path,model" {
		t.Fatalf("elicited fields before the credential step = %v", names)
	}
	chunks := acpChunks(conn)
	if !strings.Contains(chunks[0], "API key") || !strings.Contains(chunks[0], "continuing the setup wizard in chat") {
		t.Errorf("chunk = %q, want the credential question with the chat hand-off note", chunks[0])
	}

	// Answering in chat resumes the dialogs (limits, escalation) and the
	// wizard finishes without another prompt.
	acpPromptText(t, server, id, "sk-test")
	if as.pendingInit != nil {
		t.Fatal("wizard must be done after the remaining dialogs")
	}
	names = acpElicitedNames(*reqs)
	if strings.Join(names, ",") != "agent_name,provider,url,chat_path,model,context_window_tokens,escalation" {
		t.Fatalf("elicited fields = %v, want the resumed tail after the credential answer", names)
	}
	for _, r := range *reqs {
		if _, has := r.RequestedSchema.Properties["api_key"]; has {
			t.Fatal("the credential step must never be requested as a form")
		}
	}
	chunks = acpChunks(conn)
	last := chunks[len(chunks)-1]
	if !strings.Contains(last, "config written to") {
		t.Errorf("last chunk = %q, want the completion summary", last)
	}
	if got := as.st.cfg.ActiveAgent().APIKey; got != "sk-test" {
		t.Errorf("committed APIKey = %q, want the chat answer", got)
	}
}

// TestACPInit_DismissedDialogFallsBackToChat: a declined dialog hands the
// step to chat with every applied answer kept, and chat answers keep working
// (including the "default" word and provider names).
func TestACPInit_DismissedDialogFallsBackToChat(t *testing.T) {
	server, conn, id, _ := acpFormSession(t, nil)
	conn.respond = func(method string, params any) (any, error) {
		return acp.CreateElicitationResponse{Action: "decline"}, nil
	}
	as := server.session(id)

	acpPromptText(t, server, id, "/init")
	if as.pendingInit == nil {
		t.Fatal("a declined dialog must hand the wizard to chat, not end it")
	}
	chunks := acpChunks(conn)
	if !strings.Contains(chunks[0], "continuing the setup wizard in chat") || !strings.Contains(chunks[0], "primary agent name") {
		t.Errorf("chunk = %q, want the chat hand-off note + the name question", chunks[0])
	}

	acpPromptText(t, server, id, "default") // the empty turn's stand-in → name "local"
	acpPromptText(t, server, id, "local")   // provider by name, not menu number

	chunks = acpChunks(conn)
	if !strings.Contains(chunks[1], "provider — select") {
		t.Errorf("after 'default': chunk = %q, want the provider question (default applied)", chunks[1])
	}
	if !strings.Contains(chunks[2], "server URL") {
		t.Errorf("after provider by name: chunk = %q, want the URL question", chunks[2])
	}
	if as.pendingInit == nil || as.pendingInit.step != initStepURL {
		t.Fatalf("wizard state = %+v, want pending at initStepURL", as.pendingInit)
	}
}

// TestACPInit_ChatDefaultWordWithoutForms: without elicitation the chat
// wizard runs as before, plus the "default" answer word for the empty turn
// ACP chat can't send.
func TestACPInit_ChatDefaultWordWithoutForms(t *testing.T) {
	server, conn := acpTestServer(t, "model reply")
	id := acpNewSession(t, server)
	as := server.session(id)

	acpPromptText(t, server, id, "/init")
	if as.formElicit {
		t.Fatal("no initialize capabilities → the chat wizard, not forms")
	}
	acpPromptText(t, server, id, "default")
	acpPromptText(t, server, id, "bedrock")

	chunks := acpChunks(conn)
	if !strings.Contains(chunks[0], "primary agent name") || !strings.Contains(chunks[0], "type 'default'") {
		t.Errorf("chunk = %q, want the name question + the default-word hint", chunks[0])
	}
	if !strings.Contains(chunks[1], "provider — select") {
		t.Errorf("after 'default': chunk = %q, want the provider question (default applied)", chunks[1])
	}
	if !strings.Contains(chunks[2], "server URL") {
		t.Errorf("after 'bedrock': chunk = %q, want the URL question", chunks[2])
	}
	if as.pendingInit == nil || as.pendingInit.step != initStepURL {
		t.Fatalf("wizard state = %+v, want pending at initStepURL", as.pendingInit)
	}
}

// acpChoiceSession returns a session without elicitation whose client
// answers choice prompts (session/request_permission) via pick: it returns
// the option ID to select ("pick-N" / "cancel"; "" simulates the client gap —
// a response with no recognizable outcome). Recorded prompts land in reqs.
func acpChoiceSession(t *testing.T, pick func(req acp.RequestPermissionRequest) string) (*acpServer, *fakeACPConn, acp.SessionID, *[]acp.RequestPermissionRequest) {
	t.Helper()
	server, conn := acpTestServer(t, "model reply")
	id := acpNewSession(t, server)
	var reqs []acp.RequestPermissionRequest
	conn.respond = func(method string, params any) (any, error) {
		if method != acp.MethodRequestPermission {
			return nil, nil
		}
		req := params.(acp.RequestPermissionRequest)
		reqs = append(reqs, req)
		optID := pick(req)
		if optID == "" {
			return acp.RequestPermissionResponse{}, nil
		}
		return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Outcome: "selected", OptionID: optID}}, nil
	}
	return server, conn, id, &reqs
}

// TestACPInit_ChoiceClicksDriveWizard: without forms — and with no way to
// send an empty turn — the wizard still completes entirely out of the choice
// prompts, one click per step ("use default" is a button), ending with the
// config written inside the /init turn.
func TestACPInit_ChoiceClicksDriveWizard(t *testing.T) {
	server, conn, id, reqs := acpChoiceSession(t, func(acp.RequestPermissionRequest) string { return "pick-0" })
	as := server.session(id)

	acpPromptText(t, server, id, "/init")

	if as.pendingInit != nil {
		t.Fatal("the choice flow must finish the wizard inside the /init turn")
	}
	if len(*reqs) != 7 {
		t.Fatalf("choice prompts = %d, want one per step (7)", len(*reqs))
	}
	first := (*reqs)[0]
	if !strings.Contains(first.Title, "primary agent name") {
		t.Errorf("first prompt title = %q, want the step's question", first.Title)
	}
	var names []string
	for _, o := range first.Options {
		names = append(names, o.Name)
	}
	if strings.Join(names, " | ") != "use default — local | type my own value… | cancel setup" {
		t.Errorf("first prompt options = %v, want clickable default + typed escape + cancel", names)
	}

	chunks := acpChunks(conn)
	if len(chunks) != 1 || !strings.Contains(chunks[0], "config written to") {
		t.Fatalf("chunks = %q, want the single /init turn's completion summary", chunks)
	}
	if strings.Contains(chunks[0], "model reply") {
		t.Errorf("wizard output must never reach the model: %q", chunks[0])
	}
	ag := as.st.cfg.ActiveAgent()
	if ag.Name != "local" || ag.URL != "http://localhost:8080" || ag.Model != "qwen2.5-coder" {
		t.Errorf("committed agent = %+v, want the one-click default + example answers", ag)
	}
}

// TestACPInit_ChoiceCustomFallsToTypedInput: "type my own value…" hands just
// that step to typed input (question + hint), the typed answer lands, and
// the flow continues back on the buttons.
func TestACPInit_ChoiceCustomFallsToTypedInput(t *testing.T) {
	server, conn, id, reqs := acpChoiceSession(t, func(req acp.RequestPermissionRequest) string {
		if strings.Contains(req.Title, "primary agent name") {
			return "pick-1" // type my own value…
		}
		return "pick-0"
	})
	as := server.session(id)

	acpPromptText(t, server, id, "/init")
	if as.pendingInit == nil {
		t.Fatal("the typed escape must hand the step to chat, not end the wizard")
	}
	chunks := acpChunks(conn)
	if !strings.Contains(chunks[0], "primary agent name") || !strings.Contains(chunks[0], "type 'default'") {
		t.Errorf("chunk = %q, want the name question + its per-question default hint", chunks[0])
	}

	acpPromptText(t, server, id, "orb")
	if as.pendingInit != nil {
		t.Fatal("wizard must be done after the typed answer + remaining clicks")
	}
	if got := as.st.cfg.ActiveAgent().Name; got != "orb" {
		t.Errorf("committed name = %q, want the typed answer", got)
	}
	if len(*reqs) != 7 {
		t.Errorf("choice prompts = %d, want the typed step skipped and 6 clicks around it", len(*reqs))
	}
}

// TestACPInit_ChoiceCancelAborts: the "cancel setup" button ends the wizard
// without writing anything.
func TestACPInit_ChoiceCancelAborts(t *testing.T) {
	server, conn, id, reqs := acpChoiceSession(t, func(acp.RequestPermissionRequest) string { return "cancel" })
	as := server.session(id)

	acpPromptText(t, server, id, "/init")

	if as.pendingInit != nil {
		t.Fatal("cancel must end the wizard")
	}
	if len(*reqs) != 1 {
		t.Errorf("choice prompts = %d, want exactly one before cancelling", len(*reqs))
	}
	chunks := acpChunks(conn)
	if !strings.Contains(chunks[0], "setup wizard cancelled") {
		t.Errorf("chunk = %q, want the cancelled note", chunks[0])
	}
	if strings.Contains(chunks[0], "config written to") {
		t.Errorf("cancel must not write the config: %q", chunks[0])
	}
}

// TestACPInit_ChoiceClientGapFallsBackSilently: a permission response with
// no recognizable outcome is a client gap, not a user error — the step goes
// to typed input with no blame text, and that client is never asked again.
func TestACPInit_ChoiceClientGapFallsBackSilently(t *testing.T) {
	server, conn, id, reqs := acpChoiceSession(t, func(acp.RequestPermissionRequest) string { return "" })
	as := server.session(id)

	acpPromptText(t, server, id, "/init")
	chunks := acpChunks(conn)
	if !strings.Contains(chunks[0], "primary agent name") || !strings.Contains(chunks[0], "type 'default'") {
		t.Errorf("chunk = %q, want the name question + its per-question default hint", chunks[0])
	}
	for _, bad := range []string{"form dialog failed", "permission", "request_permission"} {
		if strings.Contains(chunks[0], bad) {
			t.Errorf("client gap must fall back silently, chunk mentions %q: %q", bad, chunks[0])
		}
	}

	acpPromptText(t, server, id, "default")
	if len(*reqs) != 1 {
		t.Errorf("choice prompts = %d, want the gap remembered and never retried", len(*reqs))
	}
	if as.pendingInit == nil {
		t.Fatal("the wizard must keep running as typed input")
	}
	chunks = acpChunks(conn)
	if !strings.Contains(chunks[1], "provider — select") {
		t.Errorf("after 'default': chunk = %q, want the provider question as typed input", chunks[1])
	}
}

// TestACPInit_ChoiceSecretSkipClick: the credential step's buttons carry no
// secret — just "skip — no credential" vs "type my own value…" — and one
// click on skip routes to the token-command step instead of asking for a key.
func TestACPInit_ChoiceSecretSkipClick(t *testing.T) {
	server, conn, id, reqs := acpChoiceSession(t, func(req acp.RequestPermissionRequest) string {
		if strings.Contains(req.Title, "provider") {
			return "pick-2" // bearer
		}
		return "pick-0"
	})
	as := server.session(id)

	acpPromptText(t, server, id, "/init")

	if as.pendingInit != nil {
		t.Fatal("the bearer flow must finish inside the /init turn")
	}
	var apiKeyPrompt *acp.RequestPermissionRequest
	for i := range *reqs {
		if strings.Contains((*reqs)[i].Title, "API key") {
			apiKeyPrompt = &(*reqs)[i]
		}
	}
	if apiKeyPrompt == nil {
		t.Fatal("the credential step must still offer its skip/type buttons")
	}
	var names []string
	for _, o := range apiKeyPrompt.Options {
		names = append(names, o.Name)
	}
	if strings.Join(names, " | ") != "skip — no credential | type my own value… | cancel setup" {
		t.Errorf("credential prompt options = %v, want skip/type/cancel (no secret on the wire)", names)
	}
	ag := as.st.cfg.ActiveAgent()
	if ag.Provider != "bearer" || ag.APIKey != "" || ag.URL != "https://openrouter.ai/api/v1" {
		t.Errorf("committed agent = %+v, want bearer with the key skipped", ag)
	}
	chunks := acpChunks(conn)
	if len(chunks) != 1 || !strings.Contains(chunks[0], "config written to") {
		t.Errorf("chunks = %q, want the single /init turn's completion summary", chunks)
	}
}
