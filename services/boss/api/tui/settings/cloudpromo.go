package settings

import (
	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/api/tui/ui"
)

func cloudSettingsBlock() string {
	padding := lipgloss.NewStyle().Padding(0, 2)
	heading := padding.Bold(true).Render("Bossanova Cloud")
	copy := padding.Foreground(ui.ColorMuted).Render(
		"optional: local mode stays free. press [l]ogin from Home to start a 14-day free trial.",
	)

	return heading + "\n" + copy
}
