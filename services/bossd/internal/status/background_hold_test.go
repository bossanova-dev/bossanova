package status

import (
	"testing"
	"time"

	"github.com/recurser/bossalib/displaystatus"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestBackgroundHoldVerdict_ChatPicker_AC24(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name            string
		busy, spinner   bool
		agents, changed time.Time
		working         bool
		held            bool
	}{
		{"shell under bound", true, false, time.Time{}, now.Add(-time.Minute), true, true},
		{"shell at bound", true, false, time.Time{}, now.Add(-BackgroundWorkHoldMax), false, false},
		{"shell past bound", true, false, time.Time{}, now.Add(-time.Hour), false, false},
		{"live spinner", true, true, time.Time{}, now.Add(-5 * time.Hour), true, false},
		{"subagent report", false, false, now.Add(time.Hour), now.Add(-time.Hour), true, true},
		{"expired subagent", false, false, now, now.Add(-time.Hour), false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, until, only := backgroundHoldVerdict(tt.busy, tt.spinner, tt.agents, tt.changed, now)
			if got != tt.working || only != tt.held || until.IsZero() == tt.held {
				t.Fatalf("got %v, %v, %v", got, until, only)
			}
		})
	}
}

func TestDisplayPhase_ChatPicker_AC23(t *testing.T) {
	tracker := NewTracker()
	tracker.SetPhase("a", "reviewing")
	if got := tracker.DisplayPhase("a"); got != "reviewing" {
		t.Fatal(got)
	}
	tracker.SetBackgroundHold("a", time.Now().Add(14*time.Minute))
	if got := tracker.DisplayPhase("a"); got != "background ≤14m" {
		t.Fatal(got)
	}
	tracker.SetBackgroundHold("a", time.Now().Add(-time.Second))
	if got := tracker.DisplayPhase("a"); got != "reviewing" {
		t.Fatal(got)
	}
	for remaining := time.Second; remaining <= BackgroundAgentsMaxAge; remaining += time.Second {
		label := backgroundHoldLabel(remaining)
		if _, err := displaystatus.NormalizePhase(label); err != nil {
			t.Fatalf("%q: %v", label, err)
		}
	}
	for _, tt := range []struct {
		remaining time.Duration
		want      string
	}{
		{time.Second, "background ≤1m"}, {59 * time.Minute, "background ≤59m"}, {time.Hour, "background ≤1h"},
		{time.Hour + time.Second, "background ≤2h"}, {4 * time.Hour, "background ≤4h"},
	} {
		if got := backgroundHoldLabel(tt.remaining); got != tt.want {
			t.Fatalf("%s = %q, want %q", tt.remaining, got, tt.want)
		}
	}
}

func TestBackgroundHoldCollected(t *testing.T) {
	for _, remove := range []bool{true, false} {
		tracker := NewTracker()
		tracker.Update("a", pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())
		tracker.SetBackgroundHold("a", time.Now().Add(time.Hour))
		tracker.SetBackgroundAgents("a", 1)
		if remove {
			tracker.Remove("a")
		} else {
			tracker.mu.Lock()
			tracker.entries["a"].ReceivedAt = time.Now().Add(-StaleThreshold - time.Second)
			tracker.mu.Unlock()
			tracker.Cleanup()
		}
		if !tracker.BackgroundHold("a").IsZero() || tracker.BackgroundAgentsRunning("a") {
			t.Fatal("collected chat retains background markers")
		}
	}
}
