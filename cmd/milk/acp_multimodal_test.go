package main

// Multimodal session/prompt delivery (ADR-0074): a pasted image must reach a
// vision-capable model as a multipart image_url part (the exact payload the
// TUI sends for /attach and clipboard paste), a non-vision model must get the
// same graceful notice the TUI gets instead of a hard API error, and the
// accept-echo must carry the client's own image block while session history
// stays compact (placeholders, never payloads).

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/testutil"
	"github.com/scoutme/milk/internal/transport/acp"
)

// stubRecordingBackend records every request body the stub inference server
// receives and answers with one SSE completion that reports image_tokens in
// its usage (so the silent-image-drop retry never engages).
func stubRecordingBackend(bodies *[]string, mu *sync.Mutex) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		*bodies = append(*bodies, string(raw))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"a red square"},"finish_reason":"stop"}],"usage":{"prompt_tokens":30,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":0,"image_tokens":192}}}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}
}

func acpPromptWithImage(t *testing.T, server *acpServer, conn *fakeACPConn, id acp.SessionID) (acp.PromptResponse, *acp.MessageUpsert) {
	t.Helper()
	result := acpRequest(t, server, "session/prompt", acp.PromptRequest{
		SessionID: id,
		Prompt: []acp.ContentBlock{
			acp.TextBlock("what is in this image?"),
			acp.ImageBlock(acpTinyPNG, "image/png"),
		},
	})
	resp, ok := result.(acp.PromptResponse)
	if !ok || resp.MessageID == "" {
		t.Fatalf("session/prompt result = %+v (ok=%v)", result, ok)
	}
	echo, _, found := upsertByID(sentUpdates(t, conn), resp.MessageID)
	if !found {
		t.Fatalf("no user_message upsert echoed for %s", resp.MessageID)
	}
	return resp, &echo
}

func TestACPServer_PromptImage_ReachesVisionModel(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	server, conn := acpTestServerWithAgent(t, stubRecordingBackend(&bodies, &mu), `,"vision":true`)
	cwd := t.TempDir()
	newResp := acpRequest(t, server, "session/new", acp.NewSessionRequest{CWD: cwd}).(acp.NewSessionResponse)

	_, echo := acpPromptWithImage(t, server, conn, newResp.SessionID)

	// The model received a multipart vision payload: a text part and an
	// image_url part carrying the same bytes the client pasted.
	mu.Lock()
	body := strings.Join(bodies, "\n---\n")
	mu.Unlock()
	if !strings.Contains(body, `"image_url"`) {
		t.Errorf("model request has no image_url part: %s", body)
	}
	if !strings.Contains(body, "data:image/png;base64,"+acpTinyPNG) {
		t.Errorf("model request lost the image payload: %s", body)
	}
	if !strings.Contains(body, "what is in this image?") {
		t.Errorf("model request lost the prompt text: %s", body)
	}

	// The accept-echo carries the client's own blocks — the pasted image
	// renders back in the client's transcript.
	if len(echo.Content) != 2 || echo.Content[0].Type != "text" || echo.Content[1].Type != "image" {
		t.Fatalf("echo content = %+v, want [text image]", echo.Content)
	}
	if echo.Content[1].Data != acpTinyPNG || echo.Content[1].MimeType != "image/png" {
		t.Errorf("echo lost the image block payload: %+v", echo.Content[1])
	}

	// Session history is compact: the placeholder, never the payload.
	as := server.session(newResp.SessionID)
	var userContent string
	for _, turn := range as.sess.History {
		if strings.Contains(turn.Content, "attached image") {
			userContent = turn.Content
		}
	}
	if !strings.Contains(userContent, "[attached image: image-1.png (image/png)]") {
		t.Errorf("session history missing the placeholder: %q", userContent)
	}
	if strings.Contains(userContent, acpTinyPNG) {
		t.Errorf("session history carries the payload: %q", userContent)
	}
}

