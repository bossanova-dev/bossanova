package views

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/bossalib/config"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/telemetry"
)

type fakeTelemetry struct {
	events      []telemetry.Event
	distinctIDs []string
	props       []map[string]any
}

func (f *fakeTelemetry) Capture(_ context.Context, event telemetry.Event, distinctID string, props map[string]any) {
	f.events = append(f.events, event)
	f.distinctIDs = append(f.distinctIDs, distinctID)
	f.props = append(f.props, props)
}

func (f *fakeTelemetry) Identify(context.Context, string, map[string]any) {}
func (f *fakeTelemetry) Alias(context.Context, string, string)            {}
func (f *fakeTelemetry) Close()                                           {}

func assertNoSensitiveTelemetryProps(t *testing.T, props map[string]any) {
	t.Helper()
	for _, key := range []string{"args", "prompt", "transcript", "repo_path", "branch", "path", "file_path", "comment", "email"} {
		if _, ok := props[key]; ok {
			t.Fatalf("sensitive prop %q present in %v", key, props)
		}
	}
}

func TestCaptureViewTelemetrySuppressesDisabledSettings(t *testing.T) {
	withTempConfigHome(t)
	resetViewTelemetryGate()
	t.Cleanup(resetViewTelemetryGate)
	rec := &fakeTelemetry{}

	captureViewTelemetry(context.Background(), rec, telemetry.EventChatAttached, map[string]any{
		"source": "tui",
	})

	if len(rec.events) != 0 {
		t.Fatalf("events = %d, want 0", len(rec.events))
	}
}

func TestCaptureViewTelemetryCapturesWhenEnabled(t *testing.T) {
	enableViewTelemetryForTest(t)
	rec := &fakeTelemetry{}

	captureViewTelemetry(context.Background(), rec, telemetry.EventChatAttached, map[string]any{
		"source": "tui",
	})

	if len(rec.events) != 1 {
		t.Fatalf("events = %d, want 1", len(rec.events))
	}
	if rec.events[0] != telemetry.EventChatAttached {
		t.Fatalf("event = %q, want %q", rec.events[0], telemetry.EventChatAttached)
	}
	if got := rec.props[0]["source"]; got != "tui" {
		t.Fatalf("source = %v, want tui", got)
	}
	assertNoSensitiveTelemetryProps(t, rec.props[0])
}

// TestCaptureViewTelemetryEmitsOnTheFunnelDistinctID is the sharpest anchor of
// the cutover. This test previously asserted that viewDistinctID() had a
// "local-" prefix and contained NO colon — exactly what a funnel id
// (user:<sub> / email:<address>) is. A logged-in TUI event must now land on the
// funnel person the web and bosso events target, so both halves of that old
// assertion are inverted here for the logged-in case, and preserved in
// TestViewDistinctIDFallsBackToLocalWithoutAnEmail for the anonymous one.
//
// It asserts on the id a *captured event* carries, not just on the resolver's
// return value: the defect was persons events landed on, so the event is the
// artifact under test.
func TestCaptureViewTelemetryEmitsOnTheFunnelDistinctID(t *testing.T) {
	enableViewTelemetryForTest(t)
	withViewTelemetryEmail(t, "  Person@Example.COM ")
	rec := &fakeTelemetry{}

	captureViewTelemetry(context.Background(), rec, telemetry.EventChatAttached, map[string]any{
		"source": "tui",
	})

	if len(rec.distinctIDs) != 1 {
		t.Fatalf("distinctIDs = %d, want 1", len(rec.distinctIDs))
	}
	got := rec.distinctIDs[0]
	want := telemetry.FunnelDistinctID("", "person@example.com")
	if got != want {
		t.Fatalf("captured distinctID = %q, want %q", got, want)
	}
	if got == telemetry.UserDistinctID("person@example.com") {
		t.Fatalf("captured distinctID = %q, still the retired user-<hash> namespace", got)
	}
	if got == viewLocalDistinctIDForTest(t) {
		t.Fatalf("captured distinctID = %q, still the pre-login local-<hash> namespace", got)
	}
	if !strings.Contains(got, ":") {
		t.Fatalf("captured distinctID = %q, want the colon-separated funnel form", got)
	}
}

