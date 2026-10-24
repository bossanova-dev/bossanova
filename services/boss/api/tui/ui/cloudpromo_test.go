package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossalib/config"
)

func TestCloudDiscoveryLine(t *testing.T) {
	line := CloudDiscoveryLine(false, true)
	for _, want := range []string{
		"Bossanova Cloud",
		"[l]ogin to try Bossanova Cloud for free",
		"realtime GitHub events",
		"web remote control",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("CloudDiscoveryLine(false, true) = %q, want %q", line, want)
		}
	}

	if got := CloudDiscoveryLine(true, true); got != "" {
		t.Fatalf("CloudDiscoveryLine(true, true) = %q, want empty", got)
	}

	if got := CloudDiscoveryLine(false, false); got != "" {
		t.Fatalf("CloudDiscoveryLine(false, false) = %q, want empty", got)
	}
}

func TestCloudGuestOfferVisiblePolicy(t *testing.T) {
	installedAt := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	sessionStartedAt := installedAt.Add(10 * time.Second)
	now := sessionStartedAt.Add(30 * time.Second)
	settings := config.DefaultSettings()
	settings.InstalledAt = installedAt
	settings.BossCloudValueDeliveredAt = installedAt

	if !CloudGuestOfferVisible(settings, now, sessionStartedAt, false, true) {
		t.Fatal("CloudGuestOfferVisible = false, want true for eligible guest")
	}
}

func TestCloudGuestOfferHiddenByManualSetting(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	settings := config.DefaultSettings()
	settings.InstalledAt = now
	settings.BossCloudValueDeliveredAt = now
	settings.BossCloudGuestOfferHidden = true

	if CloudGuestOfferVisible(settings, now, now, false, true) {
		t.Fatal("CloudGuestOfferVisible = true, want false when manually hidden")
	}
}

func TestCloudGuestOfferHiddenAfterSessionLimit(t *testing.T) {
	installedAt := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	sessionStartedAt := installedAt
	now := sessionStartedAt.Add(CloudGuestOfferSessionLimit + time.Second)
	settings := config.DefaultSettings()
	settings.InstalledAt = installedAt
	settings.BossCloudValueDeliveredAt = installedAt

	if CloudGuestOfferVisible(settings, now, sessionStartedAt, false, true) {
		t.Fatal("CloudGuestOfferVisible = true, want false after session limit")
	}
}

func TestCloudGuestOfferHiddenAfterValueWindow(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	settings := config.DefaultSettings()
	// Anchor the window to value-delivered, not install time.
	settings.BossCloudValueDeliveredAt = now.Add(-CloudGuestOfferValueWindow)

	if CloudGuestOfferVisible(settings, now, now, false, true) {
		t.Fatal("CloudGuestOfferVisible = true, want false after value window")
	}
}

func TestCloudGuestOfferValueWindowIsExactlyThreeDays(t *testing.T) {
	// Keep the product promise independent from the implementation constant so a
	// changed multiplier cannot move the offer window without detection.
	if got, want := CloudGuestOfferValueWindow, 72*time.Hour; got != want {
		t.Fatalf("CloudGuestOfferValueWindow = %s, want %s", got, want)
	}
}

func TestCloudGuestOfferHiddenWhenLoggedInOrAuthUnavailable(t *testing.T) {
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	settings := config.DefaultSettings()
	settings.InstalledAt = now
	settings.BossCloudValueDeliveredAt = now

	if CloudGuestOfferVisible(settings, now, now, true, true) {
		t.Fatal("CloudGuestOfferVisible = true, want false for logged-in user")
	}
	if CloudGuestOfferVisible(settings, now, now, false, false) {
		t.Fatal("CloudGuestOfferVisible = true, want false without auth configured")
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
	if !CloudGuestOfferVisible(settings, now, sessionStartedAt, false, true) {
		t.Fatal("CloudGuestOfferVisible = false, want true for eligible guest (baseline)")
	}

	// Suppressed when ProofHideGuestOffer is set.
	defer func(prev bool) { ProofHideGuestOffer = prev }(ProofHideGuestOffer)
	ProofHideGuestOffer = true
	if CloudGuestOfferVisible(settings, now, sessionStartedAt, false, true) {
		t.Fatal("CloudGuestOfferVisible = true, want false when ProofHideGuestOffer is set")
	}
}

// TestCloudGuestOfferVisible is the canonical table-driven test for
// CloudGuestOfferVisible. It covers the value-delivered gate and the 72h
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
				prev := ProofHideGuestOffer
				ProofHideGuestOffer = true
				defer func() { ProofHideGuestOffer = prev }()
			}

			// Pass zero sessionStartedAt so the session-limit gate never fires.
			got := CloudGuestOfferVisible(settings, tc.now, time.Time{}, tc.loggedIn, tc.authConfigured)
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
