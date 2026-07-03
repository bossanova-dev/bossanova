package plugin

import (
	"testing"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/status"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

func TestChatDisplaceable(t *testing.T) {
	now := time.Now()

	const agentID = "agent-1"

	cases := []struct {
		name string
		// seed, when non-nil, upserts a tracker entry before the call. When
		// nil the tracker is left empty (Get returns nil → fail-closed).
		seedStatus   *bossanovav1.ChatStatus
		lastOutputAt time.Time
		observed     time.Time
		pol          displacePolicy
		wantAllow    bool
	}{
		{
			name:      "no_tracker_entry_refuses",
			pol:       displacePolicy{MinIdle: 5 * time.Minute},
			wantAllow: false,
		},
		{
			name:         "working_refuses",
			seedStatus:   statusPtr(bossanovav1.ChatStatus_CHAT_STATUS_WORKING),
			lastOutputAt: now.Add(-1 * time.Hour),
			pol:          displacePolicy{MinIdle: 5 * time.Minute, QuestionIdle: 15 * time.Minute},
			wantAllow:    false,
		},
		{
			name:         "question_refuses_under_window",
			seedStatus:   statusPtr(bossanovav1.ChatStatus_CHAT_STATUS_QUESTION),
			lastOutputAt: now.Add(-10 * time.Minute),
			pol:          displacePolicy{MinIdle: 5 * time.Minute, QuestionIdle: 15 * time.Minute},
			wantAllow:    false,
		},
		{
			name:         "question_allows_past_window",
			seedStatus:   statusPtr(bossanovav1.ChatStatus_CHAT_STATUS_QUESTION),
			lastOutputAt: now.Add(-16 * time.Minute),
			pol:          displacePolicy{MinIdle: 5 * time.Minute, QuestionIdle: 15 * time.Minute},
			wantAllow:    true,
		},
		{
			name:         "question_zero_window_never_allows",
			seedStatus:   statusPtr(bossanovav1.ChatStatus_CHAT_STATUS_QUESTION),
			lastOutputAt: now.Add(-10 * time.Hour),
			pol:          displacePolicy{MinIdle: 5 * time.Minute, QuestionIdle: 0},
			wantAllow:    false,
		},
		{
			name:         "idle_refuses_under_min",
			seedStatus:   statusPtr(bossanovav1.ChatStatus_CHAT_STATUS_IDLE),
			lastOutputAt: now.Add(-2 * time.Minute),
			pol:          displacePolicy{MinIdle: 5 * time.Minute},
			wantAllow:    false,
		},
		{
			name:         "idle_allows_past_min",
			seedStatus:   statusPtr(bossanovav1.ChatStatus_CHAT_STATUS_IDLE),
			lastOutputAt: now.Add(-6 * time.Minute),
			pol:          displacePolicy{MinIdle: 5 * time.Minute},
			wantAllow:    true,
		},
		{
			name:         "stopped_allows",
			seedStatus:   statusPtr(bossanovav1.ChatStatus_CHAT_STATUS_STOPPED),
			lastOutputAt: now.Add(-1 * time.Second),
			pol:          displacePolicy{MinIdle: 5 * time.Minute},
			wantAllow:    true,
		},
		{
			name:         "output_after_snapshot_refuses",
			seedStatus:   statusPtr(bossanovav1.ChatStatus_CHAT_STATUS_IDLE),
			lastOutputAt: now.Add(-6 * time.Minute),
			observed:     now.Add(-10 * time.Minute), // snapshot older than last output → chat spoke since
			pol:          displacePolicy{MinIdle: 5 * time.Minute},
			wantAllow:    false,
		},
		{
			name:         "zero_observed_skips_snapshot_check",
			seedStatus:   statusPtr(bossanovav1.ChatStatus_CHAT_STATUS_IDLE),
			lastOutputAt: now.Add(-6 * time.Minute),
			observed:     time.Time{}, // zero → snapshot check skipped
			pol:          displacePolicy{MinIdle: 5 * time.Minute},
			wantAllow:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := status.NewTracker()
			if tc.seedStatus != nil {
				tr.Update(agentID, *tc.seedStatus, tc.lastOutputAt)
			}
			s := &HostServiceServer{chatTracker: tr}

			err := s.chatDisplaceable(agentID, tc.observed, tc.pol, now)

			if tc.wantAllow {
				if err != nil {
					t.Fatalf("expected displaceable (allow), got error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected refusal, got nil error")
			}
			if got := grpcstatus.Code(err); got != codes.FailedPrecondition {
				t.Fatalf("expected FailedPrecondition, got code=%s err=%v", got, err)
			}
		})
	}
}

func statusPtr(s bossanovav1.ChatStatus) *bossanovav1.ChatStatus { return &s }
