package session

import (
	"context"
	"errors"
	"testing"

	"github.com/recurser/bossd/internal/domain"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
)

func TestFinalizeSession_CleanWorktreeExistingBranchPR_DependabotSkipsTags_Sessions_AC129(t *testing.T) {
	ctx := context.Background()
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{
		statusOut:            "",
		commitSubjects:       []string{"Bump react from 18.0.0 to 18.1.0", "Fix dependency compatibility"},
		hasDiffAgainstBase:   true,
		hasDiffAgainstBaseOK: true,
	}
	vp := newMockVCSProvider()
	const branch = "dependabot/npm/react-18.1.0"
	vp.nextOpenPRs = []vcs.PRSummary{{Number: 77, HeadBranch: branch, State: vcs.PRStateOpen}}
	repos.repos["repo-1"] = &models.Repo{ID: "repo-1", LocalPath: "/tmp/repo-main", OriginURL: "git@github.com:owner/repo.git"}
	token := "hook-token"
	sessions.sessions["sess-1"] = &models.Session{
		ID: "sess-1", RepoID: "repo-1", WorktreePath: "/tmp/wt-sess1",
		BranchName: branch, BaseBranch: "main", State: machine.ImplementingPlan, HookToken: &token,
	}
	lc := newFinalizeChatLifecycle(t, sessions, repos, &recordingAgentChatStore{}, &recordingCronJobStore{}, wt, vp, zerolog.Nop())
	res, err := lc.FinalizeSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}
	if res.Outcome != domain.CronJobOutcomePRCreated {
		t.Fatalf("outcome = %q, want pr_created", res.Outcome)
	}
	if len(wt.injectPRNumbersCalls) != 0 {
		t.Fatalf("InjectPRNumbers calls = %d, want 0", len(wt.injectPRNumbersCalls))
	}
	if len(vp.markReadyCalls) != 1 || vp.markReadyCalls[0] != 77 {
		t.Fatalf("MarkReadyForReview calls = %v, want [77]", vp.markReadyCalls)
	}
	if len(wt.pushed) != 1 || wt.pushed[0] != branch {
		t.Fatalf("Push calls = %v, want [%s]", wt.pushed, branch)
	}
	if got := sessions.sessions["sess-1"].State; got != machine.ReadyForReview {
		t.Fatalf("state = %s, want ready_for_review", got)
	}
}

func TestFinalizeSession_DependabotPublicationFailurePreservesRepair(t *testing.T) {
	pushErr := errors.New("non-fast-forward: remote changed")
	fetchErr := errors.New("base unavailable")
	for _, tc := range []struct {
		name                       string
		pushErr, fetchErr, wantErr error
		wantPushes                 int
	}{
		{name: "push failed", pushErr: pushErr, wantErr: pushErr, wantPushes: 1},
		{name: "fetch failed", fetchErr: fetchErr, wantErr: fetchErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			sessions := newMockSessionStore()
			repos := newMockRepoStore()
			wt := &mockWorktreeManager{commitSubjects: []string{"Bump react", "Fix dependency compatibility"}, pushErr: tc.pushErr, fetchBaseErr: tc.fetchErr}
			vp := newMockVCSProvider()
			const branch = "dependabot/npm/react-18.1.0"
			vp.nextOpenPRs = []vcs.PRSummary{{Number: 77, HeadBranch: branch, State: vcs.PRStateOpen}}
			repos.repos["repo-1"] = &models.Repo{ID: "repo-1", LocalPath: "/tmp/repo-main", OriginURL: "git@github.com:owner/repo.git"}
			token := "hook-token"
			sessions.sessions["sess-1"] = &models.Session{ID: "sess-1", RepoID: "repo-1", WorktreePath: "/tmp/wt-sess1", BranchName: branch, BaseBranch: "main", State: machine.ImplementingPlan, HookToken: &token}
			lc := newFinalizeChatLifecycle(t, sessions, repos, &recordingAgentChatStore{}, &recordingCronJobStore{}, wt, vp, zerolog.Nop())
			res, err := lc.FinalizeSession(ctx, "sess-1")
			if err != nil {
				t.Fatalf("FinalizeSession: %v", err)
			}
			if res.Outcome != domain.CronJobOutcomePRFailed || !errors.Is(res.Err, tc.wantErr) {
				t.Fatalf("result = %+v, want pr_failed preserving publication error", res)
			}
			if len(wt.pushed) != tc.wantPushes || len(wt.injectPRNumbersCalls) != 0 {
				t.Fatalf("pushes=%v injections=%v", wt.pushed, wt.injectPRNumbersCalls)
			}
			if len(vp.markReadyCalls) != 0 || sessions.sessions["sess-1"].State == machine.ReadyForReview {
				t.Fatal("failed publication must not ready the PR or session")
			}
			if len(wt.archived) != 0 {
				t.Fatalf("repair worktree removed: %v", wt.archived)
			}
		})
	}
}

