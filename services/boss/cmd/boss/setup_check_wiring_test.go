package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/recurser/boss/internal/adapter/daemonrpc"
	"github.com/recurser/bossalib/kit/config"
	"github.com/spf13/cobra"
)

func TestInitCheckNeverAutoStartsDaemon_BossInit_AC268303(t *testing.T) {
	t.Setenv("BOSS_SOCKET", "")
	settingsDir := t.TempDir()
	settingsPath := filepath.Join(settingsDir, "settings.json")
	t.Setenv("BOSS_SETTINGS_PATH", settingsPath)

	absentSocket := filepath.Join(t.TempDir(), "absent-bossd.sock")
	settings := config.DefaultSettings()
	settings.SocketPath = absentSocket
	if err := config.SaveTo(settingsPath, settings); err != nil {
		t.Fatal(err)
	}

	oldEnsure := daemonEnsureRunning
	daemonEnsureRunning = func(string) error {
		t.Fatal("boss init check must never auto-start bossd")
		return nil
	}
	t.Cleanup(func() { daemonEnsureRunning = oldEnsure })

	g := newSkillsGroup()
	setupStartupGroup(t, g)
	// Restore BOSS_SOCKET="" and BOSS_SETTINGS_PATH after setupStartupGroup so
	// we exercise the real non-test auto-start path.
	t.Setenv("BOSS_SOCKET", "")
	t.Setenv("BOSS_SETTINGS_PATH", settingsPath)
	setStartupAgents(g, map[string]bool{"claude": true, "codex": true})
	g.StartupIsTerminal = func() bool { return true }
	g.StartupReadAnswer = func() string {
		t.Fatal("boss init check must not prompt for startup skill installation")
		return ""
	}
	g.Config.SetupCheckServiceInstalled = func() bool { return true }

	repoDir := t.TempDir()
	root := rootCmdWithSkills(g)
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"init", "check", "--dir", repoDir})

	if err := root.Execute(); err != nil {
		t.Fatalf("boss init check returned error %v, want exit 0 (stderr=%q)", err, errOut.String())
	}
	if !strings.Contains(out.String(), "MISSING  daemon-running  Bossanova daemon running") {
		t.Fatalf("stdout = %q, want daemon-running MISSING", out.String())
	}
	if !strings.Contains(out.String(), absentSocket) {
		t.Fatalf("stdout = %q, want configured socket path %q", out.String(), absentSocket)
	}
	if !strings.Contains(out.String(), "fix: boss daemon start") {
		t.Fatalf("stdout = %q, want fix: boss daemon start", out.String())
	}
}

func TestSetupCheckClientRespectsHostWithoutAutoStart(t *testing.T) {
	oldNewHostClient := newHostClient
	oldEnsureRunning := daemonEnsureRunning
	t.Cleanup(func() {
		newHostClient = oldNewHostClient
		daemonEnsureRunning = oldEnsureRunning
	})

	t.Setenv("BOSS_SOCKET", "")
	daemonEnsureRunning = func(string) error {
		t.Fatal("newSetupCheckClient must not start local daemon")
		return nil
	}
	want := daemonrpc.NewLocal("/unused")
	newHostClient = func(_ *cobra.Command, host string) (*daemonrpc.LocalClient, error) {
		if host != "user@example.test" {
			t.Fatalf("host = %q, want user@example.test", host)
		}
		return want, nil
	}

	got, err := newLocalSetupCheckClient(hostTestCommand(t))
	if err != nil || got != want {
		t.Fatalf("newLocalSetupCheckClient = (%v, %v), want (%v, nil)", got, err, want)
	}
}