// TestViewEventsShareTheCLIFunnelDistinctID pins the TUI half of the
// cross-surface parity claim. Both surfaces resolve through
// telemetry.LocalFunnelDistinctID, and services/boss/cmd/telemetry_test.go pins
// the CLI half against the same expression, so the two ids are equal by
// construction rather than by two call sites happening to agree. Package
// boundaries make a single test that calls both impossible.
func TestViewEventsShareTheCLIFunnelDistinctID(t *testing.T) {
	enableViewTelemetryForTest(t)
	withViewTelemetryEmail(t, "person@example.com")
	rec := &fakeTelemetry{}

	captureViewTelemetry(context.Background(), rec, telemetry.EventChatAttached, map[string]any{
		"source": "tui",
	})

	// The literal, not telemetry.LocalFunnelDistinctID(home, ...): computing the
	// expectation with the same production expression the code under test
	// evaluates makes this assertion unfalsifiable — a regression back to the
	// local- namespace would move both sides together. The CLI half in
	// services/boss/cmd/telemetry_test.go pins this same literal, and a shared
	// literal is the only thing that can pin parity across a package boundary.
	const want = "email:person@example.com"
	if len(rec.distinctIDs) != 1 || rec.distinctIDs[0] != want {
		t.Fatalf("captured distinctIDs = %#v, want [%q]", rec.distinctIDs, want)
	}
}

// TestViewDistinctIDFallsBackToLocalWithoutAnEmail keeps the anonymous machine
// on a stable per-home identity — the one the login-time alias bridges forward.
// "anonymous" would collapse every unauthenticated machine into one person.
func TestViewDistinctIDFallsBackToLocalWithoutAnEmail(t *testing.T) {
	withTempConfigHome(t)
	withViewTelemetryEmail(t, "")

	got := viewDistinctID()
	want := viewLocalDistinctIDForTest(t)
	if got != want {
		t.Fatalf("viewDistinctID() = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, "local-") {
		t.Fatalf("viewDistinctID() = %q, want local- prefix", got)
	}
	if got == "anonymous" {
		t.Fatalf("viewDistinctID() = %q, which merges every anonymous machine into one person", got)
	}
}

// TestViewTelemetryEmailIsCachedWithinItsTTL pins that resolving the identity is
// not a keychain read per capture. Every capture runs on Bubble Tea's update
// goroutine, which the TUI rubric requires to stay non-blocking.
//
// Both halves step the clock through viewTelemetrySignedInEmailAt rather than
// sleeping. Resetting the cache instead would zero checkedAt, and
// now.Sub(time.Time{}) exceeds ANY finite TTL — so the expiry half would pass
// against a cache that latches forever, precisely the failure it claims to catch.
func TestViewTelemetryEmailIsCachedWithinItsTTL(t *testing.T) {
	withTempConfigHome(t)
	calls := 0
	original := viewTelemetryEmailLookup
	viewTelemetryEmailLookup = func() string {
		calls++
		return "person@example.com"
	}
	resetViewTelemetryIdentity()
	t.Cleanup(func() {
		viewTelemetryEmailLookup = original
		resetViewTelemetryIdentity()
	})

	start := time.Now()
	for range 3 {
		viewTelemetrySignedInEmailAt(start)
	}
	if calls != 1 {
		t.Fatalf("lookup calls within the TTL = %d, want 1", calls)
	}

	viewTelemetrySignedInEmailAt(start.Add(viewTelemetryGateTTL))
	if calls != 2 {
		t.Fatalf("lookup calls after the TTL = %d, want 2", calls)
	}
}

// withViewTelemetryEmail replaces the keychain lookup and drops the cached
// identity in both directions, so a value cached under another test's
// environment cannot leak in or out.
func withViewTelemetryEmail(t *testing.T, email string) {
	t.Helper()
	original := viewTelemetryEmailLookup
	viewTelemetryEmailLookup = func() string { return email }
	resetViewTelemetryIdentity()
	t.Cleanup(func() {
		viewTelemetryEmailLookup = original
		resetViewTelemetryIdentity()
	})
}

// resetViewTelemetryIdentity drops the cached signed-in email. Test-only.
func resetViewTelemetryIdentity() {
	viewTelemetryIdentity.mu.Lock()
	defer viewTelemetryIdentity.mu.Unlock()
	viewTelemetryIdentity.checkedAt = time.Time{}
	viewTelemetryIdentity.email = ""
}

func viewLocalDistinctIDForTest(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir: %v", err)
	}
	return telemetry.LocalDistinctID(home)
}

