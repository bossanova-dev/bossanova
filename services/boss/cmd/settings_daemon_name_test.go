package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/recurser/bossalib/config"
)

// seedDaemonNameSettings points config at a temp settings.json and writes
// defaults into it, matching the setup shape of TestSettingsRotationFlags.
func seedDaemonNameSettings(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	t.Setenv("BOSS_SETTINGS_PATH", path)
	if err := config.Save(config.DefaultSettings()); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	return path
}

// TestSettingsDaemonNameFlag pins `boss settings --daemon-name` (BOS-1227): the
// daemon display name must be settable in one non-interactive line, with the
// same blank-resets-to-hostname semantics the TUI row already implements.
func TestSettingsDaemonNameFlag(t *testing.T) {
	// Subtests that are not specifically exercising the anyChanged disjunction
	// pass a second flag, so that "only --daemon-name" below is a genuine
	// discriminating guard for it rather than a duplicate of the first case.
	t.Run("persists the value", func(t *testing.T) {
		seedDaemonNameSettings(t)
		cmd := settingsCmd()
		cmd.SetArgs([]string{"--poll-interval", "45", "--daemon-name", "studio-mini"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("execute: %v", err)
		}
		s, err := config.Load()
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if s.DaemonName != "studio-mini" {
			t.Errorf("DaemonName = %q, want %q", s.DaemonName, "studio-mini")
		}
	})

	t.Run("persists a padded value trimmed", func(t *testing.T) {
		seedDaemonNameSettings(t)
		cmd := settingsCmd()
		cmd.SetArgs([]string{"--poll-interval", "45", "--daemon-name", "  studio-mini  "})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("execute: %v", err)
		}
		s, err := config.Load()
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		// Trim parity with services/boss/internal/views/general_settings.go.
		if s.DaemonName != "studio-mini" {
			t.Errorf("DaemonName = %q, want trimmed %q", s.DaemonName, "studio-mini")
		}
	})

	t.Run("blank clears the override and drops the JSON key", func(t *testing.T) {
		path := seedDaemonNameSettings(t)
		seeded, err := config.Load()
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		seeded.DaemonName = "studio-mini"
		if err := config.Save(seeded); err != nil {
			t.Fatalf("save seeded override: %v", err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read seeded settings: %v", err)
		}
		if !bytes.Contains(raw, []byte(`"daemon_name"`)) {
			t.Fatalf("seeded settings file should carry daemon_name, got: %s", raw)
		}

		cmd := settingsCmd()
		cmd.SetArgs([]string{"--poll-interval", "45", "--daemon-name", ""})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("execute: blank --daemon-name is the documented reset, not an error: %v", err)
		}

		reloaded, err := config.Load()
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if reloaded.DaemonName != "" {
			t.Errorf("DaemonName = %q, want cleared", reloaded.DaemonName)
		}
		// Assert on the bytes, not only the struct: daemon_name is omitempty and
		// TestDaemonNameOmittedFromDefaultSettingsJSON requires the key to stay
		// absent rather than persist as "".
		cleared, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read cleared settings: %v", err)
		}
		if bytes.Contains(cleared, []byte(`"daemon_name"`)) {
			t.Errorf("settings file still carries a daemon_name key after clearing: %s", cleared)
		}
	})

	t.Run("only --daemon-name still writes the file", func(t *testing.T) {
		// Regression guard for the anyChanged disjunction in runSettings.
		// Without the daemon-name disjunct this falls into the read-out branch
		// and returns nil having written nothing — a silent no-op that looks
		// like success. Every other subtest passes a second flag and so would
		// still pass without the disjunct.
		seedDaemonNameSettings(t)
		cmd := settingsCmd()
		cmd.SetArgs([]string{"--daemon-name", "studio-mini"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("execute: %v", err)
		}
		s, err := config.Load()
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if s.DaemonName != "studio-mini" {
			t.Errorf("DaemonName = %q, want %q — --daemon-name alone must trigger a save", s.DaemonName, "studio-mini")
		}
	})

	t.Run("a successful save prints the daemon restart note", func(t *testing.T) {
		seedDaemonNameSettings(t)
		cmd := settingsCmd()
		cmd.SetArgs([]string{"--daemon-name", "studio-mini"})
		out := captureStdout(t, func() {
			if err := cmd.Execute(); err != nil {
				t.Errorf("execute: %v", err)
			}
		})
		if !strings.Contains(out, "Restart the daemon") {
			t.Errorf("want a daemon-restart note after saving a new name, got:\n%s", out)
		}
	})

	t.Run("no-flag read-out resolves through DaemonDisplayName", func(t *testing.T) {
		seedDaemonNameSettings(t)
		s, err := config.Load()
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		s.DaemonName = "studio-mini"
		if err := config.Save(s); err != nil {
			t.Fatalf("save: %v", err)
		}
		cmd := settingsCmd()
		// An empty slice, never nil: cobra falls back to os.Args[1:] when args
		// are nil, which under `go test` is the test binary's own flags.
		cmd.SetArgs([]string{})
		out := captureStdout(t, func() {
			if err := cmd.Execute(); err != nil {
				t.Errorf("execute: %v", err)
			}
		})
		hostname, _ := os.Hostname()
		reloaded, err := config.Load()
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		want := config.DaemonDisplayName(reloaded, hostname)
		if want == "" {
			t.Skip("no override resolved and os.Hostname() is empty; nothing to compare")
		}
		if !strings.Contains(out, "Daemon name:") {
			t.Fatalf("want a 'Daemon name:' line in the read-out, got:\n%s", out)
		}
		if !strings.Contains(out, want) {
			t.Errorf("read-out should show the DaemonDisplayName value %q, got:\n%s", want, out)
		}
		// The line must never render as a bare blank.
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "Daemon name:") && strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "Daemon name:")) == "" {
				t.Errorf("'Daemon name:' rendered blank: %q", line)
			}
		}
	})

	t.Run("sibling empty-value rejections are unchanged", func(t *testing.T) {
		// R7: --daemon-name accepting a blank must not have loosened the two
		// string siblings, whose empty values remain errors.
		for _, flag := range []string{"--worktree-dir", "--default-agent"} {
			t.Run(flag, func(t *testing.T) {
				seedDaemonNameSettings(t)
				cmd := settingsCmd()
				cmd.SetArgs([]string{flag, ""})
				if err := cmd.Execute(); err == nil {
					t.Errorf("want an error for %s \"\"", flag)
				}
			})
		}
	})
}

