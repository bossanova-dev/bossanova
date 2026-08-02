package main

import (
	"context"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestPreflightHeadlessRunDispatchReturnsUnimplemented(t *testing.T) {
	s := newServer(zerolog.Nop())
	for _, method := range agentRunnerServiceDesc.Methods {
		if method.MethodName != "PreflightHeadlessRun" {
			continue
		}
		decoded := false
		_, err := method.Handler(s, context.Background(), func(value any) error {
			req, ok := value.(*bossanovav1.PreflightHeadlessRunRequest)
			if !ok {
				t.Fatalf("decoded request type = %T, want *PreflightHeadlessRunRequest", value)
			}
			req.HeadlessCapabilityProfile = bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1
			decoded = true
			return nil
		}, nil)
		if !decoded {
			t.Fatal("PreflightHeadlessRun handler did not decode its request")
		}
		if status.Code(err) != codes.Unimplemented {
			t.Fatalf("PreflightHeadlessRun code = %s, want Unimplemented; err=%v", status.Code(err), err)
		}
		return
	}
	t.Fatal("agentRunnerServiceDesc missing PreflightHeadlessRun")
}

func TestConfigureFinalizeHookReturnsUnsupported(t *testing.T) {
	s := newServer(zerolog.Nop())
	resp, err := s.ConfigureFinalizeHook(context.Background(), &bossanovav1.ConfigureFinalizeHookRequest{
		WorkDir: t.TempDir(), SessionId: "s1", HookToken: "tkn", HookPort: 12345,
	})
	if err != nil {
		t.Fatalf("ConfigureFinalizeHook: %v", err)
	}
	if resp.IsSupported {
		t.Error("IsSupported = true, want false for stub")
	}
}

func TestRemoveAgentRunHookReturnsUnsupported(t *testing.T) {
	s := newServer(zerolog.Nop())
	resp, err := s.RemoveAgentRunHook(context.Background(), &bossanovav1.RemoveAgentRunHookRequest{
		WorkDir: t.TempDir(), AgentSessionId: "agent-1",
	})
	if err != nil {
		t.Fatalf("RemoveAgentRunHook: %v", err)
	}
	if resp.IsSupported {
		t.Error("IsSupported = true, want false for stub")
	}
}

func TestDetectUsageLimitAlwaysUnlimited(t *testing.T) {
	s := newServer(zerolog.Nop())
	resp, err := s.DetectUsageLimit(context.Background(), &bossanovav1.DetectUsageLimitRequest{
		PaneContent: []byte("❯ \nusage limit reached — resets in 5 hours\n"),
	})
	if err != nil {
		t.Fatalf("DetectUsageLimit: %v", err)
	}
	if resp.GetLimited() {
		t.Error("Limited = true, want false for stub (never renders a usage-cap banner)")
	}
	if resp.GetResetAt() != nil {
		t.Error("ResetAt set, want nil for stub")
	}
}

func TestProbeRateLimitAlwaysUnsupported(t *testing.T) {
	s := newServer(zerolog.Nop())
	resp, err := s.ProbeRateLimit(context.Background(), &bossanovav1.ProbeRateLimitRequest{})
	if err != nil {
		t.Fatalf("ProbeRateLimit: %v", err)
	}
	if got := resp.GetStatus().GetStatus(); got != bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_UNSUPPORTED {
		t.Errorf("Status = %v, want UNSUPPORTED for stub (queries no provider)", got)
	}
	if resp.GetStatus().GetLimited() {
		t.Error("Limited = true, want false for stub")
	}
}

func TestAgentRunnerServiceDescIncludesRemoveAgentRunHook(t *testing.T) {
	for _, method := range agentRunnerServiceDesc.Methods {
		if method.MethodName == "RemoveAgentRunHook" {
			return
		}
	}
	t.Fatal("agentRunnerServiceDesc missing RemoveAgentRunHook")
}

// The stub builds no real command line, so any instruction suffix bossd offers
// is dropped. Declaring NONE is what lets bossd say so out loud instead of
// assuming the E2E stub carried it.
func TestBuildInteractiveCommandDeclaresNoAppendSystemPromptSupport(t *testing.T) {
	s := newServer(zerolog.Nop())
	resp, err := s.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId:          "agent-1",
		AppendSystemPrompt: "You are running inside a bossanova-managed chat. ...",
	})
	if err != nil {
		t.Fatalf("BuildInteractiveCommand: %v", err)
	}
	if got := resp.GetAppendSystemPromptSupport(); got != bossanovav1.AppendSystemPromptSupport_APPEND_SYSTEM_PROMPT_SUPPORT_NONE {
		t.Fatalf("append_system_prompt_support = %v, want NONE", got)
	}
	if strings.Contains(strings.Join(resp.GetArgv(), " "), "append-system-prompt") {
		t.Fatalf("stub argv must stay benign: %v", resp.GetArgv())
	}
}
