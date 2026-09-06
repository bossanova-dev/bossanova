package views

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/telemetry"
)

// TUI telemetry vocabulary. feature, action and status are bounded enums with
// their own named types, so handing captureTUIAction an account label,
// credential, email, repo name, branch, title, cron expression, prompt or file
// path does not compile: an untyped string constant is the only thing that
// converts implicitly, and every one of those is a string *variable*.
//
// It is a guard rail, not a proof — `tuiFeature(acct.GetLabel())` is a legal
// explicit conversion — but it makes the leak deliberate rather than accidental,
// which is what a bounded property set actually needs. The values themselves are
// pinned by the per-view table in tui_action_telemetry_test.go.
type (
	tuiFeature string
	tuiAction  string
	tuiStatus  string
)

const (
	tuiFeatureAccounts tuiFeature = "accounts"
	tuiFeatureCron     tuiFeature = "cron"
	tuiFeatureSession  tuiFeature = "session"

	tuiActionAccountAdded     tuiAction = "account_added"
	tuiActionAccountRemoved   tuiAction = "account_removed"
	tuiActionAccountDisabled  tuiAction = "account_disabled"
	tuiActionAccountEnabled   tuiAction = "account_enabled"
	tuiActionAccountRefreshed tuiAction = "account_refreshed"
	tuiActionAccountReauthed  tuiAction = "account_reauthenticated"
	tuiActionAccountSwitched  tuiAction = "account_switched"

	tuiActionCronJobCreated tuiAction = "cron_job_created"
	tuiActionCronJobUpdated tuiAction = "cron_job_updated"
	tuiActionCronJobDeleted tuiAction = "cron_job_deleted"
	tuiActionCronJobRunNow  tuiAction = "cron_job_run_now"

	tuiActionSessionMerged      tuiAction = "session_merged"
	tuiActionSessionArchived    tuiAction = "session_archived"
	tuiActionSessionRemoved     tuiAction = "session_removed"
	tuiActionSessionResurrected tuiAction = "session_resurrected"

	tuiStatusSuccess tuiStatus = "success"
	tuiStatusError   tuiStatus = "error"
)

// tuiActionStatus maps an outcome error to the bounded status enum, so no call
// site branches on err by hand and no error string can leak into a property.
func tuiActionStatus(err error) tuiStatus {
	if err != nil {
		return tuiStatusError
	}
	return tuiStatusSuccess
}

// accountStatusAction maps the status an UpdateAccount status flip requested to
// its tui_action action value. Both surfaces that flip a status — the list's
// [space] and the edit screen's status row — are *toggles* sharing one result
// message, so the requested status is the only thing that distinguishes an
// enable from a disable. It is carried on the message (and on the request) even
// when the RPC failed, so a failed enable is still reported as an enable.
//
// Both producers build the flip from accountStatusActive/accountStatusDisabled,
// which is the whole domain today. Should a third status ever appear, this maps
// it to account_enabled — wrong, but bounded: it can never emit the status
// string itself.
func accountStatusAction(status string) tuiAction {
	if status == accountStatusDisabled {
		return tuiActionAccountDisabled
	}
	return tuiActionAccountEnabled
}

// captureTUIAction emits one tui_action for a completed TUI feature action.
// Call it from the handler that receives the action's *result* message, never
// from the keypress handler: a cancelled confirmation must emit nothing, and a
// confirmed-then-failed action must report status "error".
//
// This is the only place the tui_action property map is built, so a call site
// cannot invent a property key or attach an identifier.
func captureTUIAction(ctx context.Context, client telemetry.Client, feature tuiFeature, action tuiAction, status tuiStatus) {
	// Gate BEFORE building the map. Tracing is opt-in and defaults off, so the
	// overwhelmingly common path through every instrumented handler is "do
	// nothing", and that path should not allocate.
	if client == nil || !viewTelemetryEnabled() {
		return
	}
	captureViewTelemetry(ctx, client, telemetry.EventTUIAction, map[string]any{
		// Widened back to plain strings so the wire payload is a JSON string
		// regardless of how the enum types are marshalled.
		"feature": string(feature),
		"action":  string(action),
		"status":  string(status),
		"source":  "tui",
	})
}

func captureViewTelemetry(ctx context.Context, client telemetry.Client, event telemetry.Event, props map[string]any) {
	if client == nil {
		return
	}
	if !viewTelemetryEnabled() {
		return
	}
	if props == nil {
		props = map[string]any{}
	}
	client.Capture(ctx, event, viewDistinctID(), props)
}

// viewTelemetryGateTTL bounds how stale the cached gate below may be. It is a
// compromise, not a tuning knob: long enough that a burst of captures (the trash
// delete-all batch drains one session per message) costs one settings read
// instead of N, short enough that turning tracing OFF in general settings stops
// events while the operator is still looking at the screen.
const viewTelemetryGateTTL = 3 * time.Second

// viewTelemetryGate caches the opt-in gate. config.Load is os.ReadFile +
// json.Unmarshal, and every capture runs on Bubble Tea's update goroutine — the
// one that must not block — so re-reading the settings file once per action is
// exactly the blocking work the TUI rubric forbids.
//
// It expires rather than latching (a sync.Once) because the settings toggle is
// flipped from inside the running TUI (general_settings.go). Note this only
// makes turning tracing OFF take effect live. Turning it ON still needs a
// restart, and not because of this cache: telemetry.New picks noopClient at
// launch when tracing is off, and App.WithTelemetry is called once, so the
// client that a newly-true gate would capture into discards everything. That is
// pre-existing and out of scope here; it is written down so the next reader does
// not mistake this cache for the reason.
//
// Guarded by a mutex rather than left to the update goroutine's implicit
// serialisation: Bubble Tea runs tea.Cmd closures concurrently, and nothing
// stops a future capture from moving into one.
var viewTelemetryGate struct {
	mu        sync.Mutex
	checkedAt time.Time
	enabled   bool
}

func viewTelemetryEnabled() bool { return viewTelemetryEnabledAt(time.Now()) }

// viewTelemetryEnabledAt is viewTelemetryEnabled with the clock supplied, so a
// test can step across the TTL boundary exactly rather than sleeping — and so
// the TTL is genuinely load-bearing in the test rather than being short-circuited
// by a zeroed checkedAt.
func viewTelemetryEnabledAt(now time.Time) bool {
	viewTelemetryGate.mu.Lock()
	defer viewTelemetryGate.mu.Unlock()
	if viewTelemetryGate.checkedAt.IsZero() || now.Sub(viewTelemetryGate.checkedAt) >= viewTelemetryGateTTL {
		settings, err := config.Load()
		viewTelemetryGate.enabled = err == nil && settings.EventTracingEnabled
		viewTelemetryGate.checkedAt = now
	}
	return viewTelemetryGate.enabled
}

func viewDistinctID() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return telemetry.LocalDistinctID("")
	}
	return telemetry.LocalDistinctID(home)
}
