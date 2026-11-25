package obs

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/scoutme/milk/internal/config"
)

// Emission gate (#458): /otel off|on and the runtime size-cap enforcement
// flip these flags instead of tearing down the SDK providers, so a toggle
// takes effect immediately for every writer (signal files, milk.log) without
// a restart. The zero value is "enabled" — writers only exist once Init has
// installed them, and pre-Init emission already no-ops through the unset
// global providers and the nil milk logger.
var (
	off      atomic.Bool // /otel off — manual, per-process disable
	sizeTrip atomic.Bool // max_mb exceeded at runtime — auto-disable until trim
	initd    atomic.Bool // Init built the exporters and the milk.log writer
)

// SetEnabled opens or closes the emission gate for this process. Closing it
// discards every subsequent signal record and milk.log line; opening it
// resumes writing through the already-initialized exporters (callers must
// check Initialized first — if OTel never started this session, opening the
// gate has no writers to open).
func SetEnabled(on bool) {
	off.Store(!on)
	if on {
		// Re-enabling is an explicit override: clear a stale size-cap trip
		// (the next MaybeEnforceSizeCap re-trips it if the files are still
		// over the cap, and /otel trim clears it for good).
		sizeTrip.Store(false)
	}
}

// Enabled reports whether the emission gate is open.
func Enabled() bool { return !off.Load() && !sizeTrip.Load() }

// Initialized reports whether Init actually built the exporters/writers this
// session (false when otel.enabled was off or the startup size cap skipped
// OTel — /otel on must say so instead of claiming a re-enable).
func Initialized() bool { return initd.Load() }

// gatedWriter discards p while the emission gate is closed and passes it
// through otherwise. One choke point per destination: the three signal files
// and milk.log are wrapped in Init/initMilkLogger, which covers every emit
// path (spans, metrics, batched logs, plain slog lines) without gating each
// helper individually.
type gatedWriter struct{ w io.Writer }

func (g *gatedWriter) Write(p []byte) (int, error) {
	if !Enabled() {
		return len(p), nil
	}
	return g.w.Write(p)
}

// instanceID identifies this process on every exported record's Resource
// (service.instance.id). Signal files are shared by every milk session on a
// config root; pid+nonce is what lets readers tell the writers apart (#455).
var instanceID = newInstanceID()

func newInstanceID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return strconv.Itoa(os.Getpid())
	}
	return fmt.Sprintf("%d-%s", os.Getpid(), hex.EncodeToString(b))
}

// runtimeState remembers what Init was configured with so the turn-boundary
// size re-check (MaybeEnforceSizeCap) can enforce max_mb mid-session instead
// of only at startup (#457). Set once in Init before any turn runs.
var runtimeState struct {
	dir string
	cfg config.OtelConfig
}

// enforceAt throttles MaybeEnforceSizeCap to at most one stat sweep per
// minute (turn boundaries are frequent; six stat calls are cheap but not
// worth doing every turn).
var enforceAt atomic.Int64

// enforceInterval is the minimum time between size-cap sweeps.
const enforceInterval = time.Minute

// MaybeEnforceSizeCap re-checks the shared observability files against
// otel.max_mb and closes the emission gate when one is over the cap — the
// startup-only CheckFileSizes assumption breaks as soon as more than one
// session feeds the same files (#457). Stat-only (no line scans), throttled
// to once per enforceInterval, and a no-op until Init has recorded a runtime
// state or while the gate is already closed. Emits one last warning record
// before closing so milk.log says why writing stopped.
func MaybeEnforceSizeCap() {
	if runtimeState.dir == "" || off.Load() || sizeTrip.Load() {
		return
	}
	cfg := runtimeState.cfg
	if cfg.MaxMB <= 0 {
		return
	}
	now := time.Now().UnixNano()
	last := enforceAt.Load()
	if now-last < int64(enforceInterval) || !enforceAt.CompareAndSwap(last, now) {
		return
	}
	max := int64(cfg.MaxMB) * 1024 * 1024
	for _, name := range []string{"logs.jsonl", "traces.jsonl", "metrics.jsonl", "milk.log"} {
		if tripSizeCap(filepath.Join(runtimeState.dir, name), max, name) {
			return
		}
	}
	for _, p := range debugLogPathsFn() {
		if tripSizeCap(p, max, filepath.Base(p)) {
			return
		}
	}
}

// tripSizeCap reports whether path is at/over max bytes; if so it emits one
// EventWarn while the gate is still open and then trips the auto-disable.
func tripSizeCap(path string, max int64, name string) bool {
	info, err := os.Stat(path)
	if err != nil || info.Size() < max {
		return false
	}
	EventWarn("otel.disabled_file_cap",
		"file", name, "bytes", info.Size(), "max_mb", max/(1024*1024))
	sizeTrip.Store(true)
	return true
}
