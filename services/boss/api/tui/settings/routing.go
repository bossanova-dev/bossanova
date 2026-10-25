package settings

import (
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"
)

func (m *Model) SetWidth(width int)                      { m.width = width }
func (m Model) Width() int                               { return m.width }
func (m Model) SelectedKey() string                      { return m.selectedKey() }
func (m *Model) RestoreSection(key string)               { m.restoreSection(key) }
func (m *GeneralSettingsModel) SetWidth(width int)       { m.width = width }
func (m GeneralSettingsModel) Width() int                { return m.width }
func (m GeneralSettingsModel) TextEntryActive() bool     { return m.editingRow >= 0 }
func (m GeneralSettingsModel) Err() error                { return m.err }
func (m GeneralSettingsModel) Settings() config.Settings { return m.settings }
func (m *SessionSettingsModel) SetWidth(width int)       { m.width = width }
func (m SessionSettingsModel) Width() int                { return m.width }
func (m SessionSettingsModel) Session() *pb.Session      { return m.session }
func (m SessionSettingsModel) SessionID() string         { return m.sessionID }
