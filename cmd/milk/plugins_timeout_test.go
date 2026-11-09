package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestPluginsActionRun_HungFetchCannotWedgeTheSuite (review W2): the action
// layer must never run unbounded — a stalled peer kept the goroutine alive
// and the TUI's pluginsBusy latch held forever ("another operation is in
// flight", pluginsActionMsg never arriving). Before the fix the call sites
// used http.DefaultClient (no Timeout) with context.Background(); the test's
// 2s deadline is the red, and the bounded client is the green.
func TestPluginsActionRun_HungFetchCannotWedgeTheSuite(t *testing.T) {
	resetPluginState(t)

	stop := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-stop // answers nothing until the test ends (Close waits on handlers)
	}))
	defer hang.Close()
	defer close(stop)

	// Two knobs: the shared bounded client (the fix — 2 minutes in
	// production, 100ms here) and http.DefaultClient deliberately set
	// UNBOUNDED, so a call site that still reaches for the default client
	// is proven to hang even though the bounded one exists.
	prev := pluginsHTTP
	pluginsHTTP = &http.Client{Timeout: 100 * time.Millisecond}
	defer func() { pluginsHTTP = prev }()
	prevDef := http.DefaultClient
	http.DefaultClient = &http.Client{}
	defer func() { http.DefaultClient = prevDef }()

	done := make(chan error, 1)
	go func() {
		_, _, err := pluginsActionRun([]string{"add", hang.URL + "/plugin.zip"}, pluginsActionOpts{})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("hung fetch must fail, not succeed")
		}
		if !strings.Contains(err.Error(), "Timeout") && !strings.Contains(err.Error(), "timeout") &&
			!strings.Contains(err.Error(), "deadline") && !strings.Contains(err.Error(), "Client.Timeout") {
			t.Fatalf("expected a timeout-class error, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("add against a hung server did not return within 2s — the action layer runs unbounded")
	}
}
