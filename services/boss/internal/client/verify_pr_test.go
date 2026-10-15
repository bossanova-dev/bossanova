package client

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

// verifyRPC is a daemon client that records the verify verb requests.
type verifyRPC struct {
	bossanovav1connect.DaemonServiceClient
	approveReq  *pb.ApproveSessionPRRequest
	sendBackReq *pb.SendBackSessionPRRequest
}

func (f *verifyRPC) ApproveSessionPR(_ context.Context, req *connect.Request[pb.ApproveSessionPRRequest]) (*connect.Response[pb.ApproveSessionPRResponse], error) {
	f.approveReq = req.Msg
	return connect.NewResponse(&pb.ApproveSessionPRResponse{Result: &pb.VerifyApproveResult{IsApproved: true, Waives: "ledger-open"}}), nil
}

func (f *verifyRPC) SendBackSessionPR(_ context.Context, req *connect.Request[pb.SendBackSessionPRRequest]) (*connect.Response[pb.SendBackSessionPRResponse], error) {
	f.sendBackReq = req.Msg
	return connect.NewResponse(&pb.SendBackSessionPRResponse{IsPosted: true, CommentUrl: "https://x/1"}), nil
}

func TestLocalClient_VerifyVerbsCallTheDaemon(t *testing.T) {
	rpc := &verifyRPC{}
	c := &LocalClient{rpc: rpc}

	r, err := c.ApproveSessionPR(context.Background(), "s1", true)
	if err != nil || !r.GetIsApproved() || r.GetWaives() != "ledger-open" {
		t.Fatalf("ApproveSessionPR = %+v, %v", r, err)
	}
	if rpc.approveReq.GetId() != "s1" || !rpc.approveReq.GetIsDryRun() {
		t.Fatalf("approve request = %+v", rpc.approveReq)
	}

	resp, err := c.SendBackSessionPR(context.Background(), "s1", "fix it", false)
	if err != nil || !resp.GetIsPosted() || resp.GetCommentUrl() != "https://x/1" {
		t.Fatalf("SendBackSessionPR = %+v, %v", resp, err)
	}
	if rpc.sendBackReq.GetId() != "s1" || rpc.sendBackReq.GetNote() != "fix it" || rpc.sendBackReq.GetIsDryRun() {
		t.Fatalf("send-back request = %+v", rpc.sendBackReq)
	}
}

func TestRemoteClient_VerifyVerbsAreLocalOnly(t *testing.T) {
	t.Parallel()
	c, _ := newTestRemote(t)
	if _, err := c.ApproveSessionPR(context.Background(), "s1", false); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("ApproveSessionPR err = %v, want Unimplemented", err)
	}
	if _, err := c.SendBackSessionPR(context.Background(), "s1", "x", false); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("SendBackSessionPR err = %v, want Unimplemented", err)
	}
}
