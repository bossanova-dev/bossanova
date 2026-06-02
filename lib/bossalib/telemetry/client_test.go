package telemetry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/posthog/posthog-go"
	"github.com/recurser/bossalib/config"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// fakePostHogClient is a minimal posthog.Client used to drive postHogClient.Close.
type fakePostHogClient struct {
	closeErr error
}

func (f *fakePostHogClient) Close() error                           { return f.closeErr }
func (f *fakePostHogClient) CloseWithContext(context.Context) error { return f.closeErr }
func (f *fakePostHogClient) Enqueue(posthog.Message) error          { return nil }
func (f *fakePostHogClient) IsFeatureEnabled(posthog.FeatureFlagPayload) (interface{}, error) {
	return false, nil
}
func (f *fakePostHogClient) GetFeatureFlag(posthog.FeatureFlagPayload) (interface{}, error) {
	return false, nil
}
func (f *fakePostHogClient) GetFeatureFlagResult(posthog.FeatureFlagPayload) (*posthog.FeatureFlagResult, error) {
	return nil, nil
}
func (f *fakePostHogClient) GetFeatureFlagPayload(posthog.FeatureFlagPayload) (string, error) {
	return "", nil
}
func (f *fakePostHogClient) GetRemoteConfigPayload(string) (string, error) { return "", nil }
func (f *fakePostHogClient) GetAllFlags(posthog.FeatureFlagPayloadNoKey) (map[string]interface{}, error) {
	return nil, nil
}
func (f *fakePostHogClient) EvaluateFlags(posthog.EvaluateFlagsPayload) (*posthog.FeatureFlagEvaluations, error) {
	return nil, nil
}
func (f *fakePostHogClient) ReloadFeatureFlags() error                       { return nil }
func (f *fakePostHogClient) GetFeatureFlags() ([]posthog.FeatureFlag, error) { return nil, nil }

func TestPostHogClientCloseLogsOnlyOnError(t *testing.T) {
	captureLog := func(closeErr error) string {
		var buf bytes.Buffer
		previous := log.Logger
		log.Logger = zerolog.New(&buf)
		t.Cleanup(func() { log.Logger = previous })

		c := &postHogClient{inner: &fakePostHogClient{closeErr: closeErr}}
		c.Close()
		return buf.String()
	}

	// Real code logs only when Close returns an error. The mutated condition
	// (err == nil) would invert this, so each branch must be observably distinct.
	if got := captureLog(errors.New("close boom")); !strings.Contains(got, "posthog close failed") {
		t.Fatalf("Close with error should log warning, got %q", got)
	}
	if got := captureLog(nil); strings.Contains(got, "posthog close failed") {
		t.Fatalf("Close without error should not log warning, got %q", got)
	}
}

func TestDefaultHostsUseFirstPartyDomains(t *testing.T) {
	if ProductionPostHogHost != "https://k.bossanova.dev" {
		t.Fatalf("ProductionPostHogHost = %q, want %q", ProductionPostHogHost, "https://k.bossanova.dev")
	}
	if StagingPostHogHost != "https://k-staging.bossanova.dev" {
		t.Fatalf("StagingPostHogHost = %q, want %q", StagingPostHogHost, "https://k-staging.bossanova.dev")
	}
	if DefaultHost != ProductionPostHogHost {
		t.Fatalf("DefaultHost = %q, want production host %q", DefaultHost, ProductionPostHogHost)
	}
}

func TestDistinctIDHelpersAreHyphenatedAndStable(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{name: "local", got: LocalDistinctID("home-value"), want: "local-" + stableHashForTest("home-value")[:16]},
		{name: "daemon", got: DaemonDistinctID("host-value"), want: "daemon-" + stableHashForTest("host-value")[:16]},
		{name: "user", got: UserDistinctID("  Test@Example.COM\t"), want: "user-" + stableHashForTest("test@example.com")[:16]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("distinct ID = %q, want %q", tc.got, tc.want)
			}
			if strings.Contains(tc.got, ":") {
				t.Fatalf("distinct ID %q contains colon", tc.got)
			}
		})
	}
}

