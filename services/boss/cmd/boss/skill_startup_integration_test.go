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

	"github.com/recurser/boss/api/cli/skills"
	libskillinstall "github.com/recurser/bossalib/contract/plugin/skillinstall"
	"github.com/recurser/bossalib/contract/plugin/skillpayload"
)

func setupStartupGroup(t *testing.T, g *skills.Group) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Setenv("BOSS_SETTINGS_PATH", filepath.Join(home, "settings.json"))
	t.Setenv("BOSS_SKIP_SKILLS", "")
	t.Setenv("HERMES_HOME", "")
	t.Setenv("BOSS_SOCKET", filepath.Join(home, "no-such-bossd.sock"))
	g.StartupIsTerminal = func() bool { return true }
	return home
}
func setStartupAgents(g *skills.Group, available map[string]bool) {
	g.StartupLookPath = func(command string) (string, error) {
		if available[command] {
			return "/usr/bin/" + command, nil
		}
		return "", errors.New("not found")
	}
}
func TestGenSkillSkipsStartupSkillInstall(t *testing.T) {
	g := newSkillsGroup()

	setupStartupGroup(t, g)
	setStartupAgents(g, map[string]bool{"codex": true})
	g.StartupReadAnswer = func() string {
		t.Fatal("gen-skill should not prompt for skill installation")
		return ""
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	t.Chdir(filepath.Dir(filepath.Dir(filepath.Dir(file))))

	root := rootCmdWithSkills(g)
	root.SetArgs([]string{"gen-skill", "--check"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	if err := root.Execute(); err != nil {
		t.Fatalf("gen-skill --check: %v", err)
	}
}

func TestSkillsCommandBypassesStartupInstaller(t *testing.T) {
	g := newSkillsGroup()

	home := setupStartupGroup(t, g)
	setStartupAgents(g, map[string]bool{"claude": true})
	// The startup installer must never run for the `skills` subtree: neither the
	// interactive [Y/n] prompt nor the non-TTY self-heal may pre-empt the command.
	g.StartupReadAnswer = func() string {
		t.Fatal("skills subtree must not trigger the startup prompt")
		return ""
	}
	// Interactive terminal: if the pre-run installer ran, it would prompt (fatal
	// above). A stale-but-installed tree also proves the self-heal did not run
	// before the command: `boss skills sync` reports "updated", not "up to date".
	g.StartupIsTerminal = func() bool { return true }
	claudeDir := filepath.Join(home, ".claude", "skills")
	if err := libskillinstall.Extract(claudeDir, skillpayload.SkillsFS); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	stalePath := filepath.Join(claudeDir, libskillinstall.Namespace, "boss", "SKILL.md")
	if err := os.WriteFile(stalePath, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	root := rootCmdWithSkills(g)
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

// TestTailBypassesStartupSkillInstaller drives `boss tail` through the real
// rootCmd() so PersistentPreRunE is the thing under test. Building tailCmd()
// directly — the idiom every test in tail_test.go uses — never runs the root
// pre-run at all, so a bypass test written that way would be vacuously green.
//
// tail joins fix-terminal's rationale family rather than gen-skill's: it is a
// diagnostic, and on an interactive tty the startup installer writes a [Y/n]
// prompt to stderr and then blocks in fmt.Scanln. That stalls the tail before a
// single log line appears — and under `boss tail -f | head`, where stdout is the
// pipe but stdin and stderr are still the terminal, head never receives a line
// and the process simply sits there. See BOS-921.
func TestTailBypassesStartupSkillInstaller(t *testing.T) {
	g := newSkillsGroup()

	setupStartupGroup(t, g)
	setStartupAgents(g, map[string]bool{"claude": true, "codex": true})
	g.StartupReadAnswer = func() string {
		t.Error("boss tail must not trigger the startup skill prompt")
		return ""
	}
	// An interactive tty with nothing installed yet. This is the only branch
	// that can block: leave it non-TTY and the silent selfHealSkills path runs
	// instead, which proves nothing about the reported hang.
	g.StartupIsTerminal = func() bool { return true }

	root := rootCmdWithSkills(g)
	var out bytes.Buffer
	root.SetArgs([]string{"tail"})
	root.SetOut(&out)
	root.SetErr(io.Discard)

	var err error
	// The prompt goes to os.Stderr directly, not to cmd.ErrOrStderr(), so a
	// cobra buffer could never hold it and asserting on one would be vacuous.
	stderr := captureStderr(t, func() { err = root.Execute() })
	if err != nil {
		t.Fatalf("boss tail: %v", err)
	}
	if strings.Contains(stderr, "[Y/n]") {
		t.Errorf("stderr = %q, want no installer prompt", stderr)
	}
	// XDG_STATE_HOME is an empty temp dir, so there are no logs to print — the
	// same property TestTailNoLogsAndJSONOutput relies on.
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want no output with no logs present", out.String())
	}
}

func TestCostBypassesStartupSkillInstaller(t *testing.T) {
	g := newSkillsGroup()

	setupStartupGroup(t, g)
	setStartupAgents(g, map[string]bool{"claude": true, "codex": true})
	g.StartupReadAnswer = func() string {
		t.Error("boss cost must not trigger the startup skill prompt")
		return ""
	}
	g.StartupIsTerminal = func() bool { return true }

	root := rootCmdWithSkills(g)
	root.SetArgs([]string{"cost"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)

	var err error
	stderr := captureStderr(t, func() { err = root.Execute() })
	if strings.Contains(stderr, "[Y/n]") {
		t.Errorf("stderr = %q, want no installer prompt", stderr)
	}
	if err == nil {
		t.Fatal("boss cost succeeded against the test's dead socket; it cannot have dialled, so the no-prompt assertion above proves nothing")
	}
}

// TestInitCommandIsGrouped guards the skill extractor's hard-error: every
// non-hidden command needs a GroupID drawn from clidoc.GroupOrder, and this one
// must reuse the existing `skills` group rather than mint a new one.
func TestInitCommandIsGrouped(t *testing.T) {
	g := newSkillsGroup()

	root := rootCmdWithSkills(g)
	var found bool
	for _, c := range root.Commands() {
		if c.Name() != "init" {
			continue
		}
		found = true
		if c.GroupID == "" {
			t.Fatal("boss init carries no GroupID; the skill extractor hard-errors on that")
		}
		if c.GroupID != "skills" {
			t.Errorf("boss init GroupID = %q, want the existing \"skills\" group", c.GroupID)
		}
		if !strings.Contains(c.Short, ".boss-skills.json") {
			t.Errorf("boss init short help does not name its file: %q", c.Short)
		}
	}
	if !found {
		t.Fatal("boss init is not registered on the root command")
	}
}

// TestInitBypassesStartupSkillPrompt: boss init offers `boss skills install`
// itself through the cron preflight, so the root pre-run must not prompt.
func TestInitBypassesStartupSkillPrompt(t *testing.T) {
	g := newSkillsGroup()

	setupStartupGroup(t, g)
	setStartupAgents(g, map[string]bool{"claude": true, "codex": true})
	prompted := false
	g.StartupReadAnswer = func() string {
		prompted = true
		return "n"
	}
	g.StartupIsTerminal = func() bool { return true }

	root := rootCmdWithSkills(g)
	initC, _, err := root.Find([]string{"init"})
	if err != nil || initC.CommandPath() != "boss init" {
		t.Fatalf("find boss init: %v", err)
	}
	var preErr error
	stderr := captureStderr(t, func() { preErr = root.PersistentPreRunE(initC, nil) })
	if preErr != nil {
		t.Fatalf("pre-run: %v", preErr)
	}
	if prompted || strings.Contains(stderr, "[Y/n]") {
		t.Fatalf("boss init reached the startup skills prompt (stderr %q)", stderr)
	}

	// Non-vacuity: the same pre-run on a command without the bypass prompts.
	other, _, err := root.Find([]string{"version"})
	if err != nil {
		t.Fatal(err)
	}
	captureStderr(t, func() { _ = root.PersistentPreRunE(other, nil) })
	if !prompted {
		t.Fatal("the startup prompt never fires here, so the assertion above proves nothing")
	}
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStderr := os.Stderr
	os.Stderr = write
	t.Cleanup(func() { os.Stderr = oldStderr })

	fn()
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(read)
	if err != nil {
		t.Fatal(err)
	}
	return string(got)
}
