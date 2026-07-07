package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	// Give each skill-startup test its own settings file. maybeInstallSkills
	// persists installed/declined manifest state via config.Save, so sharing the
	// package-wide BOSS_SETTINGS_PATH default would leak state between tests and
	// make them order-dependent. t.Setenv restores the default afterward.
	t.Setenv("BOSS_SETTINGS_PATH", filepath.Join(home, "settings.json"))
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

func TestMaybeInstallSkillsSelfHealsStaleTreeNonInteractively(t *testing.T) {
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
	// No prompt may be emitted on the non-TTY self-heal path.
	skillInstallReadAnswer = func() string {
		t.Fatal("self-heal must not prompt")
		return ""
	}
	skillInstallIsTerminal = func() bool { return false }

	if err := maybeInstallSkills(); err != nil {
		t.Fatalf("maybeInstallSkills: %v", err)
	}
	data, err := os.ReadFile(stalePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "stale" {
		t.Fatal("stale skill was not self-healed on the non-TTY path")
	}
}

func TestSelfHealSkillsRespectsDeclinedCurrentManifest(t *testing.T) {
	home := setupSkillStartupTest(t)
	// The user previously answered `n` to the update prompt for this exact
	// manifest in an interactive session. The non-TTY self-heal path must honor
	// that opt-out instead of silently overwriting the declined stale tree on
	// every headless daemon/tmux/cron invocation.
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
	codexDir := filepath.Join(home, ".codex", "skills")
	if err := libskillinstall.Extract(codexDir, bossskillinstall.SkillsFS); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	stalePath := filepath.Join(codexDir, libskillinstall.Namespace, "boss", "SKILL.md")
	if err := os.WriteFile(stalePath, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	setAvailableSkillAgents(map[string]bool{"codex": true})
	skillInstallReadAnswer = func() string {
		t.Fatal("self-heal must not prompt")
		return ""
	}
	skillInstallIsTerminal = func() bool { return false }

	if err := maybeInstallSkills(); err != nil {
		t.Fatalf("maybeInstallSkills: %v", err)
	}
	data, err := os.ReadFile(stalePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "stale" {
		t.Fatal("declined stale skill was refreshed by self-heal, want opt-out honored")
	}
}

func TestSelfHealSkillsRefreshesAfterDeclinedManifestChanges(t *testing.T) {
	home := setupSkillStartupTest(t)
	// A decline is recorded against an older manifest. The embedded payload has
	// since changed, so the opt-out no longer applies and self-heal should
	// refresh the stale tree — mirroring the interactive re-prompt behavior.
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
	codexDir := filepath.Join(home, ".codex", "skills")
	if err := libskillinstall.Extract(codexDir, bossskillinstall.SkillsFS); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	stalePath := filepath.Join(codexDir, libskillinstall.Namespace, "boss", "SKILL.md")
	if err := os.WriteFile(stalePath, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	setAvailableSkillAgents(map[string]bool{"codex": true})
	skillInstallReadAnswer = func() string {
		t.Fatal("self-heal must not prompt")
		return ""
	}
	skillInstallIsTerminal = func() bool { return false }

	if err := maybeInstallSkills(); err != nil {
		t.Fatalf("maybeInstallSkills: %v", err)
	}
	data, err := os.ReadFile(stalePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "stale" {
		t.Fatal("stale skill was not refreshed after the declined manifest changed")
	}
}

func TestSelfHealSkillsPreservesMalformedSettings(t *testing.T) {
	home := setupSkillStartupTest(t)
	// A malformed settings.json makes config.Load return defaults plus an error.
	// Self-heal must refresh the stale tree without persisting those defaults —
	// otherwise config.Save would clobber the user's real (broken) file.
	settingsPath := filepath.Join(home, "settings.json")
	malformed := []byte("{ this is not valid json")
	if err := os.WriteFile(settingsPath, malformed, 0o644); err != nil {
		t.Fatal(err)
	}
	codexDir := filepath.Join(home, ".codex", "skills")
	if err := libskillinstall.Extract(codexDir, bossskillinstall.SkillsFS); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	stalePath := filepath.Join(codexDir, libskillinstall.Namespace, "boss", "SKILL.md")
	if err := os.WriteFile(stalePath, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	setAvailableSkillAgents(map[string]bool{"codex": true})
	skillInstallReadAnswer = func() string {
		t.Fatal("self-heal must not prompt")
		return ""
	}
	skillInstallIsTerminal = func() bool { return false }

	if err := maybeInstallSkills(); err != nil {
		t.Fatalf("maybeInstallSkills: %v", err)
	}
	// The stale tree is still refreshed.
	data, err := os.ReadFile(stalePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "stale" {
		t.Fatal("stale skill was not self-healed on the non-TTY path")
	}
	// The malformed settings file is left untouched, not overwritten with defaults.
	saved, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(saved, malformed) {
		t.Fatalf("settings.json was rewritten to %q, want malformed content preserved", saved)
	}
}

func TestMaybeInstallSkillsNonInteractiveDoesNotFreshInstall(t *testing.T) {
	home := setupSkillStartupTest(t)
	setAvailableSkillAgents(map[string]bool{"claude": true, "codex": true})
	skillInstallReadAnswer = func() string {
		t.Fatal("self-heal must not prompt")
		return ""
	}
	skillInstallIsTerminal = func() bool { return false }

	if err := maybeInstallSkills(); err != nil {
		t.Fatalf("maybeInstallSkills: %v", err)
	}
	if libskillinstall.IsInstalled(filepath.Join(home, ".claude", "skills")) {
		t.Fatal("claude skills fresh-installed on the non-TTY path, want no-op")
	}
	if libskillinstall.IsInstalled(filepath.Join(home, ".codex", "skills")) {
		t.Fatal("codex skills fresh-installed on the non-TTY path, want no-op")
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

func TestGenSkillSkipsStartupSkillInstall(t *testing.T) {
	setupSkillStartupTest(t)
	setAvailableSkillAgents(map[string]bool{"codex": true})
	skillInstallReadAnswer = func() string {
		t.Fatal("gen-skill should not prompt for skill installation")
		return ""
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	t.Chdir(filepath.Dir(filepath.Dir(file)))

	root := rootCmd()
	root.SetArgs([]string{"gen-skill", "--check"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	if err := root.Execute(); err != nil {
		t.Fatalf("gen-skill --check: %v", err)
	}
}

func TestSkillsCommandBypassesStartupInstaller(t *testing.T) {
	home := setupSkillStartupTest(t)
	setAvailableSkillAgents(map[string]bool{"claude": true})
	// The startup installer must never run for the `skills` subtree: neither the
	// interactive [Y/n] prompt nor the non-TTY self-heal may pre-empt the command.
	skillInstallReadAnswer = func() string {
		t.Fatal("skills subtree must not trigger the startup prompt")
		return ""
	}
	// Interactive terminal: if the pre-run installer ran, it would prompt (fatal
	// above). A stale-but-installed tree also proves the self-heal did not run
	// before the command: `boss skills sync` reports "updated", not "up to date".
	skillInstallIsTerminal = func() bool { return true }
	claudeDir := filepath.Join(home, ".claude", "skills")
	if err := libskillinstall.Extract(claudeDir, bossskillinstall.SkillsFS); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	stalePath := filepath.Join(claudeDir, libskillinstall.Namespace, "boss", "SKILL.md")
	if err := os.WriteFile(stalePath, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	root := rootCmd()
	root.SetArgs([]string{"skills", "sync", "--agent", "claude"})
	root.SetOut(&out)
	root.SetErr(io.Discard)
	if err := root.Execute(); err != nil {
		t.Fatalf("boss skills sync: %v", err)
	}
	if !strings.Contains(out.String(), "boss skills: updated claude") {
		t.Fatalf("output = %q, want 'updated' (self-heal must not pre-empt the command)", out.String())
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
