package server

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/status"
)

type stuckWaitCallbacks struct {
	githubCallbackStore
	rows []*domain.GitHubCallback
}

func (f *stuckWaitCallbacks) List(_ context.Context, filter domain.ListGitHubCallbacksFilter) ([]*domain.GitHubCallback, error) {
	var rows []*domain.GitHubCallback
	for _, row := range f.rows {
		if (filter.PRNumber == nil || *filter.PRNumber == row.PRNumber) && (filter.RepoOwner == nil || *filter.RepoOwner == row.RepoOwner) && (filter.RepoName == nil || *filter.RepoName == row.RepoName) {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

func stuckWaitServer(t *testing.T, display vcs.DisplayInfo, actor, wakes string) *Server {
	t.Helper()
	n := 3795
	url := "https://github.com/acme/app/pull/3795"
	row := &models.Session{ID: "stuck-session", RepoID: "repo", State: machine.ReadyForReview, PRNumber: &n, PRURL: &url, UpdatedAt: time.Now()}
	chat := &domain.AgentChat{ID: "chat", SessionID: row.ID, AgentSessionID: "verify-chat", Title: "verify 2"}
	tracker := status.NewDisplayTracker()
	tracker.Set(row.ID, display)
	chats := status.NewTracker()
	chats.SetWaiting(chat.AgentSessionID, "awaiting checks_passed_ready on acme/app#3795")
	if actor == "working" {
		chats.Update(chat.AgentSessionID, pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())
	}
	s := newListSessionsDisplayStatusTestServer([]*models.Session{row}, map[string][]*domain.AgentChat{row.ID: {chat}}, tracker, chats)
	s.repos = &listSessionsRepoStoreFake{repos: map[string]*models.Repo{"repo": {ID: "repo", OriginURL: "https://github.com/acme/app.git", CanAutoRepair: true}}}
	s.repairLease = status.NewRepairLeaseManager()
	if actor == "lease" {
		if err := s.repairLease.Acquire(row.ID, "repair"); err != nil {
			t.Fatal(err)
		}
	}
	store := &stuckWaitCallbacks{}
	for i, trigger := range strings.Fields(wakes) {
		group := "reverify-3795-head"
		state := domain.GitHubCallbackStateActive
		if actor == "triggered" {
			state = domain.GitHubCallbackStateTriggered
		}
		store.rows = append(store.rows, &domain.GitHubCallback{ID: fmt.Sprint(i), GroupID: &group, TargetChatID: chat.AgentSessionID, RepoOwner: "acme", RepoName: "app", PRNumber: n, Trigger: domain.GitHubCallbackTrigger(trigger), State: state, ShouldRequireTransition: true, CreatedAt: time.Now().Add(-time.Hour), ExpiresAt: time.Now().Add(time.Hour)})
	}
	s.githubCallbacks = store
	return s
}
func listStuckAttention(t *testing.T, s *Server) *pb.SessionAttention {
	t.Helper()
	resp, err := s.ListSessions(t.Context(), connect.NewRequest(&pb.ListSessionsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	return onlySession(t, resp.Msg.Sessions).GetAttention()
}
func TestAttentionStuckWaitEvidence3795And3797_Sessions_AC216601(t *testing.T) {
	no := false
	for _, tt := range []struct {
		name    string
		display vcs.DisplayInfo
	}{
		{"3795", vcs.DisplayInfo{Status: vcs.DisplayStatusDraft, Mergeable: &no, HeadSHA: "77390726"}},
		{"3797", vcs.DisplayInfo{Status: vcs.DisplayStatusConflict, Mergeable: &no, HasFailures: true, HeadSHA: "e52fc1ca"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := stuckWaitServer(t, tt.display, "none", "checks_passed_ready")
			if tt.name == "3795" {
				s.githubCallbacks.(*stuckWaitCallbacks).rows[0].ShouldRequireTransition = false
			}
			got := listStuckAttention(t, s)
			want := "verify 2 waits on a wake that cannot fire: the PR conflicts and no repair is running · start a repair or resolve the conflict"
			if got.GetKind() != pb.AttentionKind_ATTENTION_KIND_FIX || got.GetSentence() != want || got.GetAlertKey() != "stuck-wait:conflict:"+tt.display.HeadSHA || got.GetTargetChatId() != "verify-chat" {
				t.Fatalf("attention = %v", got)
			}
		})
	}
}
func TestAttentionStuckWaitInvariantGrid(t *testing.T) {
	yes, no := true, false
	for _, draft := range []bool{false, true} {
		for _, conflict := range []bool{false, true} {
			for _, checks := range []string{"pending", "green", "failing"} {
				for _, state := range []string{"open", "merged", "closed"} {
					for _, actor := range []string{"none", "working", "lease", "triggered"} {
						for _, wakes := range []string{"", "merged", "checks_passed_ready", "checks_passed_ready merged"} {
							name := fmt.Sprintf("draft=%t/conflict=%t/%s/%s/%s/wakes=%s", draft, conflict, checks, state, actor, wakes)
							t.Run(name, func(t *testing.T) {
								display := vcs.DisplayInfo{Status: vcs.DisplayStatusChecking, Mergeable: &yes, HeadSHA: "head"}
								if checks == "green" {
									display.Status = vcs.DisplayStatusPassing
								}
								if checks == "failing" {
									display.Status = vcs.DisplayStatusFailing
									display.HasFailures = true
								}
								if draft {
									display.Status = vcs.DisplayStatusDraft
								}
								if conflict {
									display.Mergeable = &no
									if !draft {
										display.Status = vcs.DisplayStatusConflict
									}
								}
								if state == "merged" {
									display.Status = vcs.DisplayStatusMerged
								}
								if state == "closed" {
									display.Status = vcs.DisplayStatusClosed
								}
								got := listStuckAttention(t, stuckWaitServer(t, display, actor, wakes))
								want := state == "open" && actor == "none" && wakes == "checks_passed_ready" && (draft || conflict || checks == "failing")
								stuck := got.GetKind() == pb.AttentionKind_ATTENTION_KIND_FIX && strings.HasPrefix(got.GetAlertKey(), "stuck-wait:")
								if stuck != want {
									t.Fatalf("attention = %v; want stuck=%t", got, want)
								}
							})
						}
					}
				}
			}
		}
	}
}

func TestAttentionStuckWaitGreenDraftTransition(t *testing.T) {
	yes := true
	for _, fresh := range []bool{true, false} {
		t.Run(fmt.Sprintf("fresh=%t", fresh), func(t *testing.T) {
			s := stuckWaitServer(t, vcs.DisplayInfo{Status: vcs.DisplayStatusDraft, Mergeable: &yes, HeadSHA: "head"}, "none", "checks_passed")
			row := s.sessions.(*listSessionsSessionStoreFake).sessions[0]
			row.LastCheckState = machine.CheckStatePassed
			head := "previous-head"
			if fresh {
				head = "head"
			}
			row.LastCheckStateHeadSHA = &head
			got := listStuckAttention(t, s)
			stuck := got.GetKind() == pb.AttentionKind_ATTENTION_KIND_FIX && got.GetAlertKey() == "stuck-wait:draft:head"
			if stuck != fresh {
				t.Fatalf("attention = %v; want stuck=%t", got, fresh)
			}
		})
	}
}

func TestAttentionStuckWaitGreenDraftObservedBaseline(t *testing.T) {
	yes := true
	s := stuckWaitServer(t, vcs.DisplayInfo{Status: vcs.DisplayStatusDraft, Mergeable: &yes, HeadSHA: "head"}, "none", "checks_passed")
	row := s.sessions.(*listSessionsSessionStoreFake).sessions[0]
	row.LastCheckState = machine.CheckStatePassed
	head := "head"
	row.LastCheckStateHeadSHA = &head
	s.githubCallbacks.(*stuckWaitCallbacks).rows[0].HasObservedBaseline = true
	got := listStuckAttention(t, s)
	if strings.HasPrefix(got.GetAlertKey(), "stuck-wait:") {
		t.Fatalf("deliverable transition marked stuck: %v", got)
	}
}
