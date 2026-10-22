package session

import (
	"context"
	"errors"
	"testing"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/adapter/proofenv"
	"github.com/recurser/bossd/internal/agent"
)

func titleCronSession(title string) *models.Session {
	cj := "cron-1"
	return &models.Session{
		ID:           "s1",
		AgentName:    "fake",
		CronJobID:    &cj,
		WorktreePath: "/wt",
		BaseBranch:   "dev",
		Title:        title,
	}
}

func newTitleLifecycle(wt *mockWorktreeManager, ag *fakeAgentForLifecycle) *Lifecycle {
	return &Lifecycle{
		worktrees: wt,
		agents:    map[string]agent.RunnerClient{"fake": ag},
		logger:    zerolog.Nop(),
	}
}

func TestFinalizeTitle(t *testing.T) {
	ctx := context.Background()

	t.Run("non-cron session keeps its own title", func(t *testing.T) {
		s := titleCronSession("Session Title")
		s.CronJobID = nil
		lc := newTitleLifecycle(&mockWorktreeManager{}, newFakeAgent())
		if got := lc.finalizeTitle(ctx, s, "PR title"); got != "Session Title" {
			t.Fatalf("got %q, want %q", got, "Session Title")
		}
	})

	t.Run("single commit normalizes the commit subject without calling the agent", func(t *testing.T) {
		ag := newFakeAgent()
		ag.SuggestPRTitleFunc = func(*bossanovav1.SuggestPRTitleRequest) (*bossanovav1.SuggestPRTitleResponse, error) {
			t.Fatal("agent must not be called for a single-commit PR")
			return nil, nil
		}
		wt := &mockWorktreeManager{
			commitSubjects:      []string{"fix(x): do the thing"},
			latestCommitSubject: "fix(x): do the thing",
		}
		lc := newTitleLifecycle(wt, ag)
		// One commit: its subject is the PR intent → normalized into a clean title.
		if got := lc.finalizeTitle(ctx, titleCronSession("Session"), "[WON-1] whatever"); got != "Do the thing" {
			t.Fatalf("got %q, want normalized commit subject", got)
		}
	})

	t.Run("placeholder-only commits derive from the session title, no agent", func(t *testing.T) {
		ag := newFakeAgent()
		ag.SuggestPRTitleFunc = func(*bossanovav1.SuggestPRTitleRequest) (*bossanovav1.SuggestPRTitleResponse, error) {
			t.Fatal("agent must not be called when only the placeholder commit exists")
			return nil, nil
		}
		wt := &mockWorktreeManager{
			commitSubjects:      []string{draftPRPlaceholderCommitSubject},
			latestCommitSubject: draftPRPlaceholderCommitSubject,
		}
		lc := newTitleLifecycle(wt, ag)
		// Zero real commits → draftPRTitle → placeholder subject → session-title fallback.
		if got := lc.finalizeTitle(ctx, titleCronSession("Session Title"), "[WON-2] x"); got != "[BLOCKED] Session Title" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("multi-commit uses the agent suggestion and passes context", func(t *testing.T) {
		ag := newFakeAgent()
		var gotReq *bossanovav1.SuggestPRTitleRequest
		ag.SuggestPRTitleFunc = func(req *bossanovav1.SuggestPRTitleRequest) (*bossanovav1.SuggestPRTitleResponse, error) {
			gotReq = req
			return &bossanovav1.SuggestPRTitleResponse{Supported: true, Title: "Better overall title"}, nil
		}
		wt := &mockWorktreeManager{commitSubjects: []string{"a", "b", "c"}}
		lc := newTitleLifecycle(wt, ag)
		if got := lc.finalizeTitle(ctx, titleCronSession("Session"), "[BLOCKED] Last commit junk"); got != "Better overall title" {
			t.Fatalf("got %q", got)
		}
		if gotReq == nil {
			t.Fatal("agent was not called")
		}
		if gotReq.GetCurrentTitle() != "Last commit junk" {
			t.Errorf("current_title = %q", gotReq.GetCurrentTitle())
		}
		if gotReq.GetGitLog() != "a\nb\nc" {
			t.Errorf("git_log = %q", gotReq.GetGitLog())
		}
		if gotReq.GetBaseBranch() != "dev" {
			t.Errorf("base_branch = %q", gotReq.GetBaseBranch())
		}
	})

	t.Run("multi-commit, agent unsupported preserves the existing PR title", func(t *testing.T) {
		ag := newFakeAgent() // default SuggestPRTitle returns supported=false
		wt := &mockWorktreeManager{commitSubjects: []string{"a", "b"}}
		lc := newTitleLifecycle(wt, ag)
		if got := lc.finalizeTitle(ctx, titleCronSession("Session"), "[BLOCKED] [WON-9] Existing"); got != "[WON-9] Existing" {
			t.Fatalf("got %q, want preserved title", got)
		}
	})

	t.Run("multi-commit, agent error preserves the existing PR title", func(t *testing.T) {
		ag := newFakeAgent()
		ag.SuggestPRTitleFunc = func(*bossanovav1.SuggestPRTitleRequest) (*bossanovav1.SuggestPRTitleResponse, error) {
			return nil, errors.New("boom")
		}
		wt := &mockWorktreeManager{commitSubjects: []string{"a", "b"}}
		lc := newTitleLifecycle(wt, ag)
		if got := lc.finalizeTitle(ctx, titleCronSession("Session"), "Existing PR title"); got != "Existing PR title" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("multi-commit, agent returns empty title preserves the existing PR title", func(t *testing.T) {
		ag := newFakeAgent()
		ag.SuggestPRTitleFunc = func(*bossanovav1.SuggestPRTitleRequest) (*bossanovav1.SuggestPRTitleResponse, error) {
			return &bossanovav1.SuggestPRTitleResponse{Supported: true, Title: "   "}, nil
		}
		wt := &mockWorktreeManager{commitSubjects: []string{"a", "b"}}
		lc := newTitleLifecycle(wt, ag)
		if got := lc.finalizeTitle(ctx, titleCronSession("Session"), "Existing"); got != "Existing" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("no existing title falls back to the normalized last commit subject", func(t *testing.T) {
		ag := newFakeAgent()
		wt := &mockWorktreeManager{
			commitSubjects:      []string{"a", "b"},
			latestCommitSubject: "fix(x): do the thing",
		}
		lc := newTitleLifecycle(wt, ag)
		// Empty current title + unsupported agent → deterministicCronTitle →
		// draftPRTitle → normalizeCronPRTitle("fix(x): do the thing").
		if got := lc.finalizeTitle(ctx, titleCronSession("Session Title"), ""); got != "Do the thing" {
			t.Fatalf("got %q, want %q", got, "Do the thing")
		}
	})

	t.Run("marked cron default is derived when agent has no suggestion", func(t *testing.T) {
		lc := newTitleLifecycle(&mockWorktreeManager{commitSubjects: []string{"fix: newest", "feat: first"}, latestCommitSubject: "fix: newest"}, newFakeAgent())
		if got := lc.finalizeTitle(ctx, titleCronSession("Session Title"), "[BLOCKED] Session Title"); got != "Newest" {
			t.Fatalf("title = %q", got)
		}
	})
	t.Run("unreadable or empty latest subject does not invent marker", func(t *testing.T) {
		for _, wt := range []*mockWorktreeManager{{}, {latestCommitSubjectErr: errors.New("git unavailable")}} {
			lc := newTitleLifecycle(wt, newFakeAgent())
			if got := lc.draftPRTitle(ctx, titleCronSession("Session Title")); got != "Session Title" {
				t.Fatalf("title = %q", got)
			}
		}
	})
	t.Run("CommitSubjects error falls back to preserving the existing title", func(t *testing.T) {
		ag := newFakeAgent()
		ag.SuggestPRTitleFunc = func(*bossanovav1.SuggestPRTitleRequest) (*bossanovav1.SuggestPRTitleResponse, error) {
			t.Fatal("agent must not be called when commit history is unavailable")
			return nil, nil
		}
		wt := &mockWorktreeManager{commitSubjectsErr: errors.New("no base")}
		lc := newTitleLifecycle(wt, ag)
		if got := lc.finalizeTitle(ctx, titleCronSession("Session"), "[WON-3] Keep"); got != "[WON-3] Keep" {
			t.Fatalf("got %q", got)
		}
	})
}

// TestAgentSuggestedTitleSendsRepoPinnedEnv pins BOS-1562 for the PR-title
// one-shot: the plugin runs an agent in the session worktree, so the request
// must carry the same OverlayWithRepo pins a session spawn gets — empty proof
// keys for an untrusted repo, the repo's own Linear key for a configured one.
func TestAgentSuggestedTitleSendsRepoPinnedEnv(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name        string
		repo        *models.Repo
		wantLinear  string
		wantProofed bool
	}{
		{name: "untrusted repo pins proof keys empty", repo: &models.Repo{ID: "r1"}, wantProofed: true},
		{name: "unknown repo counts as untrusted", repo: nil, wantProofed: true},
		{name: "trusted repo leaves proof keys to the plugin", repo: &models.Repo{ID: "r1", IsTrusted: true, LinearAPIKey: "repo-linear"}, wantLinear: "repo-linear"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ag := newFakeAgent()
			var gotReq *bossanovav1.SuggestPRTitleRequest
			ag.SuggestPRTitleFunc = func(req *bossanovav1.SuggestPRTitleRequest) (*bossanovav1.SuggestPRTitleResponse, error) {
				gotReq = req
				return &bossanovav1.SuggestPRTitleResponse{Supported: true, Title: "T"}, nil
			}
			lc := newTitleLifecycle(&mockWorktreeManager{commitSubjects: []string{"a", "b"}}, ag)
			repos := newMockRepoStore()
			if tc.repo != nil {
				repos.repos[tc.repo.ID] = tc.repo
			}
			lc.repos = repos
			s := titleCronSession("Session")
			s.RepoID = "r1"
			s.WorktreePath = t.TempDir()
			if got := lc.finalizeTitle(ctx, s, "Old"); got != "T" {
				t.Fatalf("got %q", got)
			}
			wantTrusted := tc.repo != nil && tc.repo.IsTrusted
			if got := gotReq.GetShouldTrustProjectConfig(); got != wantTrusted {
				t.Fatalf("one-shot trust = %v, want %v", got, wantTrusted)
			}
			env := gotReq.GetExtraEnv()
			if v, ok := env["LINEAR_API_KEY"]; !ok || v != tc.wantLinear {
				t.Errorf("LINEAR_API_KEY = %q (present=%v), want %q", v, ok, tc.wantLinear)
			}
			for _, key := range proofenv.Keys() {
				v, ok := env[key]
				if tc.wantProofed && (!ok || v != "") {
					t.Errorf("%s = %q (present=%v), want pinned empty for an untrusted repo", key, v, ok)
				}
				if !tc.wantProofed && ok {
					t.Errorf("%s present for a trusted repo; the pin must not fire", key)
				}
			}
		})
	}
}