func enableViewTelemetryForTest(t *testing.T) {
	t.Helper()
	withTempConfigHome(t)
	settings := config.DefaultSettings()
	settings.EventTracingEnabled = true
	if err := config.Save(settings); err != nil {
		t.Fatalf("config.Save: %v", err)
	}
	// The gate is cached with a TTL, so a value read under a previous test's
	// config home would otherwise leak into this one — in both directions.
	resetViewTelemetryGate()
	t.Cleanup(resetViewTelemetryGate)
}

// resetViewTelemetryGate drops the cached gate. Test-only: tests flip the
// settings file directly and must not observe a value cached under a previous
// test's config home.
func resetViewTelemetryGate() {
	viewTelemetryGate.mu.Lock()
	defer viewTelemetryGate.mu.Unlock()
	viewTelemetryGate.checkedAt = time.Time{}
	viewTelemetryGate.enabled = false
}

// TestViewTelemetryGateCachesWithinItsTTL pins that the opt-in gate is not a
// settings-file read per capture. config.Load is os.ReadFile + json.Unmarshal on
// Bubble Tea's update goroutine, and the trash delete-all batch drains one
// session per message, so an uncached gate puts N synchronous disk reads on the
// path the TUI rubric requires to stay non-blocking.
//
// Both halves step the clock explicitly through viewTelemetryEnabledAt rather
// than sleeping or resetting the gate. Resetting would zero checkedAt, and
// `now.Sub(time.Time{})` exceeds ANY finite TTL — so the expiry half would pass
// against a gate that latches forever, which is precisely the failure it claims
// to catch. Stepping the clock makes viewTelemetryGateTTL load-bearing in both
// directions, and removes the wall-clock flake of a >3s stall mid-test.
func TestViewTelemetryGateCachesWithinItsTTL(t *testing.T) {
	enableViewTelemetryForTest(t)
	base := time.Now()

	if !viewTelemetryEnabledAt(base) {
		t.Fatal("gate should be enabled after enableViewTelemetryForTest")
	}
	// Flip the file underneath the cache. Within the TTL the gate must NOT
	// re-read it; an uncached implementation returns false here.
	settings := config.DefaultSettings()
	settings.EventTracingEnabled = false
	if err := config.Save(settings); err != nil {
		t.Fatalf("config.Save: %v", err)
	}
	if !viewTelemetryEnabledAt(base.Add(viewTelemetryGateTTL - time.Millisecond)) {
		t.Fatal("gate re-read the settings file within its TTL; every capture would pay a " +
			"synchronous os.ReadFile + json.Unmarshal on the Bubble Tea update goroutine")
	}

	// ...and it must re-read once the TTL elapses, or turning tracing OFF in
	// general settings would not stop events until the operator restarts.
	if viewTelemetryEnabledAt(base.Add(viewTelemetryGateTTL)) {
		t.Fatalf("gate did not re-read the settings file after %s; turning event tracing "+
			"off in general settings would not take effect until restart", viewTelemetryGateTTL)
	}

	// The two assertions above step by the TTL itself, so they pin the caching
	// MECHANISM for any value of it — including one so long the gate never
	// expires in a real session, which is the sync.Once behaviour the design
	// deliberately rejected. Bound the value separately.
	if viewTelemetryGateTTL > 5*time.Second {
		t.Fatalf("viewTelemetryGateTTL = %s. The gate exists to be re-read while the "+
			"operator is still looking at the screen: turning event tracing off in general "+
			"settings must stop events within seconds, not at the next restart.", viewTelemetryGateTTL)
	}
	if viewTelemetryGateTTL <= 0 {
		t.Fatalf("viewTelemetryGateTTL = %s, which disables the cache entirely and puts a "+
			"synchronous settings read back on every capture", viewTelemetryGateTTL)
	}
}