func TestDependabotPublicationRequiresSafeWorktree(t *testing.T) {
	for _, tc := range []struct {
		name, path, status string
		statusErr          error
	}{
		{name: "tracked edit", path: "/tmp/wt", status: " M repair.go"},
		{name: "unknown status", path: "/tmp/wt", statusErr: errors.New("status unavailable")},
		{name: "missing worktree"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessions := newMockSessionStore()
			pr := 77
			sessions.sessions["sess-1"] = &models.Session{ID: "sess-1", PRNumber: &pr, BranchName: "dependabot/npm/react", BaseBranch: "main", WorktreePath: tc.path}
			wt := &mockWorktreeManager{statusOut: tc.status, statusErr: tc.statusErr}
			lc := newFinalizeChatLifecycle(t, sessions, newMockRepoStore(), &recordingAgentChatStore{}, &recordingCronJobStore{}, wt, newMockVCSProvider(), zerolog.Nop())
			if err := lc.injectPRTagsAndPush(context.Background(), "sess-1"); err == nil {
				t.Fatal("unsafe worktree accepted")
			}
			if len(wt.pushed) != 0 || len(wt.injectPRNumbersCalls) != 0 {
				t.Fatal("unsafe worktree published")
			}
		})
	}
}

func TestDependabotPublicationRefreshesBase(t *testing.T) {
	for _, fetchErr := range []error{nil, errors.New("base unavailable")} {
		name := "fresh base"
		if fetchErr != nil {
			name = "fetch failed"
		}
		t.Run(name, func(t *testing.T) {
			sessions := newMockSessionStore()
			pr := 77
			const branch = "dependabot/npm/react"
			sessions.sessions["sess-1"] = &models.Session{ID: "sess-1", PRNumber: &pr, BranchName: branch, BaseBranch: "main", WorktreePath: "/tmp/wt"}
			wt := &mockWorktreeManager{fetchBaseErr: fetchErr}
			vp := newMockVCSProvider()
			lc := newFinalizeChatLifecycle(t, sessions, newMockRepoStore(), &recordingAgentChatStore{}, &recordingCronJobStore{}, wt, vp, zerolog.Nop())
			err := lc.injectPRTagsAndPush(context.Background(), "sess-1")
			if !errors.Is(err, fetchErr) {
				t.Fatalf("publication error = %v, want %v", err, fetchErr)
			}
			if len(wt.fetchedBases) != 1 || wt.fetchedBases[0] != "main" {
				t.Fatalf("fetched bases = %v, want [main]", wt.fetchedBases)
			}
			if fetchErr != nil {
				if len(wt.pushed) != 0 {
					t.Fatalf("failed fetch published branch: %v", wt.pushed)
				}
			} else if len(wt.pushed) != 1 || wt.pushed[0] != branch {
				t.Fatalf("pushes = %v, want [%s]", wt.pushed, branch)
			}
			if len(wt.injectPRNumbersCalls) != 0 {
				t.Fatal("Dependabot history rewritten")
			}
		})
	}
}
