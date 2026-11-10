package session

import (
	"context"
	"testing"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/domain"
)

func TestDependabotOrphanResumePerRepo_Sessions_AC248002_AC248004(t *testing.T) {
	for _, sameRepo := range []bool{true, false} {
		t.Run(map[bool]string{true: "same repo", false: "different repos"}[sameRepo], func(t *testing.T) {
			f := newOrphanResumeFixture(t)
			f.lc.SetAgents(map[string]agent.RunnerClient{"claude": newFakeAgent()})
			first := f.sessions.sessions[f.sessionID]
			first.BranchName = "dependabot/npm/a"
			second := *first
			second.ID = "second"
			second.BranchName = "dependabot/npm/b"
			if !sameRepo {
				second.RepoID = "other"
			}
			f.sessions.sessions[second.ID] = &second
			want := 2
			if sameRepo {
				want = 1
			}
			if got := f.lc.ResumeOrphanedHeadlessRuns(context.Background()); got != want {
				t.Fatalf("resumed %d want %d", got, want)
			}
			if f.sessions.claimOrphanCalls != want {
				t.Fatalf("orphan claim writes = %d, want %d", f.sessions.claimOrphanCalls, want)
			}
			orphaned := 0
			for _, s := range f.sessions.sessions {
				if s.State == machine.Orphaned {
					orphaned++
					if s.BlockedReason == nil || *s.BlockedReason != OrphanedHeadlessRunReason {
						t.Fatal("deferred orphan marker changed")
					}
				}
			}
			if orphaned != 2-want {
				t.Fatalf("orphaned=%d", orphaned)
			}
		})
	}
}

func TestDependabotBusySkipsOrphan(t *testing.T) {
	f := newOrphanResumeFixture(t)
	f.lc.SetAgents(map[string]agent.RunnerClient{"claude": newFakeAgent()})
	s := f.sessions.sessions[f.sessionID]
	s.BranchName = "dependabot/npm/a"
	f.sessions.sessions["holder"] = &models.Session{ID: "holder", RepoID: s.RepoID, BranchName: "dependabot/npm/b", State: machine.ImplementingPlan}
	f.lc.agentChats = &mockAgentChatStore{chatsBySession: map[string][]*domain.AgentChat{"holder": {chatWithPane("holder")}, f.sessionID: {{ID: "primary", SessionID: f.sessionID, AgentSessionID: *s.AgentSessionID}}}}
	f.lc.tmux, _ = paneTmux(reattachPane)
	if got := f.lc.ResumeOrphanedHeadlessRuns(context.Background()); got != 0 {
		t.Fatalf("resumed=%d", got)
	}
	if f.sessions.claimOrphanCalls != 0 {
		t.Fatalf("busy repo wrote %d orphan claims", f.sessions.claimOrphanCalls)
	}
	if s.State != machine.Orphaned {
		t.Fatalf("state=%s", s.State)
	}
}