// --- Terminal conversion steps (BOS-1260) ---------------------------------

// cloudOfferLine is the copy the guest cloud offer renders. Both render sites
// draw exactly this, so a test that finds it in renderEmptyState and in
// renderSessionTableFooter has genuinely exercised both.
const cloudOfferLine = "[l]ogin to try Bossanova Cloud for free"

// newAppForCloudOfferTelemetry builds a real App whose Home is eligible for the
// guest cloud offer: value delivered inside the 72h window, the per-session
// timer not yet elapsed, signed out, and auth configured.
//
// A real App is required, not a bare HomeModel: the impression is captured at
// the App root so the latch survives Home being rebuilt, and driving
// HomeModel.Update directly would exercise none of the capture.
func newAppForCloudOfferTelemetry(client telemetry.Client, offerEligible bool) App {
	a := NewApp(nil, &auth.Manager{})
	a.telemetry = client
	startedAt := a.home.startedAt
	settings := config.DefaultSettings()
	settings.InstalledAt = startedAt
	if offerEligible {
		settings.BossCloudValueDeliveredAt = startedAt
	}
	a.home.SetSettings(settings)
	// Pin the clock a second into the session so the one-minute fatigue timer
	// has not elapsed, rather than racing wall-clock time.
	a.home.now = func() time.Time { return startedAt.Add(time.Second) }
	return a
}

func cloudOfferCaptures(rec *fakeTelemetry, event telemetry.Event) []map[string]any {
	captured := make([]map[string]any, 0, len(rec.events))
	for i, got := range rec.events {
		if got == event {
			captured = append(captured, rec.props[i])
		}
	}
	return captured
}

// TestGuestCloudOfferImpressionIsOncePerSessionAcrossBothRenderSites pins
// requirement 1 and requirement 2 together: the offer is drawn from two render
// functions and Bubble Tea re-renders on every message, so a capture on a
// render path would be unbounded. One session that renders the offer through
// renderEmptyState AND renderSessionTableFooter must report exactly ONE
// impression — not one per render, and not one per site.
func TestGuestCloudOfferImpressionIsOncePerSessionAcrossBothRenderSites(t *testing.T) {
	enableViewTelemetryForTest(t)
	rec := &fakeTelemetry{}
	a := newAppForCloudOfferTelemetry(rec, true)

	// Empty state: the poll clears Home's loading screen with no sessions.
	model, _ := a.Update(sessionListMsg{})
	app, ok := model.(App)
	if !ok {
		t.Fatalf("Update returned %T, want App", model)
	}
	if empty := app.home.renderEmptyState(); !strings.Contains(empty, cloudOfferLine) {
		t.Fatalf("renderEmptyState did not draw the guest offer: %q", empty)
	}
	// Render it again: a render-path capture would fire on every frame, so a
	// second render of the same screen is what separates "latched" from
	// "happened to be called once".
	_ = app.home.renderEmptyState()

	// Populated table: the SAME offer is drawn by the footer instead.
	model, _ = app.Update(sessionListMsg{sessions: []*pb.Session{{Id: "s1", Title: "BOS-1260"}}})
	app, ok = model.(App)
	if !ok {
		t.Fatalf("Update returned %T, want App", model)
	}
	if footer := app.home.renderSessionTableFooter(); !strings.Contains(footer, cloudOfferLine) {
		t.Fatalf("renderSessionTableFooter did not draw the guest offer: %q", footer)
	}
	_ = app.home.renderSessionTableFooter()

	captured := cloudOfferCaptures(rec, telemetry.EventCloudGuestOfferShown)
	if len(captured) != 1 {
		t.Fatalf("cloud_guest_offer_shown captures = %d, want exactly 1 (%v)", len(captured), rec.events)
	}
	if got := captured[0]["entry_point"]; got != "tui_home" {
		t.Fatalf("entry_point = %v, want tui_home", got)
	}
	if got := captured[0]["product_area"]; got != "billing" {
		t.Fatalf("product_area = %v, want billing", got)
	}
	if got := captured[0]["source"]; got != "tui" {
		t.Fatalf("source = %v, want tui", got)
	}
	assertNoSensitiveTelemetryProps(t, captured[0])
}

