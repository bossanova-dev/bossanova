package session

import (
	"context"
	"strings"
	"testing"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/domain"
	"github.com/rs/zerolog"
)

func finalizePublishFixture(t *testing.T, status string) (*Lifecycle, *mockSessionStore, *mockWorktreeManager, *mockVCSProvider, *recordingCronJobStore) {
	t.Helper()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", LocalPath: "/tmp/repo-main", OriginURL: "git@github.com:owner/repo.git"}
	cronID, token := "cron-1", "secret-token"
	sessions.sessions["sess-1"] = &models.Session{
		ID: "sess-1", RepoID: "repo-1", Title: "Cron job", Plan: "/boss-build private plan",
		WorktreePath: "/tmp/wt-sess1", BranchName: "cron-br-1", BaseBranch: "main",
		State: machine.ImplementingPlan, CronJobID: &cronID, HookToken: &token,
	}
	wt := &mockWorktreeManager{statusOut: status, latestCommitSubject: "fix(bossd): complete work"}
	vp := newMockVCSProvider()
	cron := &recordingCronJobStore{}
	lc := newFinalizeChatLifecycle(t, sessions, repos, &recordingAgentChatStore{}, cron, wt, vp, zerolog.Nop())
	return lc, sessions, wt, vp, cron
}

func TestFinalizeSession_DirtyTrackedOutputPublishesNothing_Sessions_AC114(t *testing.T) {
	entries := []struct {
		name   string
		finish func(*Lifecycle) (*FinalizeResult, error)
	}{
		{"stop", func(l *Lifecycle) (*FinalizeResult, error) { return l.FinalizeSession(context.Background(), "sess-1") }},
		{"recovery", func(l *Lifecycle) (*FinalizeResult, error) {
			return l.finalizeSessionFrom(context.Background(), "sess-1", strandedReapStateInts(), finalizeTriggerStrandedRecovery)
		}},
		{"finish", func(l *Lifecycle) (*FinalizeResult, error) {
			return l.FinishUnattendedRun(context.Background(), "sess-1")
		}},
	}
	for _, entry := range entries {
		for _, status := range []string{" M tracked.go\n", "?? scratch\n M tracked.go\n", "M  tracked.go\n"} {
			t.Run(entry.name+"/"+status, func(t *testing.T) {
				lc, sessions, wt, vp, cron := finalizePublishFixture(t, status)
				res, err := entry.finish(lc)
				if err != nil {
					t.Fatal(err)
				}
				if res.Outcome != domain.CronJobOutcomePRFailed {
					t.Fatalf("outcome = %q", res.Outcome)
				}
				if res.Err == nil || !strings.Contains(res.Err.Error(), "uncommitted tracked changes") || !strings.Contains(res.Err.Error(), "nothing was pushed or opened") {
					t.Fatalf("error = %v", res.Err)
				}
				assertFinalizeUnpublished(t, wt, vp)
				sess := sessions.sessions["sess-1"]
				if sess.State != machine.Blocked || sess.HookToken == nil {
					t.Fatalf("state=%v token=%v", sess.State, sess.HookToken)
				}
				if len(wt.archived) != 0 {
					t.Fatal("worktree was archived")
				}
				if len(cron.lastRunCalls) != 1 || cron.lastRunCalls[0].outcome != domain.CronJobOutcomePRFailed {
					t.Fatalf("last run = %+v", cron.lastRunCalls)
				}
			})
		}
	}
	t.Run("interactive", func(t *testing.T) {
		lc, sessions, wt, vp, _ := finalizePublishFixture(t, " M tracked.go\n")
		sessions.sessions["sess-1"].CronJobID = nil
		res, err := lc.FinalizeSession(context.Background(), "sess-1")
		if err != nil || res.Outcome != domain.CronJobOutcomePRFailed {
			t.Fatalf("result=%+v err=%v", res, err)
		}
		assertFinalizeUnpublished(t, wt, vp)
	})
}

func assertFinalizeUnpublished(t *testing.T, wt *mockWorktreeManager, vp *mockVCSProvider) {
	t.Helper()
	if len(wt.pushed) != 0 || len(vp.createPRCalls) != 0 || wt.emptyCommitCalls != 0 || len(wt.injectPRNumbersCalls) != 0 || len(vp.markReadyCalls) != 0 {
		t.Fatalf("published: pushes=%v PRs=%d placeholders=%d injections=%d ready=%v", wt.pushed, len(vp.createPRCalls), wt.emptyCommitCalls, len(wt.injectPRNumbersCalls), vp.markReadyCalls)
	}
}

