package settings

import (
	tea "charm.land/bubbletea/v2"
	"github.com/recurser/bossalib/kit/config"
)

func (m GeneralSettingsModel) updateCloudControlConfirm(msg tea.Msg) GeneralSettingsModel {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "y", "Y", "enter":
			m.confirmCloudControl = false
			m = m.persistSettingsWithRollback(func(settings *config.Settings) error {
				settings.CloudControl = string(config.CloudControlFull)
				return nil
			})
		case "n", "N", "esc":
			m.confirmCloudControl = false
		}
	}
	return m
}

func (m GeneralSettingsModel) cycleCloudControl(direction int) GeneralSettingsModel {
	row := m.rows[m.cursor]
	current := string(config.ResolveCloudControl(m.settings.CloudControl))
	next := row.Allowed[0]
	for i, value := range row.Allowed {
		if value == current {
			next = row.Allowed[(i+direction+len(row.Allowed))%len(row.Allowed)]
			break
		}
	}
	if next == string(config.CloudControlFull) {
		m.confirmCloudControl = true
		return m
	}
	return m.persistSettingsWithRollback(func(settings *config.Settings) error {
		settings.CloudControl = next
		return nil
	})
}

func (m GeneralSettingsModel) cloudControlSelected() bool {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return false
	}
	row := m.rows[m.cursor]
	return row.Kind == settingsRowKindEnum && row.Plugin == "" && row.Key == "cloud_control"
}

func (m GeneralSettingsModel) cycleCloudControlArrow(key string) GeneralSettingsModel {
	if !m.cloudControlSelected() {
		return m
	}
	direction := 1
	if key == "left" {
		direction = -1
	}
	return m.cycleCloudControl(direction)
}
