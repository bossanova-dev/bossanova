package ui

import (
	"context"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/telemetry"
)

// TUI telemetry vocabulary. feature, action and status are bounded enums with
// their own named types, so handing CaptureTUIAction an account label,
// credential, email, repo name, branch, title, cron expression, prompt or file
// path does not compile: an untyped string constant is the only thing that
// converts implicitly, and every one of those is a string *variable*.
//
// It is a guard rail, not a proof — `TUIFeature(acct.GetLabel())` is a legal
// explicit conversion — but it makes the leak deliberate rather than accidental,
// which is what a bounded property set actually needs. The values themselves are
// pinned by the per-view table in tui_action_telemetry_test.go.
type (
	TUIFeature string
	TUIAction  string
	TUIStatus  string
	// TUIEntryPoint bounds the entry_point property of the terminal-side
	// conversion-funnel events for the same reason and in the same way: it is
	// the only free-form-looking value they carry, and a named type makes
	// handing it an email, an account label or a URL a compile error.
	TUIEntryPoint string
)

const (
	TUIFeatureAgentAccounts TUIFeature = "accounts"
	TUIFeatureCron          TUIFeature = "cron"
	TUIFeatureSession       TUIFeature = "session"

	TUIActionAgentAccountAdded     TUIAction = "account_added"
	TUIActionAgentAccountRemoved   TUIAction = "account_removed"
	TUIActionAgentAccountDisabled  TUIAction = "account_disabled"
	TUIActionAgentAccountEnabled   TUIAction = "account_enabled"
	TUIActionAgentAccountRefreshed TUIAction = "account_refreshed"
	TUIActionAgentAccountReauthed  TUIAction = "account_reauthenticated"
	TUIActionAgentAccountSwitched  TUIAction = "account_switched"

	TUIActionCronJobCreated TUIAction = "cron_job_created"
	TUIActionCronJobUpdated TUIAction = "cron_job_updated"
	TUIActionCronJobDeleted TUIAction = "cron_job_deleted"
	TUIActionCronJobRunNow  TUIAction = "cron_job_run_now"

	TUIActionSessionMerged      TUIAction = "session_merged"
	TUIActionSessionArchived    TUIAction = "session_archived"
	TUIActionSessionRemoved     TUIAction = "session_removed"
	TUIActionSessionResurrected TUIAction = "session_resurrected"

	TUIStatusSuccess TUIStatus = "success"
	TUIStatusError   TUIStatus = "error"

	// TUIEntryPointHome is Home's guest cloud offer line; TUIEntryPointLogin is
	// the TUI login/upgrade subscription flow. The CLI cloud gate's own
	// "cli_login" lives in services/boss/cmd, which is a different package and
	// a different distinct-id scheme — it is deliberately NOT mirrored here, so
	// this type cannot be used to emit a CLI event from a view.
	TUIEntryPointHome  TUIEntryPoint = "tui_home"
	TUIEntryPointLogin TUIEntryPoint = "tui_login"

	// cloudConversionProductArea matches the product_area the CLI gate and the
	// bosso billing handlers already emit, so the terminal steps land in the
	// same funnel area as the server-side ones rather than beside it.
	cloudConversionProductArea = "billing"
)

// TUIActionStatus maps an outcome error to the bounded status enum, so no call
// site branches on err by hand and no error string can leak into a property.
func TUIActionStatus(err error) TUIStatus {
	if err != nil {
		return TUIStatusError
	}
	return TUIStatusSuccess
}

// CaptureTUIAction emits one tui_action for a completed TUI feature action.
// Call it from the handler that receives the action's *result* message, never
// from the keypress handler: a cancelled confirmation must emit nothing, and a
// confirmed-then-failed action must report status "error".
//
// This is the only place the tui_action property map is built, so a call site
// cannot invent a property key or attach an identifier.
func CaptureTUIAction(ctx context.Context, client telemetry.Client, feature TUIFeature, action TUIAction, status TUIStatus) {
	// Gate BEFORE building the map. Tracing is opt-in and defaults off, so the
	// overwhelmingly common path through every instrumented handler is "do
	// nothing", and that path should not allocate.
	if client == nil || !viewTelemetryEnabled() {
		return
	}
	CaptureViewTelemetry(ctx, client, telemetry.EventTUIAction, map[string]any{
		// Widened back to plain strings so the wire payload is a JSON string
		// regardless of how the enum types are marshalled.
		"feature": string(feature),
		"action":  string(action),
		"status":  string(status),
		"source":  "tui",
	})
}

