package cron

import (
	"context"
	"path/filepath"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/api/tui/ui"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"
	"github.com/recurser/bossalib/kit/telemetry"
)

type fakeTelemetry struct {
	events      []telemetry.Event
	distinctIDs []string
	props       []map[string]any
}

func (f *fakeTelemetry) Capture(_ context.Context, event telemetry.Event, distinctID string, props map[string]any) {
	f.events = append(f.events, event)
	f.distinctIDs = append(f.distinctIDs, distinctID)
	f.props = append(f.props, props)
}

func (f *fakeTelemetry) Identify(context.Context, string, map[string]any) {}
func (f *fakeTelemetry) Alias(context.Context, string, string)            {}
func (f *fakeTelemetry) Close()                                           {}

func enableViewTelemetryForTest(t *testing.T) {
	t.Helper()
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tempHome, ".config"))
	t.Setenv("BOSS_SETTINGS_PATH", filepath.Join(tempHome, "settings.json"))
	t.Setenv("BOSS_KEYRING_BACKEND", "file")
	settings := config.DefaultSettings()
	settings.EventTracingEnabled = true
	if err := config.Save(settings); err != nil {
		t.Fatal(err)
	}
	ui.ResetTelemetryCache()
	t.Cleanup(ui.ResetTelemetryCache)
}

func newCronListForTelemetry(client telemetry.Client) ListModel {
	m := NewListModel(nil, context.Background())
	m.SetTelemetry(client)
	return m
}
func TestTelemetryTUIActionCancelledConfirmationEmitsNothing(t *testing.T) {
	cancelKeys := []tea.KeyPressMsg{
		{Code: 'n', Text: "n"},
		{Code: tea.KeyEscape},
	}
	// Typed as tea.Cmd: the signature is fixed by the prompt, so its always-nil
	// result is not a parameter to prune.
	var neverRun tea.Cmd = func() tea.Msg {
		t.Fatal("the confirm action ran on a cancel keystroke")
		// Unreachable after t.Fatal. A non-constant result keeps unparam from
		// reporting "always nil", which it does on the CI linux runner only.
		return t.Name()
	}

	t.Run("cron_delete", func(t *testing.T) {
		for _, key := range cancelKeys {
			enableViewTelemetryForTest(t)
			rec := &fakeTelemetry{}
			m := newCronListForTelemetry(rec)
			m.jobs = []*pb.CronJob{{Id: "job-1"}}
			m.rebuildTable()
			m.confirm = ui.NewConfirmPrompt("delete?", neverRun)

			model, cmd := m.Update(key)

			if cmd != nil {
				t.Fatalf("key %v returned a command; a cancelled confirmation must fire no RPC", key)
			}
			if model.(ListModel).confirm.Active {
				t.Fatalf("key %v left the prompt open; the cancel branch never ran", key)
			}
			if len(rec.events) != 0 {
				t.Fatalf("cancelled confirmation emitted %d events, want 0: %v", len(rec.events), rec.props)
			}
		}
	})
}

func TestTelemetryTUIActionConfirmKeypressEmitsNothing(t *testing.T) {
	confirmKeys := []tea.KeyPressMsg{
		{Code: 'y', Text: "y"},
		{Code: tea.KeyEnter},
	}
	// A command that resolves to nothing: these tests never run it, and its only
	// job is to make confirmPrompt return a non-nil cmd.
	noopAction := func() tea.Msg { return nil }

	t.Run("cron_delete", func(t *testing.T) {
		for _, key := range confirmKeys {
			enableViewTelemetryForTest(t)
			rec := &fakeTelemetry{}
			m := newCronListForTelemetry(rec)
			// selectedJob() must resolve for the confirmed branch to run.
			m.jobs = []*pb.CronJob{{Id: "job-1"}}
			m.rebuildTable()
			m.confirm = ui.NewConfirmPrompt("delete?", noopAction)

			model, _ := m.Update(key)

			if !model.(ListModel).deleting["job-1"] {
				t.Fatalf("key %v did not reach the confirmed branch; the assertion below would be vacuous", key)
			}
			if len(rec.events) != 0 {
				t.Fatalf("confirm keypress emitted %d events, want 0: %v", len(rec.events), rec.props)
			}
		}
	})
}
