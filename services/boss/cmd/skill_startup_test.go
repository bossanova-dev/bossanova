package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	bossskillinstall "github.com/recurser/boss/internal/skillinstall"
	"github.com/recurser/bossalib/config"
	libskillinstall "github.com/recurser/bossalib/skillinstall"
)

func setupSkillStartupTest(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Setenv("BOSS_SKIP_SKILLS", "")

	origAgents := skillInstallAgents
	origLookPath := skillInstallLookPath
	origIsTerminal := skillInstallIsTerminal
	origReadAnswer := skillInstallReadAnswer
	t.Cleanup(func() {
		skillInstallAgents = origAgents
		skillInstallLookPath = origLookPath
		skillInstallIsTerminal = origIsTerminal
		skillInstallReadAnswer = origReadAnswer
	})
	skillInstallIsTerminal = func() bool { return true }
	return home
}

func setAvailableSkillAgents(available map[string]bool) {
	skillInstallLookPath = func(command string) (string, error) {
		if available[command] {
			return "/usr/bin/" + command, nil
		}
		return "", errors.New("not found")
	}
}

func setSkillPromptAnswers(t *testing.T, answers ...string) *int {
	t.Helper()
	calls := 0
	skillInstallReadAnswer = func() string {
		if calls >= len(answers) {
			t.Fatalf("unexpected prompt %d", calls+1)
		}
		answer := answers[calls]
		calls++
		return answer
	}
	return &calls
}

func TestMaybeInstallSkillsPromptsAndInstallsEachAvailableAgentInOrder(t *testing.T) {
	home := setupSkillStartupTest(t)
	setAvailableSkillAgents(map[string]bool{"claude": true, "codex": true})
	calls := setSkillPromptAnswers(t, "", "")

	if err := maybeInstallSkills(); err != nil {
		t.Fatalf("maybeInstallSkills: %v", err)
	}
	if *calls != 2 {
		t.Fatalf("prompts = %d, want 2", *calls)
	}

	assertAgentSkillsInstalled(t, filepath.Join(home, ".claude", "skills"))
	assertAgentSkillsInstalled(t, filepath.Join(home, ".codex", "skills"))

	settings, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	manifest := currentTestSkillManifest(t)
	if settings.SkillsInstalledManifestByAgent["claude"] != manifest {
		t.Fatalf("claude installed manifest = %q, want current", settings.SkillsInstalledManifestByAgent["claude"])
	}
	if settings.SkillsInstalledManifestByAgent["codex"] != manifest {
		t.Fatalf("codex installed manifest = %q, want current", settings.SkillsInstalledManifestByAgent["codex"])
	}
}

func TestMaybeInstallSkillsLegacyDeclineSuppressesClaudeOnly(t *testing.T) {
	home := setupSkillStartupTest(t)
	if err := config.Save(config.Settings{SkillsDeclined: true}); err != nil {
		t.Fatalf("config.Save: %v", err)
	}
	setAvailableSkillAgents(map[string]bool{"claude": true, "codex": true})
	calls := setSkillPromptAnswers(t, "")

	if err := maybeInstallSkills(); err != nil {
		t.Fatalf("maybeInstallSkills: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("prompts = %d, want 1", *calls)
	}
	if libskillinstall.IsInstalled(filepath.Join(home, ".claude", "skills")) {
		t.Fatal("claude skills installed despite legacy decline")
	}
	assertAgentSkillsInstalled(t, filepath.Join(home, ".codex", "skills"))
}

func TestMaybeInstallSkillsSkipsPreviouslyDeclinedCurrentManifest(t *testing.T) {
	home := setupSkillStartupTest(t)
	manifest := currentTestSkillManifest(t)
	if err := config.Save(config.Settings{
		SkillsDeclinedByAgent: map[string]bool{
			"codex": true,
		},
		SkillsDeclinedManifestByAgent: map[string]string{
			"codex": manifest,
		},
	}); err != nil {
		t.Fatalf("config.Save: %v", err)
	}
	setAvailableSkillAgents(map[string]bool{"codex": true})
	calls := setSkillPromptAnswers(t)

	if err := maybeInstallSkills(); err != nil {
		t.Fatalf("maybeInstallSkills: %v", err)
	}
	if *calls != 0 {
		t.Fatalf("prompts = %d, want 0", *calls)
	}
	if libskillinstall.IsInstalled(filepath.Join(home, ".codex", "skills")) {
		t.Fatal("codex skills installed despite current manifest decline")
	}
}

func TestMaybeInstallSkillsPromptsAgainAfterDeclinedManifestChanges(t *testing.T) {
	home := setupSkillStartupTest(t)
	if err := config.Save(config.Settings{
		SkillsDeclinedByAgent: map[string]bool{
			"codex": true,
		},
		SkillsDeclinedManifestByAgent: map[string]string{
			"codex": "old-manifest",
		},
	}); err != nil {
		t.Fatalf("config.Save: %v", err)
	}
	setAvailableSkillAgents(map[string]bool{"codex": true})
	calls := setSkillPromptAnswers(t, "")

	if err := maybeInstallSkills(); err != nil {
		t.Fatalf("maybeInstallSkills: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("prompts = %d, want 1", *calls)
	}
	assertAgentSkillsInstalled(t, filepath.Join(home, ".codex", "skills"))
}

func TestMaybeInstallSkillsPromptsForStaleInstalledSkills(t *testing.T) {
	home := setupSkillStartupTest(t)
	codexDir := filepath.Join(home, ".codex", "skills")
	if err := libskillinstall.Extract(codexDir, bossskillinstall.SkillsFS); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	stalePath := filepath.Join(codexDir, libskillinstall.Namespace, "boss", "SKILL.md")
	if err := os.WriteFile(stalePath, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	setAvailableSkillAgents(map[string]bool{"codex": true})
	calls := setSkillPromptAnswers(t, "")

	if err := maybeInstallSkills(); err != nil {
		t.Fatalf("maybeInstallSkills: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("prompts = %d, want 1", *calls)
	}
	data, err := os.ReadFile(stalePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "stale" {
		t.Fatal("stale skill was not refreshed")
	}
}

func TestMaybeInstallSkillsSkipsNonInteractiveAndEnvOptOut(t *testing.T) {
	setupSkillStartupTest(t)
	setAvailableSkillAgents(map[string]bool{"claude": true, "codex": true})
	calls := setSkillPromptAnswers(t)
	skillInstallIsTerminal = func() bool { return false }

	if err := maybeInstallSkills(); err != nil {
		t.Fatalf("maybeInstallSkills non-interactive: %v", err)
	}
	t.Setenv("BOSS_SKIP_SKILLS", "1")
	skillInstallIsTerminal = func() bool { return true }
	if err := maybeInstallSkills(); err != nil {
		t.Fatalf("maybeInstallSkills opt-out: %v", err)
	}
	if *calls != 0 {
		t.Fatalf("prompts = %d, want 0", *calls)
	}
}

func assertAgentSkillsInstalled(t *testing.T, dir string) {
	t.Helper()
	if !libskillinstall.IsInstalled(dir) {
		t.Fatalf("skills not installed in %s", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "boss", "SKILL.md")); err != nil {
		t.Fatalf("boss skill not readable through symlink: %v", err)
	}
}

func currentTestSkillManifest(t *testing.T) string {
	t.Helper()
	manifest, err := libskillinstall.Manifest(bossskillinstall.SkillsFS)
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	return manifest
}
