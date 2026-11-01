package daemonrpc

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/domain/rpcerr"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

type verifyBlockersRPC struct {
	bossanovav1connect.DaemonServiceClient
	acceptReq    *pb.AcceptSessionPRBlockerRequest
	listReq      *pb.ListSessionPRBlockersRequest
	acceptResult *pb.AcceptSessionPRBlockerResponse
	listResult   *pb.ListSessionPRBlockersResponse
	err          error
}

func (f *verifyBlockersRPC) AcceptSessionPRBlocker(_ context.Context, req *connect.Request[pb.AcceptSessionPRBlockerRequest]) (*connect.Response[pb.AcceptSessionPRBlockerResponse], error) {
	f.acceptReq = req.Msg
	return connect.NewResponse(f.acceptResult), f.err
}
func (f *verifyBlockersRPC) ListSessionPRBlockers(_ context.Context, req *connect.Request[pb.ListSessionPRBlockersRequest]) (*connect.Response[pb.ListSessionPRBlockersResponse], error) {
	f.listReq = req.Msg
	return connect.NewResponse(f.listResult), f.err
}

func TestLocalClient_VerifyBlockers(t *testing.T) {
	f := &verifyBlockersRPC{acceptResult: &pb.AcceptSessionPRBlockerResponse{IsAccepted: true, TicketIdentifier: "TASK-1"}, listResult: &pb.ListSessionPRBlockersResponse{HeadSha: "head", Blockers: []*pb.VerifyBlocker{{Id: "review:012345abcdef"}}}}
	c := &LocalClient{rpc: f}
	req := &pb.AcceptSessionPRBlockerRequest{Id: "s-a", BlockerId: "review:012345abcdef", Reason: "reason", ExpectedHeadSha: "head", IsDryRun: true}
	accepted, err := c.AcceptSessionPRBlocker(context.Background(), req)
	if err != nil || f.acceptReq != req || accepted != f.acceptResult {
		t.Fatalf("accept=%v err=%v req=%v", accepted, err, f.acceptReq)
	}
	listed, err := c.ListSessionPRBlockers(context.Background(), "s-a")
	if err != nil || f.listReq.GetId() != "s-a" || listed != f.listResult {
		t.Fatalf("list=%v err=%v req=%v", listed, err, f.listReq)
	}
	f.err = connect.NewError(connect.CodePermissionDenied, errors.New("operator only"))
	if _, err = c.AcceptSessionPRBlocker(context.Background(), req); rpcerr.KindOf(err) != rpcerr.PermissionDenied {
		t.Fatalf("accept error=%v", err)
	}
	if _, err = c.ListSessionPRBlockers(context.Background(), "s-a"); rpcerr.KindOf(err) != rpcerr.PermissionDenied {
		t.Fatalf("list error=%v", err)
	}
}
