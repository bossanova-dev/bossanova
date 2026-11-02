package daemonrpc

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

// verifyRPC is a daemon client that records the verify verb requests.
type verifyRPC struct {
	bossanovav1connect.DaemonServiceClient
	followUpReq    *pb.FollowUpSessionPRNotesRequest
	followUpErr    error
	followUpResult *pb.VerifyFollowUpResult
	approveReq     *pb.ApproveSessionPRRequest
	mergeReq       *pb.OperatorMergeSessionRequest
}

func (f *verifyRPC) ApproveSessionPR(_ context.Context, req *connect.Request[pb.ApproveSessionPRRequest]) (*connect.Response[pb.ApproveSessionPRResponse], error) {
	f.approveReq = req.Msg
	return connect.NewResponse(&pb.ApproveSessionPRResponse{Result: &pb.VerifyApproveResult{IsApproved: true, Waives: "ledger-open"}}), nil
}

func (f *verifyRPC) OperatorMergeSession(_ context.Context, req *connect.Request[pb.OperatorMergeSessionRequest]) (*connect.Response[pb.OperatorMergeSessionResponse], error) {
	f.mergeReq = req.Msg
	return connect.NewResponse(&pb.OperatorMergeSessionResponse{IsMerged: true, TrackerWrites: "ok"}), nil
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

	head := "0123456789abcdef0123456789abcdef01234567"
	resp, err := c.OperatorMergeSession(context.Background(), "s1", head, true, []string{"human-review:ui"})
	if err != nil || !resp.GetIsMerged() || resp.GetTrackerWrites() != "ok" {
		t.Fatalf("OperatorMergeSession = %+v, %v", resp, err)
	}
	if m := rpc.mergeReq; m.GetId() != "s1" || m.GetExpectedHeadSha() != head || !m.GetShouldApprove() || len(m.GetWaives()) != 1 || m.GetWaives()[0] != "human-review:ui" {
		t.Fatalf("operator merge request = %+v", m)
	}
}

func (f *verifyRPC) FollowUpSessionPRNotes(_ context.Context, req *connect.Request[pb.FollowUpSessionPRNotesRequest]) (*connect.Response[pb.FollowUpSessionPRNotesResponse], error) {
	f.followUpReq = req.Msg
	return connect.NewResponse(&pb.FollowUpSessionPRNotesResponse{Result: f.followUpResult}), f.followUpErr
}
func TestLocalFollowUpSessionPRNotes(t *testing.T) {
	for _, all := range []bool{false, true} {
		ids := []string{"note:000000000001", "note:000000000002"}
		if all {
			ids = nil
		}
		result := &pb.VerifyFollowUpResult{Reason: "unknown-note", UnknownVerifyNoteIds: ids}
		fake := &verifyRPC{followUpResult: result}
		c := &LocalClient{rpc: fake}
		got, err := c.FollowUpSessionPRNotes(t.Context(), "s1", ids, all, true)
		if err != nil || got != result {
			t.Fatalf("result=%v err=%v", got, err)
		}
		req := fake.followUpReq
		if req.GetId() != "s1" || !reflect.DeepEqual(req.GetVerifyNoteIds(), ids) || req.GetShouldFollowUpAll() != all || !req.GetIsDryRun() {
			t.Fatalf("request=%v", req)
		}
		fake.followUpErr = connect.NewError(connect.CodePermissionDenied, errors.New("operator only"))
		if _, err = c.FollowUpSessionPRNotes(t.Context(), "s1", ids, all, false); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("err=%v", err)
		}
	}
}
