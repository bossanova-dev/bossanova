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
			// The post-snapshot output is still FRESH (younger than MinIdle):
			// the chat may currently be producing, so the refusal stands.
			name:         "output_after_snapshot_fresh_refuses",
			seedStatus:   statusPtr(bossanovav1.ChatStatus_CHAT_STATUS_IDLE),
			lastOutputAt: now.Add(-2 * time.Minute),
			observed:     now.Add(-10 * time.Minute), // snapshot older than last output → chat spoke since
			pol:          displacePolicy{MinIdle: 5 * time.Minute},
			wantAllow:    false,
		},
		{
			// BOS-515: the chat spoke once since the caller's (never-advanced)
			// snapshot and has been quiet past MinIdle ever since. It must not
			// stay pinned forever against that stale snapshot.
			name:         "output_after_snapshot_stale_idle_allows",
			seedStatus:   statusPtr(bossanovav1.ChatStatus_CHAT_STATUS_IDLE),
			lastOutputAt: now.Add(-6 * time.Minute),
			observed:     now.Add(-10 * time.Minute),
			pol:          displacePolicy{MinIdle: 5 * time.Minute},
			wantAllow:    true,
		},
		{
			// Falling through to the status switch must not weaken it: a
			// WORKING chat is still refused however stale its last output is.
			name:         "output_after_snapshot_stale_working_refuses",
			seedStatus:   statusPtr(bossanovav1.ChatStatus_CHAT_STATUS_WORKING),
			lastOutputAt: now.Add(-30 * time.Minute),
			observed:     now.Add(-40 * time.Minute),
			pol:          displacePolicy{MinIdle: 5 * time.Minute, QuestionIdle: 15 * time.Minute},
			wantAllow:    false,
		},
		{
			// …and a QUESTION still owes QuestionIdle, not just MinIdle.
			name:         "output_after_snapshot_stale_question_under_window_refuses",
			seedStatus:   statusPtr(bossanovav1.ChatStatus_CHAT_STATUS_QUESTION),
			lastOutputAt: now.Add(-10 * time.Minute),
			observed:     now.Add(-20 * time.Minute),
			pol:          displacePolicy{MinIdle: 5 * time.Minute, QuestionIdle: 15 * time.Minute},
			wantAllow:    false,
		},
		{
			// Fail-closed: a policy with no MinIdle has no staleness yardstick,
			// so the post-snapshot output can never be judged quiet.
			name:         "output_after_snapshot_zero_min_idle_refuses",
			seedStatus:   statusPtr(bossanovav1.ChatStatus_CHAT_STATUS_IDLE),
			lastOutputAt: now.Add(-10 * time.Hour),
			observed:     now.Add(-20 * time.Hour),
			pol:          displacePolicy{MinIdle: 0},
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