// TestGuestCloudOfferImpressionLatchSurvivesHomeBeingRebuilt pins why the latch
// lives on App rather than HomeModel: navigating away from Home and back runs
// newHomeModel, which would re-arm a latch held there and report a second
// impression of the same offer.
func TestGuestCloudOfferImpressionLatchSurvivesHomeBeingRebuilt(t *testing.T) {
	enableViewTelemetryForTest(t)
	rec := &fakeTelemetry{}
	a := newAppForCloudOfferTelemetry(rec, true)

	model, _ := a.Update(sessionListMsg{})
	app := model.(App)
	if got := len(cloudOfferCaptures(rec, telemetry.EventCloudGuestOfferShown)); got != 1 {
		t.Fatalf("captures after first visible poll = %d, want 1", got)
	}

	// Rebuild Home exactly as returning to it does, preserving the eligible
	// settings and clock so the offer is visible again on the new model.
	settings, now := app.home.settings, app.home.now
	app.home = app.newHomeModel()
	app.home.SetSettings(settings)
	app.home.now = now

	model, _ = app.Update(sessionListMsg{})
	app = model.(App)
	if offer := app.home.renderEmptyState(); !strings.Contains(offer, cloudOfferLine) {
		t.Fatalf("rebuilt Home did not draw the guest offer, so this test proves nothing: %q", offer)
	}
	if got := len(cloudOfferCaptures(rec, telemetry.EventCloudGuestOfferShown)); got != 1 {
		t.Fatalf("captures after Home was rebuilt = %d, want 1", got)
	}
}

// TestGuestCloudOfferImpressionSuppressedWhenGateClosed is the absence
// assertion, written against a model that is identical to the emitting one
// except for the offer gate — the sibling sub-test below shows it able to fire,
// so a zero here means the gate suppressed the event rather than the harness
// never reaching the capture.
func TestGuestCloudOfferImpressionSuppressedWhenGateClosed(t *testing.T) {
	enableViewTelemetryForTest(t)

	t.Run("gate closed", func(t *testing.T) {
		rec := &fakeTelemetry{}
		a := newAppForCloudOfferTelemetry(rec, false)

		model, _ := a.Update(sessionListMsg{})
		app := model.(App)
		if offer := app.home.renderEmptyState(); strings.Contains(offer, cloudOfferLine) {
			t.Fatalf("offer was drawn although the gate is closed: %q", offer)
		}
		_ = app.home.renderEmptyState()

		model, _ = app.Update(sessionListMsg{sessions: []*pb.Session{{Id: "s1", Title: "BOS-1260"}}})
		app = model.(App)
		_ = app.home.renderSessionTableFooter()

		if got := len(cloudOfferCaptures(rec, telemetry.EventCloudGuestOfferShown)); got != 0 {
			t.Fatalf("cloud_guest_offer_shown captures = %d, want 0", got)
		}
	})

	t.Run("gate open emits, proving the absence above can fire", func(t *testing.T) {
		rec := &fakeTelemetry{}
		a := newAppForCloudOfferTelemetry(rec, true)

		model, _ := a.Update(sessionListMsg{})
		_ = model.(App)

		if got := len(cloudOfferCaptures(rec, telemetry.EventCloudGuestOfferShown)); got != 1 {
			t.Fatalf("cloud_guest_offer_shown captures = %d, want 1", got)
		}
	})
}

