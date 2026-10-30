package session

import (
	"context"
	"errors"
	"testing"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/adapter/proofenv"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/domain"
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
				if (tc.wantProofed || key != proofenv.EnvAnthropicAPIKey) && (!ok || v != "") {
					t.Errorf("%s = %q (present=%v), want pinned empty for an untrusted repo", key, v, ok)
				}
				if !tc.wantProofed && key == proofenv.EnvAnthropicAPIKey && ok {
					t.Errorf("%s present for a trusted repo; the pin must not fire", key)
				}
			}
		})
	}
}

func TestFinalizeSession_AttachedTrackerPRKeepsTitle_Sessions_AC132(t *testing.T) {
	lc, sessions, wt, vp, _ := finalizePublishFixture(t, "")
	sess := sessions.sessions["sess-1"]
	tracker, number := "BOS-1", 77
	sess.TrackerID, sess.PRNumber, sess.Title = &tracker, &number, "[BOS-1] X"
	wt.commitSubjects = []string{"feat: first", "fix: newest"}
	wt.latestCommitSubject = "fix: newest"
	vp.nextOpenPRs = []vcs.PRSummary{{Number: number, HeadBranch: sess.BranchName, State: vcs.PRStateOpen, Title: sess.Title}}
	vp.nextPRStatus = &vcs.PRStatus{State: vcs.PRStateOpen, Title: sess.Title}
	res, err := lc.FinalizeSession(context.Background(), sess.ID)
	if err != nil || res.Outcome != domain.CronJobOutcomePRCreated {
		t.Fatalf("result=%+v err=%v", res, err)
	}
	if len(vp.updatePRTitleCalls) != 0 {
		t.Fatalf("unexpected title updates: %+v", vp.updatePRTitleCalls)
	}
	if got := sessions.sessions[sess.ID].Title; got != "[BOS-1] X" {
		t.Fatalf("title=%q", got)
	}
}

func TestFinalizeTitle_ResolvedCronPlaceholder(t *testing.T) {
	for _, tc := range []struct{ name, current, want string }{
		{"real title equal to session", "[BOS-1] X", "[BOS-1] X"},
		{"cron default", "Bossanova auto-implement", "Newest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lc := newTitleLifecycle(&mockWorktreeManager{commitSubjects: []string{"feat: first", "fix: newest"}, latestCommitSubject: "fix: newest"}, newFakeAgent())
			lc.cronJobs = &namedCronJobStore{name: "Bossanova auto-implement"}
			if got := lc.finalizeTitle(context.Background(), titleCronSession(tc.current), tc.current); got != tc.want {
				t.Fatalf("title=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestAttachOpenPRForBranch_TrackerSuggestion_Sessions_AC133(t *testing.T) {
	lc, sessions, wt, vp, _ := finalizePublishFixture(t, "")
	sess := sessions.sessions["sess-1"]
	tracker := "BOS-1"
	sess.TrackerID = &tracker
	wt.commitSubjects = []string{"feat: first", "fix: newest"}
	ag := newFakeAgent()
	ag.SuggestPRTitleFunc = func(*bossanovav1.SuggestPRTitleRequest) (*bossanovav1.SuggestPRTitleResponse, error) {
		return &bossanovav1.SuggestPRTitleResponse{Supported: true, Title: "Better title"}, nil
	}
	sess.AgentName = "fake"
	lc.agents = map[string]agent.RunnerClient{"fake": ag}
	vp.nextOpenPRs = []vcs.PRSummary{{Number: 77, HeadBranch: sess.BranchName, State: vcs.PRStateOpen, Title: "[BOS-1] X"}}
	found, err := lc.attachOpenPRForBranch(context.Background(), sess.ID, sess, &models.Repo{OriginURL: "git@github.com:owner/repo.git"})
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if len(vp.updatePRTitleCalls) != 1 || vp.updatePRTitleCalls[0].title != "[BOS-1] Better title" {
		t.Fatalf("updates=%+v", vp.updatePRTitleCalls)
	}
	if got := sessions.sessions[sess.ID].Title; got != "[BOS-1] Better title" {
		t.Fatalf("title=%q", got)
	}
}

func TestWithTrackerTitlePrefix_Sessions_AC133(t *testing.T) {
	id, empty := "BOS-1", ""
	for _, tc := range []struct {
		name, title string
		id          *string
		want        string
	}{
		{"nil", "X", nil, "X"}, {"empty ID", "X", &empty, "X"}, {"empty title", "", &id, ""},
		{"prefix present", "[BOS-1] X", &id, "[BOS-1] X"}, {"ID elsewhere", "X [bos-1]", &id, "X [bos-1]"},
		{"missing", "X", &id, "[BOS-1] X"}, {"blocked", "[BLOCKED] X", &id, "[BLOCKED] [BOS-1] X"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := withTrackerTitlePrefix(tc.title, tc.id); got != tc.want {
				t.Fatalf("got=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestFinalizeSession_DraftTrackerTitle_Sessions_AC133(t *testing.T) {
	lc, sessions, wt, vp, _ := finalizePublishFixture(t, "")
	tracker := "BOS-1"
	sessions.sessions["sess-1"].TrackerID = &tracker
	wt.commitSubjects = []string{"fix: newest"}
	wt.isAncestorFn = func(_, _, _ string) (bool, error) { return false, nil }
	res, err := lc.FinalizeSession(context.Background(), "sess-1")
	if err != nil || res.Outcome != domain.CronJobOutcomePRCreated {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if len(vp.createPRCalls) != 1 || vp.createPRCalls[0].Title != "[BOS-1] Complete work" {
		t.Fatalf("draft=%+v", vp.createPRCalls)
	}
	if got := sessions.sessions["sess-1"].Title; got != "[BOS-1] Complete work" {
		t.Fatalf("title=%q", got)
	}
}

func TestClearNoCommitsPRTitle_Tracker_Sessions_AC133(t *testing.T) {
	lc, sessions, _, vp, _ := finalizePublishFixture(t, "")
	sess := sessions.sessions["sess-1"]
	number, tracker := 77, "BOS-1"
	sess.PRNumber, sess.TrackerID = &number, &tracker
	vp.nextPRStatus = &vcs.PRStatus{Title: "[BLOCKED] Cron job"}
	resolver := &PRAssociationResolver{sessions: sessions, repos: lc.repos, provider: vp, cronJobs: &namedCronJobStore{name: "Cron job"}}
	updated, changed, err := resolver.clearNoCommitsPRTitle(context.Background(), sess, []string{"feat: first", "fix: newest"})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if len(vp.updatePRTitleCalls) != 1 || vp.updatePRTitleCalls[0].title != "[BOS-1] Newest" || updated.Title != "[BOS-1] Newest" {
		t.Fatalf("updates=%+v title=%q", vp.updatePRTitleCalls, updated.Title)
	}
}
