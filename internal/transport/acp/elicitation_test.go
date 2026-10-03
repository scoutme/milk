package acp

import (
	"context"
	"testing"
)

// elicitation wiring (elicitation.go): elicitation/create + elicitation/complete
// behind Host.Elicit — generic structured user-input form/select prompts.

// exampleElicitationRequest builds a minimal, generic single-select prompt
// for exercising ElicitSession/ACPHost.Elicit's mechanics — not tied to any
// specific milk feature (Elicit has no production caller yet; see its doc
// comment in acp.go).
func exampleElicitationRequest(message string) ElicitationRequest {
	return ElicitationRequest{
		Message: message,
		Schema: ElicitationSchema{
			Type:  "object",
			Title: "Confirm",
			Properties: map[string]ElicitationProperty{
				"choice": SelectProperty("Choice",
					EnumOption{Const: "yes", Title: "Yes"},
					EnumOption{Const: "no", Title: "No"},
				),
			},
			Required: []string{"choice"},
		},
	}
}

func TestElicitSessionRoundTrip(t *testing.T) {
	conn := &fakeConn{respond: func(method string, params any) (any, error) {
		if method != MethodElicitationCreate {
			t.Fatalf("unexpected request %q", method)
		}
		return CreateElicitationResponse{
			Action:  ElicitationAccept,
			Content: map[string]any{"choice": "yes"},
		}, nil
	}}
	host := NewACPHost(conn, "sess-9")
	res, err := host.Elicit(context.Background(), exampleElicitationRequest("proceed?"))
	if err != nil {
		t.Fatalf("Elicit: %v", err)
	}

	// elicitation/create: form mode, session scope, schema, _meta id.
	reqs := conn.requested()
	if len(reqs) != 1 || reqs[0].Method != MethodElicitationCreate {
		t.Fatalf("requests = %+v", reqs)
	}
	body := jsonBody(t, reqs[0].Params)
	if body["mode"] != "form" {
		t.Fatalf("mode = %v", body["mode"])
	}
	if body["sessionId"] != "sess-9" {
		t.Fatalf("sessionId = %v", body["sessionId"])
	}
	if _, ok := body["requestedSchema"]; !ok {
		t.Fatalf("requestedSchema missing: %v", body)
	}
	meta, _ := body["_meta"].(map[string]any)
	if meta[ElicitationIDText] != "elicit-1" {
		t.Fatalf("_meta = %v", meta)
	}

	// elicitation/complete: same elicitation ID, sent after the result.
	nots := conn.sent()
	if len(nots) != 1 || nots[0].Method != MethodElicitationComplete {
		t.Fatalf("notifications = %+v", nots)
	}
	done := jsonBody(t, nots[0].Params)
	if done["elicitationId"] != "elicit-1" {
		t.Fatalf("complete params = %v", done)
	}

	if res.Action != ElicitationAccept || res.Content["choice"] != "yes" {
		t.Fatalf("result = %+v", res)
	}
}

func TestElicitSessionDecline(t *testing.T) {
	conn := &fakeConn{respond: func(string, any) (any, error) {
		return CreateElicitationResponse{Action: ElicitationDecline}, nil
	}}
	host := NewACPHost(conn, "sess-9")
	res, err := host.Elicit(context.Background(), exampleElicitationRequest("keep going?"))
	if err != nil {
		t.Fatalf("Elicit: %v", err)
	}
	if res.Action != ElicitationDecline {
		t.Fatalf("result = %+v", res)
	}
}

func TestMultiSelectProperty(t *testing.T) {
	prop := MultiSelectProperty("Tags", StringMultiSelect([]string{"docs", "release"}))
	body := jsonBody(t, prop)
	if body["type"] != "array" {
		t.Fatalf("type = %v", body["type"])
	}
	items, ok := body["items"].(map[string]any)
	if !ok || items["type"] != "string" {
		t.Fatalf("items = %v", body["items"])
	}
	enum, ok := items["enum"].([]any)
	if !ok || len(enum) != 2 {
		t.Fatalf("items.enum = %v", items)
	}
	titled := MultiSelectProperty("Pick", TitledMultiSelect([]EnumOption{{Const: "a", Title: "A"}}))
	tb := jsonBody(t, titled)
	ti := tb["items"].(map[string]any)
	if _, ok := ti["anyOf"]; !ok {
		t.Fatalf("titled items = %v", ti)
	}
}
