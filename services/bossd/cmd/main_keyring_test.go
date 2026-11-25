package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/99designs/keyring"
	"github.com/recurser/bossalib/kit/config"

	"github.com/recurser/bossd/internal/adapter/accountcred"
)

// TestMain keeps every daemon started by this package's tests off the OS
// keyring and real user skill directories: startup persists the refresh broker
// endpoint and launches the skill-refresh background worker, and a test run
// must never overwrite a developer's live daemon state or installed skill trees.
func TestMain(m *testing.M) {
	newAccountCredentialStore = func() *accountcred.Store {
		return accountcred.NewWithKeyring(keyring.NewArrayKeyring(nil))
	}
	if err := os.Setenv("BOSS_SKIP_SKILLS", "1"); err != nil {
		panic("set BOSS_SKIP_SKILLS: " + err.Error())
	}
	os.Exit(m.Run())
}

// TestRunSkipsSkillRefreshInTests proves that TestMain isolates daemon runs in
// this package from mutating installed skill trees under HOME or HERMES_HOME,
// even when the parent environment does not set BOSS_SKIP_SKILLS.
func TestRunSkipsSkillRefreshInTests(t *testing.T) {
	baseDir, err := os.MkdirTemp("/tmp", "bossd-skill-iso-")
	if err != nil {
		t.Fatalf("mkdir base: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(baseDir) })

	homeDir := filepath.Join(baseDir, "home")
	hermesHome := filepath.Join(baseDir, "hermes")
	t.Setenv("HOME", homeDir)
	t.Setenv("HERMES_HOME", hermesHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(baseDir, ".config"))
	t.Setenv("XDG_STATE_HOME", baseDir)
	t.Setenv("BOSS_SETTINGS_PATH", filepath.Join(baseDir, "settings.json"))
	t.Setenv("BOSSD_ORCHESTRATOR_URL", "")

	const sentinelSkill = "sentinel skill content\n"
	const sentinelRecord = "{\"origin\":\"embedded\",\"revision\":\"1111111\",\"version\":\"v0.1.0\",\"writer\":\"unattended\"}\n"

	skillRoots := []string{
		filepath.Join(homeDir, ".claude", "skills"),
		filepath.Join(hermesHome, "skills"),
	}
	for _, root := range skillRoots {
		skillDir := filepath.Join(root, "bossanova", "boss-build")
		if err := os.MkdirAll(skillDir, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", skillDir, err)
		}
		if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(sentinelSkill), 0o644); err != nil {
			t.Fatalf("write SKILL.md: %v", err)
		}
		if err := os.WriteFile(filepath.Join(root, ".bossanova-skills.payload.json"), []byte(sentinelRecord), 0o644); err != nil {
			t.Fatalf("write payload record: %v", err)
		}
	}

	stopSig := make(chan os.Signal, 1)
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- run(runOpts{
			stopSig:    stopSig,
			dbPath:     filepath.Join(baseDir, "bossd.db"),
			socketPath: filepath.Join(baseDir, "bossd.sock"),
			plugins:    []config.PluginConfig{},
			onReady:    func() { close(ready) },
		})
	}()

	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("run exited before ready: %v", err)
	}
	stopSig <- syscall.SIGTERM
	if err := <-done; err != nil {
		t.Fatalf("run returned error: %v", err)
	}

	for _, root := range skillRoots {
		gotSkill, err := os.ReadFile(filepath.Join(root, "bossanova", "boss-build", "SKILL.md"))
		if err != nil {
			t.Fatalf("read %s SKILL.md: %v", root, err)
		}
		if string(gotSkill) != sentinelSkill {
			t.Fatalf("%s SKILL.md was mutated by skill-refresh during cmd test", root)
		}
		gotRec, err := os.ReadFile(filepath.Join(root, ".bossanova-skills.payload.json"))
		if err != nil {
			t.Fatalf("read %s payload record: %v", root, err)
		}
		if string(gotRec) != sentinelRecord {
			t.Fatalf("%s payload record mutated to %q, want %q", root, string(gotRec), sentinelRecord)
		}
	}
}
