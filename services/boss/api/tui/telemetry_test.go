package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/api/tui/ui"
	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/bossalib/kit/config"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/telemetry"
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

func enableViewTelemetryForTest(t *testing.T) {
	t.Helper()
	withTempConfigHome(t)
	settings := config.DefaultSettings()
	settings.EventTracingEnabled = true
	if err := config.Save(settings); err != nil {
		t.Fatal(err)
	}
	ui.ResetTelemetryCache()
	t.Cleanup(ui.ResetTelemetryCache)
}
func resetViewTelemetryGate() { ui.ResetTelemetryCache() }

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
	startedAt := a.home.StartedAt
	settings := config.DefaultSettings()
	settings.InstalledAt = startedAt
	if offerEligible {
		settings.BossCloudValueDeliveredAt = startedAt
	}
	a.home.SetSettings(settings)
	// Pin the clock a second into the session so the one-minute fatigue timer
	// has not elapsed, rather than racing wall-clock time.
	a.home.Now = func() time.Time { return startedAt.Add(time.Second) }
	return a
}

func cloudOfferCaptures(rec *fakeTelemetry) []map[string]any {
	captured := make([]map[string]any, 0, len(rec.events))
	for i, got := range rec.events {
		if got == telemetry.EventCloudGuestOfferShown {
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
	if empty := app.home.RenderEmptyState(); !strings.Contains(empty, cloudOfferLine) {
		t.Fatalf("renderEmptyState did not draw the guest offer: %q", empty)
	}
	// Render it again: a render-path capture would fire on every frame, so a
	// second render of the same screen is what separates "latched" from
	// "happened to be called once".
	_ = app.home.RenderEmptyState()

	// Populated table: the SAME offer is drawn by the footer instead.
	model, _ = app.Update(sessionListMsg{Sessions: []*pb.Session{{Id: "s1", Title: "BOS-1260"}}})
	app, ok = model.(App)
	if !ok {
		t.Fatalf("Update returned %T, want App", model)
	}
	if footer := app.home.RenderSessionTableFooter(); !strings.Contains(footer, cloudOfferLine) {
		t.Fatalf("renderSessionTableFooter did not draw the guest offer: %q", footer)
	}
	_ = app.home.RenderSessionTableFooter()

	captured := cloudOfferCaptures(rec)
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
	if got := len(cloudOfferCaptures(rec)); got != 1 {
		t.Fatalf("captures after first visible poll = %d, want 1", got)
	}

	// Rebuild Home exactly as returning to it does, preserving the eligible
	// settings and clock so the offer is visible again on the new model.
	settings, now := app.home.Settings, app.home.Now
	app.home = app.newHomeModel()
	app.home.SetSettings(settings)
	app.home.Now = now

	model, _ = app.Update(sessionListMsg{})
	app = model.(App)
	if offer := app.home.RenderEmptyState(); !strings.Contains(offer, cloudOfferLine) {
		t.Fatalf("rebuilt Home did not draw the guest offer, so this test proves nothing: %q", offer)
	}
	if got := len(cloudOfferCaptures(rec)); got != 1 {
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
		if offer := app.home.RenderEmptyState(); strings.Contains(offer, cloudOfferLine) {
			t.Fatalf("offer was drawn although the gate is closed: %q", offer)
		}
		_ = app.home.RenderEmptyState()

		model, _ = app.Update(sessionListMsg{Sessions: []*pb.Session{{Id: "s1", Title: "BOS-1260"}}})
		app = model.(App)
		_ = app.home.RenderSessionTableFooter()

		if got := len(cloudOfferCaptures(rec)); got != 0 {
			t.Fatalf("cloud_guest_offer_shown captures = %d, want 0", got)
		}
	})

	t.Run("gate open emits, proving the absence above can fire", func(t *testing.T) {
		rec := &fakeTelemetry{}
		a := newAppForCloudOfferTelemetry(rec, true)

		model, _ := a.Update(sessionListMsg{})
		_ = model.(App)

		if got := len(cloudOfferCaptures(rec)); got != 1 {
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
	model, _ := a.Update(repoCountMsg{Count: 1})
	app := model.(App)
	if !app.home.Loading {
		t.Fatal("Home left the loading screen, so this test proves nothing")
	}
	if got := len(cloudOfferCaptures(rec)); got != 0 {
		t.Fatalf("captures while Home was still loading = %d, want 0", got)
	}

	model, _ = app.Update(sessionListMsg{})
	app = model.(App)
	if got := len(cloudOfferCaptures(rec)); got != 1 {
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
		{"rename editor open", func(h *HomeModel) { h.Rename, _ = ui.NewRenamePrompt("s1", "BOS-1260") }},
		{"confirm prompt open", func(h *HomeModel) { h.Confirm = ui.NewConfirmPrompt("Log out?", nil) }},
		{"upgrade in progress", func(h *HomeModel) { h.Upgrading = true }},
		{"restart in progress", func(h *HomeModel) { h.Restarting = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &fakeTelemetry{}
			a := newAppForCloudOfferTelemetry(rec, true)
			tc.suppress(&a.home)

			model, _ := a.Update(sessionListMsg{Sessions: sessions})
			app, ok := model.(App)
			if !ok {
				t.Fatalf("Update returned %T, want App", model)
			}
			// The offer gate itself must still be open, or a zero below would
			// say nothing about the footer.
			if !app.home.GuestCloudOfferVisible() {
				t.Fatal("the offer gate is closed, so this case proves nothing")
			}
			if board := app.home.RenderSessionTable(); strings.Contains(board, cloudOfferLine) {
				t.Fatalf("renderSessionTable drew the offer although the footer is replaced: %q", board)
			}
			if got := len(cloudOfferCaptures(rec)); got != 0 {
				t.Fatalf("cloud_guest_offer_shown captures = %d, want 0", got)
			}
		})
	}

	t.Run("footer on screen emits, proving the absences above can fire", func(t *testing.T) {
		rec := &fakeTelemetry{}
		a := newAppForCloudOfferTelemetry(rec, true)

		model, _ := a.Update(sessionListMsg{Sessions: sessions})
		app, ok := model.(App)
		if !ok {
			t.Fatalf("Update returned %T, want App", model)
		}
		if board := app.home.RenderSessionTable(); !strings.Contains(board, cloudOfferLine) {
			t.Fatalf("renderSessionTable did not draw the offer on the unsuppressed board: %q", board)
		}
		if got := len(cloudOfferCaptures(rec)); got != 1 {
			t.Fatalf("cloud_guest_offer_shown captures = %d, want 1", got)
		}
	})
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
