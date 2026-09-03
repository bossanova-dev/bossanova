package views

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/bossalib/config"
)

func TestCloudDiscoveryLine(t *testing.T) {
	line := cloudDiscoveryLine(false, true)
	for _, want := range []string{
		"Bossanova Cloud",
		"[l]ogin to try Bossanova Cloud for free",
		"realtime GitHub events",
		"web remote control",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("cloudDiscoveryLine(false, true) = %q, want %q", line, want)
		}
	}

	if got := cloudDiscoveryLine(true, true); got != "" {
		t.Fatalf("cloudDiscoveryLine(true, true) = %q, want empty", got)
	}

	if got := cloudDiscoveryLine(false, false); got != "" {
		t.Fatalf("cloudDiscoveryLine(false, false) = %q, want empty", got)
	}
}

func TestCloudSettingsBlock(t *testing.T) {
	block := cloudSettingsBlock()
	for _, want := range []string{
		"Bossanova Cloud",
		"14-day free trial",
		"press [l]ogin from Home",
		"local mode stays free",
		"optional",
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("cloudSettingsBlock() = %q, want %q", block, want)
		}
	}
}

func TestSettingsViewHidesCloudBlockWithoutAuth(t *testing.T) {
	m := NewSettingsModel(nil, context.Background())
	view := m.View().Content
	if strings.Contains(view, "press [l]ogin from Home") {
		t.Fatalf("settings view showed cloud login prompt without auth configured: %q", view)
	}
}

func TestCloudGuestOfferVisiblePolicy(t *testing.T) {
	installedAt := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	sessionStartedAt := installedAt.Add(10 * time.Second)
	now := sessionStartedAt.Add(30 * time.Second)
	settings := config.DefaultSettings()
	settings.InstalledAt = installedAt
	settings.BossCloudValueDeliveredAt = installedAt

	if !cloudGuestOfferVisible(settings, now, sessionStartedAt, false, true) {
		t.Fatal("cloudGuestOfferVisible = false, want true for eligible guest")
	}
}

func TestCloudGuestOfferHiddenByManualSetting(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	settings := config.DefaultSettings()
	settings.InstalledAt = now
	settings.BossCloudValueDeliveredAt = now
	settings.BossCloudGuestOfferHidden = true

	if cloudGuestOfferVisible(settings, now, now, false, true) {
		t.Fatal("cloudGuestOfferVisible = true, want false when manually hidden")
	}
}

func TestCloudGuestOfferHiddenAfterSessionLimit(t *testing.T) {
	installedAt := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	sessionStartedAt := installedAt
	now := sessionStartedAt.Add(cloudGuestOfferSessionLimit + time.Second)
	settings := config.DefaultSettings()
	settings.InstalledAt = installedAt
	settings.BossCloudValueDeliveredAt = installedAt

	if cloudGuestOfferVisible(settings, now, sessionStartedAt, false, true) {
		t.Fatal("cloudGuestOfferVisible = true, want false after session limit")
	}
}

func TestCloudGuestOfferHiddenAfterValueWindow(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	settings := config.DefaultSettings()
	// Anchor the window to value-delivered, not install time.
	settings.BossCloudValueDeliveredAt = now.Add(-cloudGuestOfferValueWindow)

	if cloudGuestOfferVisible(settings, now, now, false, true) {
		t.Fatal("cloudGuestOfferVisible = true, want false after value window")
	}
}

func TestCloudGuestOfferValueWindowIsExactlyThreeDays(t *testing.T) {
	// Keep the product promise independent from the implementation constant so a
	// changed multiplier cannot move the offer window without detection.
	if got, want := cloudGuestOfferValueWindow, 72*time.Hour; got != want {
		t.Fatalf("cloudGuestOfferValueWindow = %s, want %s", got, want)
	}
}

func TestCloudGuestOfferHiddenWhenLoggedInOrAuthUnavailable(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	settings := config.DefaultSettings()
	settings.InstalledAt = now
	settings.BossCloudValueDeliveredAt = now

	if cloudGuestOfferVisible(settings, now, now, true, true) {
		t.Fatal("cloudGuestOfferVisible = true, want false for logged-in user")
	}
	if cloudGuestOfferVisible(settings, now, now, false, false) {
		t.Fatal("cloudGuestOfferVisible = true, want false without auth configured")
	}
}

