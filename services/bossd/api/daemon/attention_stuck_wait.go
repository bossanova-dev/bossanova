package server

import (
	"context"
	"strings"
	"time"

	"github.com/recurser/bossalib/attention"
	"github.com/recurser/bossalib/displaystatus"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossd/internal/domain"
)

func (s *Server) stuckWaitFix(ctx context.Context, p *pb.Session, d attentionDeps, agentWorking bool) *attention.FixFact {
	pr := displaystatus.WaitPRFromSession(p)
	if p.GetPrNumber() == 0 || !pr.Open || p.GetDisplayStatus() == pb.DisplayStatus_DISPLAY_STATUS_NEEDS_HUMAN || (!pr.Draft && p.GetPrMergeable() && pr.Checks == displaystatus.WaitChecksGreen) || agentWorking || s.repairLease.Active(p.GetId()) || s.githubCallbacks == nil {
		return nil
	}
	if d.display != nil && (d.display.SettingUp || d.display.Merging) {
		return nil
	}
	owner, repo, ok := strings.Cut(vcs.GitHubNWO(p.GetPrUrl()), "/")
	if !ok {
		return nil
	}
	number := int(p.GetPrNumber())
	callbacks, err := s.githubCallbacks.List(ctx, domain.ListGitHubCallbacksFilter{RepoOwner: &owner, RepoName: &repo, PRNumber: &number})
	if err != nil {
		return nil
	}
	chats := make(map[string]*domain.AgentChat, len(d.chats))
	for _, chat := range d.chats {
		if chat != nil {
			chats[chat.AgentSessionID] = chat
		}
	}
	chosen, blocker, since := stuckWaitCallback(callbacks, chats, pr, time.Now())
	if chosen == nil {
		return nil
	}
	title := chats[chosen.TargetChatID].Title
	if title == "" {
		title = "verify"
	}
	clause := map[displaystatus.WaitBlocker]string{
		"conflict": "the PR conflicts and no repair is running · start a repair or resolve the conflict",
		"failing":  "checks failed and no repair is running · start a repair or fix the checks",
		"draft":    "the PR is a draft and nothing will mark it ready · mark it ready or resume the build",
	}[blocker]
	head := ""
	if d.display != nil {
		head = d.display.HeadSHA
	}
	return &attention.FixFact{AlertKey: "stuck-wait:" + string(blocker) + ":" + head, ChatID: chosen.TargetChatID, Since: since, Sentence: title + " waits on a wake that cannot fire: " + clause}
}

func stuckWaitCallback(callbacks []*domain.GitHubCallback, chats map[string]*domain.AgentChat, pr displaystatus.WaitPR, now time.Time) (*domain.GitHubCallback, displaystatus.WaitBlocker, time.Time) {
	var chosen *domain.GitHubCallback
	var blocker displaystatus.WaitBlocker
	var since time.Time
	for _, cb := range callbacks {
		if cb == nil || chats[cb.TargetChatID] == nil || !now.Before(cb.ExpiresAt) {
			continue
		}
		if cb.State == domain.GitHubCallbackStateTriggered {
			return nil, "", time.Time{}
		}
		if cb.State != domain.GitHubCallbackStateActive && cb.State != domain.GitHubCallbackStateLeased {
			continue
		}
		reach := displaystatus.CallbackWaitReach(string(cb.Trigger), cb.ShouldRequireTransition && !cb.HasObservedBaseline, pr)
		if reach.Reachable {
			return nil, "", time.Time{}
		}
		if since.IsZero() || cb.CreatedAt.Before(since) {
			since = cb.CreatedAt
		}
		if chosen == nil || waitBlockerRank(reach.Blocker) < waitBlockerRank(blocker) || (reach.Blocker == blocker && (cb.CreatedAt.Before(chosen.CreatedAt) || (cb.CreatedAt.Equal(chosen.CreatedAt) && cb.ID < chosen.ID))) {
			chosen = cb
			blocker = reach.Blocker
		}
	}
	return chosen, blocker, since
}

func waitBlockerRank(blocker displaystatus.WaitBlocker) int {
	switch blocker {
	case "conflict":
		return 0
	case "failing":
		return 1
	default:
		return 2
	}
}
