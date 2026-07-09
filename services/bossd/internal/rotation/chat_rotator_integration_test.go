package rotation_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/config"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/rotation"
	"github.com/recurser/bossd/internal/status"
)

// TestChatRotator_TrackerTransitionDrivesRotation mirrors the main.go wiring: a
// real status.Tracker → chained on-update hook → ChatRotator, with fakes only at
// the decide/switch seams. It proves the edge-triggered trigger end to end: a
// WORKING chat is never rotated, a transition to LIMITED drives exactly one
// rotation, and a same-status redraw does not re-fire the tracker hook.
//
// It lives in the external rotation_test package because status → db → rotation,
// so a white-box (package rotation) test importing status would form an import
// cycle; a black-box test breaks it.
func TestChatRotator_TrackerTransitionDrivesRotation(t *testing.T) {
	tracker := status.NewTracker()

	var mu sync.Mutex
	var switches []rotation.SwitchRequest
	rotator := rotation.NewChatRotator(rotation.ChatRotatorDeps{
		Logger:     zerolog.Nop(),
		LoadConfig: func() (config.ManagedAccountsConfig, error) { return config.ManagedAccountsConfig{}, nil },
		ChatContext: func(_ context.Context, _ string) (rotation.ChatContext, error) {
			return rotation.ChatContext{SessionID: "sess-1", RepoID: "repo-1", Provider: "claude", AccountID: "acct-capped"}, nil
		},
		CurrentStatus: func(id string) bossanovav1.ChatStatus {
			if e := tracker.Get(id); e != nil {
				return e.Status
			}
			return bossanovav1.ChatStatus_CHAT_STATUS_UNSPECIFIED
		},
		RateLimitProbe: func(context.Context, string) (models.UsageSnapshot, error) {
			fetched := time.Now().UTC()
			reset := fetched.Add(2 * time.Hour)
			return models.UsageSnapshot{
				Util5h:    1,
				Reset5h:   &reset,
				Status:    "RATE_LIMIT_PLAN_STATUS_RATE_LIMITED",
				FetchedAt: &fetched,
			}, nil
		},
		Decide: func(_ context.Context, _ rotation.DecideRequest) (rotation.Decision, error) {
			return rotation.Decision{Kind: rotation.DecisionSwitch, AccountID: "acct-b-id"}, nil
		},
		Switch: func(_ context.Context, req rotation.SwitchRequest) (rotation.SwitchResult, error) {
			mu.Lock()
			defer mu.Unlock()
			switches = append(switches, req)
			return rotation.SwitchResult{SwitchedToLabel: "acct-b"}, nil
		},
	})
	tracker.SetOnUpdate(func(agentSessionID string) {
		if e := tracker.Get(agentSessionID); e != nil {
			rotator.OnChatStatus(agentSessionID, e.Status, e.ResetAt)
		}
	})

	// WORKING → no rotation (false-positive regression guard).
	tracker.Update("chat-1", bossanovav1.ChatStatus_CHAT_STATUS_WORKING, time.Now())
	// Transition to LIMITED → exactly one rotation.
	tracker.UpdateLimited("chat-1", time.Time{}, time.Now())
	// Same-status redraw → tracker hook does not re-fire (edge trigger).
	tracker.UpdateLimited("chat-1", time.Time{}, time.Now())

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(switches)
		mu.Unlock()
		if n >= 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Settle: let any (erroneous) second dispatch land before asserting exactly one.
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(switches) != 1 {
		t.Fatalf("switches = %d, want exactly 1", len(switches))
	}
	if switches[0].AgentSessionID != "chat-1" || !switches[0].Auto {
		t.Fatalf("unexpected switch: %+v", switches[0])
	}
}
