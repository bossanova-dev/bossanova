package tui

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/recurser/boss/api/tui/repos"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type repoSettingsFake struct{ repos []*pb.Repo }

func (f *repoSettingsFake) ListRepos(context.Context) ([]*pb.Repo, error) { return f.repos, nil }
func (f *repoSettingsFake) UpdateRepo(context.Context, *pb.UpdateRepoRequest) (*pb.Repo, error) {
	return nil, nil
}
func repoInputModel(t *testing.T, clone bool) repos.RepoAddModel {
	t.Helper()
	m := repos.NewRepoAddModel(nil, context.Background())
	if clone {
		v, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		m = v.(repos.RepoAddModel)
	}
	v, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	return v.(repos.RepoAddModel)
}
func repoDetailsModel(t *testing.T) repos.RepoAddModel {
	t.Helper()
	m := repoInputModel(t, false)
	v, _ := m.Update(repos.RepoValidatedMsg{Resp: &pb.ValidateRepoPathResponse{IsValid: true}})
	return v.(repos.RepoAddModel)
}

func drainCmd(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, child := range batch {
			out = append(out, drainCmd(child)...)
		}
		return out
	}
	if msg == nil {
		return nil
	}
	return []tea.Msg{msg}
}
