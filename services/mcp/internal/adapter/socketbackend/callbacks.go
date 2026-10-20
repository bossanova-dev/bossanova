package socketbackend

import (
	"context"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// --- GitHub callbacks ---

func (b *Backend) CreateGitHubCallback(ctx context.Context, req *pb.CreateGitHubCallbackRequest) (*pb.CreateGitHubCallbackResponse, error) {
	resp, err := b.rpc.CreateGitHubCallback(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func (b *Backend) ListGitHubCallbacks(ctx context.Context, req *pb.ListGitHubCallbacksRequest) ([]*pb.GitHubCallback, error) {
	resp, err := b.rpc.ListGitHubCallbacks(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetGithubCallbacks(), nil
}

// DeleteGitHubCallback ignores targetChatID: this adapter's own daemon owns
// every callback in its registry, so the id alone resolves it. The parameter
// exists for the hosted gateway, which uses it to route the delete.
func (b *Backend) DeleteGitHubCallback(ctx context.Context, targetChatID, id string) (*pb.DeleteGitHubCallbackResponse, error) {
	resp, err := b.rpc.DeleteGitHubCallback(ctx, connect.NewRequest(&pb.DeleteGitHubCallbackRequest{
		Id:                 id,
		ExpectTargetChatId: stringPtrIfNotEmpty(targetChatID),
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}
