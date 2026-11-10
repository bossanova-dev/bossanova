package server

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"
	"github.com/recurser/bossalib/kit/safego"
	"github.com/recurser/bossd/internal/domain"
)

const repoIdentityBudget = 3 * time.Second
const repoIdentityConcurrency = 4

// ListRepoCloudControls is a local-socket read for the operator's settings
// surfaces. No corresponding upstream command or MCP tool exists.
func (s *Server) ListRepoCloudControls(ctx context.Context, _ *connect.Request[pb.ListRepoCloudControlsRequest]) (*connect.Response[pb.ListRepoCloudControlsResponse], error) {
	settings, err := config.Load()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("load settings: %w", err))
	}
	repos, err := s.repos.List(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list repos: %w", err))
	}
	identities := s.resolveRepoIdentities(ctx, repos)
	response := &pb.ListRepoCloudControlsResponse{}
	for i, r := range repos {
		id := identities[i]
		mode, source := config.ResolveRepoCloudControl(settings, id)
		response.Repos = append(response.Repos, &pb.RepoCloudControl{DisplayName: r.DisplayName, Host: id.Host, Checkout: id.LocalPath, ProviderRepoId: id.ProviderRepoID, IsIdVerified: id.IsIDVerified, Name: id.Name, OriginUrl: r.OriginURL, EffectiveMode: string(mode), Source: string(source), HasUnverifiedGrant: config.HasUnverifiedRepoGrant(settings, id)})
	}
	return connect.NewResponse(response), nil
}

func (s *Server) resolveRepoIdentities(ctx context.Context, repos []*domain.Repo) []config.RepoIdentity {
	lookupCtx, cancel := context.WithTimeout(ctx, repoIdentityBudget)
	defer cancel()
	identities := make([]config.RepoIdentity, len(repos))
	workers := min(repoIdentityConcurrency, len(repos))
	queue := make(chan int, len(repos))
	for i := range repos {
		queue <- i
	}
	close(queue)
	done := make([]<-chan struct{}, 0, workers)
	for range workers {
		done = append(done, safego.Go(s.logger, func() {
			for i := range queue {
				identities[i] = s.repoIdentity.Identity(lookupCtx, *repos[i])
			}
		}))
	}
	for _, finished := range done {
		<-finished
	}
	return identities
}
