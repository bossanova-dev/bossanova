package main

import (
	"context"
	"testing"

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

func TestAgentRunnerServiceDescIncludesRemoveAgentRunHook(t *testing.T) {
	for _, method := range agentRunnerServiceDesc.Methods {
		if method.MethodName == "RemoveAgentRunHook" {
			return
		}
	}
	t.Fatal("agentRunnerServiceDesc missing RemoveAgentRunHook")
}