func TestFinalizeSession_DirtyTrackedExistingPRRepairsTitle_Sessions_AC114(t *testing.T) {
	lc, sessions, wt, vp, _ := finalizePublishFixture(t, " M tracked.go\n")
	number := 42
	sessions.sessions["sess-1"].PRNumber = &number
	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, Title: "[BLOCKED] Completed work"}
	lc.cronJobs = &namedCronJobStore{name: "Cron job"}
	res, err := lc.FinalizeSession(context.Background(), "sess-1")
	if err != nil || res.Outcome != domain.CronJobOutcomePRFailed {
		t.Fatalf("result=%+v err=%v", res, err)
	}
	assertFinalizeUnpublished(t, wt, vp)
	if got := sessions.sessions["sess-1"].Title; got != "Completed work" {
		t.Fatalf("title = %q", got)
	}
}

func TestFinalizeSession_UntrackedCommittedOutputPublishes_Sessions_AC114(t *testing.T) {
	lc, _, wt, vp, _ := finalizePublishFixture(t, "?? scratch/notes.md\n")
	wt.commitSubjects = []string{"fix(bossd): completed work"}
	wt.isAncestorFn = func(_, _, _ string) (bool, error) { return false, nil }
	res, err := lc.FinalizeSession(context.Background(), "sess-1")
	if err != nil || res.Outcome != domain.CronJobOutcomePRCreated {
		t.Fatalf("result=%+v err=%v", res, err)
	}
	if len(wt.pushed) != 1 || len(vp.createPRCalls) != 1 || len(wt.injectPRNumbersCalls) != 1 || wt.emptyCommitCalls != 0 {
		t.Fatalf("pushes=%v PRs=%d injection=%d placeholders=%d", wt.pushed, len(vp.createPRCalls), len(wt.injectPRNumbersCalls), wt.emptyCommitCalls)
	}
}

func TestFinalizeSession_RecoveryPRBody_Sessions_AC115(t *testing.T) {
	lc, sessions, wt, vp, _ := finalizePublishFixture(t, "")
	wt.commitSubjects = []string{"fix(bossd): completed work"}
	wt.isAncestorFn = func(_, _, _ string) (bool, error) { return false, nil }
	sess := sessions.sessions["sess-1"]
	res, err := lc.finalizeSessionFrom(context.Background(), sess.ID, strandedReapStateInts(), finalizeTriggerStrandedRecovery)
	if err != nil || res.Outcome != domain.CronJobOutcomePRCreated {
		t.Fatalf("result=%+v err=%v", res, err)
	}
	if len(vp.createPRCalls) != 1 {
		t.Fatalf("PR calls=%d", len(vp.createPRCalls))
	}
	body := vp.createPRCalls[0].Body
	if body != finalizePRBody(sess, finalizeTriggerStrandedRecovery) || body == finalizePRBody(sess, finalizeTriggerRunEnd) || !strings.Contains(body, sess.ID) || strings.Contains(body, sess.Plan) {
		t.Fatalf("body=%q", body)
	}
}

func TestFinalizePRBody_LinkedSessionLeadsWithVerifyTicketLine(t *testing.T) {
	id, url := "BOS-1945", "https://linear.app/acme/issue/BOS-1945/add-a-registry"
	sess := &models.Session{ID: "session-provenance", Plan: "/boss-build headless", TrackerID: &id, TrackerURL: &url}
	for _, trigger := range []finalizeTrigger{finalizeTriggerRunEnd, finalizeTriggerStrandedRecovery, finalizeTriggerFinishRequest, finalizeTriggerBlockedFinish, finalizeTriggerArchive} {
		body := finalizePRBody(sess, trigger)
		first, rest, _ := strings.Cut(body, "\n")
		if first != "Tracker issue: "+url || !strings.Contains(rest, sess.ID) || strings.Contains(body, sess.Plan) {
			t.Fatalf("trigger %d body=%q", trigger, body)
		}
	}
}

func TestFinalizePRBody_Sessions_AC115(t *testing.T) {
	seen := map[string]bool{}
	ticket := "BOS-2014"
	for _, trigger := range []finalizeTrigger{finalizeTriggerRunEnd, finalizeTriggerStrandedRecovery, finalizeTriggerFinishRequest, finalizeTriggerBlockedFinish, finalizeTriggerArchive} {
		for _, tracked := range []bool{false, true} {
			sess := &models.Session{ID: "session-provenance", Plan: "/boss-build private prompt"}
			if tracked {
				sess.TrackerID = &ticket
			}
			body := finalizePRBody(sess, trigger)
			if body == "" || !strings.Contains(body, sess.ID) || !strings.Contains(body, "bossd") || strings.Contains(body, sess.Plan) || strings.Contains(body, "## Partial") || strings.Contains(body, ticket) != tracked {
				t.Fatalf("body=%q", body)
			}
			if !tracked {
				if seen[body] {
					t.Fatalf("duplicate trigger body: %q", body)
				}
				seen[body] = true
			}
		}
	}
}
