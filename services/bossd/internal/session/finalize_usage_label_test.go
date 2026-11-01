package session

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/adapter/tmux"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/ghusage"
)

// labelRecordingVCS records the usage label each ListOpenPRs call carries.
type labelRecordingVCS struct {
	*mockVCSProvider
	labels []string
}

func (v *labelRecordingVCS) ListOpenPRs(ctx context.Context, repoPath string) ([]vcs.PRSummary, error) {
	v.labels = append(v.labels, ghusage.LabelFrom(ctx))
	return v.mockVCSProvider.ListOpenPRs(ctx, repoPath)
}

// TestFinalizeAttributesGitHubCallsToFinalize pins that the GitHub calls the
// finalize pipeline makes reach the provider labelled stage:finalize, even
// when the caller's context carried no label, so the usage ledger can tell
// finalize apart from the pollers and the repair dispatcher.
func TestFinalizeAttributesGitHubCallsToFinalize(t *testing.T) {
	sessions := newMockSessionStore()
	repos := newMockRepoStore()
	wt := &mockWorktreeManager{
		commitSubjects:       []string{"feat(session): label finalize calls"},
		hasDiffAgainstBase:   true,
		hasDiffAgainstBaseOK: true,
	}
	vp := newMockVCSProvider()
	vp.nextOpenPRs = []vcs.PRSummary{{Number: 93, HeadBranch: "label-finalize", State: vcs.PRStateOpen}}
	repos.repos["repo-1"] = &models.Repo{
		ID:        "repo-1",
		LocalPath: "/tmp/repo-main",
		OriginURL: "git@github.com:owner/repo.git",
	}
	sessions.sessions["sess-1"] = &models.Session{
		ID:           "sess-1",
		RepoID:       "repo-1",
		WorktreePath: "/tmp/wt-sess1",
		BranchName:   "label-finalize",
		BaseBranch:   "main",
		State:        machine.ImplementingPlan,
	}
	labelled := &labelRecordingVCS{mockVCSProvider: vp}
	runner := newRecordingFinalizeAgentRunner()
	tmuxFake := newFakeTmux()
	tmuxClient := tmux.NewClient(tmux.WithPasteSettleWait(testPasteSettleWait), tmux.WithCommandFactory(tmuxFake.factory))
	sessions.sessions["sess-1"].AgentName = "claude"
	lc := newTestLifecycle(sessions, repos, &recordingAgentChatStore{}, &recordingCronJobStore{}, wt, newMockAgentRunner(), tmuxClient, labelled, zerolog.Nop())
	lc.SetAgents(map[string]agent.RunnerClient{"claude": runner})
	lc.SetAgentLogsDir(t.TempDir())

	if ghusage.LabelFrom(context.Background()) != ghusage.LabelUnattributed {
		t.Fatal("an unlabelled context must read as daemon:unattributed")
	}
	if _, err := lc.FinalizeSession(context.Background(), "sess-1"); err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}
	if len(labelled.labels) == 0 {
		t.Fatal("finalize made no GitHub call to attribute")
	}
	for i, label := range labelled.labels {
		if label != ghusage.LabelFinalize {
			t.Fatalf("GitHub call %d label = %q, want %q", i, label, ghusage.LabelFinalize)
		}
	}
}
