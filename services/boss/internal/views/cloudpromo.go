package views

import "charm.land/lipgloss/v2"

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
