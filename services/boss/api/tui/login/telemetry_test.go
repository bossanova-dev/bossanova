package login

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/recurser/boss/api/tui/ui"
	"github.com/recurser/bossalib/config"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/telemetry"
)

type fakeTelemetry struct {
	events []telemetry.Event
	props  []map[string]any
}

func (f *fakeTelemetry) Capture(_ context.Context, e telemetry.Event, _ string, p map[string]any) {
	f.events = append(f.events, e)
	f.props = append(f.props, p)
}
func (*fakeTelemetry) Identify(context.Context, string, map[string]any) {}
func (*fakeTelemetry) Alias(context.Context, string, string)            {}
func (*fakeTelemetry) Close()                                           {}
func enableViewTelemetryForTest(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("BOSS_SETTINGS_PATH", filepath.Join(dir, "settings.json"))
	t.Setenv("BOSS_KEYRING_BACKEND", "file")
	s := config.DefaultSettings()
	s.EventTracingEnabled = true
	if err := config.Save(s); err != nil {
		t.Fatal(err)
	}
	ui.ResetTelemetryCache()
	t.Cleanup(ui.ResetTelemetryCache)
}
func cloudOfferCaptures(f *fakeTelemetry, e telemetry.Event) []map[string]any {
	var out []map[string]any
	for i, v := range f.events {
		if v == e {
			out = append(out, f.props[i])
		}
	}
	return out
}
func assertNoSensitiveTelemetryProps(t *testing.T, p map[string]any) {
	t.Helper()
	for _, k := range []string{"args", "prompt", "transcript", "repo_path", "branch", "path", "file_path", "comment", "email"} {
		if _, ok := p[k]; ok {
			t.Fatalf("sensitive prop %q present in %v", k, p)
		}
	}
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

			m := NewModel(nil, nil, context.Background())
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

	m := NewModel(nil, nil, context.Background())
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

	m := NewModel(nil, nil, context.Background())
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
