package views

import (
	"os"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/recurser/bossalib/config"
)

const (
	cloudGuestOfferSessionLimit = time.Minute
	// cloudGuestOfferValueWindow is the duration after the first value-delivery
	// event during which the guest cloud offer is shown. The window is anchored
	// to settings.BossCloudValueDeliveredAt, not the install time.
	cloudGuestOfferValueWindow = 72 * time.Hour
)

// proofHideGuestOffer suppresses the guest cloud offer during proof capture
// so screenshots aren't cluttered by the subscribe prompt. Read once at
// startup from BOSS_PROOF_HIDE_GUEST_OFFER; it hides the line only — it does
// not change cloud-gate copy or pretend the user is subscribed.
var proofHideGuestOffer = os.Getenv("BOSS_PROOF_HIDE_GUEST_OFFER") != ""

// cloudGuestOfferVisible returns true when the guest cloud offer should be
// shown to the user. The offer is gated on:
//   - A value-delivery event having occurred (BossCloudValueDeliveredAt set):
//     we only invite users to upgrade after they have seen what Bossanova
//     actually does for them.
//   - The offer window (cloudGuestOfferValueWindow) not yet having expired
//     since that value-delivery moment.
//   - The per-session timer (cloudGuestOfferSessionLimit) not yet having
//     elapsed, so the nag disappears quietly as the session progresses.
//   - The user not being logged in, auth being configured, and the offer not
//     having been manually hidden.
func cloudGuestOfferVisible(settings config.Settings, now, sessionStartedAt time.Time, loggedIn bool, authConfigured bool) bool {
	// No value delivered yet — never nag a user who has received nothing.
	if settings.BossCloudValueDeliveredAt.IsZero() {
		return false
	}
	if proofHideGuestOffer {
		return false
	}
	if loggedIn || !authConfigured {
		return false
	}
	if settings.BossCloudGuestOfferHidden {
		return false
	}
	// Window anchored to the value-delivery moment, not the install time.
	if !now.Before(settings.BossCloudValueDeliveredAt.Add(cloudGuestOfferValueWindow)) {
		return false
	}
	if !sessionStartedAt.IsZero() && !now.Before(sessionStartedAt.Add(cloudGuestOfferSessionLimit)) {
		return false
	}
	return true
}

func cloudDiscoveryLine(loggedIn bool, authConfigured bool) string {
	if loggedIn || !authConfigured {
		return ""
	}

	return styleActionBar.Render("[l]ogin to try Bossanova Cloud for free: realtime GitHub events and web remote control.")
}

func cloudSettingsBlock() string {
	padding := lipgloss.NewStyle().Padding(0, 2)
	heading := padding.Bold(true).Render("Bossanova Cloud")
	copy := padding.Foreground(colorMuted).Render(
		"optional: local mode stays free. press [l]ogin from Home to start a 7-day free trial.",
	)

	return heading + "\n" + copy
}
