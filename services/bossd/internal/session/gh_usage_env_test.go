package session

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/recurser/bossalib/models"

	"github.com/recurser/bossd/internal/adapter/dotenv"
	"github.com/recurser/bossd/internal/ghusage"
)

func installSessionShim(t *testing.T) (*ghusage.Shim, string) {
	t.Helper()
	fakeDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeDir, "gh"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil { // #nosec G306 -- test fixture must be executable
		t.Fatal(err)
	}
	ledgerDir := filepath.Join(t.TempDir(), ghusage.DirName)
	shim, err := ghusage.InstallShim(ledgerDir, fakeDir)
	if err != nil || shim == nil {
		t.Fatalf("InstallShim = (%v, %v), want a wrapper", shim, err)
	}
	return shim, filepath.Join(ledgerDir, "bin")
}

func TestManagedSessionEnvLabelsGHCallsBySessionKind_GitHubUsage_AC220401(t *testing.T) {
	shim, binDir := installSessionShim(t)
	job := "cron-42"
	for _, tc := range []struct {
		name string
		sess *models.Session
		want string
	}{
		{name: "cron", sess: &models.Session{ID: "s1", Title: "bs-verify", CronJobID: &job}, want: "cron:bs-verify"},
		{name: "quick chat", sess: &models.Session{ID: "s2", IsQuickChat: true}, want: "session:quick-chat"},
		{name: "unattended", sess: &models.Session{ID: "s3", IsTmuxUnattended: true}, want: "session:unattended"},
		{name: "detach", sess: &models.Session{ID: "s4", Detach: true}, want: "session:unattended"},
		{name: "interactive", sess: &models.Session{ID: "s5"}, want: "session:interactive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := ManagedSessionEnv(tc.sess, "agent-1", "claude", shim)
			if got := env[ghusage.EnvLabel]; got != tc.want {
				t.Fatalf("label = %q, want %q", got, tc.want)
			}
			if first := strings.SplitN(env["PATH"], string(os.PathListSeparator), 2)[0]; first != binDir {
				t.Fatalf("PATH starts with %q, want the gh wrapper dir %q", first, binDir)
			}
			if want := filepath.Join(binDir, "gh-path.sh"); env[ghusage.EnvClaudeEnvFile] != want {
				t.Fatalf("%s = %q, want the wrapper's PATH script %q", ghusage.EnvClaudeEnvFile, env[ghusage.EnvClaudeEnvFile], want)
			}
			if env["BOSS_SESSION_ID"] != tc.sess.ID {
				t.Fatalf("metering dropped the managed env: %v", env)
			}
		})
	}
}

func TestManagedSessionEnvWithoutWrapperSetsNoMeteringEnv(t *testing.T) {
	env := ManagedSessionEnv(&models.Session{ID: "s1"}, "agent-1", "claude", nil)
	for _, key := range []string{"PATH", ghusage.EnvReal, ghusage.EnvDir, ghusage.EnvLabel, ghusage.EnvClaudeEnvFile} {
		if v, ok := env[key]; ok {
			t.Errorf("%s = %q without an installed wrapper", key, v)
		}
	}
}

// A repo's worktree .env must not take the wrapper off the chat's PATH: the
// spawn path overlays the .env beneath the managed env.
func TestWorktreeDotenvCannotOverrideTheMeteredPath(t *testing.T) {
	shim, binDir := installSessionShim(t)
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, ".env"), []byte("PATH=/evil\nBOSS_GH_USAGE_LABEL=forged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sess := &models.Session{ID: "s1", WorktreePath: worktree}
	env := dotenv.OverlayWithRepo(meteredSessionEnv(ResolveSessionFacts(sess, "agent-1", "claude"), shim), worktree, &models.Repo{IsTrusted: true})
	if first := strings.SplitN(env["PATH"], string(os.PathListSeparator), 2)[0]; first != binDir {
		t.Fatalf("PATH = %q, want the gh wrapper dir first", env["PATH"])
	}
	if env[ghusage.EnvLabel] != "session:interactive" {
		t.Fatalf("label = %q, want session:interactive", env[ghusage.EnvLabel])
	}
}

// The tmux spawn path hands the chat the metered env: the wrapper dir leads
// the pane's PATH and the label names the session kind.
func TestStartTmuxChatPutsTheGHWrapperOnThePanePath_GitHubUsage_AC220401(t *testing.T) {
	shim, binDir := installSessionShim(t)
	h := newStartTmuxChatHarness(t)
	h.lc.SetHookPort(54321)
	h.lc.SetGHUsageShim(shim)
	if _, err := h.lc.StartTmuxChat(t.Context(), "sess-1", ChatInput{Prompt: "hi"}, "Chat", HookOpts{Token: "tok"}); err != nil {
		t.Fatal(err)
	}
	call := h.findCall("new-session")
	if call == nil {
		t.Fatal("no new-session")
	}
	if !slices.Contains(call.args, ghusage.EnvLabel+"=session:interactive") {
		t.Errorf("new-session args lack the session:interactive label: %v", call.args)
	}
	if !slices.ContainsFunc(call.args, func(arg string) bool {
		return strings.HasPrefix(arg, "PATH="+binDir+string(os.PathListSeparator))
	}) {
		t.Errorf("new-session args lack a PATH led by %s: %v", binDir, call.args)
	}
}
