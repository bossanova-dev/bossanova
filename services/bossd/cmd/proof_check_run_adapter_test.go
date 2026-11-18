package main

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	server "github.com/recurser/bossd/api/daemon"
)

type proofCheckRunRPC struct {
	request *connect.Request[pb.UpsertProofCheckRunRequest]
	err     error
}

func (r *proofCheckRunRPC) UpsertProofCheckRun(_ context.Context, req *connect.Request[pb.UpsertProofCheckRunRequest]) (*connect.Response[pb.UpsertProofCheckRunResponse], error) {
	r.request = req
	return connect.NewResponse(&pb.UpsertProofCheckRunResponse{HtmlUrl: "https://github.com/run/1", IsCreated: true}), r.err
}

func TestProofCheckRunClientSendsDaemonBearer(t *testing.T) {
	rpc := &proofCheckRunRPC{}
	client := proofCheckRunClient{rpc: rpc}
	in := server.ProofCheckRun{RepoOriginURL: "https://github.com/acme/widgets", HeadSHA: "head", Title: "title", Summary: "summary", DetailsURL: "https://proof.example/run"}
	out, err := client.UpsertProofCheckRun(context.Background(), "daemon-session-token", in)
	if err != nil {
		t.Fatal(err)
	}
	if rpc.request.Header().Get("Authorization") != "Bearer daemon-session-token" {
		t.Fatal("missing daemon bearer")
	}
	m := rpc.request.Msg
	if m.RepoOriginUrl != in.RepoOriginURL || m.HeadSha != in.HeadSHA || m.Title != in.Title || m.Summary != in.Summary || m.DetailsUrl != in.DetailsURL {
		t.Fatalf("request = %v", m)
	}
	if out.HTMLURL != "https://github.com/run/1" || !out.IsCreated {
		t.Fatalf("result = %+v", out)
	}
	rpc.err = connect.NewError(connect.CodePermissionDenied, errors.New("refused"))
	if _, err := client.UpsertProofCheckRun(context.Background(), "token", in); err != rpc.err {
		t.Fatalf("error = %v, want unchanged upstream error", err)
	}
}
