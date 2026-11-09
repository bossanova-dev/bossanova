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
			if len(m.cloudControlTargets) > 0 {
				return m.saveRepoCloudControl("full")
			}
			pending := m.cloudControlPending
			m.cloudControlPending = ""
			m = m.persistSettingsWithRollback(func(settings *config.Settings) error {
				settings.CloudControl = pending
				return nil
			})
		case "n", "N", "esc":
			m.confirmCloudControl = false
			m.cloudControlTargets = nil
			m.cloudControlPending = ""
		}
	}
	return m
}

func (m GeneralSettingsModel) cycleCloudControl(direction int) GeneralSettingsModel {
	row := m.rows[m.cursor]
	current := m.settings.CloudControl
	if current == "" {
		current = "default"
	}
	next := row.Allowed[0]
	for i, value := range row.Allowed {
		if value == current {
			next = row.Allowed[(i+direction+len(row.Allowed))%len(row.Allowed)]
			break
		}
	}
	if next == "default" {
		next = ""
	}
	// The default is full, so choosing it needs the same confirmation as full.
	if config.ResolveCloudControl(next) == config.CloudControlFull {
		m.confirmCloudControl = true
		m.cloudControlTargets = nil
		m.cloudControlPending = next
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
	return row.Kind == settingsRowKindRepoCloud || row.Kind == settingsRowKindOwnerCloud || row.Kind == settingsRowKindEnum && row.Plugin == "" && row.Key == "cloud_control"
}

func (m GeneralSettingsModel) cycleCloudControlArrow(key string) GeneralSettingsModel {
	if !m.cloudControlSelected() {
		return m
	}
	direction := 1
	if key == "left" {
		direction = -1
	}
	if m.rows[m.cursor].Kind == settingsRowKindRepoCloud || m.rows[m.cursor].Kind == settingsRowKindOwnerCloud {
		return m.cycleRepoCloudControl(direction)
	}
	return m.cycleCloudControl(direction)
}
