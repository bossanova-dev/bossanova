package telemetry

import (
	"context"
	"testing"

	"github.com/recurser/bossalib/config"
)

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
		"file":          "secret.txt",
		"file_path":     "/Users/dave/private.txt",
		"nested":        map[string]any{"prompt": "secret"},
		"repo":          "bossanova",
		"repo_path":     "/Users/dave/repo",
		"transcript":    "secret transcript",
		"worktree_path": "/Users/dave/worktree",
		"ok":            true,
	})

	for _, key := range []string{"args", "branch", "branch_name", "file", "file_path", "nested", "repo", "repo_path", "transcript", "worktree_path"} {
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

func TestNoopClientDoesNotError(t *testing.T) {
	client := New(Config{})
	client.Identify(context.Background(), "user_1", map[string]any{"email": "a@example.com"})
	client.Capture(context.Background(), EventCLICommandInvoked, "user_1", map[string]any{"command": "boss"})
	client.Close()
}
