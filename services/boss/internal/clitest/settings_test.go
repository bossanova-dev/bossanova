package clitest_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/recurser/boss/internal/clitest"
)

// settingsPath returns the settings.json path for the given HOME dir, matching
// config.Path()'s os.UserConfigDir() logic.
func settingsPath(home string) string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "bossanova", "settings.json")
	}
	return filepath.Join(home, ".config", "bossanova", "settings.json")
}

type testSettings struct {
	DangerouslySkipPermissions bool   `json:"dangerously_skip_permissions"`
	WorktreeBaseDir            string `json:"worktree_base_dir"`
	PollIntervalSeconds        int    `json:"poll_interval_seconds"`
}

func readSettings(t *testing.T, home string) testSettings {
	t.Helper()
	p := settingsPath(home)
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

func TestCLI_Settings_Show(t *testing.T) {
	home := t.TempDir()
	h := clitest.New(t, clitest.WithEnv("HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config")))
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
	home := t.TempDir()
	h := clitest.New(t, clitest.WithEnv("HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config")))

	res := h.Run("settings", "--skip-permissions")
	if res.ExitCode != 0 {
		t.Fatalf("enable: exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if s := readSettings(t, home); !s.DangerouslySkipPermissions {
		t.Errorf("expected DangerouslySkipPermissions=true, got false")
	}

	res = h.Run("settings", "--no-skip-permissions")
	if res.ExitCode != 0 {
		t.Fatalf("disable: exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if s := readSettings(t, home); s.DangerouslySkipPermissions {
		t.Errorf("expected DangerouslySkipPermissions=false, got true")
	}
}

func TestCLI_Settings_SetWorktreeDir(t *testing.T) {
	home := t.TempDir()
	h := clitest.New(t, clitest.WithEnv("HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config")))

	custom := filepath.Join(home, "custom", "worktrees")
	res := h.Run("settings", "--worktree-dir", custom)
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}

	if s := readSettings(t, home); s.WorktreeBaseDir != custom {
		t.Errorf("expected WorktreeBaseDir=%q, got %q", custom, s.WorktreeBaseDir)
	}
}

func TestCLI_Settings_SetPollInterval(t *testing.T) {
	home := t.TempDir()
	h := clitest.New(t, clitest.WithEnv("HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config")))

	res := h.Run("settings", "--poll-interval", "45")
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}

	if s := readSettings(t, home); s.PollIntervalSeconds != 45 {
		t.Errorf("expected PollIntervalSeconds=45, got %d", s.PollIntervalSeconds)
	}
}
