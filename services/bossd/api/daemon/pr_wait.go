package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"

	"github.com/recurser/bossd/internal/prstate"
)

// PRWaiter blocks on one PR condition (*prstate.WaitHub).
type PRWaiter interface {
	Wait(ctx context.Context, req prstate.WaitRequest) (prstate.WaitResult, error)
}

// prWaitConditions maps the wire condition to the hub's; the values share
// numbering (pinned by TestPRWaitEnumNumbering).
var prWaitConditions = map[pb.PRWaitCondition]prstate.WaitCondition{
	pb.PRWaitCondition_PR_WAIT_CONDITION_CHECKS_SETTLED: prstate.WaitChecksSettled,
	pb.PRWaitCondition_PR_WAIT_CONDITION_MERGEABLE:      prstate.WaitMergeable,
	pb.PRWaitCondition_PR_WAIT_CONDITION_HEAD_CHANGED:   prstate.WaitHeadChanged,
}

// WaitPRState blocks until a PR condition is met or unreachable, or the
// timeout passes. The request is validated and its repo resolved before any
// GitHub read, exactly as GetPRState does. The timeout is defaulted and
// clamped to config.PRWaitCeiling, never refused for its length, and the
// applied value is echoed back.
func (s *Server) WaitPRState(ctx context.Context, req *connect.Request[pb.WaitPRStateRequest]) (*connect.Response[pb.WaitPRStateResponse], error) {
	if s.prWaits == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("PR wait hub not configured"))
	}
	cond, ok := prWaitConditions[req.Msg.GetCondition()]
	if !ok {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("condition must be checks-settled, mergeable or head-changed, got %s", req.Msg.GetCondition()))
	}
	if req.Msg.GetTimeoutSeconds() < 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("timeout_seconds must not be negative, got %d", req.Msg.GetTimeoutSeconds()))
	}
	timeout := config.PRWaitTimeout(req.Msg.GetTimeoutSeconds())
	slug, pr, err := s.registeredPR(ctx, req.Msg.GetRepo(), req.Msg.GetPrNumber())
	if err != nil {
		return nil, err
	}
	res, err := s.prWaits.Wait(ctx, prstate.WaitRequest{
		Repo:            slug,
		PR:              pr,
		Condition:       cond,
		SinceHeadSHA:    req.Msg.GetSinceHeadSha(),
		Timeout:         timeout,
		Fresh:           req.Msg.GetShouldBypassCache(),
		AcceptNoGateRan: req.Msg.GetShouldAcceptNoGateRan(),
	})
	if err != nil {
		return nil, prReadError("wait on PR state", slug, pr, err)
	}
	return connect.NewResponse(&pb.WaitPRStateResponse{
		Outcome:        pb.PRWaitOutcome(clampInt32(int(res.Outcome))),
		Snapshot:       prStateResponse(slug, pr, res.Snapshot),
		TimeoutSeconds: clampInt32(int(timeout / time.Second)),
	}), nil
}