// TestGuestCloudOfferImpressionWaitsForHomeToLeaveTheLoadingScreen pins that the
// impression counts a screen the user actually saw. Home opens on "Loading
// sessions…", which renders neither offer site.
func TestGuestCloudOfferImpressionWaitsForHomeToLeaveTheLoadingScreen(t *testing.T) {
	enableViewTelemetryForTest(t)
	rec := &fakeTelemetry{}
	a := newAppForCloudOfferTelemetry(rec, true)

	// A message that does not clear Home's loading screen.
	model, _ := a.Update(repoCountMsg{count: 1})
	app := model.(App)
	if !app.home.loading {
		t.Fatal("Home left the loading screen, so this test proves nothing")
	}
	if got := len(cloudOfferCaptures(rec, telemetry.EventCloudGuestOfferShown)); got != 0 {
		t.Fatalf("captures while Home was still loading = %d, want 0", got)
	}

	model, _ = app.Update(sessionListMsg{})
	app = model.(App)
	if got := len(cloudOfferCaptures(rec, telemetry.EventCloudGuestOfferShown)); got != 1 {
		t.Fatalf("captures after the loading screen cleared = %d, want 1", got)
	}
}

// TestGuestCloudOfferImpressionSuppressedWhileTheFooterIsReplaced pins that the
// once-per-session latch is only ever spent on a frame the offer is actually
// on. On a POPULATED board the offer is drawn exclusively by
// renderSessionTableFooter, and renderSessionTable reaches that call only in
// its final arm: the rename editor, the confirm prompt and the in-progress
// upgrade status each stand in for the footer and take the offer down with
// them. Capturing in any of those states would burn the permanent latch on a
// frame the user never saw the offer on AND suppress the real impression that
// follows forever — and the gate is not static for the session, so a modal open
// at the moment it first passes is reachable.
//
// The control sub-test renders the same populated board with none of the four
// flags set and DOES capture, so a zero above means the footer was replaced
// rather than the harness never reaching the capture.
func TestGuestCloudOfferImpressionSuppressedWhileTheFooterIsReplaced(t *testing.T) {
	enableViewTelemetryForTest(t)

	sessions := []*pb.Session{{Id: "s1", Title: "BOS-1260"}}

	for _, tc := range []struct {
		name     string
		suppress func(h *HomeModel)
	}{
		{"rename editor open", func(h *HomeModel) { h.rename, _ = newRenamePrompt("s1", "BOS-1260") }},
		{"confirm prompt open", func(h *HomeModel) { h.confirm = newConfirmPrompt("Log out?", nil) }},
		{"upgrade in progress", func(h *HomeModel) { h.upgrading = true }},
		{"restart in progress", func(h *HomeModel) { h.restarting = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &fakeTelemetry{}
			a := newAppForCloudOfferTelemetry(rec, true)
			tc.suppress(&a.home)

			model, _ := a.Update(sessionListMsg{sessions: sessions})
			app, ok := model.(App)
			if !ok {
				t.Fatalf("Update returned %T, want App", model)
			}
			// The offer gate itself must still be open, or a zero below would
			// say nothing about the footer.
			if !app.home.guestCloudOfferVisible() {
				t.Fatal("the offer gate is closed, so this case proves nothing")
			}
			if board := app.home.renderSessionTable(); strings.Contains(board, cloudOfferLine) {
				t.Fatalf("renderSessionTable drew the offer although the footer is replaced: %q", board)
			}
			if got := len(cloudOfferCaptures(rec, telemetry.EventCloudGuestOfferShown)); got != 0 {
				t.Fatalf("cloud_guest_offer_shown captures = %d, want 0", got)
			}
		})
	}

	t.Run("footer on screen emits, proving the absences above can fire", func(t *testing.T) {
		rec := &fakeTelemetry{}
		a := newAppForCloudOfferTelemetry(rec, true)

		model, _ := a.Update(sessionListMsg{sessions: sessions})
		app, ok := model.(App)
		if !ok {
			t.Fatalf("Update returned %T, want App", model)
		}
		if board := app.home.renderSessionTable(); !strings.Contains(board, cloudOfferLine) {
			t.Fatalf("renderSessionTable did not draw the offer on the unsuppressed board: %q", board)
		}
		if got := len(cloudOfferCaptures(rec, telemetry.EventCloudGuestOfferShown)); got != 1 {
			t.Fatalf("cloud_guest_offer_shown captures = %d, want 1", got)
		}
	})
}

