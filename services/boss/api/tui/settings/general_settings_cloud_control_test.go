package settings

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/bossalib/kit/config"
)

func cloudControlModel(t *testing.T, raw string) GeneralSettingsModel {
	t.Helper()
	withTempConfigHome(t)
	if err := config.Save(config.Settings{CloudControl: raw, DaemonName: "my-machine"}); err != nil {
		t.Fatal(err)
	}
	m := NewGeneralSettingsModel(nil, context.Background(), nil)
	for i, row := range m.rows {
		if row.Kind == settingsRowKindEnum && row.Key == "cloud_control" {
			m.cursor = i
			return m
		}
	}
	t.Fatal("cloud control row missing")
	return m
}

func cloudControlKey(m GeneralSettingsModel, key tea.KeyPressMsg) GeneralSettingsModel {
	updated, _ := m.Update(key)
	return updated.(GeneralSettingsModel)
}

func assertCloudControlSaved(t *testing.T, m GeneralSettingsModel, want string) {
	t.Helper()
	if m.settings.CloudControl != want {
		t.Fatalf("in-memory mode = %q, want %q", m.settings.CloudControl, want)
	}
	saved, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if saved.CloudControl != want || saved.DaemonName != "my-machine" {
		t.Fatalf("saved mode/name = %q/%q", saved.CloudControl, saved.DaemonName)
	}
}

func Test_Settings_AC210502_CloudControlConfirm(t *testing.T) {
	for _, key := range []tea.KeyPressMsg{{Code: 'y', Text: "y"}, {Code: 'Y', Text: "Y"}, {Code: tea.KeyEnter}} {
		t.Run(key.String(), func(t *testing.T) {
			m := cloudControlModel(t, "read-only")
			m = cloudControlKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
			assertCloudControlSaved(t, m, "read-only")
			if !strings.Contains(m.View().Content, "Anyone who can act as you in Boss Cloud can then drive agents on this machine.") {
				t.Fatal("enable confirmation warning missing")
			}
			m = cloudControlKey(m, key)
			assertCloudControlSaved(t, m, "full")
			if !strings.Contains(m.View().Content, "full") {
				t.Fatal("full selector value missing")
			}
		})
	}
}

func TestGeneralSettings_CloudControlCancel(t *testing.T) {
	for _, key := range []tea.KeyPressMsg{{Code: 'n', Text: "n"}, {Code: 'N', Text: "N"}, {Code: tea.KeyEscape}} {
		t.Run(key.String(), func(t *testing.T) {
			m := cloudControlModel(t, "read-only")
			settingsPath, err := config.Path()
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(settingsPath)
			if err != nil {
				t.Fatal(err)
			}
			m = cloudControlKey(m, tea.KeyPressMsg{Code: ' ', Text: " "})
			m = cloudControlKey(m, key)
			after, err := os.ReadFile(settingsPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("cancel changed the settings file")
			}
			assertCloudControlSaved(t, m, "read-only")
			if m.Cancelled() || !strings.Contains(m.View().Content, "read-only") {
				t.Fatal("cancel did not return to read-only settings row")
			}
		})
	}
}

func TestGeneralSettings_CloudControlDefaultIsFullAndConfirms(t *testing.T) {
	m := cloudControlModel(t, "full")
	m = cloudControlKey(m, tea.KeyPressMsg{Code: ' ', Text: " "})
	assertCloudControlSaved(t, m, "full")
	if !m.confirmCloudControl {
		t.Fatal("choosing the full default did not request confirmation")
	}
	m = cloudControlKey(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	assertCloudControlSaved(t, m, "full")
	m = cloudControlKey(m, tea.KeyPressMsg{Code: ' ', Text: " "})
	m = cloudControlKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	assertCloudControlSaved(t, m, "")
	if m.confirmCloudControl || !strings.Contains(m.View().Content, "full (default)") {
		t.Fatal("confirmed default did not return to the full default row")
	}
}

func TestGeneralSettings_CloudControlResolvedValue(t *testing.T) {
	for _, raw := range []string{"", "unrecognized", " full ", "private"} {
		t.Run(raw, func(t *testing.T) {
			m := cloudControlModel(t, raw)
			want := string(config.ResolveCloudControl(raw))
			if !strings.Contains(m.View().Content, want) {
				t.Fatalf("resolved mode missing: %s", want)
			}
		})
	}
}

func TestGeneralSettings_CloudControlSaveRollback(t *testing.T) {
	for _, raw := range []string{"private", "read-only", "full"} {
		t.Run(raw, func(t *testing.T) {
			m := cloudControlModel(t, raw)
			badPath := filepath.Join(t.TempDir(), "settings-dir")
			if err := os.Mkdir(badPath, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("BOSS_SETTINGS_PATH", badPath)
			m = cloudControlKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
			if raw == "read-only" || raw == "full" {
				m = cloudControlKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
			}
			if m.err == nil || m.settings.CloudControl != raw {
				t.Fatalf("failed save did not roll back: mode=%q err=%v", m.settings.CloudControl, m.err)
			}
			t.Setenv("BOSS_SETTINGS_PATH", filepath.Join(t.TempDir(), "settings.json"))
			for i, row := range m.rows {
				if row.Kind == settingsRowKindNotifications {
					m.cursor = i
					break
				}
			}
			m = cloudControlKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
			assertCloudControlSaved(t, m, raw)
			if m.err != nil {
				t.Fatalf("successful save kept error: %v", m.err)
			}
		})
	}
}

func Test_Settings_AC218602_CloudControlNarrow(t *testing.T) {
	m := cloudControlModel(t, "read-only")
	m = cloudControlKey(m, tea.KeyPressMsg{Code: tea.KeyLeft})
	assertCloudControlSaved(t, m, "private")
	if m.confirmCloudControl {
		t.Fatal("narrowing requested confirmation")
	}
	m = cloudControlKey(m, tea.KeyPressMsg{Code: tea.KeyRight})
	assertCloudControlSaved(t, m, "read-only")
	if m.confirmCloudControl {
		t.Fatal("read-only requested confirmation")
	}
}

func TestGeneralSettings_CloudControlEnumOrder(t *testing.T) {
	m := cloudControlModel(t, "private")
	if !reflect.DeepEqual(m.rows[m.cursor].Allowed, []string{"default", "private", "read-only", "full"}) {
		t.Fatalf("cloud control choices = %v", m.rows[m.cursor].Allowed)
	}
	m = cloudControlKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	assertCloudControlSaved(t, m, "read-only")
	m = cloudControlKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	assertCloudControlSaved(t, m, "read-only")
	if !m.confirmCloudControl {
		t.Fatal("full did not request confirmation")
	}
	m = cloudControlKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	assertCloudControlSaved(t, m, "full")
	m = cloudControlKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	assertCloudControlSaved(t, m, "full")
	if !m.confirmCloudControl {
		t.Fatal("default did not request confirmation")
	}
	m = cloudControlKey(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	assertCloudControlSaved(t, m, "")
}
