package main

import (
	"context"
	"testing"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/telemetry"
	"github.com/spf13/cobra"
)

type fakeTelemetry struct {
	events []telemetry.Event
	props  []map[string]any
}

func (f *fakeTelemetry) Capture(_ context.Context, event telemetry.Event, _ string, props map[string]any) {
	f.events = append(f.events, event)
	f.props = append(f.props, props)
}

func (f *fakeTelemetry) Identify(context.Context, string, map[string]any) {}
func (f *fakeTelemetry) Close()                                           {}

func TestCommandTelemetryConfigDisabledByDefault(t *testing.T) {
	cfg := commandTelemetryConfig(config.DefaultSettings())
	if cfg.Enabled {
		t.Fatal("commandTelemetryConfig(config.DefaultSettings()).Enabled = true, want false")
	}
}

func TestCommandTelemetryPropertiesExcludeArgs(t *testing.T) {
	props := commandTelemetryProperties("boss session create", []string{"secret"})
	if _, ok := props["args"]; ok {
		t.Fatal("commandTelemetryProperties included args")
	}
	if got := props["command"]; got != "boss session create" {
		t.Fatalf("commandTelemetryProperties command = %v, want %q", got, "boss session create")
	}
}

func TestCommandTelemetryUsesSharedDefaults(t *testing.T) {
	settings := config.DefaultSettings()
	settings.EventTracingEnabled = true

	cfg := commandTelemetryConfig(settings)
	if cfg.ProjectToken != telemetry.ProductionProjectToken {
		t.Fatalf("commandTelemetryConfig ProjectToken = %q, want %q", cfg.ProjectToken, telemetry.ProductionProjectToken)
	}
}

func TestCaptureCommandSuppressesDisabledSettings(t *testing.T) {
	_, cleanup := setupTestConfigEnv(t)
	defer cleanup()
	rec := &fakeTelemetry{}
	cmd := &cobra.Command{Use: "login"}

	captureCommand(context.Background(), rec, cmd, nil)

	if len(rec.events) != 0 {
		t.Fatalf("events = %d, want 0", len(rec.events))
	}
}

func TestCaptureAuthChangedExcludesSensitiveProps(t *testing.T) {
	enableCommandTelemetryForTest(t)
	rec := &fakeTelemetry{}

	captureAuthChanged(context.Background(), rec, "login")
	captureAuthChanged(context.Background(), rec, "logout")

	if len(rec.events) != 2 {
		t.Fatalf("events = %d, want 2", len(rec.events))
	}
	for i, action := range []string{"login", "logout"} {
		if rec.events[i] != telemetry.EventAuthChanged {
			t.Fatalf("event[%d] = %q, want %q", i, rec.events[i], telemetry.EventAuthChanged)
		}
		if got := rec.props[i]["action"]; got != action {
			t.Fatalf("action[%d] = %v, want %s", i, got, action)
		}
		assertCommandTelemetryNoSensitiveProps(t, rec.props[i])
	}
}

func TestCaptureAuthChangedSuppressesDisabledSettings(t *testing.T) {
	_, cleanup := setupTestConfigEnv(t)
	defer cleanup()
	rec := &fakeTelemetry{}

	captureAuthChanged(context.Background(), rec, "login")

	if len(rec.events) != 0 {
		t.Fatalf("events = %d, want 0", len(rec.events))
	}
}

func TestCaptureRepairStartedAndCompletedExcludesSensitiveProps(t *testing.T) {
	enableCommandTelemetryForTest(t)
	rec := &fakeTelemetry{}

	captureRepairStarted(context.Background(), rec)
	captureRepairCompleted(context.Background(), rec, "success")
	captureRepairCompleted(context.Background(), rec, "error")

	if len(rec.events) != 3 {
		t.Fatalf("events = %d, want 3", len(rec.events))
	}
	if rec.events[0] != telemetry.EventRepairStarted {
		t.Fatalf("event[0] = %q, want %q", rec.events[0], telemetry.EventRepairStarted)
	}
	for i, status := range []string{"success", "error"} {
		idx := i + 1
		if rec.events[idx] != telemetry.EventRepairCompleted {
			t.Fatalf("event[%d] = %q, want %q", idx, rec.events[idx], telemetry.EventRepairCompleted)
		}
		if got := rec.props[idx]["status"]; got != status {
			t.Fatalf("status[%d] = %v, want %s", idx, got, status)
		}
	}
	for _, props := range rec.props {
		assertCommandTelemetryNoSensitiveProps(t, props)
	}
}

func enableCommandTelemetryForTest(t *testing.T) {
	t.Helper()
	_, cleanup := setupTestConfigEnv(t)
	t.Cleanup(cleanup)
	settings := config.DefaultSettings()
	settings.EventTracingEnabled = true
	if err := config.Save(settings); err != nil {
		t.Fatalf("config.Save: %v", err)
	}
}

func assertCommandTelemetryNoSensitiveProps(t *testing.T, props map[string]any) {
	t.Helper()
	for _, key := range []string{"args", "prompt", "transcript", "repo_path", "branch", "path", "file_path", "comment", "email"} {
		if _, ok := props[key]; ok {
			t.Fatalf("sensitive prop %q present in %v", key, props)
		}
	}
}