// TestSubscribePageOpenedCapturedOnTUIHandOff pins the TUI half of requirement
// 3, including the failing-browser case: openSubscriptionCheckoutURL returning
// an error still shows the user the URL, so the hand-off happened and must be
// counted.
func TestSubscribePageOpenedCapturedOnTUIHandOff(t *testing.T) {
	for _, tt := range []struct {
		name     string
		openErr  error
		wantOpen bool
	}{
		{name: "browser opened", openErr: nil, wantOpen: true},
		{name: "browser open failed", openErr: errors.New("no browser"), wantOpen: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			enableViewTelemetryForTest(t)
			rec := &fakeTelemetry{}

			orig := openSubscriptionCheckoutURL
			opened := false
			openSubscriptionCheckoutURL = func(string) error {
				opened = true
				return tt.openErr
			}
			t.Cleanup(func() { openSubscriptionCheckoutURL = orig })

			m := NewLoginModel(nil, nil, context.Background())
			m.SetTelemetry(rec)
			cmd := m.subscriptionOpenBrowser("https://billing.example.test/checkout", 1)
			if cmd == nil {
				t.Fatal("subscriptionOpenBrowser returned no command")
			}
			msg := cmd()
			if _, ok := msg.(subscriptionBrowserOpenedMsg); !ok {
				t.Fatalf("command returned %T, want subscriptionBrowserOpenedMsg", msg)
			}
			if opened != tt.wantOpen {
				t.Fatalf("browser opened = %t, want %t", opened, tt.wantOpen)
			}

			captured := cloudOfferCaptures(rec, telemetry.EventCloudSubscribePageOpened)
			if len(captured) != 1 {
				t.Fatalf("cloud_subscribe_page_opened captures = %d, want 1 (%v)", len(captured), rec.events)
			}
			// The discriminator: this must NOT be the CLI gate's cli_login, or
			// the two hand-off paths are indistinguishable in PostHog.
			if got := captured[0]["entry_point"]; got != "tui_login" {
				t.Fatalf("entry_point = %v, want tui_login", got)
			}
			if got := captured[0]["product_area"]; got != "billing" {
				t.Fatalf("product_area = %v, want billing", got)
			}
			// The checkout URL carries a Stripe session id; it must never reach
			// a property.
			for _, value := range captured[0] {
				if text, isText := value.(string); isText && strings.Contains(text, "billing.example.test") {
					t.Fatalf("checkout URL leaked into a property: %v", captured[0])
				}
			}
			assertNoSensitiveTelemetryProps(t, captured[0])
		})
	}
}

// TestCheckoutReturnedCapturedOnceFromTUI pins the TUI-side checkout return.
// The terminal receives no browser redirect: the first poll that comes back
// ACTIVE after the TUI started a checkout IS the return, and two poll results
// for one attempt can both take that branch.
func TestCheckoutReturnedCapturedOnceFromTUI(t *testing.T) {
	enableViewTelemetryForTest(t)
	rec := &fakeTelemetry{}

	m := NewLoginModel(nil, nil, context.Background())
	m.SetTelemetry(rec)
	m.subscription.attempt = 1
	m.subscription.checkoutStarted = true
	m.subscription.phase = subscriptionPhaseWaiting

	active := &pb.CloudAccessStatus{State: pb.CloudAccessState_CLOUD_ACCESS_STATE_ACTIVE}
	updated, _ := m.updateSubscriptionAccess(subscriptionAccessMsg{status: active, attempt: 1})
	if updated.subscription.phase != subscriptionPhaseSuccess {
		t.Fatalf("phase = %v, want success", updated.subscription.phase)
	}
	// A second ACTIVE result for the same attempt — the initial check and an
	// armed poll tick genuinely race here.
	updated, _ = updated.updateSubscriptionAccess(subscriptionAccessMsg{status: active, attempt: 1})

	captured := cloudOfferCaptures(rec, telemetry.EventCloudCheckoutReturned)
	if len(captured) != 1 {
		t.Fatalf("cloud_checkout_returned captures = %d, want exactly 1 (%v)", len(captured), rec.events)
	}
	if got := captured[0]["entry_point"]; got != "tui_login" {
		t.Fatalf("entry_point = %v, want tui_login", got)
	}
	if got := captured[0]["source"]; got != "tui" {
		t.Fatalf("source = %v, want tui", got)
	}
	assertNoSensitiveTelemetryProps(t, captured[0])
	_ = updated
}

