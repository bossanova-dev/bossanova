package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	gitpkg "github.com/recurser/bossd/internal/adapter/git"
	"github.com/recurser/bossd/internal/domain"
)

// setupResidueWorktree is a worktree whose setup script rewrote lock.json.
func setupResidueWorktree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "lock.json"), []byte("{\"by\":\"setup\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := gitpkg.WriteSetupResidue(dir, []string{"lock.json"}); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestFinalizeSession_SetupResidueIsNotSessionOutput_Sessions_AC236401_AC236402(t *testing.T) {
	t.Run("only setup residue finishes as no changes", func(t *testing.T) {
		lc, sessions, wt, vp, cron := finalizePublishFixture(t, " M lock.json\n")
		sessions.sessions["sess-1"].WorktreePath = setupResidueWorktree(t)
		res, err := lc.FinalizeSession(context.Background(), "sess-1")
		if err != nil {
			t.Fatal(err)
		}
		if res.Outcome != domain.CronJobOutcomeDeletedNoChanges || !res.Deleted {
			t.Fatalf("result = %+v, want deleted_no_changes", res)
		}
		assertFinalizeUnpublished(t, wt, vp)
		if len(cron.lastRunCalls) != 1 || cron.lastRunCalls[0].outcome != domain.CronJobOutcomeDeletedNoChanges {
			t.Fatalf("last run = %+v", cron.lastRunCalls)
		}
	})

	t.Run("setup residue beside committed work still publishes", func(t *testing.T) {
		lc, sessions, wt, vp, _ := finalizePublishFixture(t, " M lock.json\n")
		sessions.sessions["sess-1"].WorktreePath = setupResidueWorktree(t)
		wt.commitSubjects = []string{"fix(bossd): completed work"}
		wt.isAncestorFn = func(_, _, _ string) (bool, error) { return false, nil }
		res, err := lc.FinalizeSession(context.Background(), "sess-1")
		if err != nil || res.Outcome != domain.CronJobOutcomePRCreated {
			t.Fatalf("result=%+v err=%v", res, err)
		}
		if len(wt.pushed) != 1 || len(vp.createPRCalls) != 1 || wt.emptyCommitCalls != 0 {
			t.Fatalf("pushes=%v PRs=%d placeholders=%d", wt.pushed, len(vp.createPRCalls), wt.emptyCommitCalls)
		}
	})

	for name, edit := range map[string]func(t *testing.T, dir string) string{
		"a residue file edited after setup is session output": func(t *testing.T, dir string) string {
			if err := os.WriteFile(filepath.Join(dir, "lock.json"), []byte("{\"by\":\"agent\"}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return " M lock.json\n"
		},
		"another tracked edit beside residue is session output": func(*testing.T, string) string {
			return " M lock.json\n M tracked.go\n"
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := setupResidueWorktree(t)
			lc, sessions, wt, vp, _ := finalizePublishFixture(t, edit(t, dir))
			sessions.sessions["sess-1"].WorktreePath = dir
			res, err := lc.FinalizeSession(context.Background(), "sess-1")
			if err != nil || res.Outcome != domain.CronJobOutcomePRFailed {
				t.Fatalf("result=%+v err=%v, want pr_failed", res, err)
			}
			assertFinalizeUnpublished(t, wt, vp)
		})
	}
}
