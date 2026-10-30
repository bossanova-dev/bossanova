package main

import (
	"context"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/contract/plugin/agentruntime"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestBuildArgvCompanionSentinel_Accounts_AC30(t *testing.T) {
	const sentinel = "sk-ant-boss-managed-rotation-sentinel-00000000000000"
	for _, tt := range []struct {
		name, key string
		want      bool
	}{
		{"absent", "", false},
		{"sentinel", sentinel, true},
		{"other key", "unrelated-api-key", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			argv := NewRunner(zerolog.Nop()).buildArgv(agentruntime.BuildArgvInput{ExtraEnv: map[string]string{"ANTHROPIC_API_KEY": tt.key}})
			joined := strings.Join(argv, "\x00")
			override := `shell_environment_policy.set.ANTHROPIC_API_KEY="` + sentinel + `"`
			if got := strings.Contains(joined, "-c\x00"+override); got != tt.want {
				t.Fatalf("sentinel override present = %v, want %v", got, tt.want)
			}
			if tt.key != "" && !tt.want && strings.Contains(joined, tt.key) {
				t.Fatal("non-sentinel key leaked into argv")
			}
		})
	}
}

func TestInteractiveCompanionSentinel_Accounts_AC30(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		resp, err := newTestServer(t).BuildInteractiveCommand(context.Background(), &bossanovav1.BuildInteractiveCommandRequest{IsClaudeProxyEnabled: enabled})
		if err != nil {
			t.Fatal(err)
		}
		override := `shell_environment_policy.set.ANTHROPIC_API_KEY="` + companionSentinelAPIKey + `"`
		if got := strings.Contains(strings.Join(resp.Argv, "\x00"), "-c\x00"+override); got != enabled {
			t.Fatalf("sentinel override present = %v, want %v", got, enabled)
		}
	}
}
