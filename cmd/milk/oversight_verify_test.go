package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/oversight"
)

// telegramTestConfig is a complete, valid telegram oversight config.
func telegramTestConfig() config.Config {
	return config.Config{RemoteOversight: &config.RemoteOversightConfig{
		Backend: "telegram",
		Telegram: &config.TelegramConfig{
			Token:  "123456:TEST-token",
			ChatID: 4242,
		},
	}}
}

// useTelegramAPI points newNotifier's telegram API base at srv and restores
// the production default afterwards (the telegramAPIBase test seam).
func useTelegramAPI(t *testing.T, srv *httptest.Server) {
	t.Helper()
	prev := telegramAPIBase
	telegramAPIBase = srv.URL
	t.Cleanup(func() { telegramAPIBase = prev })
}

// TestNewNotifier_BuildsWithoutTouchingNetwork pins issue #388's core
// invariant: construction is network-free. The old newNotifier Pinged the
// backend synchronously here — a blocked api.telegram.org held startup for
// up to the 30s HTTP timeout before the prompt painted. A valid telegram
// config must now build a real notifier without a single request, and the
// background arm is what verifies it.
func TestNewNotifier_BuildsWithoutTouchingNetwork(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer srv.Close()
	useTelegramAPI(t, srv)

	n := newNotifier(telegramTestConfig())
	if _, isNoop := n.(oversight.Noop); isNoop {
		t.Fatal("a complete telegram config must build a real notifier, not Noop")
	}
	if got := hits.Load(); got != 0 {
		t.Errorf("newNotifier issued %d request(s) — construction must be network-free (issue #388)", got)
	}
}

