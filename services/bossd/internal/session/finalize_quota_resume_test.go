package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sessionreason"
	"github.com/recurser/bossd/internal/domain"
	"github.com/rs/zerolog"
)

type finalizeQuotaTestError struct{ reset time.Time }

func (e *finalizeQuotaTestError) Error() string        { return "quota exhausted" }
func (e *finalizeQuotaTestError) Is(target error) bool { return target == vcs.ErrRateLimited }
func (e *finalizeQuotaTestError) ResetAt() time.Time   { return e.reset }

func newQuotaFinalizeLifecycle(t *testing.T) (*Lifecycle, *runBlockedFixture) {
	t.Helper()
	f := newRunBlockedFixture()
	f.sessions.sessions["sess-1"].AgentSessionID = nil
	f.wt.hasDiffAgainstBase, f.wt.hasDiffAgainstBaseOK = true, true
	f.wt.commitSubjects = []string{"feat(x): real work"}
	f.wt.latestCommitSubject = "feat(x): real work"
	f.wt.isAncestorFn = func(_, ref, target string) (bool, error) {
		return ref != "HEAD" || target != "refs/remotes/origin/main", nil
	}
	l := newTestLifecycle(f.sessions, f.repos, nil, f.cron, f.wt, newMockAgentRunner(), nil, f.vp, zerolog.Nop())
	return l, f
}

func assertQuotaBlocked(t *testing.T, f *runBlockedFixture, reset time.Time) {
	t.Helper()
	sess := f.sessions.sessions["sess-1"]
	if sess.State != machine.Blocked || sess.BlockedReason == nil {
		t.Fatalf("session not blocked: %+v", sess)
	}
	got, ok := sessionreason.ParseFinalizeQuotaWait(*sess.BlockedReason)
	if !ok || !got.Equal(reset) {
		t.Fatalf("blocked reset = %v, %v; want %v (reason %s)", got, ok, reset, *sess.BlockedReason)
	}
}

func TestFinalizeQuotaResume_Sessions_AC220101(t *testing.T) {
	ctx := context.Background()
	reset := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	l, f := newQuotaFinalizeLifecycle(t)
	f.vp.markReadyErr = &finalizeQuotaTestError{reset}
	result, err := l.FinalizeSession(ctx, "sess-1")
	if err != nil || result.Outcome != domain.CronJobOutcomePRFailed {
		t.Fatalf("finalize = %+v, %v", result, err)
	}
	assertQuotaBlocked(t, f, reset)
	now := reset.Add(-time.Minute)
	l.SetClockForTest(func() time.Time { return now })
	for _, before := range []time.Time{now, reset, reset.Add(29 * time.Second)} {
		now = before
		n, err := l.ResumeQuotaBlockedFinalizesPeriodic(ctx)
		if err != nil || n != 0 {
			t.Fatalf("early sweep = %d, %v", n, err)
		}
	}
	f.vp.markReadyErr = nil
	now = reset.Add(30 * time.Second)
	n, err := l.ResumeQuotaBlockedFinalizesPeriodic(ctx)
	if err != nil || n != 1 {
		t.Fatalf("resume = %d, %v", n, err)
	}
	sess := f.sessions.sessions["sess-1"]
	if sess.State != machine.ReadyForReview || sess.BlockedReason != nil || len(f.vp.markReadyCalls) != 2 {
		t.Fatalf("resume did not ready and clear the session: %+v; ready calls %v", sess, f.vp.markReadyCalls)
	}
}

func TestFinalizeQuotaResumeRestartAndSecondLimit(t *testing.T) {
	ctx := context.Background()
	reset := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	l, f := newQuotaFinalizeLifecycle(t)
	// Start directly from the persisted row, as a fresh daemon does.
	reason := sessionreason.FinalizeQuotaWait(reset)
	f.sessions.sessions["sess-1"].State = machine.Blocked
	f.sessions.sessions["sess-1"].BlockedReason = &reason
	now := reset.Add(time.Minute)
	l.SetClockForTest(func() time.Time { return now })
	next := reset.Add(time.Hour)
	f.vp.markReadyErr = &finalizeQuotaTestError{next}
	n, err := l.ResumeQuotaBlockedFinalizesPeriodic(ctx)
	if err != nil || n != 1 {
		t.Fatalf("resume = %d, %v", n, err)
	}
	assertQuotaBlocked(t, f, next)
	now = next.Add(time.Minute)
	f.vp.markReadyErr = nil
	n, err = l.ResumeQuotaBlockedFinalizesPeriodic(ctx)
	if err != nil || n != 1 || f.sessions.sessions["sess-1"].State != machine.ReadyForReview {
		t.Fatalf("second resume = %d, %v", n, err)
	}
}

