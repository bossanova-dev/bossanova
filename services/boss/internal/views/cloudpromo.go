package views

import (
	"time"

	"charm.land/lipgloss/v2"
	"github.com/recurser/bossalib/config"
)

const (
	cloudGuestOfferSessionLimit = time.Minute
	cloudGuestOfferInstallLimit = 72 * time.Hour
)

func cloudGuestOfferVisible(settings config.Settings, now, sessionStartedAt time.Time, loggedIn bool, authConfigured bool) bool {
	if loggedIn || !authConfigured {
		return false
	}
	if settings.BossCloudGuestOfferHidden {
		return false
	}
	if !settings.InstalledAt.IsZero() && !now.Before(settings.InstalledAt.Add(cloudGuestOfferInstallLimit)) {
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
