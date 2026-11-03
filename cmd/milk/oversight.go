package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/oversight"
	"github.com/scoutme/milk/internal/oversight/telegram"
)

// telegramAPIBase overrides the Telegram API base URL — a test seam over
// telegram.Config.APIBase ("override for testing") so construction and
// background verification can run against a local server instead of the
// network. Empty (production) keeps https://api.telegram.org.
var telegramAPIBase = ""

// oversightVerifyTimeout bounds the background verification ping (issue
// #388): a blocked Telegram endpoint delivers its verdict — and with it the
// fail-closed "oversight disabled" outcome — within this window, while
// startup itself never waits on it.
const oversightVerifyTimeout = 10 * time.Second

// newNotifier constructs the appropriate Notifier from config.
// Returns oversight.Noop when remote oversight is disabled or misconfigured.
//
// Construction is deliberately network-free (issue #388): it used to Ping the
// backend synchronously — with an HTTP client whose timeout is 30s, a blocked
// api.telegram.org held up every startup for up to 30s before the prompt even
// painted. Verification now happens in armOversight, in the background, and
// reaches the same verdict the sync ping produced: a backend that never
// answers ends up as oversight.Noop with a warning.
func newNotifier(cfg config.Config) oversight.Notifier {
	ro := cfg.RemoteOversight
	if ro == nil || ro.Backend == "" {
		return oversight.Noop{}
	}
	switch ro.Backend {
	case "telegram":
		if ro.Telegram == nil || ro.Telegram.Token == "" || ro.Telegram.ChatID == 0 {
			fmt.Fprintln(os.Stderr, milkTag()+" warning: remote_oversight.telegram missing token or chat_id — oversight disabled")
			return oversight.Noop{}
		}
		return telegram.New(telegram.Config{
			APIBase:       telegramAPIBase,
			Token:         ro.Telegram.Token,
			ChatID:        ro.Telegram.ChatID,
			PermTimeout:   ro.PermTimeoutDuration(),
			TimeoutAction: ro.TimeoutActionValue(),
			BusyIndicator: ro.BusyIndicatorEnabled(),
		})
	default:
		fmt.Fprintf(os.Stderr, "%s warning: unknown remote_oversight backend %q — oversight disabled\n", milkTag(), ro.Backend)
		return oversight.Noop{}
	}
}

// armOversight wires and verifies a freshly built notifier without ever
// blocking the caller (issue #388): input routing registers immediately, the
// backend is Pinged in the background (bounded by oversightVerifyTimeout),
// and the poll loop starts only for a backend that answered — the same
// fail-closed verdict the old synchronous startup ping produced (onFailed
// hands the owner the error so it can swap in oversight.Noop), just without
// holding up startup. On success the "oversight active" announcement fires
// from the background goroutine too. Backends that don't expose Ping (Noop,
// test fakes) start polling immediately, exactly as before; a verification
// that ends because ctx was cancelled (notifier replaced, process shutting
// down) reports nothing.
func armOversight(ctx context.Context, n oversight.Notifier, onInput func(string), onFailed func(error)) {
	verified, ok := n.(interface {
		SetOnInput(func(string))
		StartPolling(context.Context)
		Ping(context.Context) error
	})
	if !ok {
		if plain, ok := n.(interface {
			SetOnInput(func(string))
			StartPolling(context.Context)
		}); ok {
			if onInput != nil {
				plain.SetOnInput(onInput)
			}
			plain.StartPolling(ctx)
		}
		return
	}
	if onInput != nil {
		verified.SetOnInput(onInput)
	}
	go func() {
		pingCtx, cancel := context.WithTimeout(ctx, oversightVerifyTimeout)
		err := verified.Ping(pingCtx)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return // cancelled before the verdict: replaced or shutting down
			}
			if onFailed != nil {
				onFailed(err)
			}
			return
		}
		notifyAnnouncement(n, "milk", "startup", "oversight active")
		verified.StartPolling(ctx)
	}()
}

// notifyAnnouncement routes one-off "turn-start-style" notices (startup,
// /setup confirmations) around the busy indicator (issue #284): backends
// that pair NotifyTurnStart with a busy status — Telegram's "typing…" —
// expose Announce, and an announcement that never gets a matching
// NotifyTurnDone would otherwise leave it running forever. Backends without
// Announce (Noop, test fakes) keep the old path unchanged.
func notifyAnnouncement(n oversight.Notifier, agent, target, prompt string) {
	if a, ok := n.(interface{ Announce(string, string, string) }); ok {
		a.Announce(agent, target, prompt)
		return
	}
	n.NotifyTurnStart(context.Background(), agent, target, prompt)
}

// newNotifierFor is newNotifier behind a seam: the ACP serve process builds
// its notifier through it (acp_oversight.go's resetNotifier), so tests can
// substitute a fake and never spawn a poller.
var newNotifierFor = newNotifier
