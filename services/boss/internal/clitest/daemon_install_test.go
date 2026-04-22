//go:build darwin

package clitest_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/recurser/boss/internal/clitest"
)

// plistPathFor returns the expected plist path when HOME is overridden to home.
func plistPathFor(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents", "com.bossanova.bossd.plist")
}

// daemonTestEnv returns the env vars required for daemon CLI tests: a fake
// HOME (so the plist lands in a test-owned dir) and the skip-launchctl flag
// (so the test never interacts with the host's real launchd).
func daemonTestEnv(t *testing.T) (home string, env []string) {
	t.Helper()
	home = t.TempDir()

	// The `boss daemon install` CLI also needs to find a `bossd` binary via
	// ResolveBossdPath. That function checks next-to-exe and then $PATH.
	// Provide a stub bossd in a tempdir prepended to PATH.
	binDir := t.TempDir()
	stubBossd := filepath.Join(binDir, "bossd")
	if err := os.WriteFile(stubBossd, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write stub bossd: %v", err)
	}

	env = []string{
		"HOME=" + home,
		"BOSS_DAEMON_SKIP_LAUNCHCTL=1",
		"PATH=" + binDir + ":" + os.Getenv("PATH"),
	}
	return home, env
}

func TestCLI_DaemonInstall_CreatesPlist(t *testing.T) {
	home, env := daemonTestEnv(t)
	h := clitest.New(t, clitest.WithEnv(env...))

	res := h.Run("daemon", "install")
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q stdout=%q", res.ExitCode, res.Stderr, res.Stdout)
	}

	p := plistPathFor(home)
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("plist not written at %s: %v", p, err)
	}
	content := string(data)
	for _, want := range []string{
		"<string>com.bossanova.bossd</string>",
		"/bossd", // stub bossd path should be referenced
		"<key>RunAtLoad</key>",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("plist missing %q\n---\n%s", want, content)
		}
	}
}

func TestCLI_DaemonStatus_NotInstalled(t *testing.T) {
	_, env := daemonTestEnv(t)
	h := clitest.New(t, clitest.WithEnv(env...))

	res := h.Run("daemon", "status")
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "not installed") {
		t.Errorf("expected 'not installed' message, got: %q", res.Stdout)
	}
}

func TestCLI_DaemonStatus_Installed(t *testing.T) {
	home, env := daemonTestEnv(t)
	h := clitest.New(t, clitest.WithEnv(env...))

	if res := h.Run("daemon", "install"); res.ExitCode != 0 {
		t.Fatalf("install: exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}

	res := h.Run("daemon", "status")
	if res.ExitCode != 0 {
		t.Fatalf("status: exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if strings.Contains(res.Stdout, "not installed") {
		t.Errorf("expected installed status, got 'not installed': %q", res.Stdout)
	}
	if !strings.Contains(res.Stdout, plistPathFor(home)) {
		t.Errorf("status should reference plist path %q, got: %q", plistPathFor(home), res.Stdout)
	}
}

func TestCLI_DaemonUninstall_RemovesPlist(t *testing.T) {
	home, env := daemonTestEnv(t)
	h := clitest.New(t, clitest.WithEnv(env...))

	if res := h.Run("daemon", "install"); res.ExitCode != 0 {
		t.Fatalf("install: exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	p := plistPathFor(home)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("plist not created: %v", err)
	}

	if res := h.Run("daemon", "uninstall"); res.ExitCode != 0 {
		t.Fatalf("uninstall: exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}

	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("expected plist to be removed, err=%v", err)
	}
}

func TestCLI_DaemonInstall_NoOverwriteWithoutForce(t *testing.T) {
	_, env := daemonTestEnv(t)
	h := clitest.New(t, clitest.WithEnv(env...))

	if res := h.Run("daemon", "install"); res.ExitCode != 0 {
		t.Fatalf("first install: exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}

	// Second install without --force: should refuse.
	res := h.Run("daemon", "install")
	if res.ExitCode == 0 {
		t.Fatalf("expected non-zero exit on second install without --force")
	}
	if !strings.Contains(res.Stderr, "already exists") {
		t.Errorf("expected 'already exists' in stderr, got: %q", res.Stderr)
	}

	// Second install WITH --force: should succeed.
	res = h.Run("daemon", "install", "--force")
	if res.ExitCode != 0 {
		t.Fatalf("install --force: exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
}
