package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	bossskillinstall "github.com/recurser/boss/internal/skillinstall"
	libskillinstall "github.com/recurser/bossalib/skillinstall"
)

// runSkillSync and the interactive prompt share the skillInstall* seams; reuse
// setupSkillStartupTest for the temp HOME + isolated settings file, then stub the
// agents on PATH per case.

func TestRunSkillSyncUpToDateWritesNothing(t *testing.T) {
	home := setupSkillStartupTest(t)
	claudeDir := filepath.Join(home, ".claude", "skills")
	if err := libskillinstall.Extract(claudeDir, bossskillinstall.SkillsFS); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	stamp := filepath.Join(claudeDir, libskillinstall.Namespace, "boss", "SKILL.md")
	before, err := os.Stat(stamp)
	if err != nil {
		t.Fatal(err)
	}
	setAvailableSkillAgents(map[string]bool{"claude": true})

	var out bytes.Buffer
	if err := runSkillSync(&out, skillSyncUpdateOnly, ""); err != nil {
		t.Fatalf("runSkillSync: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "boss skills: claude up to date") {
		t.Fatalf("output = %q, want an 'up to date' line", got)
	}
	if strings.Contains(got, "[Y/n]") {
		t.Fatalf("output prompted: %q", got)
	}
	after, err := os.Stat(stamp)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("current tree was rewritten by sync")
	}
}