func TestFinalizeQuotaResumeSkipsOtherBlocksAndOwners(t *testing.T) {
	for _, mode := range []string{"generic", "run-blocked", "other-owner", "archived", "not-blocked"} {
		t.Run(mode, func(t *testing.T) {
			l, f := newQuotaFinalizeLifecycle(t)
			reset := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
			reason := sessionreason.FinalizeQuotaWait(reset)
			sess := f.sessions.sessions["sess-1"]
			sess.State, sess.BlockedReason = machine.Blocked, &reason
			switch mode {
			case "generic":
				reason = sessionreason.FinalizeFailure("pr_failed", errors.New("forbidden"))
			case "run-blocked":
				report := "needs a person"
				sess.RunBlockedReason = &report
			case "other-owner":
				pr := 77
				sess.PRNumber = &pr
				f.sessions.sessions["peer"] = &models.Session{ID: "peer", RepoID: sess.RepoID, PRNumber: &pr, BranchName: sess.BranchName, State: machine.ImplementingPlan}
			case "archived":
				sess.ArchivedAt = &reset
			case "not-blocked":
				sess.State = machine.ReadyForReview
			}
			l.SetClockForTest(func() time.Time { return reset.Add(time.Hour) })
			n, err := l.ResumeQuotaBlockedFinalizesPeriodic(context.Background())
			if err != nil || n != 0 || len(f.vp.markReadyCalls) != 0 {
				t.Fatalf("ineligible resume = %d, %v, ready calls %v", n, err, f.vp.markReadyCalls)
			}
		})
	}
}

func TestFinalizeQuotaCreateFailureAndReportedBlock(t *testing.T) {
	for _, reported := range []bool{false, true} {
		t.Run(map[bool]string{false: "quota", true: "reported-block"}[reported], func(t *testing.T) {
			l, f := newQuotaFinalizeLifecycle(t)
			reset := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
			f.vp.nextOpenPRs = nil
			f.vp.createPRErr = &finalizeQuotaTestError{reset}
			if reported {
				report := "requires human review"
				f.sessions.sessions["sess-1"].RunBlockedReason = &report
			}
			result, err := l.FinalizeSession(context.Background(), "sess-1")
			if err != nil || result.Outcome != domain.CronJobOutcomePRFailed {
				t.Fatalf("create failure = %+v, %v", result, err)
			}
			if !reported {
				assertQuotaBlocked(t, f, reset)
				f.vp.createPRErr = nil
				l.SetClockForTest(func() time.Time { return reset.Add(time.Minute) })
				n, err := l.ResumeQuotaBlockedFinalizesPeriodic(context.Background())
				sess := f.sessions.sessions["sess-1"]
				if err != nil || n != 1 || sess.State != machine.ReadyForReview || sess.PRNumber == nil || sess.BlockedReason != nil {
					t.Fatalf("PR-create quota resume = %d, %v, session %+v", n, err, sess)
				}
				return
			}
			sess := f.sessions.sessions["sess-1"]
			if sess.State != machine.Blocked || sess.BlockedReason == nil {
				t.Fatal("reported run was not blocked")
			}
			if _, ok := sessionreason.ParseFinalizeQuotaWait(*sess.BlockedReason); ok {
				t.Fatal("human-blocked run became auto-resumable")
			}
		})
	}
}

func TestFinalizeQuotaResumeCooldownAndCap(t *testing.T) {
	l, f := newQuotaFinalizeLifecycle(t)
	reset := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	now := reset.Add(time.Minute)
	l.SetClockForTest(func() time.Time { return now })
	f.vp.markReadyErr = &finalizeQuotaTestError{reset}
	reason := sessionreason.FinalizeQuotaWait(reset)
	for _, id := range []string{"sess-1", "sess-2", "sess-3", "sess-4"} {
		sess := *f.sessions.sessions["sess-1"]
		sess.ID, sess.RepoID, sess.BranchName = id, id, id
		sess.State, sess.BlockedReason = machine.Blocked, &reason
		f.sessions.sessions[id] = &sess
		repo := *f.repos.repos["repo-1"]
		repo.ID = id
		f.repos.repos[id] = &repo
	}
	n, err := l.ResumeQuotaBlockedFinalizesPeriodic(context.Background())
	if err != nil || n != 3 {
		t.Fatalf("capped resume = %d, %v", n, err)
	}
	n, err = l.ResumeQuotaBlockedFinalizesPeriodic(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("cooldown resume = %d, %v", n, err)
	}
	n, err = l.ResumeQuotaBlockedFinalizesPeriodic(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("cooldown re-entry = %d, %v", n, err)
	}
	now = now.Add(5 * time.Minute)
	n, err = l.ResumeQuotaBlockedFinalizesPeriodic(context.Background())
	if err != nil || n != 3 {
		t.Fatalf("expired cooldown = %d, %v", n, err)
	}
}

func TestFinalizeQuotaResumeCancellation(t *testing.T) {
	l, f := newQuotaFinalizeLifecycle(t)
	reset := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	reason := sessionreason.FinalizeQuotaWait(reset)
	f.sessions.sessions["sess-1"].State = machine.Blocked
	f.sessions.sessions["sess-1"].BlockedReason = &reason
	l.SetClockForTest(func() time.Time { return reset.Add(time.Hour) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n, err := l.ResumeQuotaBlockedFinalizesPeriodic(ctx)
	if err != nil || n != 0 || f.sessions.sessions["sess-1"].State != machine.Blocked || len(f.vp.markReadyCalls) != 0 {
		t.Fatalf("cancelled sweep = %d, %v", n, err)
	}
}
