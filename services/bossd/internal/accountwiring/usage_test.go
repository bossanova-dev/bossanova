package accountwiring

import (
	"context"
	"testing"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/domain"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestMaterializer_RecordUsageProbeStoresMetadataOnly_Accounts_AC5(t *testing.T) {
	reset5h := time.Now().Add(5 * time.Hour).UTC().Truncate(time.Millisecond)
	reset7d := time.Now().Add(7 * 24 * time.Hour).UTC().Truncate(time.Millisecond)
	store := &spyStore{accounts: map[string]*domain.AgentAccount{"a1": newClaudeAccount()}}
	client := &fakeRotationClient{
		env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "token"},
		probe: &bossanovav1.RateLimitStatus{
			Status:       bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_WARNING,
			Util_5H:      0.4,
			Reset_5H:     timestamppb.New(reset5h),
			Util_7D:      0.8,
			Reset_7D:     timestamppb.New(reset7d),
			PlanTier:     "max",
			ModelWindows: []*bossanovav1.UsageModelWindow{{ModelName: "Fable", Util: 0.7, ResetsAt: timestamppb.New(reset7d)}, {ModelName: "Opus", Util: 0.2}},
		},
	}
	creds := &fakeCreds{blob: []byte("secret-blob")}
	m := NewMaterializer(map[string]agent.RunnerClient{"claude": client}, store, creds, zerolog.Nop())
	probeCache := m.(interface {
		RecordUsageProbe(context.Context, string) error
	})

	if err := probeCache.RecordUsageProbe(context.Background(), "a1"); err != nil {
		t.Fatalf("RecordUsageProbe: %v", err)
	}
	if client.matCalls != 1 || client.probeCalls != 1 || creds.calls != 1 {
		t.Fatalf("calls mat=%d probe=%d creds=%d, want 1/1/1", client.matCalls, client.probeCalls, creds.calls)
	}
	if client.lastProbeEnv["CLAUDE_CODE_OAUTH_TOKEN"] != "token" {
		t.Fatalf("probe env = %v, want materialized token env", client.lastProbeEnv)
	}
	if store.usageCalls != 1 {
		t.Fatalf("RecordUsageProbe store calls = %d, want 1", store.usageCalls)
	}
	got := store.usageSnap
	if got.Util5h != 0.4 || got.Util7d != 0.8 {
		t.Errorf("utils = %v/%v, want 0.4/0.8", got.Util5h, got.Util7d)
	}
	if got.Status != "RATE_LIMIT_PLAN_STATUS_WARNING" || got.PlanTier != "max" {
		t.Errorf("status/plan = %q/%q, want warning/max", got.Status, got.PlanTier)
	}
	if got.Reset5h == nil || !got.Reset5h.Equal(reset5h) {
		t.Errorf("reset5h = %v, want %v", got.Reset5h, reset5h)
	}
	if got.Reset7d == nil || !got.Reset7d.Equal(reset7d) {
		t.Errorf("reset7d = %v, want %v", got.Reset7d, reset7d)
	}
	if len(got.ModelWindows) != 2 {
		t.Fatalf("model windows = %#v", got.ModelWindows)
	}
	if got.ModelWindows[0].ModelName != "Fable" || got.ModelWindows[0].Util != 0.7 || got.ModelWindows[0].ResetsAt == nil || !got.ModelWindows[0].ResetsAt.Equal(reset7d) || got.ModelWindows[1].ModelName != "Opus" || got.ModelWindows[1].Util != 0.2 || got.ModelWindows[1].ResetsAt != nil {
		t.Fatalf("model windows = %#v", got.ModelWindows)
	}
	if got.FetchedAt == nil {
		t.Fatal("FetchedAt = nil, want probe time")
	}
}
