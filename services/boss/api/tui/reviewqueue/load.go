package reviewqueue

import (
	"context"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/internal/domain/rpc"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

const queueRequestTimeout = 15 * time.Second

func (m Model) load() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, queueRequestTimeout)
		out := LoadedMsg{generation: m.generation, Counts: map[pb.ConfidenceLevel]int32{}}
		repos, err := m.client.ListReviewQueueRepos(ctx)
		cancel()
		if err != nil {
			out.RepoErrs = append(out.RepoErrs, err.Error())
			if len(repos) == 0 {
				return out
			}
		}
		for _, repo := range repos {
			m.loadQueue(&out, repo)
			m.loadHealth(&out, repo, len(repos) > 1)
		}
		if m.action.Waiting != "" {
			out.WaitingSession, _ = m.getSession(m.action.SessionID)
		}
		return out
	}
}

func (m Model) refresh() (Model, tea.Cmd) {
	if m.fetching {
		m.refreshPending = true
		return m, nil
	}
	m.fetching = true
	m.lastPoll = time.Now()
	return m, m.load()
}

func (m Model) loadQueue(out *LoadedMsg, repo *pb.Repo) {
	if repo.GetOriginUrl() == "" {
		return
	}
	ctx, cancel := context.WithTimeout(m.ctx, queueRequestTimeout)
	defer cancel()
	response, err := m.client.ListReviewQueue(ctx, repo, m.order)
	if err != nil {
		out.RepoErrs = append(out.RepoErrs, fmt.Sprintf("%s: %v", repo.GetDisplayName(), err))
	}
	if response == nil {
		return
	}
	out.Rows = append(out.Rows, response.GetRows()...)
	for _, count := range response.GetLevelCounts() {
		out.Counts[count.GetConfidenceLevel()] += count.GetSessionCount()
	}
}
func (m Model) loadHealth(out *LoadedMsg, repo *pb.Repo, multi bool) {
	ctx, cancel := context.WithTimeout(m.ctx, queueRequestTimeout)
	defer cancel()
	health, err := m.client.GetFactoryHealth(ctx, repo.GetId())
	if err != nil {
		return
	}
	for _, problem := range health.GetProblems() {
		text := problem.GetText()
		if multi {
			text = repo.GetDisplayName() + ": " + text
		}
		out.Health = append(out.Health, text)
	}
}

func (m Model) getSession(id string) (*pb.Session, error) {
	ctx, cancel := context.WithTimeout(m.ctx, queueRequestTimeout)
	defer cancel()
	return m.client.GetSession(ctx, id, rpc.SessionReadOptions{})
}
