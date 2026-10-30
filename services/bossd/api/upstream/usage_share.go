package upstream

import (
	"context"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/domain"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// UsageShareState receives the poller role and shared throttle hint from bosso.
type UsageShareState interface {
	Set(accountID string, isPoller bool, throttledUntil time.Time)
}

// SharedUsageRecorder updates the local selection cache with a shared reading.
type SharedUsageRecorder interface {
	RecordUsageProbe(context.Context, string, domain.UsageSnapshot) error
}

func (c *StreamClient) dispatchApplyAgentAccountUsage(ctx context.Context, cmdID string, req *pb.ApplyAgentAccountUsageCommand) *pb.DaemonEvent {
	if req.GetLocalAgentAccountId() == "" {
		return commandErrCode(cmdID, "local account id is required", pb.CommandResult_ERROR_CODE_INVALID_ARGUMENT)
	}
	if c.usageShare == nil || c.usageRecorder == nil {
		return commandErrCode(cmdID, "shared usage is not wired", pb.CommandResult_ERROR_CODE_UNIMPLEMENTED)
	}
	var until time.Time
	if req.GetThrottledUntilAt() != nil {
		until = req.GetThrottledUntilAt().AsTime()
	}
	c.usageShare.Set(req.GetLocalAgentAccountId(), req.GetIsUsagePoller(), until)
	if usage := req.GetUsage(); usage != nil {
		snapshot := domain.UsageSnapshot{
			Util5h: usage.GetUtil_5H(), Util7d: usage.GetUtil_7D(),
			Reset5h: sharedUsageTime(usage.GetReset_5H()), Reset7d: sharedUsageTime(usage.GetReset_7D()),
			Status: usage.GetStatus(), PlanTier: usage.GetPlanTier(), FetchedAt: sharedUsageTime(usage.GetFetchedAt()),
		}
		for _, window := range usage.GetModelWindows() {
			snapshot.ModelWindows = append(snapshot.ModelWindows, domain.UsageModelWindow{
				ModelName: window.GetModelName(), Util: window.GetUtil(), ResetsAt: sharedUsageTime(window.GetResetsAt()),
			})
		}
		if err := c.usageRecorder.RecordUsageProbe(ctx, req.GetLocalAgentAccountId(), snapshot); err != nil {
			return commandErr(cmdID, err.Error())
		}
	}
	return commandOK(cmdID, nil)
}

func sharedUsageTime(timestamp *timestamppb.Timestamp) *time.Time {
	if timestamp == nil {
		return nil
	}
	value := timestamp.AsTime()
	return &value
}