// CaptureCloudConversionStep emits one terminal-side conversion-funnel step:
// the guest-offer impression, the subscribe hand-off, or the TUI's own
// checkout return. It is the only place their property map is built, so a call
// site cannot invent a key or attach an identifier — the same rule
// CaptureTUIAction enforces for tui_action.
//
// NEVER call it from a View() or render helper. Bubble Tea re-renders on every
// message, so a capture on a render path is unbounded and would make the
// funnel denominator meaningless. Every caller is an Update path, and the
// once-per-session steps latch before calling.
func CaptureCloudConversionStep(
	ctx context.Context,
	client telemetry.Client,
	event telemetry.Event,
	entryPoint TUIEntryPoint,
) {
	// Gate BEFORE building the map, for the reason CaptureTUIAction states:
	// tracing is opt-in and defaults off, so the common path must not allocate.
	if client == nil || !viewTelemetryEnabled() {
		return
	}
	CaptureViewTelemetry(ctx, client, event, map[string]any{
		"product_area": cloudConversionProductArea,
		// Widened back to a plain string so the wire payload is a JSON string
		// regardless of how the enum type is marshalled, as CaptureTUIAction does.
		"entry_point": string(entryPoint),
		"source":      "tui",
	})
}

func CaptureViewTelemetry(ctx context.Context, client telemetry.Client, event telemetry.Event, props map[string]any) {
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
//
// It bounds two caches, not one: viewTelemetryIdentity below reuses it, so this
// value is also how long events may keep carrying a stale identity after a
// login or logout inside the running TUI.
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

// viewTelemetryIdentity caches the signed-in email behind the same TTL as the
// opt-in gate above, and for the same reason: resolving it is a keychain read,
// and every capture runs on Bubble Tea's update goroutine — the one the TUI
// rubric requires to stay non-blocking. The trash delete-all batch drains one
// session per message, so an uncached lookup would put N synchronous keychain
// reads on that path.
//
// It expires rather than latching because a human can log in from inside the
// running TUI; a latched empty email would keep emitting the anonymous local-
// identity for the rest of the process.
var viewTelemetryIdentity struct {
	mu        sync.Mutex
	checkedAt time.Time
	email     string
}

// viewTelemetryEmailLookup is the seam a test replaces; production reads the
// keychain.
var viewTelemetryEmailLookup = viewTelemetryEmail

func viewTelemetryEmail() string {
	store, err := auth.NewKeychainStore(true)
	if err != nil {
		return ""
	}
	status := auth.NewManager(store, auth.Config{ClientID: ""}).Status()
	if status == nil || !status.LoggedIn {
		return ""
	}
	return strings.TrimSpace(status.Email)
}

// viewTelemetrySignedInEmailAt resolves the signed-in email with the clock
// supplied, so a test can step across the TTL boundary exactly.
func viewTelemetrySignedInEmailAt(now time.Time) string {
	viewTelemetryIdentity.mu.Lock()
	defer viewTelemetryIdentity.mu.Unlock()
	if viewTelemetryIdentity.checkedAt.IsZero() || now.Sub(viewTelemetryIdentity.checkedAt) >= viewTelemetryGateTTL {
		viewTelemetryIdentity.email = viewTelemetryEmailLookup()
		viewTelemetryIdentity.checkedAt = now
	}
	return viewTelemetryIdentity.email
}

// viewDistinctID is the id TUI events are captured on. It resolves through the
// one shared local-surface definition, so a TUI event and a CLI event for the
// same logged-in human are the same PostHog person — and the same person the
// web and bosso funnel events target.
func viewDistinctID() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	return telemetry.LocalFunnelDistinctID(home, viewTelemetrySignedInEmailAt(time.Now()))
}

// ResetTelemetryCache invalidates cached settings and identity after an external
// change. The next capture resolves both values again.
func ResetTelemetryCache() {
	viewTelemetryGate.mu.Lock()
	viewTelemetryGate.checkedAt = time.Time{}
	viewTelemetryGate.enabled = false
	viewTelemetryGate.mu.Unlock()
	viewTelemetryIdentity.mu.Lock()
	viewTelemetryIdentity.checkedAt = time.Time{}
	viewTelemetryIdentity.email = ""
	viewTelemetryIdentity.mu.Unlock()
}
