package main

import (
	"context"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	server "github.com/recurser/bossd/api/daemon"
)

type hostedProofViewUpstream interface {
	MintProofViewLink(context.Context, *connect.Request[pb.MintProofViewLinkRequest]) (*connect.Response[pb.MintProofViewLinkResponse], error)
}

// hostedProofViewClient mints hosted proof view links as the signed-in user.
// The orchestrator refuses a daemon token, so it sends the user's own.
type hostedProofViewClient struct {
	rpc       hostedProofViewUpstream
	userToken func() string
}

var _ server.HostedProofViews = hostedProofViewClient{}

func (c hostedProofViewClient) MintProofViewLink(ctx context.Context, in server.HostedProofViewTarget) (server.HostedProofViewLink, error) {
	token := ""
	if c.userToken != nil {
		token = c.userToken()
	}
	if token == "" {
		return server.HostedProofViewLink{}, server.ErrHostedProofViewSignedOut
	}
	req := connect.NewRequest(&pb.MintProofViewLinkRequest{RepoOriginUrl: in.RepoOriginURL, PrNumber: in.PRNumber, HeadSha: in.HeadSHA})
	req.Header().Set("Authorization", "Bearer "+token)
	resp, err := c.rpc.MintProofViewLink(ctx, req)
	if err != nil {
		return server.HostedProofViewLink{}, err
	}
	return server.HostedProofViewLink{ViewURL: resp.Msg.GetViewUrl(), ExpiresAt: resp.Msg.GetExpiresAt().AsTime()}, nil
}

// userTokenOr reads the provider's refreshed user token, falling back to the
// fixed one (BOSSD_USER_JWT) when the provider holds none, as register does.
func userTokenOr(provider func() string, fallback string) func() string {
	return func() string {
		if token := provider(); token != "" {
			return token
		}
		return fallback
	}
}