func TestCloudGuestOfferSuppressedByProofFlag(t *testing.T) {
	// Use the same eligible-guest setup as TestCloudGuestOfferVisiblePolicy.
	installedAt := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	sessionStartedAt := installedAt.Add(10 * time.Second)
	now := sessionStartedAt.Add(30 * time.Second)
	settings := config.DefaultSettings()
	settings.InstalledAt = installedAt
	settings.BossCloudValueDeliveredAt = installedAt

	// Verify baseline: eligible guest returns true without the flag.
	if !cloudGuestOfferVisible(settings, now, sessionStartedAt, false, true) {
		t.Fatal("cloudGuestOfferVisible = false, want true for eligible guest (baseline)")
	}

	// Suppressed when proofHideGuestOffer is set.
	defer func(prev bool) { proofHideGuestOffer = prev }(proofHideGuestOffer)
	proofHideGuestOffer = true
	if cloudGuestOfferVisible(settings, now, sessionStartedAt, false, true) {
		t.Fatal("cloudGuestOfferVisible = true, want false when proofHideGuestOffer is set")
	}
}

// TestCloudGuestOfferVisible is the canonical table-driven test for
// cloudGuestOfferVisible. It covers the value-delivered gate and the 72h
// window anchored to BossCloudValueDeliveredAt.
func TestCloudGuestOfferVisible(t *testing.T) {
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name             string
		loggedIn         bool
		authConfigured   bool
		offerHidden      bool
		proofHide        bool
		valueDeliveredAt time.Time
		now              time.Time
		want             bool
	}{
		{
			name:           "no value delivered -> hidden",
			authConfigured: true,
			now:            base,
			want:           false,
		},
		{
			name:             "value delivered, within 72h -> shown",
			authConfigured:   true,
			valueDeliveredAt: base,
			now:              base.Add(time.Hour),
			want:             true,
		},
		{
			name:             "value delivered, past 72h window -> hidden",
			authConfigured:   true,
			valueDeliveredAt: base,
			now:              base.Add(73 * time.Hour),
			want:             false,
		},
		{
			name:             "logged in -> hidden",
			loggedIn:         true,
			authConfigured:   true,
			valueDeliveredAt: base,
			now:              base.Add(time.Hour),
			want:             false,
		},
		{
			name:             "auth not configured -> hidden",
			authConfigured:   false,
			valueDeliveredAt: base,
			now:              base.Add(time.Hour),
			want:             false,
		},
		{
			name:             "offer hidden setting -> hidden",
			authConfigured:   true,
			offerHidden:      true,
			valueDeliveredAt: base,
			now:              base.Add(time.Hour),
			want:             false,
		},
		{
			name:             "proof hide env -> hidden",
			authConfigured:   true,
			proofHide:        true,
			valueDeliveredAt: base,
			now:              base.Add(time.Hour),
			want:             false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			settings := config.DefaultSettings()
			settings.BossCloudValueDeliveredAt = tc.valueDeliveredAt
			settings.BossCloudGuestOfferHidden = tc.offerHidden

			if tc.proofHide {
				prev := proofHideGuestOffer
				proofHideGuestOffer = true
				defer func() { proofHideGuestOffer = prev }()
			}

			// Pass zero sessionStartedAt so the session-limit gate never fires.
			got := cloudGuestOfferVisible(settings, tc.now, time.Time{}, tc.loggedIn, tc.authConfigured)
			if got != tc.want {
				t.Fatalf("want %v, got %v", tc.want, got)
			}
		})
	}
}

// The settings promo renders the same "14-day free trial" line as Home's guest
// offer, so it inherits the same suppression: a retained re-login state reports
// LoggedIn=false, but that user is a signed-out account holder rather than a
// guest, and may well have used their trial already. See guestCloudOfferVisible
// in home_view.go, which is where this policy was first decided.
func TestShouldShowCloudSettings(t *testing.T) {
	cases := []struct {
		name   string
		tokens *auth.Tokens
		want   bool
	}{
		{name: "never logged in", tokens: nil, want: true},
		{
			name: "signed-out account holder needing re-login",
			tokens: &auth.Tokens{
				Email:         "dev@example.test",
				NeedsRelogin:  true,
				ReloginReason: auth.ReloginReasonRefreshTokenRejected,
			},
			want: false,
		},
		{
			name: "logged in",
			tokens: &auth.Tokens{
				AccessToken:  "access",
				RefreshToken: "refresh",
				Email:        "dev@example.test",
				ExpiresAt:    time.Now().Add(time.Hour),
			},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr := auth.NewManager(&repoAddTokenStore{tokens: tc.tokens}, auth.Config{})
			if got := shouldShowCloudSettings(mgr); got != tc.want {
				t.Fatalf("shouldShowCloudSettings = %v, want %v", got, tc.want)
			}
		})
	}

	if shouldShowCloudSettings() {
		t.Fatal("shouldShowCloudSettings() with no manager = true, want false")
	}
	if shouldShowCloudSettings(nil) {
		t.Fatal("shouldShowCloudSettings(nil) = true, want false")
	}
}
