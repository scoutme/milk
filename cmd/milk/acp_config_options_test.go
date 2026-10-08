package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/scoutme/milk/internal/transport/acp"
)

func acpOptionValue(opts []acp.SessionConfigOption, id acp.SessionConfigID) any {
	for _, o := range opts {
		if o.ConfigID == id {
			return o.CurrentValue
		}
	}
	return nil
}

func acpConfigUpdates(conn *fakeACPConn) []acp.ConfigOptionUpdate {
	var out []acp.ConfigOptionUpdate
	for _, n := range conn.sent() {
		if w, ok := n.Params.(acp.UpdateSessionNotification); ok {
			if u, ok := w.Update.(acp.ConfigOptionUpdate); ok {
				out = append(out, u)
			}
		}
	}
	return out
}

func acpSetOption(t *testing.T, s *acpServer, id acp.SessionID, req acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	t.Helper()
	req.SessionID = id
	raw, _ := json.Marshal(req)
	res, err := s.HandleRequest(context.Background(), acp.MethodSessionSetConfigOption, raw)
	if err != nil {
		return acp.SetSessionConfigOptionResponse{}, err
	}
	return res.(acp.SetSessionConfigOptionResponse), nil
}

func TestACPConfigOptions_AdvertisedOnNewSession(t *testing.T) {
	server, _ := acpTestServer(t, "ok")
	resp := acpRequest(t, server, "session/new", acp.NewSessionRequest{CWD: t.TempDir()}).(acp.NewSessionResponse)
	if got := acpOptionValue(resp.ConfigOptions, acp.ConfigIDRouting); got != acp.RoutingAuto {
		t.Errorf("routing = %v, want %q", got, acp.RoutingAuto)
	}
	if got := acpOptionValue(resp.ConfigOptions, acp.ConfigIDThink); got != true {
		t.Errorf("think = %v, want true (default shows reasoning)", got)
	}
}

func TestACPConfigOptions_SetRoutingPinsLikeSlashCommands(t *testing.T) {
	server, conn := acpTestServer(t, "ok")
	id := acpNewSession(t, server)
	as := server.session(id)

	resp, err := acpSetOption(t, server, id, acp.SetSessionConfigOptionRequest{ConfigID: acp.ConfigIDRouting, Type: "id", Value: acp.RoutingEscalation})
	if err != nil {
		t.Fatal(err)
	}
	if !as.st.stickyEscalate || as.st.stickyPrimary {
		t.Errorf("escalation option: stickyEscalate=%v stickyPrimary=%v", as.st.stickyEscalate, as.st.stickyPrimary)
	}
	if got := acpOptionValue(resp.ConfigOptions, acp.ConfigIDRouting); got != acp.RoutingEscalation {
		t.Errorf("response routing = %v", got)
	}

	if _, err := acpSetOption(t, server, id, acp.SetSessionConfigOptionRequest{ConfigID: acp.ConfigIDRouting, Type: "id", Value: acp.RoutingPrimary}); err != nil {
		t.Fatal(err)
	}
	if as.st.stickyEscalate || !as.st.stickyPrimary {
		t.Errorf("primary option: stickyEscalate=%v stickyPrimary=%v", as.st.stickyEscalate, as.st.stickyPrimary)
	}

	as.st.autoStickyEscalate = true
	if _, err := acpSetOption(t, server, id, acp.SetSessionConfigOptionRequest{ConfigID: acp.ConfigIDRouting, Type: "id", Value: acp.RoutingAuto}); err != nil {
		t.Fatal(err)
	}
	if as.st.stickyEscalate || as.st.stickyPrimary || as.st.autoStickyEscalate {
		t.Errorf("auto option must clear every pin: %+v", as.st)
	}
	if n := len(acpConfigUpdates(conn)); n != 0 {
		t.Errorf("set_config_option answers in its response; got %d extra config_option_update pushes", n)
	}
}

func TestACPConfigOptions_SlashCommandsPushUpdate(t *testing.T) {
	server, conn := acpTestServer(t, "ok")
	id := acpNewSession(t, server)

	acpPromptText(t, server, id, "/escalate")
	ups := acpConfigUpdates(conn)
	if len(ups) != 1 || acpOptionValue(ups[0].ConfigOptions, acp.ConfigIDRouting) != acp.RoutingEscalation {
		t.Fatalf("/escalate: updates = %+v", ups)
	}

	acpPromptText(t, server, id, "/think off")
	ups = acpConfigUpdates(conn)
	if len(ups) != 2 || acpOptionValue(ups[1].ConfigOptions, acp.ConfigIDThink) != false {
		t.Fatalf("/think off: updates = %+v", ups)
	}

	acpPromptText(t, server, id, "/think off") // no change → no push
	if n := len(acpConfigUpdates(conn)); n != 2 {
		t.Errorf("unchanged state pushed an update: %d", n)
	}

	resume := acpRequest(t, server, "session/resume", acp.ResumeSessionRequest{SessionID: id, CWD: server.session(id).sess.CWD})
	rr, ok := resume.(resumeResult)
	if !ok {
		t.Fatalf("session/resume result = %T", resume)
	}
	if got := acpOptionValue(rr.ConfigOptions, acp.ConfigIDRouting); got != acp.RoutingEscalation {
		t.Errorf("resume reports routing = %v, want the live pin", got)
	}
}

func TestACPConfigOptions_SetThinkGatesThoughtChunks(t *testing.T) {
	server, conn := acpTestServer(t, "ok")
	id := acpNewSession(t, server)
	as := server.session(id)

	if _, err := acpSetOption(t, server, id, acp.SetSessionConfigOptionRequest{ConfigID: acp.ConfigIDThink, Type: "boolean", Value: false}); err != nil {
		t.Fatal(err)
	}
	before := len(conn.sent())
	as.onThinking("secret reasoning")
	if len(conn.sent()) != before {
		t.Error("thought forwarded after think option set to false")
	}
}

func TestACPConfigOptions_Rejections(t *testing.T) {
	server, _ := acpTestServer(t, "ok")
	id := acpNewSession(t, server)
	as := server.session(id)

	for name, req := range map[string]acp.SetSessionConfigOptionRequest{
		"unknown option": {ConfigID: "nope", Type: "boolean", Value: true},
		"bad routing":    {ConfigID: acp.ConfigIDRouting, Type: "id", Value: "sideways"},
		"wrong type":     {ConfigID: acp.ConfigIDRouting, Type: "boolean", Value: true},
	} {
		if _, err := acpSetOption(t, server, id, req); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if as.st.stickyEscalate || as.st.stickyPrimary {
		t.Error("a rejected request changed routing")
	}
	if _, err := acpSetOption(t, server, "no-such-session", acp.SetSessionConfigOptionRequest{ConfigID: acp.ConfigIDThink, Type: "boolean", Value: true}); err == nil {
		t.Error("unknown session must error")
	}
}