func TestDistinctIDHelpersFallbackToUnknown(t *testing.T) {
	if got := LocalDistinctID(""); got != "local-unknown" {
		t.Fatalf("LocalDistinctID empty = %q, want local-unknown", got)
	}
	if got := DaemonDistinctID(""); got != "daemon-unknown" {
		t.Fatalf("DaemonDistinctID empty = %q, want daemon-unknown", got)
	}
	if got := UserDistinctID(""); got != "" {
		t.Fatalf("UserDistinctID empty = %q, want empty", got)
	}
}

func stableHashForTest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func TestFromSettingsDisabledByDefault(t *testing.T) {
	cfg := FromSettings(config.DefaultSettings(), "boss")
	if cfg.Enabled {
		t.Fatal("Enabled default = true, want false")
	}
}

func TestFromSettingsUsesUserOverride(t *testing.T) {
	s := config.DefaultSettings()
	s.EventTracingEnabled = true
	s.PostHogProjectToken = "phc_override"
	s.PostHogHost = "https://example.com"

	cfg := FromSettings(s, "boss")
	if !cfg.Enabled {
		t.Fatal("Enabled = false, want true")
	}
	if cfg.ProjectToken != "phc_override" {
		t.Fatalf("ProjectToken = %q", cfg.ProjectToken)
	}
	if cfg.Host != "https://example.com" {
		t.Fatalf("Host = %q", cfg.Host)
	}
}

func TestFromSettingsSeedsProductionDefaultsWhenEnabled(t *testing.T) {
	s := config.DefaultSettings()
	s.EventTracingEnabled = true

	cfg := FromSettings(s, "boss")
	if cfg.ProjectToken != ProductionProjectToken {
		t.Fatalf("ProjectToken = %q, want production token", cfg.ProjectToken)
	}
	if cfg.Host != DefaultHost {
		t.Fatalf("Host = %q, want %q", cfg.Host, DefaultHost)
	}
}

func TestAllowlistRejectsPollingEvents(t *testing.T) {
	if !IsAllowed(EventCLICommandInvoked) {
		t.Fatal("cli_command_invoked should be allowed")
	}
	if IsAllowed(Event("daemon_poll_tick")) {
		t.Fatal("daemon_poll_tick should be rejected")
	}
}

func TestFilterPropertiesDropsSensitiveValues(t *testing.T) {
	props := FilterProperties(map[string]any{
		"args":          []string{"secret"},
		"branch":        "main",
		"branch_name":   "feature",
		"command":       "session create",
		"email":         "person@example.com",
		"file":          "secret.txt",
		"file_path":     "/Users/dave/private.txt",
		"nested":        map[string]any{"prompt": "secret"},
		"repo":          "bossanova",
		"repo_path":     "/Users/dave/repo",
		"transcript":    "secret transcript",
		"worktree_path": "/Users/dave/worktree",
		"ok":            true,
	})

	for _, key := range []string{"args", "branch", "branch_name", "email", "file", "file_path", "nested", "repo", "repo_path", "transcript", "worktree_path"} {
		if _, ok := props[key]; ok {
			t.Fatalf("%s should be dropped", key)
		}
	}
	if props["command"] != "session create" || props["ok"] != true {
		t.Fatalf("safe properties not preserved: %#v", props)
	}
}

func TestFilterPropertiesPreservesAllowedScalarKeys(t *testing.T) {
	props := FilterProperties(map[string]any{
		"action":            "login",
		"authenticated":     true,
		"command":           "boss login",
		"context_has_error": false,
		"ok":                true,
		"report_id":         "report_123",
		"resume":            false,
		"source":            "cli",
		"status":            "success",
	})

	for _, key := range []string{"action", "authenticated", "command", "context_has_error", "ok", "report_id", "resume", "source", "status"} {
		if _, ok := props[key]; !ok {
			t.Fatalf("%s should be preserved", key)
		}
	}
}