func TestRunSkillSyncUpdatesStaleTree(t *testing.T) {
	home := setupSkillStartupTest(t)
	claudeDir := filepath.Join(home, ".claude", "skills")
	if err := libskillinstall.Extract(claudeDir, bossskillinstall.SkillsFS); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	stalePath := filepath.Join(claudeDir, libskillinstall.Namespace, "boss", "SKILL.md")
	if err := os.WriteFile(stalePath, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	setAvailableSkillAgents(map[string]bool{"claude": true})

	var out bytes.Buffer
	if err := runSkillSync(&out, skillSyncUpdateOnly, ""); err != nil {
		t.Fatalf("runSkillSync: %v", err)
	}
	if !strings.Contains(out.String(), "boss skills: updated claude") {
		t.Fatalf("output = %q, want an 'updated' line", out.String())
	}
	data, err := os.ReadFile(stalePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "stale" {
		t.Fatal("stale tree not refreshed by sync")
	}
}

// A malformed settings.json makes config.Load return defaults plus an error.
// runSkillSync must still refresh the stale tree without persisting those
// defaults — otherwise config.Save would clobber the user's real (broken) file.
func TestRunSkillSyncPreservesMalformedSettings(t *testing.T) {
	home := setupSkillStartupTest(t)
	settingsPath := filepath.Join(home, "settings.json")
	malformed := []byte("{ this is not valid json")
	if err := os.WriteFile(settingsPath, malformed, 0o644); err != nil {
		t.Fatal(err)
	}
	claudeDir := filepath.Join(home, ".claude", "skills")
	if err := libskillinstall.Extract(claudeDir, bossskillinstall.SkillsFS); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	stalePath := filepath.Join(claudeDir, libskillinstall.Namespace, "boss", "SKILL.md")
	if err := os.WriteFile(stalePath, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	setAvailableSkillAgents(map[string]bool{"claude": true})

	var out bytes.Buffer
	if err := runSkillSync(&out, skillSyncUpdateOnly, ""); err != nil {
		t.Fatalf("runSkillSync: %v", err)
	}
	// The stale tree is still refreshed.
	if !strings.Contains(out.String(), "boss skills: updated claude") {
		t.Fatalf("output = %q, want an 'updated' line", out.String())
	}
	data, err := os.ReadFile(stalePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "stale" {
		t.Fatal("stale tree not refreshed by sync")
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

// sync is strictly update-only: it must never fresh-install into an empty dir,
// and it must report that honestly (not as "up to date"), pointing at `install`.
func TestRunSkillSyncDoesNotFreshInstall(t *testing.T) {
	home := setupSkillStartupTest(t)
	setAvailableSkillAgents(map[string]bool{"claude": true})

	var out bytes.Buffer
	if err := runSkillSync(&out, skillSyncUpdateOnly, ""); err != nil {
		t.Fatalf("runSkillSync: %v", err)
	}
	if libskillinstall.IsInstalled(filepath.Join(home, ".claude", "skills")) {
		t.Fatal("sync fresh-installed into an empty dir, want update-only no-op")
	}
	got := out.String()
	if !strings.Contains(got, "not installed") || !strings.Contains(got, "boss skills install") {
		t.Fatalf("output = %q, want a 'not installed ... boss skills install' line for the empty tree", got)
	}
	if strings.Contains(got, "up to date") {
		t.Fatalf("output = %q, must not report a not-installed tree as 'up to date'", got)
	}
}

// install (without --force) doubles as first-time setup: it fresh-installs an
// empty dir, unlike sync.
func TestRunSkillSyncInstallFreshInstallsEmptyDir(t *testing.T) {
	home := setupSkillStartupTest(t)
	claudeDir := filepath.Join(home, ".claude", "skills")
	setAvailableSkillAgents(map[string]bool{"claude": true})

	var out bytes.Buffer
	if err := runSkillSync(&out, skillSyncInstall, ""); err != nil {
		t.Fatalf("runSkillSync install: %v", err)
	}
	if !libskillinstall.IsInstalled(claudeDir) {
		t.Fatal("install did not fresh-install into an empty dir")
	}
	if !strings.Contains(out.String(), "boss skills: installed claude") {
		t.Fatalf("output = %q, want an 'installed' line", out.String())
	}
	assertAgentSkillsInstalled(t, claudeDir)
}

// install (without --force) is a no-op on an already-current tree — it does not
// rewrite files, only fresh-installs what is missing.
func TestRunSkillSyncInstallNoOpOnCurrentTree(t *testing.T) {
	home := setupSkillStartupTest(t)
	claudeDir := filepath.Join(home, ".claude", "skills")
	if err := libskillinstall.Extract(claudeDir, bossskillinstall.SkillsFS); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	stamp := filepath.Join(claudeDir, libskillinstall.Namespace, "boss", "SKILL.md")
	before, err := os.Stat(stamp)
	if err != nil {
		t.Fatal(err)
	}
	setAvailableSkillAgents(map[string]bool{"claude": true})

	var out bytes.Buffer
	if err := runSkillSync(&out, skillSyncInstall, ""); err != nil {
		t.Fatalf("runSkillSync install: %v", err)
	}
	if !strings.Contains(out.String(), "boss skills: claude up to date") {
		t.Fatalf("output = %q, want an 'up to date' line", out.String())
	}
	after, err := os.Stat(stamp)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("current tree was rewritten by install")
	}
}

func TestRunSkillSyncForceReinstallsCurrentTree(t *testing.T) {
	home := setupSkillStartupTest(t)
	claudeDir := filepath.Join(home, ".claude", "skills")
	if err := libskillinstall.Extract(claudeDir, bossskillinstall.SkillsFS); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	setAvailableSkillAgents(map[string]bool{"claude": true})

	var out bytes.Buffer
	if err := runSkillSync(&out, skillSyncForce, ""); err != nil {
		t.Fatalf("runSkillSync --force: %v", err)
	}
	if !strings.Contains(out.String(), "boss skills: reinstalled claude") {
		t.Fatalf("output = %q, want a 'reinstalled' line", out.String())
	}
	assertAgentSkillsInstalled(t, claudeDir)
}

func TestRunSkillSyncAgentFilterTouchesOnlySelectedAgent(t *testing.T) {
	home := setupSkillStartupTest(t)
	setAvailableSkillAgents(map[string]bool{"claude": true, "codex": true})

	var out bytes.Buffer
	if err := runSkillSync(&out, skillSyncForce, "codex"); err != nil {
		t.Fatalf("runSkillSync --agent codex: %v", err)
	}
	if libskillinstall.IsInstalled(filepath.Join(home, ".claude", "skills")) {
		t.Fatal("--agent codex touched the claude dir")
	}
	assertAgentSkillsInstalled(t, filepath.Join(home, ".codex", "skills"))
	if strings.Contains(out.String(), "claude") {
		t.Fatalf("output mentioned claude under --agent codex: %q", out.String())
	}
}

func TestRunSkillSyncUnknownAgentErrors(t *testing.T) {
	setupSkillStartupTest(t)
	setAvailableSkillAgents(map[string]bool{"claude": true})

	var out bytes.Buffer
	err := runSkillSync(&out, skillSyncUpdateOnly, "gemini")
	if err == nil {
		t.Fatal("runSkillSync with unknown agent returned nil, want error")
	}
	if !strings.Contains(err.Error(), "unknown agent") {
		t.Fatalf("err = %v, want 'unknown agent'", err)
	}
}

func TestRunSkillSyncSelectedAgentMissingBinaryErrors(t *testing.T) {
	setupSkillStartupTest(t)
	setAvailableSkillAgents(map[string]bool{"claude": true}) // codex absent

	var out bytes.Buffer
	err := runSkillSync(&out, skillSyncUpdateOnly, "codex")
	if err == nil {
		t.Fatal("runSkillSync for an absent selected agent returned nil, want error")
	}
	if !strings.Contains(out.String(), "not found on PATH") {
		t.Fatalf("output = %q, want a 'not found on PATH' note", out.String())
	}
}

// The leaf subcommands select an agent via --agent, so a positional operand like
// `boss skills sync codex` must error (cobra.NoArgs) instead of silently ignoring
// "codex" and mutating every agent's global skill dir on PATH.
func TestSkillsSubcommandsRejectPositionalArgs(t *testing.T) {
	setupSkillStartupTest(t)
	setAvailableSkillAgents(map[string]bool{"claude": true, "codex": true})
	skillInstallReadAnswer = func() string {
		t.Fatal("skills subcommands must not prompt")
		return ""
	}

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"sync positional", []string{"skills", "sync", "codex"}},
		{"install positional", []string{"skills", "install", "codex"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := skillsCmd()
			cmd.SetArgs(tc.args)
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			err := cmd.Execute()
			if err == nil {
				t.Fatalf("%v accepted a positional agent, want an error", tc.args)
			}
			if !strings.Contains(err.Error(), "unknown command") &&
				!strings.Contains(err.Error(), "accepts") {
				t.Fatalf("err = %v, want a cobra positional-args rejection", err)
			}
		})
	}
}