func TestACPServer_PromptImage_NonVisionAgent(t *testing.T) {
	// No "vision" flag: the image must not be sent as image_url (that is a
	// hard API error on many backends) — the model gets the same graceful
	// system-reminder the TUI gets, and the client still sees its image echoed.
	var mu sync.Mutex
	var bodies []string
	server, conn := acpTestServerWithAgent(t, stubRecordingBackend(&bodies, &mu), "")
	cwd := t.TempDir()
	newResp := acpRequest(t, server, "session/new", acp.NewSessionRequest{CWD: cwd}).(acp.NewSessionResponse)

	_, echo := acpPromptWithImage(t, server, conn, newResp.SessionID)

	mu.Lock()
	body := strings.Join(bodies, "\n---\n")
	mu.Unlock()
	if strings.Contains(body, `"image_url"`) {
		t.Errorf("non-vision model must not receive image_url parts: %s", body)
	}
	if !strings.Contains(body, "not configured for vision input") {
		t.Errorf("non-vision model missing the drop notice: %s", body)
	}
	if echo.Content[len(echo.Content)-1].Type != "image" {
		t.Errorf("echo must still carry the pasted image: %+v", echo.Content)
	}
}

func TestACPServer_PromptImageOnly(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	server, conn := acpTestServerWithAgent(t, stubRecordingBackend(&bodies, &mu), `,"vision":true`)
	cwd := t.TempDir()
	newResp := acpRequest(t, server, "session/new", acp.NewSessionRequest{CWD: cwd}).(acp.NewSessionResponse)

	result := acpRequest(t, server, "session/prompt", acp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt:    []acp.ContentBlock{acp.ImageBlock(acpTinyPNG, "image/png")},
	})
	resp, ok := result.(acp.PromptResponse)
	if !ok || resp.MessageID == "" {
		t.Fatalf("session/prompt result = %+v (ok=%v)", result, ok)
	}
	if _, _, found := upsertByID(sentUpdates(t, conn), resp.MessageID); !found {
		t.Error("image-only prompt was not echoed as a user message")
	}
	mu.Lock()
	body := strings.Join(bodies, "\n---\n")
	mu.Unlock()
	if !strings.Contains(body, `"image_url"`) {
		t.Errorf("image-only prompt did not reach the model as a vision part: %s", body)
	}
}

func TestACPServer_PromptAudio_NotedNotForwarded(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	server, conn := acpTestServerWithAgent(t, stubRecordingBackend(&bodies, &mu), `,"vision":true`)
	cwd := t.TempDir()
	newResp := acpRequest(t, server, "session/new", acp.NewSessionRequest{CWD: cwd}).(acp.NewSessionResponse)

	result := acpRequest(t, server, "session/prompt", acp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt: []acp.ContentBlock{
			acp.TextBlock("transcribe"),
			acp.AudioBlock("YXVk", "audio/ogg"),
		},
	})
	resp, ok := result.(acp.PromptResponse)
	if !ok {
		t.Fatalf("session/prompt result = %+v (ok=%v)", result, ok)
	}

	// The model sees the placeholder, never the payload; the client gets a
	// notice that audio input is not forwarded.
	mu.Lock()
	body := strings.Join(bodies, "\n---\n")
	mu.Unlock()
	if strings.Contains(body, "YXVk") {
		t.Errorf("audio payload leaked to the model: %s", body)
	}
	sawNote := false
	for _, u := range sentUpdates(t, conn) {
		if c, ok := u.(acp.ContentChunk); ok && strings.Contains(c.Content.Text, "audio attachments are not forwarded") {
			sawNote = true
		}
	}
	if !sawNote {
		t.Errorf("no audio-not-forwarded notice sent to the client (id %s)", resp.MessageID)
	}
}

