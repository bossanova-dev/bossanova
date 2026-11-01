package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/domain"
)

func TestFinalizeSession_OnceReadyPreservesDraft_Sessions_AC218002(t *testing.T) {
	for _, tt := range []struct {
		name    string
		hasDiff bool
		outcome domain.CronJobOutcome
		state   machine.State
	}{
		{"real change", true, domain.CronJobOutcomePRCreated, machine.ReadyForReview},
		{"empty diff backstop", false, domain.CronJobOutcomePRNoChanges, machine.Blocked},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			sessions := newMockSessionStore()
			repos := newMockRepoStore()
			wt := &mockWorktreeManager{
				commitSubjects:       []string{"feat(session): retain draft choice"},
				hasDiffAgainstBase:   tt.hasDiff,
				hasDiffAgainstBaseOK: true,
			}
			vp := newMockVCSProvider()
			vp.nextOpenPRs = []vcs.PRSummary{
				{Number: 93, HeadBranch: "preserve-draft", State: vcs.PRStateOpen},
			}
			repos.repos["repo-1"] = &models.Repo{
				ID:        "repo-1",
				LocalPath: "/tmp/repo-main",
				OriginURL: "git@github.com:owner/repo.git",
			}
			readiedAt := time.Now().Add(-time.Hour)
			sessions.sessions["sess-1"] = &models.Session{
				ID:           "sess-1",
				RepoID:       "repo-1",
				WorktreePath: "/tmp/wt-sess1",
				BranchName:   "preserve-draft",
				BaseBranch:   "main",
				State:        machine.ImplementingPlan,
				PRReadiedAt:  &readiedAt,
			}
			lc := newFinalizeChatLifecycle(t, sessions, repos, &recordingAgentChatStore{}, &recordingCronJobStore{}, wt, vp, zerolog.Nop())
			res, err := lc.FinalizeSession(ctx, "sess-1")
			if err != nil {
				t.Fatalf("FinalizeSession: %v", err)
			}
			if res.Outcome != tt.outcome {
				t.Fatalf("outcome = %q, want %q", res.Outcome, tt.outcome)
			}
			if tt.hasDiff && res.Err != nil {
				t.Fatalf("finalize result error = %v, want nil", res.Err)
			}
			if !tt.hasDiff && !errors.Is(res.Err, errEmptyDiffRefusedReady) {
				t.Fatalf("finalize result error = %v, want empty diff refusal", res.Err)
			}
			if len(vp.markReadyCalls) != 0 {
				t.Errorf("MarkReadyForReview calls = %v, want none for a once-ready PR", vp.markReadyCalls)
			}
			if len(wt.hasDiffAgainstBaseRefs) == 0 {
				t.Error("empty-diff backstop did not run")
			}
			cur, err := sessions.Get(ctx, "sess-1")
			if err != nil {
				t.Fatalf("Get session: %v", err)
			}
			if cur.State != tt.state {
				t.Errorf("state = %v, want %v", cur.State, tt.state)
			}
		})
	}
}
