package main

import (
	"context"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	server "github.com/recurser/bossd/api/daemon"
)

type proofCheckRunUpstream interface {
	UpsertProofCheckRun(context.Context, *connect.Request[pb.UpsertProofCheckRunRequest]) (*connect.Response[pb.UpsertProofCheckRunResponse], error)
}

type proofCheckRunClient struct {
	rpc proofCheckRunUpstream
}

var _ server.ProofCheckRuns = proofCheckRunClient{}

func (c proofCheckRunClient) UpsertProofCheckRun(ctx context.Context, sessionToken string, in server.ProofCheckRun) (server.ProofCheckRunResult, error) {
	req := connect.NewRequest(&pb.UpsertProofCheckRunRequest{RepoOriginUrl: in.RepoOriginURL, HeadSha: in.HeadSHA, Title: in.Title, Summary: in.Summary, DetailsUrl: in.DetailsURL})
	req.Header().Set("Authorization", "Bearer "+sessionToken)
	resp, err := c.rpc.UpsertProofCheckRun(ctx, req)
	if err != nil {
		return server.ProofCheckRunResult{}, err
	}
	return server.ProofCheckRunResult{HTMLURL: resp.Msg.HtmlUrl, IsCreated: resp.Msg.IsCreated}, nil
}