// TestSettingsDaemonNameLine covers the read-out resolution helper directly.
// The hostname-unavailable branch is unreachable through runSettings — you
// cannot make the real os.Hostname() return "" — so the helper takes the
// hostname as a parameter and the sentinel is asserted here, mirroring the TUI
// twin's coverage in services/boss/internal/views/general_settings_test.go.
func TestSettingsDaemonNameLine(t *testing.T) {
	const sentinel = "(machine hostname unavailable — set one with --daemon-name)"

	tests := []struct {
		name     string
		override string
		hostname string
		want     string
	}{
		{
			name:     "override wins over the hostname",
			override: "studio-mini",
			hostname: "some-machine",
			want:     "studio-mini",
		},
		{
			name:     "override wins even with no hostname",
			override: "studio-mini",
			hostname: "",
			want:     "studio-mini",
		},
		{
			name:     "no override falls back to the hostname",
			override: "",
			hostname: "some-machine",
			want:     "some-machine",
		},
		{
			name:     "no override and no hostname prompts for a name",
			override: "",
			hostname: "",
			want:     sentinel,
		},
		{
			name:     "whitespace-only override and no hostname prompts for a name",
			override: "   ",
			hostname: "",
			want:     sentinel,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := settingsDaemonNameLine(config.Settings{DaemonName: tt.override}, tt.hostname)
			if got != tt.want {
				t.Errorf("settingsDaemonNameLine(%q, %q) = %q, want %q", tt.override, tt.hostname, got, tt.want)
			}
		})
	}
}
