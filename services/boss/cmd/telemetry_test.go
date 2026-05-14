package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/telemetry"
	"github.com/spf13/cobra"
)

type fakeTelemetry struct {
	events      []telemetry.Event
	distinctIDs []string
	props       []map[string]any
	identifies  []struct {
		distinctID string
		props      map[string]any
	}
	aliases [][2]string
}

func (f *fakeTelemetry) Capture(_ context.Context, event telemetry.Event, distinctID string, props map[string]any) {
	f.events = append(f.events, event)
	f.distinctIDs = append(f.distinctIDs, distinctID)
	f.props = append(f.props, props)
}

func (f *fakeTelemetry) Identify(_ context.Context, distinctID string, props map[string]any) {
	f.identifies = append(f.identifies, struct {
		distinctID string
		props      map[string]any
	}{distinctID: distinctID, props: props})
}

func (f *fakeTelemetry) Alias(_ context.Context, alias, distinctID string) {
	f.aliases = append(f.aliases, [2]string{alias, distinctID})
}

func (f *fakeTelemetry) Close() {}

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

func TestLocalDistinctIDUsesHyphenatedSharedHelper(t *testing.T) {
	got := localDistinctID()
	if !strings.HasPrefix(got, "local-") {
		t.Fatalf("localDistinctID() = %q, want local- prefix", got)
	}
	if strings.Contains(got, ":") {
		t.Fatalf("localDistinctID() = %q, want no colon", got)
	}
}

func TestTelemetryDistinctIDUsesSignedInEmail(t *testing.T) {
	enableCommandTelemetryForTest(t)
	writeAuthTokensForTest(t, "person@example.com")

	got := commandDistinctID()
	want := telemetry.UserDistinctID("person@example.com")
	if got != want {
		t.Fatalf("commandDistinctID() = %q, want %q", got, want)
	}
}

func TestIdentifySignedInUserSendsEmailProperty(t *testing.T) {
	enableCommandTelemetryForTest(t)
	writeAuthTokensForTest(t, "person@example.com")
	rec := &fakeTelemetry{}

	identifyCommandUser(context.Background(), rec)

	if len(rec.identifies) != 1 {
		t.Fatalf("identifies = %d, want 1", len(rec.identifies))
	}
	wantDistinctID := telemetry.UserDistinctID("person@example.com")
	if got := rec.identifies[0].distinctID; got != wantDistinctID {
		t.Fatalf("identify distinctID = %q, want %q", got, wantDistinctID)
	}
	if got := rec.identifies[0].props["email"]; got != "person@example.com" {
		t.Fatalf("identify email = %v, want person@example.com", got)
	}
}

func TestCaptureAuthChangedAliasesLocalUserOnLogin(t *testing.T) {
	enableCommandTelemetryForTest(t)
	writeAuthTokensForTest(t, "person@example.com")
	rec := &fakeTelemetry{}

	captureAuthChanged(context.Background(), rec, "login")

	if len(rec.aliases) != 1 {
		t.Fatalf("aliases = %d, want 1", len(rec.aliases))
	}
	want := [2]string{localDistinctID(), telemetry.UserDistinctID("person@example.com")}
	if rec.aliases[0] != want {
		t.Fatalf("alias = %#v, want %#v", rec.aliases[0], want)
	}
}

func TestCaptureAuthChangedReadsSignedInEmailOnceOnLogin(t *testing.T) {
	enableCommandTelemetryForTest(t)
	rec := &fakeTelemetry{}
	calls := 0
	original := commandTelemetryEmailLookup
	commandTelemetryEmailLookup = func() string {
		calls++
		return "person@example.com"
	}
	t.Cleanup(func() { commandTelemetryEmailLookup = original })

	captureAuthChanged(context.Background(), rec, "login")

	if calls != 1 {
		t.Fatalf("commandTelemetryEmail calls = %d, want 1", calls)
	}
	wantDistinctID := telemetry.UserDistinctID("person@example.com")
	if len(rec.identifies) != 1 {
		t.Fatalf("identifies = %d, want 1", len(rec.identifies))
	}
	if len(rec.aliases) != 1 {
		t.Fatalf("aliases = %d, want 1", len(rec.aliases))
	}
	if len(rec.distinctIDs) != 1 || rec.distinctIDs[0] != wantDistinctID {
		t.Fatalf("capture distinctIDs = %#v, want [%q]", rec.distinctIDs, wantDistinctID)
	}
}

func TestCaptureAuthChangedWithEmailPreservesLogoutUserIdentity(t *testing.T) {
	enableCommandTelemetryForTest(t)
	rec := &fakeTelemetry{}

	captureAuthChangedWithEmail(context.Background(), rec, "logout", "person@example.com")

	wantDistinctID := telemetry.UserDistinctID("person@example.com")
	if len(rec.distinctIDs) != 1 {
		t.Fatalf("distinctIDs = %d, want 1", len(rec.distinctIDs))
	}
	if rec.distinctIDs[0] != wantDistinctID {
		t.Fatalf("logout distinctID = %q, want %q", rec.distinctIDs[0], wantDistinctID)
	}
}

func enableCommandTelemetryForTest(t *testing.T) {
	t.Helper()
	_, cleanup := setupTestConfigEnv(t)
	t.Cleanup(cleanup)
	t.Setenv("BOSS_KEYRING_BACKEND", "file")
	settings := config.DefaultSettings()
	settings.EventTracingEnabled = true
	if err := config.Save(settings); err != nil {
		t.Fatalf("config.Save: %v", err)
	}
}

func writeAuthTokensForTest(t *testing.T, email string) {
	t.Helper()
	store, err := auth.NewKeychainStore(true)
	if err != nil {
		t.Fatalf("new keychain store: %v", err)
	}
	if err := store.Save(&auth.Tokens{
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
		Email:        email,
		ExpiresAt:    time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("save tokens: %v", err)
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