func TestFilterIdentifyPropertiesPreservesEmail(t *testing.T) {
	props := FilterIdentifyProperties(map[string]any{
		"email":     "person@example.com",
		"file_path": "/Users/dave/private.txt",
		"source":    "cli",
	})

	if props["email"] != "person@example.com" {
		t.Fatalf("email = %v, want person@example.com", props["email"])
	}
	if props["source"] != "cli" {
		t.Fatalf("source = %v, want cli", props["source"])
	}
	if _, ok := props["file_path"]; ok {
		t.Fatal("file_path should be dropped")
	}
}

func TestFilterPropertiesDropsUnsafeValues(t *testing.T) {
	props := FilterProperties(map[string]any{
		"action":            []string{"login"},
		"authenticated":     map[string]any{"value": true},
		"command":           nil,
		"context_has_error": struct{ value bool }{value: true},
		"ok":                true,
		"source":            "cli",
	})

	for _, key := range []string{"action", "authenticated", "command", "context_has_error"} {
		if _, ok := props[key]; ok {
			t.Fatalf("%s should be dropped", key)
		}
	}
	if props["ok"] != true || props["source"] != "cli" {
		t.Fatalf("safe scalar properties not preserved: %#v", props)
	}
}

func TestNewSelectsClientByEnabledAndToken(t *testing.T) {
	cases := []struct {
		name     string
		cfg      Config
		wantNoop bool
	}{
		{name: "disabled with token", cfg: Config{Enabled: false, ProjectToken: "phc_token"}, wantNoop: true},
		{name: "enabled without token", cfg: Config{Enabled: true, ProjectToken: ""}, wantNoop: true},
		{name: "disabled without token", cfg: Config{Enabled: false, ProjectToken: ""}, wantNoop: true},
		{name: "enabled with token", cfg: Config{Enabled: true, ProjectToken: "phc_token", Host: "https://example.com"}, wantNoop: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := New(tc.cfg)
			_, isNoop := client.(noopClient)
			if isNoop != tc.wantNoop {
				t.Fatalf("New(%+v) noop = %v, want %v (got %T)", tc.cfg, isNoop, tc.wantNoop, client)
			}
			if !tc.wantNoop {
				if _, ok := client.(*postHogClient); !ok {
					t.Fatalf("New(%+v) = %T, want *postHogClient", tc.cfg, client)
				}
				client.Close()
			}
		})
	}
}

func TestNoopClientDoesNotError(t *testing.T) {
	client := New(Config{})
	client.Identify(context.Background(), "user_1", map[string]any{"email": "a@example.com"})
	client.Capture(context.Background(), EventCLICommandInvoked, "user_1", map[string]any{"command": "boss"})
	client.Close()
}

func TestPostHogConfigUsesSharedLogger(t *testing.T) {
	cfg := postHogConfig("https://example.com")
	if cfg.Endpoint != "https://example.com" {
		t.Fatalf("Endpoint = %q", cfg.Endpoint)
	}
	if cfg.Logger == nil {
		t.Fatal("Logger = nil, want shared logger")
	}
}

func TestPostHogLoggerWritesThroughZerolog(t *testing.T) {
	var buf bytes.Buffer
	previous := log.Logger
	log.Logger = zerolog.New(&buf)
	t.Cleanup(func() { log.Logger = previous })

	postHogLogger{}.Warnf("sending request - %s", "timeout")

	got := buf.String()
	if !strings.Contains(got, `"component":"posthog"`) {
		t.Fatalf("log missing posthog component: %s", got)
	}
	if !strings.Contains(got, `"message":"sending request - timeout"`) {
		t.Fatalf("log missing formatted message: %s", got)
	}
}