// TestACPServer_PromptImagePDF_NotForwarded pins the end-to-end guard: a PDF
// sent in an image block never reaches the model as image_url (a hard 400 on
// most backends) — the model gets the placeholder text instead.
func TestACPServer_PromptImagePDF_NotForwarded(t *testing.T) {
	var bodies []string
	var mu sync.Mutex
	server, conn := acpTestServerWithAgent(t, stubRecordingBackend(&bodies, &mu), `,"vision":true`)
	cwd := t.TempDir()
	newResp := acpRequest(t, server, "session/new", acp.NewSessionRequest{CWD: cwd}).(acp.NewSessionResponse)

	pdf := base64.StdEncoding.EncodeToString([]byte("%PDF-1.7 binary\x00\x01"))
	prompt := []acp.ContentBlock{
		acp.TextBlock("read this"),
		acp.ImageBlock(pdf, "application/pdf"),
	}
	acpRequest(t, server, "session/prompt", acp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt:    prompt,
	})

	mu.Lock()
	defer mu.Unlock()
	for _, b := range bodies {
		if strings.Contains(b, "image_url") {
			t.Fatalf("a non-raster payload reached the model as image_url: %s", b[:min(400, len(b))])
		}
	}
	var noted bool
	for _, u := range sentUpdates(t, conn) {
		if c, ok := u.(acp.ContentChunk); ok && strings.Contains(c.Content.Text, "not forwarded to the model") {
			noted = true
		}
	}
	if !noted {
		t.Error("expected a client-visible note about the undeliverable attachment")
	}
}

// TestACPSession_ConfigHotReload pins the ACP config hot-reload: a change to
// config.json while a session is open (e.g. enabling "vision": true) must
// reach the next prompt — sessions used to freeze their config at creation,
// so images kept being dropped until the client was restarted.
func TestACPSession_ConfigHotReload(t *testing.T) {
	var bodies []string
	var mu sync.Mutex
	srv := httptest.NewServer(stubRecordingBackend(&bodies, &mu))
	t.Cleanup(srv.Close)

	home := testutil.SandboxHome(t)
	milkDir := filepath.Join(home, ".milk")
	if err := os.MkdirAll(milkDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	cfgPath := filepath.Join(milkDir, "config.json")
	writeCfg := func(vision bool) {
		cfgJSON := fmt.Sprintf(`{"agent":"test-local","agents":[{"name":"test-local","url":%q,"model":"test-model","provider":"local","vision":%v}]}`, srv.URL, vision)
		if err := os.WriteFile(cfgPath, []byte(cfgJSON), 0o600); err != nil {
			t.Fatalf("WriteFile config: %v", err)
		}
	}
	writeCfg(false)
	t.Setenv("HOME", home)

	cfg, err := config.LoadMerged()
	if err != nil {
		t.Fatalf("LoadMerged: %v", err)
	}
	conn := &fakeACPConn{}
	server := newACPServer(cfg, conn)

	cwd := t.TempDir()
	newResp := acpRequest(t, server, "session/new", acp.NewSessionRequest{CWD: cwd}).(acp.NewSessionResponse)

	// Flip vision on while the session is open. mtime granularity is coarse
	// on some filesystems — nudge it forward explicitly.
	writeCfg(true)
	future := time.Now().Add(2 * time.Second)
	os.Chtimes(cfgPath, future, future) //nolint:errcheck

	acpRequest(t, server, "session/prompt", acp.PromptRequest{
		SessionID: newResp.SessionID,
		Prompt: []acp.ContentBlock{
			acp.TextBlock("what is in this image"),
			acp.ImageBlock(acpTinyPNG, "image/png"),
		},
	})

	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(bodies, "\n")
	if !strings.Contains(joined, "image_url") {
		t.Errorf("config change did not reach the open session — image still dropped; bodies=%d", len(bodies))
	}
	var noticed bool
	for _, u := range sentUpdates(t, conn) {
		if c, ok := u.(acp.ContentChunk); ok && strings.Contains(c.Content.Text, "config reloaded") {
			noticed = true
		}
	}
	if !noticed {
		t.Error("expected a 'config reloaded' notice chunk")
	}
}
