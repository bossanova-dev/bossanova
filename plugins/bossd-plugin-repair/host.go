package main

import (
	"context"
	"math"
	"os"
	"strconv"
	"time"

	goplugin "github.com/hashicorp/go-plugin"
	"github.com/recurser/bossalib/config"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/plugin/hostclient"
	"github.com/rs/zerolog"
)

// hostClient is the repair consumer surface; PR polling belongs to task-source plugins.
type hostClient interface {
	ListSessions(ctx context.Context) (*bossanovav1.HostServiceListSessionsResponse, error)
	GetReviewComments(ctx context.Context, req *bossanovav1.GetReviewCommentsRequest) (*bossanovav1.GetReviewCommentsResponse, error)
	GetBranchHead(ctx context.Context, req *bossanovav1.GetBranchHeadRequest) (*bossanovav1.GetBranchHeadResponse, error)
	FireSessionEvent(ctx context.Context, req *bossanovav1.FireSessionEventRequest) (*bossanovav1.FireSessionEventResponse, error)
	SetRepairStatus(ctx context.Context, req *bossanovav1.SetRepairStatusRequest) (*bossanovav1.SetRepairStatusResponse, error)
	StartAgentRun(ctx context.Context, req *bossanovav1.StartAgentRunHostRequest) (*bossanovav1.StartAgentRunHostResponse, error)
	WaitAgentRun(ctx context.Context, req *bossanovav1.WaitAgentRunHostRequest) (*bossanovav1.WaitAgentRunHostResponse, error)
	StartChatRun(ctx context.Context, req *bossanovav1.StartChatRunHostRequest) (*bossanovav1.StartChatRunHostResponse, error)
	WaitChatRun(ctx context.Context, req *bossanovav1.WaitChatRunHostRequest) (*bossanovav1.WaitChatRunHostResponse, error)
	ReclaimRepairChat(ctx context.Context, req *bossanovav1.ReclaimRepairChatHostRequest) (*bossanovav1.ReclaimRepairChatHostResponse, error)
	RecordRepairOutcome(ctx context.Context, req *bossanovav1.RecordRepairOutcomeRequest) (*bossanovav1.RecordRepairOutcomeResponse, error)
	RecordRunTelemetry(ctx context.Context, req *bossanovav1.RecordRunTelemetryRequest) (*bossanovav1.RecordRunTelemetryResponse, error)
}

// newEagerHostServiceClient creates an eager host service client that dials
// the host service in the background via the go-plugin broker.
func newEagerHostServiceClient(broker *goplugin.GRPCBroker, logger zerolog.Logger) hostClient {
	return hostclient.NewEagerClient(broker, logger, hostclient.WithStartChatRunTimeout(startChatRunBudgetFromEnv()))
}

func startChatRunBudgetFromEnv() time.Duration {
	seconds, err := strconv.ParseInt(os.Getenv("BOSS_PLUGIN_"+config.SessionStartReadyDeadlinePluginKey), 10, 64)
	if err != nil || seconds <= 0 {
		return config.StartChatRunBudgetFor(0)
	}
	if seconds > math.MaxInt64/int64(time.Second) {
		return config.StartChatRunBudgetFor(time.Duration(math.MaxInt64))
	}
	return config.StartChatRunBudgetFor(time.Duration(seconds) * time.Second)
}
