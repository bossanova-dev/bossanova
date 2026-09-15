package main

import (
	"context"
	"os"
	"path/filepath"
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

// TestTelemetryDistinctIDUsesSignedInEmail pins the CLI onto the funnel
// namespace. It previously asserted the retired telemetry.UserDistinctID form;
// that person is one no web or bosso event ever writes to, so a funnel spanning
// the CLI and the web returned zero completions.
func TestTelemetryDistinctIDUsesSignedInEmail(t *testing.T) {
	testEnv := enableCommandTelemetryForTest(t)
	writeAuthTokensForTest(t, "person@example.com")
	assertAuthTokensStoredInTestHome(t, testEnv)

	got := commandDistinctID()
	want := telemetry.FunnelDistinctID("", "person@example.com")
	if got != want {
		t.Fatalf("commandDistinctID() = %q, want %q", got, want)
	}
	if got == telemetry.UserDistinctID("person@example.com") {
		t.Fatalf("commandDistinctID() = %q, still the retired user-<hash> namespace", got)
	}
}

// TestCommandEventsShareTheTUIFunnelDistinctID pins the CLI half of the
// cross-surface parity claim. The TUI half is pinned against the same
// telemetry.LocalFunnelDistinctID expression in
// services/boss/internal/views/telemetry_test.go, so the two surfaces resolve to
// one PostHog person by construction. Package boundaries make a single test
// that calls both impossible.
func TestCommandEventsShareTheTUIFunnelDistinctID(t *testing.T) {
	enableCommandTelemetryForTest(t)
	rec := &fakeTelemetry{}
	original := commandTelemetryEmailLookup
	commandTelemetryEmailLookup = func() string { return "person@example.com" }
	t.Cleanup(func() { commandTelemetryEmailLookup = original })

	captureRepairStarted(context.Background(), rec)

	// The literal, not telemetry.LocalFunnelDistinctID(home, ...): see the TUI
	// half in services/boss/internal/views/telemetry_test.go. Deriving the
	// expectation from the production expression would make both sides move
	// together on a regression; the shared literal is what actually pins parity
	// across the package boundary.
	const want = "email:person@example.com"
	if len(rec.distinctIDs) != 1 || rec.distinctIDs[0] != want {
		t.Fatalf("captured distinctIDs = %#v, want [%q]", rec.distinctIDs, want)
	}
}

// TestCommandDistinctIDFallsBackToLocalWithoutAnEmail keeps an anonymous
// machine on a stable per-home identity rather than collapsing every logged-out
// machine into the shared "anonymous" person FunnelDistinctID would return.
func TestCommandDistinctIDFallsBackToLocalWithoutAnEmail(t *testing.T) {
	enableCommandTelemetryForTest(t)
	original := commandTelemetryEmailLookup
	commandTelemetryEmailLookup = func() string { return "" }
	t.Cleanup(func() { commandTelemetryEmailLookup = original })

	got := commandDistinctID()
	if want := localDistinctID(); got != want {
		t.Fatalf("commandDistinctID() = %q, want %q", got, want)
	}
	if got == "anonymous" {
		t.Fatalf("commandDistinctID() = %q, which merges every anonymous machine into one person", got)
	}
}

func TestIdentifySignedInUserSendsEmailProperty(t *testing.T) {
	testEnv := enableCommandTelemetryForTest(t)
	writeAuthTokensForTest(t, "person@example.com")
	assertAuthTokensStoredInTestHome(t, testEnv)
	rec := &fakeTelemetry{}

	identifyCommandUser(context.Background(), rec)

	if len(rec.identifies) != 1 {
		t.Fatalf("identifies = %d, want 1", len(rec.identifies))
	}
	wantDistinctID := telemetry.FunnelDistinctID("", "person@example.com")
	if got := rec.identifies[0].distinctID; got != wantDistinctID {
		t.Fatalf("identify distinctID = %q, want %q", got, wantDistinctID)
	}
	if got := rec.identifies[0].props["email"]; got != "person@example.com" {
		t.Fatalf("identify email = %v, want person@example.com", got)
	}
}

// TestIdentifyTargetsTheSamePersonEventsAreCapturedOn compares the two ids
// against each other rather than each against a literal: person properties
// written to a person no event touches are worse than none, and only a direct
// comparison catches the two drifting apart.
func TestIdentifyTargetsTheSamePersonEventsAreCapturedOn(t *testing.T) {
	enableCommandTelemetryForTest(t)
	rec := &fakeTelemetry{}
	original := commandTelemetryEmailLookup
	commandTelemetryEmailLookup = func() string { return "person@example.com" }
	t.Cleanup(func() { commandTelemetryEmailLookup = original })

	captureAuthChanged(context.Background(), rec, "login")

	if len(rec.identifies) != 1 || len(rec.distinctIDs) != 1 {
		t.Fatalf("identifies = %d, captures = %d, want 1 each", len(rec.identifies), len(rec.distinctIDs))
	}
	if rec.identifies[0].distinctID != rec.distinctIDs[0] {
		t.Fatalf("identify distinctID = %q, captured distinctID = %q, want the same person",
			rec.identifies[0].distinctID, rec.distinctIDs[0])
	}
	if want := telemetry.FunnelDistinctID("", "person@example.com"); rec.identifies[0].distinctID != want {
		t.Fatalf("identify distinctID = %q, want %q", rec.identifies[0].distinctID, want)
	}
}

func TestCaptureAuthChangedAliasesLocalUserOnLogin(t *testing.T) {
	testEnv := enableCommandTelemetryForTest(t)
	writeAuthTokensForTest(t, "person@example.com")
	assertAuthTokensStoredInTestHome(t, testEnv)
	rec := &fakeTelemetry{}

	captureAuthChanged(context.Background(), rec, "login")

	if len(rec.aliases) != 1 {
		t.Fatalf("aliases = %d, want 1", len(rec.aliases))
	}
	// The bridge, retargeted: it still starts at this machine's pre-login
	// identity, but now lands inside the funnel namespace instead of the
	// retired user-<hash> one. Nothing aliases FROM a pre-change user-<hash>
	// person — the migration is forward-only.
	want := [2]string{telemetry.LocalDistinctID(homeDirForTest(t)), telemetry.FunnelDistinctID("", "person@example.com")}
	if rec.aliases[0] != want {
		t.Fatalf("alias = %#v, want %#v", rec.aliases[0], want)
	}
	if rec.aliases[0][1] == telemetry.UserDistinctID("person@example.com") {
		t.Fatalf("alias target = %q, still the retired user-<hash> namespace", rec.aliases[0][1])
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
	wantDistinctID := telemetry.FunnelDistinctID("", "person@example.com")
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

	wantDistinctID := telemetry.FunnelDistinctID("", "person@example.com")
	if len(rec.distinctIDs) != 1 {
		t.Fatalf("distinctIDs = %d, want 1", len(rec.distinctIDs))
	}
	if rec.distinctIDs[0] != wantDistinctID {
		t.Fatalf("logout distinctID = %q, want %q", rec.distinctIDs[0], wantDistinctID)
	}
}

type commandTelemetryTestEnv struct {
	home         string
	originalHome string
}

func enableCommandTelemetryForTest(t *testing.T) commandTelemetryTestEnv {
	t.Helper()
	originalHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir: %v", err)
	}
	testHome := t.TempDir()
	t.Setenv("HOME", testHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(testHome, ".config"))
	_, cleanup := setupTestConfigEnv(t)
	t.Cleanup(cleanup)
	t.Setenv("BOSS_KEYRING_BACKEND", "file")
	settings := config.DefaultSettings()
	settings.EventTracingEnabled = true
	if err := config.Save(settings); err != nil {
		t.Fatalf("config.Save: %v", err)
	}
	return commandTelemetryTestEnv{home: testHome, originalHome: originalHome}
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

func assertAuthTokensStoredInTestHome(t *testing.T, testEnv commandTelemetryTestEnv) {
	t.Helper()
	// Current auth writers persist only the authoritative versioned record;
	// workos-tokens remains a migration-read key for older binaries.
	tokenPath := filepath.Join(testEnv.home, ".config", "bossanova", "keyring", "workos-tokens-v1")
	if _, err := os.Stat(tokenPath); err != nil {
		t.Fatalf("auth tokens file = %q, stat: %v", tokenPath, err)
	}
	originalTokenPath := filepath.Join(testEnv.originalHome, ".config", "bossanova", "keyring", "workos-tokens-v1")
	if filepath.Clean(tokenPath) == filepath.Clean(originalTokenPath) {
		t.Fatalf("auth tokens file = %q, want path outside caller home %q", tokenPath, testEnv.originalHome)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir: %v", err)
	}
	if home != testEnv.home {
		t.Fatalf("os.UserHomeDir() = %q, want test home %q", home, testEnv.home)
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

func homeDirForTest(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir: %v", err)
	}
	return home
}
