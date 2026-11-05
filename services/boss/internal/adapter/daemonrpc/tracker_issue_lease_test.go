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

type trackerIssueLeaseRPC struct {
	bossanovav1connect.DaemonServiceClient
	acquireReq    *pb.AcquireTrackerIssueLeaseRequest
	releaseReq    *pb.ReleaseTrackerIssueLeaseRequest
	acquireResult *pb.AcquireTrackerIssueLeaseResponse
	releaseResult *pb.ReleaseTrackerIssueLeaseResponse
	err           error
}

func (f *trackerIssueLeaseRPC) AcquireTrackerIssueLease(_ context.Context, req *connect.Request[pb.AcquireTrackerIssueLeaseRequest]) (*connect.Response[pb.AcquireTrackerIssueLeaseResponse], error) {
	f.acquireReq = req.Msg
	return connect.NewResponse(f.acquireResult), f.err
}

func (f *trackerIssueLeaseRPC) ReleaseTrackerIssueLease(_ context.Context, req *connect.Request[pb.ReleaseTrackerIssueLeaseRequest]) (*connect.Response[pb.ReleaseTrackerIssueLeaseResponse], error) {
	f.releaseReq = req.Msg
	return connect.NewResponse(f.releaseResult), f.err
}

func TestLocalClient_TrackerIssueLease(t *testing.T) {
	f := &trackerIssueLeaseRPC{
		acquireResult: &pb.AcquireTrackerIssueLeaseResponse{IsAcquired: true, Scope: pb.TrackerIssueLeaseScope_TRACKER_ISSUE_LEASE_SCOPE_DAEMON},
		releaseResult: &pb.ReleaseTrackerIssueLeaseResponse{},
	}
	c := &LocalClient{rpc: f}
	acquireReq := &pb.AcquireTrackerIssueLeaseRequest{IssueKey: "linear:BOS-1", HolderId: "0123456789abcdef0123456789abcdef", TtlSeconds: 60}
	acquired, err := c.AcquireTrackerIssueLease(context.Background(), acquireReq)
	if err != nil || f.acquireReq != acquireReq || acquired != f.acquireResult {
		t.Fatalf("acquire=%v err=%v req=%v", acquired, err, f.acquireReq)
	}
	releaseReq := &pb.ReleaseTrackerIssueLeaseRequest{IssueKey: "linear:BOS-1", HolderId: "0123456789abcdef0123456789abcdef"}
	released, err := c.ReleaseTrackerIssueLease(context.Background(), releaseReq)
	if err != nil || f.releaseReq != releaseReq || released != f.releaseResult {
		t.Fatalf("release=%v err=%v req=%v", released, err, f.releaseReq)
	}
	f.err = connect.NewError(connect.CodeInvalidArgument, errors.New("bad holder"))
	if _, err = c.AcquireTrackerIssueLease(context.Background(), acquireReq); rpcerr.KindOf(err) != rpcerr.InvalidArgument {
		t.Fatalf("acquire error=%v", err)
	}
	if _, err = c.ReleaseTrackerIssueLease(context.Background(), releaseReq); rpcerr.KindOf(err) != rpcerr.InvalidArgument {
		t.Fatalf("release error=%v", err)
	}
}
