package main

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/recurser/boss/internal/adapter/daemonrpc"
	"github.com/recurser/bossalib/kit/config"
	"github.com/spf13/cobra"
)

func TestGHReadCredentialLocalFallbackBypassesStartup_GitHubUsage_AC232201(t *testing.T) {
	t.Setenv("BOSS_SOCKET", filepath.Join(t.TempDir(), "absent.sock"))
	t.Setenv("BOSS_REMOTE", "https://example.invalid")
	g := newSkillsGroup()
	setupStartupGroup(t, g)
	setStartupAgents(g, map[string]bool{"claude": true, "codex": true})
	g.StartupIsTerminal = func() bool { return true }
	g.StartupReadAnswer = func() string { t.Fatal("helper prompted for skills"); return "" }
	oldHost := newHostClient
	newHostClient = func(*cobra.Command, string) (*daemonrpc.LocalClient, error) {
		t.Fatal("helper dialed a host")
		return nil, nil
	}
	t.Cleanup(func() { newHostClient = oldHost })
	for _, ghArgs := range [][]string{{"pr", "merge", "1"}, {"api", "user"}} {
		root := rootCmdWithSkills(g)
		var out, stderr bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&stderr)
		root.SetArgs(append([]string{"--host", "example.invalid", "--remote", "https://example.invalid", "__gh-read-credential", "--"}, ghArgs...))
		if err := root.Execute(); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if out.Len() != 0 || stderr.Len() != 0 {
			t.Fatalf("stdout=%q stderr=%q", out.String(), stderr.String())
		}
	}
}

func TestGHReadCredentialFactoryDoesNotStartDaemon(t *testing.T) {
	t.Setenv("BOSS_SOCKET", "")
	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	t.Setenv("BOSS_SETTINGS_PATH", settingsPath)
	settings := config.DefaultSettings()
	settings.SocketPath = filepath.Join(t.TempDir(), "absent.sock")
	if err := config.SaveTo(settingsPath, settings); err != nil {
		t.Fatal(err)
	}
	oldEnsure := daemonEnsureRunning
	daemonEnsureRunning = func(string) error { t.Fatal("credential helper started the daemon"); return nil }
	t.Cleanup(func() { daemonEnsureRunning = oldEnsure })
	client, err := newGitHubReadCredentialClient(&cobra.Command{})
	if err != nil || client == nil {
		t.Fatalf("client=%v err=%v", client, err)
	}
}
