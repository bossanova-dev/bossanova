package server

import (
	"cmp"
	"slices"

	"github.com/recurser/bossalib/kit/chatlabel"
	"github.com/recurser/bossd/internal/domain"
)

// timelineChatSteps adds chat history without changing what the session waits on.
// A head owns the window from its first poll up to the next head's first poll;
// the first window is open on the left and the last on the right. A verify-role
// chat anywhere in that window, including a failed or reclaimed chat, suppresses
// the synthetic verify step for that head.
func timelineChatSteps(f timelineFacts) []*tlStep {
	chats := slices.Clone(f.Chats)
	slices.SortStableFunc(chats, func(a, b *domain.AgentChat) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return cmp.Compare(a.Seq, b.Seq)
	})
	var labels []chatlabel.Chat
	var steps []*tlStep
	var ids []string
	for _, chat := range chats {
		if !chat.Role.IsAgentOwned() {
			continue
		}
		labels = append(labels, chatlabel.Chat{ID: chat.ID, Role: chatlabel.Role(chat.Role), Title: chat.Title, CreatedAt: chat.CreatedAt})
		st := &tlStep{at: chat.CreatedAt, rank: rankChat, chat: true}
		switch {
		case chat.StartError != nil:
			st.failed, st.detail = true, "failed to start"
		case chat.ReclaimedAt != nil:
			st.detail = "replaced"
		}
		steps = append(steps, st)
		ids = append(ids, chat.ID)
	}
	for i, head := range f.HeadStarts {
		if head.VerifyStartedAt.IsZero() {
			continue
		}
		hasChat := slices.ContainsFunc(chats, func(chat *domain.AgentChat) bool {
			return chat.Role == domain.AgentChatRoleVerify &&
				(i == 0 || !chat.CreatedAt.Before(head.FirstPolledAt)) &&
				(i+1 == len(f.HeadStarts) || chat.CreatedAt.Before(f.HeadStarts[i+1].FirstPolledAt))
		})
		if hasChat {
			continue
		}
		id := "verify-head:" + head.HeadSHA
		labels = append(labels, chatlabel.Chat{ID: id, Role: chatlabel.RoleVerify, CreatedAt: head.VerifyStartedAt})
		steps = append(steps, &tlStep{at: head.VerifyStartedAt, rank: rankChat, chat: true, detail: "no chat"})
		ids = append(ids, id)
	}
	titles := chatlabel.Labels(labels, f.SessionTitle)
	for i, st := range steps {
		st.title = titles[ids[i]]
	}
	// No apiversion bump: these additive steps have verbatim titles and details;
	// neither TUI nor web parses them and both render whatever steps arrive.
	// See docs/structure/contracts.md#skipping-a-bump-justify-it-in-the-diff.
	return steps
}
