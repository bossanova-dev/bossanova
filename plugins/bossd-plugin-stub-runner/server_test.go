package main

import (
	"context"
	"net"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

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

func TestRemoveAgentRunHookDispatch(t *testing.T) {
	client := newGeneratedTestClient(t)
	resp, err := client.RemoveAgentRunHook(context.Background(), &bossanovav1.RemoveAgentRunHookRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetIsSupported() {
		t.Fatal("stub hook unexpectedly supported")
	}
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

func TestIgnoresUnattendedPermissionPolicy(t *testing.T) {
	s := newServer(zerolog.Nop())
	worktree := t.TempDir()
	policy := &bossanovav1.UnattendedPermissionPolicy{
		WorktreePath:           worktree,
		AllowedCommandPrefixes: []string{"git", "make"},
		IsNetworkAllowed:       true,
	}

	runResp, err := s.StartAgentRun(context.Background(), &bossanovav1.StartAgentRunRequest{
		WorkDir:                    worktree,
		Plan:                       "work",
		SessionId:                  "stub-policy-run",
		UnattendedPermissionPolicy: policy,
	})
	if err != nil {
		t.Fatalf("StartAgentRun with policy: %v", err)
	}
	if runResp.GetSessionId() != "stub-policy-run" {
		t.Fatalf("SessionId = %q, want stub-policy-run", runResp.GetSessionId())
	}

	plainResp, err := s.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId:    "stub-plain",
		WorktreePath: worktree,
	})
	if err != nil {
		t.Fatalf("BuildInteractiveCommand without policy: %v", err)
	}
	policyResp, err := s.BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{
		SessionId:                  "stub-policy",
		WorktreePath:               worktree,
		UnattendedPermissionPolicy: policy,
	})
	if err != nil {
		t.Fatalf("BuildInteractiveCommand with policy: %v", err)
	}
	if len(policyResp.GetArgv()) == 0 || strings.Join(policyResp.GetArgv(), "\x00") != strings.Join(plainResp.GetArgv(), "\x00") {
		t.Fatalf("stub interactive argv with policy = %v, want unchanged %v", policyResp.GetArgv(), plainResp.GetArgv())
	}
}

func newGeneratedTestClient(t *testing.T) bossanovav1.AgentRunnerServiceClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	plugin := newAgentRunnerPlugin(zerolog.Nop())
	if err := plugin.GRPCServer(nil, srv); err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufconn", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(); srv.Stop(); _ = lis.Close() })
	return bossanovav1.NewAgentRunnerServiceClient(conn)
}
