package views

// Async tea.Cmd builders for the chat picker's session, chat-list, merge and
// archive RPCs. Split out of chatpicker.go (BOS-527); the declarations are
// unchanged. Per-concern commands stay with their concern rather than moving
// here — WakeChat and DeleteChat in chatpicker_keys.go, ListAccounts and
// SwitchSessionAccount in chatpicker_switch.go.

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/agent"
	"github.com/recurser/boss/internal/client"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/vcs"
)

func (m ChatPickerModel) fetchSession() tea.Cmd {
	return func() tea.Msg {
		sess, err := m.client.GetSession(m.ctx, m.sessionID, client.SessionReadOptions{IncludeLocalHTTPEndpoints: true})
		if err != nil {
			return chatPickerErrMsg{err: err}
		}
		return chatPickerSessionMsg{session: sess}
	}
}

// parseChatStatuses fetches daemon-side heartbeat statuses and converts them
// into maps keyed by Claude ID. The third map carries the reason a chat is
// parked on an external event (BOS-668) and is populated only for the chats
// that have one — it stays empty against a daemon too old to stamp the field.
func parseChatStatuses(c client.BossClient, ctx context.Context, sessionID string) (map[string]string, map[string]time.Time, map[string]string) {
	entries, err := c.GetChatStatuses(ctx, sessionID)
	if err != nil {
		return nil, nil, nil
	}
	statuses := make(map[string]string, len(entries))
	lastOutput := make(map[string]time.Time, len(entries))
	waitingReasons := make(map[string]string)
	for _, e := range entries {
		statuses[e.AgentSessionId] = chatStatusString(e.Status)
		if e.LastOutputAt != nil {
			lastOutput[e.AgentSessionId] = e.LastOutputAt.AsTime()
		}
		if reason := e.GetWaitingReason(); reason != "" {
			waitingReasons[e.AgentSessionId] = reason
		}
	}
	return statuses, lastOutput, waitingReasons
}

func (m ChatPickerModel) listChats() tea.Cmd {
	return func() tea.Msg {
		chats, err := m.client.ListChats(m.ctx, m.sessionID)
		if err != nil {
			return chatsListedMsg{err: err}
		}
		statuses, lastOutput, waitingReasons := parseChatStatuses(m.client, m.ctx, m.sessionID)
		return chatsListedMsg{chats: chats, daemonStatuses: statuses, daemonLastOutput: lastOutput, daemonWaitingReasons: waitingReasons}
	}
}

func (m ChatPickerModel) fetchRepoWebLink() tea.Cmd {
	if m.session == nil || m.session.GetRepoId() == "" {
		return nil
	}
	repoID := m.session.GetRepoId()
	prNumber := int(m.session.GetPrNumber())
	return func() tea.Msg {
		tag := repoWebLinkMsg{repoID: repoID, prNumber: prNumber}
		repos, err := m.client.ListRepos(m.ctx)
		if err != nil {
			return tag
		}
		for _, repo := range repos {
			if repo.GetId() != repoID {
				continue
			}
			if provider, webURL, ok := vcs.PullRequestWebLink(repo.GetOriginUrl(), prNumber); ok {
				tag.link = repoWebLink{provider: provider, url: webURL}
				return tag
			}
			provider, webURL, ok := vcs.RepoWebLink(repo.GetOriginUrl())
			if !ok {
				return tag
			}
			tag.link = repoWebLink{provider: provider, url: webURL}
			return tag
		}
		return tag
	}
}

// refreshStatuses fetches the latest session (for PR status) and daemon
// chat statuses without re-listing all chats.
func (m ChatPickerModel) refreshStatuses() tea.Cmd {
	return func() tea.Msg {
		sess, err := m.client.GetSession(m.ctx, m.sessionID, client.SessionReadOptions{IncludeLocalHTTPEndpoints: true})
		if err != nil {
			return chatPickerRefreshMsg{}
		}
		statuses, lastOutput, waitingReasons := parseChatStatuses(m.client, m.ctx, m.sessionID)
		return chatPickerRefreshMsg{
			session:              sess,
			daemonStatuses:       statuses,
			daemonLastOutput:     lastOutput,
			daemonWaitingReasons: waitingReasons,
		}
	}
}

// backfillTitles reads JSONL files for chats still titled "New chat" and
// updates their titles via RPC. This is best-effort and non-blocking.
func (m ChatPickerModel) backfillTitles() tea.Cmd {
	if m.session == nil {
		return nil
	}
	var needsUpdate []*pb.ClaudeChat
	for _, c := range m.chats {
		if c.Title == "" || c.Title == "New chat" {
			needsUpdate = append(needsUpdate, c)
		}
	}
	if len(needsUpdate) == 0 {
		return nil
	}
	worktreePath := m.session.GetWorktreePath()
	return func() tea.Msg {
		updates := make(map[string]string)
		for _, c := range needsUpdate {
			title := agent.ChatTitle(worktreePath, c.AgentSessionId)
			if title != "" {
				updates[c.AgentSessionId] = title
				_ = m.client.UpdateChatTitle(m.ctx, c.AgentSessionId, title)
			}
		}
		return chatTitlesBackfilledMsg{updates: updates}
	}
}

// mergeCmd runs MergeSession off the update loop. Captures everything by value.
func (m ChatPickerModel) mergeCmd() tea.Cmd {
	client := m.client
	ctx := m.ctx
	id := m.sessionID
	return func() tea.Msg {
		_, err := client.MergeSession(ctx, id)
		return mergeResultMsg{sessionID: id, err: err}
	}
}

func (m ChatPickerModel) archiveCmd() tea.Cmd {
	client := m.client
	ctx := m.ctx
	id := m.sessionID
	return func() tea.Msg {
		_, err := client.ArchiveSession(ctx, id)
		return archiveResultMsg{sessionID: id, err: err}
	}
}