// TestCheckoutReturnedNotCapturedWithoutACheckout pins the gate that keeps the
// funnel honest: a user who was ALREADY subscribed when the flow opened reaches
// the same ACTIVE branch on the very first status check, and counting that as a
// checkout return would put completions into the funnel that had no hand-off.
func TestCheckoutReturnedNotCapturedWithoutACheckout(t *testing.T) {
	enableViewTelemetryForTest(t)
	rec := &fakeTelemetry{}

	m := NewLoginModel(nil, nil, context.Background())
	m.SetTelemetry(rec)
	m.subscription.attempt = 1
	m.subscription.phase = subscriptionPhaseChecking
	// checkoutStarted deliberately false: no checkout was created in this flow.

	active := &pb.CloudAccessStatus{State: pb.CloudAccessState_CLOUD_ACCESS_STATE_ACTIVE}
	updated, _ := m.updateSubscriptionAccess(subscriptionAccessMsg{status: active, attempt: 1})
	if updated.subscription.phase != subscriptionPhaseSuccess {
		t.Fatal("the already-subscribed path did not reach the success branch, so this test proves nothing")
	}

	if got := len(cloudOfferCaptures(rec, telemetry.EventCloudCheckoutReturned)); got != 0 {
		t.Fatalf("cloud_checkout_returned captures = %d, want 0", got)
	}
}

// TestCloudConversionStepsAreInertWithoutAClient pins that every new capture
// tolerates the nil client a test or an un-wired view hands it, the same
// contract captureTUIAction holds.
func TestCloudConversionStepsAreInertWithoutAClient(t *testing.T) {
	enableViewTelemetryForTest(t)

	for _, event := range []telemetry.Event{
		telemetry.EventCloudGuestOfferShown,
		telemetry.EventCloudSubscribePageOpened,
		telemetry.EventCloudCheckoutReturned,
	} {
		captureCloudConversionStep(context.Background(), nil, event, tuiEntryPointHome)
	}
}

// TestCloudConversionStepsSuppressedWhenTracingIsOff pins that the new captures
// honour the same opt-in gate as every other view capture.
func TestCloudConversionStepsSuppressedWhenTracingIsOff(t *testing.T) {
	withTempConfigHome(t)
	resetViewTelemetryGate()
	t.Cleanup(resetViewTelemetryGate)
	rec := &fakeTelemetry{}

	captureCloudConversionStep(
		context.Background(),
		rec,
		telemetry.EventCloudGuestOfferShown,
		tuiEntryPointHome,
	)

	if len(rec.events) != 0 {
		t.Fatalf("events = %d, want 0 with tracing off", len(rec.events))
	}
}

// TestCloudConversionEventsAreRegisteredWithTheirEmittedProperties pins that
// every property the TUI emit sites supply survives the registry filter. A
// property the Registry does not list is dropped silently, so a typo here is
// otherwise invisible until a PostHog funnel comes back empty.
func TestCloudConversionEventsAreRegisteredWithTheirEmittedProperties(t *testing.T) {
	enableViewTelemetryForTest(t)
	rec := &fakeTelemetry{}

	captureCloudConversionStep(
		context.Background(),
		rec,
		telemetry.EventCloudGuestOfferShown,
		tuiEntryPointHome,
	)
	if len(rec.props) != 1 {
		t.Fatalf("captures = %d, want 1", len(rec.props))
	}
	for key := range rec.props[0] {
		if !telemetry.IsAllowedProperty(telemetry.EventCloudGuestOfferShown, key) {
			t.Errorf("cloud_guest_offer_shown emits %q, which the registry drops", key)
		}
		if !telemetry.IsAllowedProperty(telemetry.EventCloudSubscribePageOpened, key) {
			t.Errorf("cloud_subscribe_page_opened emits %q, which the registry drops", key)
		}
		if !telemetry.IsAllowedProperty(telemetry.EventCloudCheckoutReturned, key) {
			t.Errorf("cloud_checkout_returned emits %q, which the registry drops", key)
		}
	}
}
