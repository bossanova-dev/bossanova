package status

import (
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestPhaseLifecycle(t *testing.T) {
	tracker := NewTracker()
	tracker.SetPhase("chat", "reviewing")
	if tracker.Phase("chat") != "reviewing" {
		t.Fatal("phase not stored")
	}
	for _, state := range []pb.ChatStatus{pb.ChatStatus_CHAT_STATUS_WORKING, pb.ChatStatus_CHAT_STATUS_QUESTION} {
		tracker.Update("chat", state, time.Now())
		if tracker.Phase("chat") != "reviewing" {
			t.Fatal("active phase cleared")
		}
	}
	tracker.UpdateLimited("chat", time.Now(), time.Now().Add(time.Hour))
	if tracker.Phase("chat") != "reviewing" {
		t.Fatal("limited phase cleared")
	}
	for _, state := range []pb.ChatStatus{pb.ChatStatus_CHAT_STATUS_IDLE, pb.ChatStatus_CHAT_STATUS_STOPPED} {
		tracker.SetPhase("chat", "reviewing")
		tracker.Update("chat", state, time.Now())
		if tracker.Phase("chat") != "" {
			t.Fatal("resting phase retained")
		}
	}
	tracker.SetPhase("chat", "reviewing")
	tracker.SetPhase("chat", "")
	if tracker.Phase("chat") != "" {
		t.Fatal("explicit clear failed")
	}
	tracker.SetPhase("chat", "reviewing")
	tracker.Remove("chat")
	if tracker.Phase("chat") != "" {
		t.Fatal("removed phase retained")
	}
	tracker.Update("chat", pb.ChatStatus_CHAT_STATUS_WORKING, time.Now())
	tracker.SetPhase("chat", "reviewing")
	tracker.entries["chat"].ReceivedAt = time.Now().Add(-2 * StaleThreshold)
	tracker.SetPhase("orphan", "building")
	tracker.Cleanup()
	if tracker.Phase("chat") != "" || tracker.Phase("orphan") != "" {
		t.Fatal("cleanup retained phase")
	}
}
