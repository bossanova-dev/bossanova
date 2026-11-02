package daemonrpc

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"
)

// prWaitRPC records the deadline each WaitPRState call carried.
type prWaitRPC struct {
	fakeDaemonRPC
	remaining []time.Duration
	err       error
}

func (f *prWaitRPC) WaitPRState(ctx context.Context, req *connect.Request[pb.WaitPRStateRequest]) (*connect.Response[pb.WaitPRStateResponse], error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		f.remaining = append(f.remaining, -1)
	} else {
		f.remaining = append(f.remaining, time.Until(deadline))
	}
	if f.err != nil {
		return nil, f.err
	}
	return connect.NewResponse(&pb.WaitPRStateResponse{Outcome: pb.PRWaitOutcome_PR_WAIT_OUTCOME_MET, TimeoutSeconds: req.Msg.GetTimeoutSeconds()}), nil
}

func TestLocalClientWaitPRStateBoundsTheCallByTheWaitPlusGrace(t *testing.T) {
	for _, tc := range []struct {
		seconds int32
		want    time.Duration
	}{
		{0, config.PRWaitDefault + prWaitGrace},
		{60, time.Minute + prWaitGrace},
		{int32(config.PRWaitCeiling/time.Second) * 2, config.PRWaitCeiling + prWaitGrace},
	} {
		rpc := &prWaitRPC{}
		resp, err := (&LocalClient{rpc: rpc}).WaitPRState(context.Background(), &pb.WaitPRStateRequest{TimeoutSeconds: tc.seconds})
		if err != nil || resp.GetOutcome() != pb.PRWaitOutcome_PR_WAIT_OUTCOME_MET {
			t.Fatalf("timeout %d: WaitPRState = %v, %v", tc.seconds, resp, err)
		}
		if got := rpc.remaining[0]; got > tc.want || got < tc.want-5*time.Second {
			t.Fatalf("timeout %d: call deadline in %v, want %v", tc.seconds, got, tc.want)
		}
	}
}

func TestLocalClientWaitPRStateKeepsACallerDeadline(t *testing.T) {
	rpc := &prWaitRPC{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := (&LocalClient{rpc: rpc}).WaitPRState(ctx, &pb.WaitPRStateRequest{}); err != nil {
		t.Fatalf("WaitPRState: %v", err)
	}
	if got := rpc.remaining[0]; got > 10*time.Second {
		t.Fatalf("call deadline in %v, want the caller's 10s", got)
	}
}

func TestLocalClientWaitPRStateKeepsTheDaemonCode(t *testing.T) {
	rpc := &prWaitRPC{err: connect.NewError(connect.CodeNotFound, errors.New("repo acme/gadgets is not registered on this daemon"))}
	if _, err := (&LocalClient{rpc: rpc}).WaitPRState(context.Background(), &pb.WaitPRStateRequest{}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("err = %v, want not_found", err)
	}
}
