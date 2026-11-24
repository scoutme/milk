package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Live probe for the Telegram delivery path (issue #439): five sends through
// the real Bot API, one per delivery suspect — the harness issue #439 points
// at, kept so the fixes can be verified end to end instead of unit-level.
//
// Opt-in like the repo's other live tests (MILK_LIVE_TEST, MCP_LIVE_TEST):
// it messages a real chat, so credentials alone never arm it.
//
//	TG_LIVE_TEST=1 TG_TOKEN=<bot token> TG_CHAT=<chat id> \
//	  [TG_ANALYSIS_FILE=<analysis reply to replay>] \
//	  go test ./internal/oversight/telegram -run TestLiveProbe -v
//
// Probes (P1–P4 return raw sendMessage bodies; P3–P5 also need eyes on the
// chat — the Bot API has no read-back of sent messages):
//
//	P1 control  plain escMD one-liner — must be ok:true (Fatalf otherwise:
//	             without a working control nothing else is interpretable)
//	P2 control  turnStartMessage, single-line prompt — must be ok:true
//	P3 control  turnStartMessage, multi-line prompt — must be ok:true since
//	             #439's fix: turnStartMessage flattens through codeText, so
//	             no newline can reach the code span (pre-fix this probe was
//	             the latent-bug reproducer; a rejection here now means the
//	             flatten regressed)
//	P4 control  literal backslashes through escMD — must be ok:true AND
//	             render verbatim (#439's escMD now escapes '\'): "C:\Users"
//	             must not appear as "C:Users" in the chat
//	P5          NotifyResponse over a long reply (chunked, escaped, FIFO,
//	             paced) — TG_ANALYSIS_FILE when set, else a synthetic payload
//	             sized to force chunking (TestSyntheticProbePayloadChunks
//	             pins that). sendOnce now counts every outcome (Stats), so a
//	             log-side flood/failure would be visible there — but the
//	             chat is still the oracle for chunk count/order/completeness.
//
// TestSyntheticProbePayloadChunks runs offline (no credentials) and guards
// the harness's own payload machinery.

// rawSend posts one sendMessage and returns the raw API response body — the
// diagnostic mirror of sendOnce, which swallows everything but the id.
func rawSend(t *testing.T, token string, chat int64, text string) string {
	t.Helper()
	payload := map[string]any{
		"chat_id":    chat,
		"text":       text,
		"parse_mode": "MarkdownV2",
	}
	body, _ := json.Marshal(payload)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.telegram.org/bot"+token+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return "TRANSPORT ERROR: " + err.Error()
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	b, _ := json.Marshal(out)
	return fmt.Sprintf("http=%d %s", resp.StatusCode, string(b))
}

// probeOK reports whether the raw sendMessage body accepted the message —
// "ok":true. Transport errors and parse rejections (HTTP 400 "can't parse
// entities") both lack it.
func probeOK(resp string) bool {
	return strings.Contains(resp, `"ok":true`)
}

// syntheticAnalysisPayload is P5's payload when TG_ANALYSIS_FILE is unset:
// analysis-shaped markdown carrying the probe shapes — backslashes, newlines,
// reserved characters — long enough that NotifyResponse must chunk it (see
// TestSyntheticProbePayloadChunks).
func syntheticAnalysisPayload() string {
	block := "## probe section\n\n" +
		"- path: C:\\Users\\build\\milk\\cmd\\milk\\theme.go\n" +
		"- regex \\d+ and escaped \\ backslashes\n" +
		"- `inline code`, **bold**, [link](https://example.com/a_b)\n\n"
	return strings.Repeat(block, 60)
}

// TestSyntheticProbePayloadChunks keeps P5's default payload honest without
// network access: it must outgrow one message budget after escaping (so P5
// really exercises NotifyResponse's chunked FIFO) and must carry the shapes
// the probes exist for.
func TestSyntheticProbePayloadChunks(t *testing.T) {
	payload := syntheticAnalysisPayload()
	if u := utf16Len(escMD(payload)); u <= maxMessageUnits {
		t.Fatalf("synthetic payload is %d escaped UTF-16 units — must exceed maxMessageUnits (%d) to force chunking",
			u, maxMessageUnits)
	}
	if !strings.Contains(payload, "\\") || !strings.Contains(payload, "\n") {
		t.Fatal("synthetic payload must carry backslashes and newlines — the probe's subjects")
	}
}

// TestLiveProbe sends the five probes. Opt-in (messages a real chat):
//
//	TG_LIVE_TEST=1 TG_TOKEN=… TG_CHAT=… [TG_ANALYSIS_FILE=…] \
//	  go test ./internal/oversight/telegram -run TestLiveProbe -v
func TestLiveProbe(t *testing.T) {
	if os.Getenv("TG_LIVE_TEST") == "" {
		t.Skip("set TG_LIVE_TEST=1 (with TG_TOKEN and TG_CHAT) to run the live Telegram delivery probes — issue #439")
	}
	token := os.Getenv("TG_TOKEN")
	if token == "" {
		t.Skip("TG_TOKEN not set")
	}
	// Group chat ids are negative — only 0 means "unset/garbage".
	chat, err := strconv.ParseInt(os.Getenv("TG_CHAT"), 10, 64)
	if err != nil || chat == 0 {
		t.Skipf("TG_CHAT missing or not a chat id: %q", os.Getenv("TG_CHAT"))
	}

	// 1 — control: a plain escaped one-liner (what a short response sends).
	// Fatal: if the control can't be accepted, every later failure is noise.
	p1 := rawSend(t, token, chat,
		escMD("[oversight probe 1/5] basic send"))
	t.Logf("P1 basic: %s", p1)
	if !probeOK(p1) {
		t.Fatalf("control probe failed — token/chat/network unusable, nothing below is interpretable: %s", p1)
	}

	// 2 — control: turn-start shape, single-line prompt preview.
	p2 := rawSend(t, token, chat,
		turnStartMessage("mimo-pro-local", "escalation",
			"It seems to me that current theme management implies new code for every theme."))
	t.Logf("P2 turn-start single-line: %s", p2)
	if !probeOK(p2) {
		t.Errorf("control turn-start (single-line) must be accepted: %s", p2)
	}

	// 3 — control: turn-start shape, multi-line prompt. Since #439's fix
	// turnStartMessage flattens through codeText, so this must be accepted;
	// a rejection means the flatten regressed.
	p3 := rawSend(t, token, chat,
		turnStartMessage("mimo-pro-local", "escalation",
			"It seems to me that current theme management implies new code for every theme (currently \"off\").\nThemes should be configuration over a common basis, and new themes should be simple add-ons.\nA theme builder via wizard would be neat."))
	t.Logf("P3 turn-start multi-line: %s", p3)
	if !probeOK(p3) {
		t.Errorf("multi-line turn-start must be accepted after the codeText fix: %s", p3)
	}

	// 4 — control: literal backslashes through escMD (#439: escMD now
	// escapes '\' so MarkdownV2 cannot re-parse C:\ as an escape). The
	// label is escaped too so only the payload's backslashes are under
	// test. Accepted + verbatim rendering both required.
	p4 := rawSend(t, token, chat,
		escMD("[oversight probe 4/5] escMD with backslashes")+"\n"+
			escMD(`JSON: {"a": "x"} `+` and Windows path C:\Users\test\file.txt and regex \d+ and a trailing \ `))
	t.Logf("P4 backslashes: %s", p4)
	if !probeOK(p4) {
		t.Errorf("backslash probe must be accepted: %s", p4)
	} else {
		t.Log(`P4 accepted — verify in the chat: backslashes must render verbatim ("C:\Users", not "C:Users") — #439 escMD fix`)
	}

	// 5 — the real payload shape: NotifyResponse over a long reply (chunked,
	// escaped, FIFO) — the delivery path that went missing. No assertion
	// possible: sendOnce swallows non-429 failures (bug #2), so the API
	// never reports chunks; count them in the chat instead.
	payload, source := syntheticAnalysisPayload(), "synthetic"
	if f := os.Getenv("TG_ANALYSIS_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("TG_ANALYSIS_FILE: %v", err)
		}
		payload, source = string(b), f
	}
	// Mirror NotifyResponse's budget exactly (header = 💬 *escMD(agent)*,
	// worst-case " (n/m)" suffix room −10) so the log says how many messages
	// to expect in the chat.
	budget := maxMessageUnits - utf16Len("💬 *"+escMD("mimo-pro-local")+"*") - 10
	if budget < 256 {
		budget = 256
	}
	t.Logf("P5 payload: %s, %d bytes → %d chunk(s)", source, len(payload),
		len(chunkMessage(payload, budget)))
	n := New(Config{Token: token, ChatID: chat})
	n.NotifyResponse(context.Background(), "mimo-pro-local", payload)
	time.Sleep(20 * time.Second) // let the FIFO queue drain (paced; no completion signal exists)
	t.Logf("P5 chunks enqueued — verify in the chat: count/order/completeness; Stats: %v", n.Stats())
}
