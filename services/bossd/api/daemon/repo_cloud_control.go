package server

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"
	"github.com/recurser/bossd/internal/repoidentity"
)

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
	reader, _ := s.provider.(repoidentity.Reader)
	verifier := repoidentity.New(reader, s.logger)
	// Leave the settings callers' five-second deadline room to deliver metadata
	// even when GitHub is unavailable. Later repos fall back to unverified identity.
	identityCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	response := &pb.ListRepoCloudControlsResponse{}
	for _, r := range repos {
		id := verifier.Identity(identityCtx, *r)
		mode, source := config.ResolveRepoCloudControl(settings, id)
		response.Repos = append(response.Repos, &pb.RepoCloudControl{DisplayName: r.DisplayName, Host: id.Host, Checkout: id.LocalPath, ProviderRepoId: id.ProviderRepoID, IsIdVerified: id.IsIDVerified, Name: id.Name, OriginUrl: r.OriginURL, EffectiveMode: string(mode), Source: string(source), HasUnverifiedGrant: config.HasUnverifiedRepoGrant(settings, id)})
	}
	return connect.NewResponse(response), nil
}
