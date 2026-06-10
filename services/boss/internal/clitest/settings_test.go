package clitest_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/recurser/boss/internal/clitest"
	"github.com/recurser/bossalib/config"
)

type testSettings struct {
	WorktreeBaseDir     string `json:"worktree_base_dir"`
	PollIntervalSeconds int    `json:"poll_interval_seconds"`
}

// readSettings reads the settings.json the harness isolated the subprocess to
// (BOSS_SETTINGS_PATH), not a HOME-derived path.
func readSettings(t *testing.T, h *clitest.Harness) testSettings {
	t.Helper()
	p := h.SettingsPath()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read settings %s: %v", p, err)
	}
	var s testSettings
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("parse settings: %v", err)
	}
	return s
}

// readSkipPermissions reads the dangerously_skip_permissions value from
// the claude plugin's Config map in the harness settings.json file.
func readSkipPermissions(t *testing.T, h *clitest.Harness) bool {
	t.Helper()
	loaded, err := config.LoadFrom(h.SettingsPath())
	if err != nil {
		t.Fatalf("config.LoadFrom: %v", err)
	}
	return config.PluginConfigBool(&loaded, "claude", "dangerously_skip_permissions")
}

func TestCLI_Settings_Show(t *testing.T) {
	h := clitest.New(t)
	res := h.Run("settings")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	for _, want := range []string{"Skip permissions", "Worktree dir", "Poll interval"} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("stdout missing %q", want)
		}
	}
}

func TestCLI_Settings_Toggle_SkipPermissions(t *testing.T) {
	h := clitest.New(t)

	res := h.Run("settings", "--skip-permissions")
	if res.ExitCode != 0 {
		t.Fatalf("enable: exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !readSkipPermissions(t, h) {
		t.Errorf("expected dangerously_skip_permissions=true after enable")
	}

	res = h.Run("settings", "--no-skip-permissions")
	if res.ExitCode != 0 {
		t.Fatalf("disable: exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if readSkipPermissions(t, h) {
		t.Errorf("expected dangerously_skip_permissions=false after disable")
	}
}

func TestCLI_Settings_SetWorktreeDir(t *testing.T) {
	h := clitest.New(t)

	custom := t.TempDir()
	res := h.Run("settings", "--worktree-dir", custom)
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}

	if s := readSettings(t, h); s.WorktreeBaseDir != custom {
		t.Errorf("expected WorktreeBaseDir=%q, got %q", custom, s.WorktreeBaseDir)
	}
}

func TestCLI_Settings_SetDefaultAgent(t *testing.T) {
	h := clitest.New(t)

	res := h.Run("settings", "--default-agent", "opencode")
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}

	loaded, err := config.LoadFrom(h.SettingsPath())
	if err != nil {
		t.Fatalf("config.LoadFrom: %v", err)
	}
	if loaded.DefaultAgent != "opencode" {
		t.Errorf("expected DefaultAgent=%q, got %q", "opencode", loaded.DefaultAgent)
	}
}

func TestCLI_Settings_SetPollInterval(t *testing.T) {
	h := clitest.New(t)

	res := h.Run("settings", "--poll-interval", "45")
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}

	if s := readSettings(t, h); s.PollIntervalSeconds != 45 {
		t.Errorf("expected PollIntervalSeconds=45, got %d", s.PollIntervalSeconds)
	}
}