// TestArmOversight_VerifiesThenStartsPolling walks the whole background
// flow: armOversight returns immediately (startup never waits), the backend
// gets pinged, the "oversight active" announcement goes out, the poll loop
// starts only after verification, and remote input reaches the onInput
// callback wired through the same call.
func TestArmOversight_VerifiesThenStartsPolling(t *testing.T) {
	var getMe, sendMessage, getUpdates atomic.Int32
	var announceBody atomic.Value
	firstPoll := atomic.Bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			getMe.Add(1)
			_, _ = w.Write([]byte(`{"ok":true,"result":{"id":1,"is_bot":true,"username":"milkbot"}}`))
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			sendMessage.Add(1)
			var payload struct {
				Text string `json:"text"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			announceBody.Store(payload.Text)
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			getUpdates.Add(1)
			time.Sleep(20 * time.Millisecond) // don't spin the loop
			if firstPoll.CompareAndSwap(false, true) {
				_, _ = w.Write([]byte(`{"ok":true,"result":[{"update_id":1,"message":{"chat":{"id":4242},"text":"hello-from-remote"}}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"ok":false}`))
		}
	}))
	defer srv.Close()
	useTelegramAPI(t, srv)

	n := newNotifier(telegramTestConfig())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	failed := make(chan error, 1)
	input := make(chan string, 8)
	start := time.Now()
	armOversight(ctx, n,
		func(text string) { input <- text },
		func(err error) {
			select {
			case failed <- err:
			default:
			}
		},
	)
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("armOversight blocked %v — it must never wait on the backend (issue #388)", elapsed)
	}

	// Verification verdict: success must arrive as polling, never as failure.
	deadline := time.Now().Add(5 * time.Second)
	for getUpdates.Load() == 0 {
		select {
		case err := <-failed:
			t.Fatalf("verification failed on a healthy backend: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("polling never started after successful verification")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if getMe.Load() == 0 {
		t.Error("backend was never pinged — verification did not run")
	}

	// The startup announcement went out through the same background path.
	for sendMessage.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if sendMessage.Load() == 0 {
		t.Fatal("startup announcement (\"oversight active\") was never sent")
	}
	if body, _ := announceBody.Load().(string); !strings.Contains(body, "oversight") {
		t.Errorf("announcement body = %q, want it to mention oversight", body)
	}

	// Remote input flows through the callback registered by armOversight.
	select {
	case text := <-input:
		if text != "hello-from-remote" {
			t.Errorf("onInput got %q, want %q", text, "hello-from-remote")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("remote input never reached the onInput callback")
	}
}

// TestArmOversight_FailureNeverStartsPolling pins the fail-closed verdict:
// a backend that rejects the ping reports through onFailed, and polling
// never starts — the same outcome the old synchronous startup ping produced.
func TestArmOversight_FailureNeverStartsPolling(t *testing.T) {
	var getUpdates atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"ok":false}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			getUpdates.Add(1)
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
	}))
	defer srv.Close()
	useTelegramAPI(t, srv)

	n := newNotifier(telegramTestConfig())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	failed := make(chan error, 1)
	armOversight(ctx, n, nil, func(err error) { failed <- err })

	select {
	case err := <-failed:
		if err == nil {
			t.Fatal("onFailed delivered a nil error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("verification verdict never arrived")
	}
	time.Sleep(100 * time.Millisecond) // give a wrongly-started poller time to show
	if getUpdates.Load() != 0 {
		t.Errorf("polling started (%d getUpdates) after failed verification — it must never start", getUpdates.Load())
	}
}

// TestArmOversight_HungBackendDoesNotBlockOrSpam: the arm call returns
// instantly while getMe hangs (the shape of a blackholed endpoint), and a
// cancellation mid-verification (notifier replaced, process shutting down)
// reports nothing — no failure message for a verdict nobody awaits anymore.
func TestArmOversight_HungBackendDoesNotBlockOrSpam(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
	}))
	defer srv.Close()
	defer close(release)
	useTelegramAPI(t, srv)

	n := newNotifier(telegramTestConfig())
	ctx, cancel := context.WithCancel(context.Background())

	failed := make(chan error, 1)
	start := time.Now()
	armOversight(ctx, n, nil, func(err error) { failed <- err })
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("armOversight blocked %v on a hung backend", elapsed)
	}

	cancel() // replacement / shutdown arrives before the verdict
	select {
	case err := <-failed:
		t.Errorf("cancellation reported as failure: %v — a superseded verdict must be silent", err)
	case <-time.After(300 * time.Millisecond):
	}
}

// pollFakeNotifier is a notifier without Ping (a test-fake shape): it must
// keep the old behaviour — SetOnInput and StartPolling applied synchronously,
// no verification step invented for it.
type pollFakeNotifier struct {
	fakeNotifier
	started atomic.Bool
	cb      func(string)
}

func (p *pollFakeNotifier) SetOnInput(cb func(string)) { p.cb = cb }
func (p *pollFakeNotifier) StartPolling(context.Context) {
	p.started.Store(true)
}

// TestArmOversight_FallbackStartsImmediately: backends that don't expose
// Ping (Noop-shaped fakes) are not delayed by a verification they don't
// have — polling starts synchronously, exactly like before.
func TestArmOversight_FallbackStartsImmediately(t *testing.T) {
	fake := &pollFakeNotifier{}
	armOversight(context.Background(), fake, func(string) {}, func(error) {
		t.Error("a backend without Ping must not fail verification")
	})
	if !fake.started.Load() {
		t.Error("polling must start immediately for a non-verifiable backend")
	}
	if fake.cb == nil {
		t.Error("onInput must be registered on the fallback path too")
	}
}

// TestOversightFailedMsg_SwapsOnlyTheNotifierItNames: the Update handler
// applies the fail-closed swap — but only when the failed notifier is still
// the active one, so a late verdict from a replaced notifier can't disable
// a newer one (that's the whole point of naming the instance in the msg).
func TestOversightFailedMsg_SwapsOnlyTheNotifierItNames(t *testing.T) {
	m := layoutTestModel(t, 100, 30)
	active := &fakeNotifier{}
	m.st.notifier = active

	out, _ := m.Update(oversightFailedMsg{n: active, err: errors.New("boom")})
	mm := out.(model)
	if _, isNoop := mm.st.notifier.(oversight.Noop); !isNoop {
		t.Errorf("failed active notifier must swap to Noop, got %T", mm.st.notifier)
	}
	if !strings.Contains(mm.transcript.String(), "oversight disabled") {
		t.Errorf("transcript must carry the fail-closed warning, got: %q", mm.transcript.String())
	}

	// Stale verdict: a replaced notifier's failure must change nothing.
	fresh := layoutTestModel(t, 100, 30)
	stillActive := &fakeNotifier{}
	fresh.st.notifier = stillActive
	out, _ = fresh.Update(oversightFailedMsg{n: &fakeNotifier{}, err: errors.New("stale")})
	mm = out.(model)
	if mm.st.notifier != oversight.Notifier(stillActive) {
		t.Error("a stale verdict must not disable the newer notifier")
	}
	if strings.Contains(mm.transcript.String(), "oversight disabled") {
		t.Error("a stale verdict must not warn — nothing was disabled")
	}
}
