package home

import (
	"context"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type stubClient struct {
	Client
	repos            []*pb.Repo
	reposErr         error
	AuthChanges      []string
	notifyAuthChange func(context.Context, string) (*pb.NotifyAuthChangeResponse, error)
}

func (s *stubClient) ListRepos(context.Context) ([]*pb.Repo, error) { return s.repos, s.reposErr }

type routerClientFixture struct{ Client }

func (s *stubClient) NotifyAuthChange(ctx context.Context, action string) (*pb.NotifyAuthChangeResponse, error) {
	if s.notifyAuthChange != nil {
		return s.notifyAuthChange(ctx, action)
	}
	s.AuthChanges = append(s.AuthChanges, action)
	return nil, nil
}
