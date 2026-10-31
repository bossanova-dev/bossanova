package main

import (
	"fmt"
	"testing"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

func TestRepairAttemptTitles_Sessions_AC152(t *testing.T) {
	for _, tc := range []struct {
		status bossanovav1.DisplayStatus
		title  string
	}{
		{bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT, "repair: conflict"},
		{bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, "repair: checks"},
		{bossanovav1.DisplayStatus_DISPLAY_STATUS_REJECTED, "repair: review"},
		{bossanovav1.DisplayStatus_DISPLAY_STATUS_UNSPECIFIED, "repair"},
	} {
		t.Run(tc.status.String(), func(t *testing.T) {
			for _, reclaim := range []bool{false, true} {
				t.Run(fmt.Sprintf("reclaim=%v", reclaim), func(t *testing.T) {
					mock := newTestMock()
					calls := 0
					mock.startFunc = func(*bossanovav1.StartChatRunHostRequest) (*bossanovav1.StartChatRunHostResponse, error) {
						calls++
						if reclaim && calls == 1 {
							return nil, grpcstatus.Error(codes.AlreadyExists, "tmux chat already active for session s1 (agent_session_id=stale-agent)")
						}
						return &bossanovav1.StartChatRunHostResponse{AgentSessionId: "fresh-agent"}, nil
					}
					rm := newTestMonitor(mock)
					repairFlagSet := false
					_, ok := rm.runRepairAttempt(t.Context(), "s1", "repo", "long session name", tc.status,
						true, "head", "", time.Time{}, "", true, "", "prior-agent", &repairFlagSet)
					require.True(t, ok)
					wantCalls := 1
					if reclaim {
						wantCalls = 2
					}
					require.Len(t, mock.startReqs, wantCalls)
					for _, req := range mock.startReqs {
						require.Equal(t, tc.title, req.GetTitle())
						require.Equal(t, "prior-agent", req.GetResumeAgentSessionId())
					}
				})
			}
		})
	}
}
