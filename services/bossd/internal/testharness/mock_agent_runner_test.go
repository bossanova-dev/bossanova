package testharness

import (
	"context"
	"testing"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/agent"
)

func TestMockAgentRunnerSupportsHeadlessCapabilityProfileDispatch(t *testing.T) {
	runner := NewMockAgentRunner()
	profile := bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1

	var preflight agent.HeadlessCapabilityProfilePreflightDispatcher = runner
	if err := preflight.PreflightByAgentWithHeadlessCapabilityProfile(context.Background(), "codex", t.TempDir(), "", "", nil, profile); err != nil {
		t.Fatalf("preflight capability profile: %v", err)
	}

	var dispatcher agent.HeadlessCapabilityProfileDispatcher = runner
	sessionID, err := dispatcher.StartByAgentWithHeadlessCapabilityProfile(
		context.Background(), "codex", t.TempDir(), "plan", nil, "", "", "", nil, profile,
	)
	if err != nil {
		t.Fatalf("start capability profile: %v", err)
	}
	if !runner.IsRunning(sessionID) {
		t.Fatal("capability-profile dispatch did not start the mock session")
	}
}
