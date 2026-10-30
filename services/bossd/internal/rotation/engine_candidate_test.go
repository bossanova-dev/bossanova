package rotation

import (
	"context"
	"testing"
	"time"

	"github.com/recurser/bossd/internal/domain"
)

func TestDecideSkipsStatusOnlyRateLimited(t *testing.T) {
	a := mkAcct("A", 0, domain.AgentAccountHealthOK, domain.AgentAccountStatusActive, nil, nil)
	b := mkAcct("B", 1, domain.AgentAccountHealthOK, domain.AgentAccountStatusActive, nil, nil)
	eng := newEngineForTest(newFakeStore(a, b))
	out, err := eng.Decide(context.Background(), Signal{Provider: claude, Kind: UsageLimited, RotationCapable: true, CandidateUsage: map[string]domain.UsageSnapshot{
		"A": {Status: "RATE_LIMIT_PLAN_STATUS_RATE_LIMITED", FetchedAt: tp(0)}, "B": {Util7d: .3, FetchedAt: tp(0)},
	}})
	if err != nil || out.NextAccount == nil || out.NextAccount.ID != "B" {
		t.Fatalf("candidate %v error %v, want B", out.NextAccount, err)
	}
}

func TestSelectProactiveCandidateUtilizationCeiling_Accounts_AC31(t *testing.T) {
	for _, tt := range []struct {
		name    string
		ceiling float64
		model   string
		windows []domain.UsageModelWindow
		want    string
	}{
		{name: "fall through to later bucket", ceiling: .67, want: "B"},
		{name: "no qualifying candidate", ceiling: .05},
		{name: "model window above ceiling", ceiling: .67, model: "opus", windows: []domain.UsageModelWindow{{ModelName: "opus", Util: .90}}},
		{name: "ceiling includes equality", ceiling: .08, want: "B"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a := mkAcct("A", 0, domain.AgentAccountHealthOK, domain.AgentAccountStatusActive, nil, nil)
			b := mkAcct("B", 0, domain.AgentAccountHealthOK, domain.AgentAccountStatusActive, nil, nil)
			eng := newEngineForTest(newFakeStore(a, b))
			usage := map[string]domain.UsageSnapshot{"A": {Util7d: .83, Reset7d: tp(24 * time.Hour)}, "B": {Util7d: .08, Reset7d: tp(120 * time.Hour), ModelWindows: tt.windows}}
			got, err := eng.SelectProactiveCandidate(context.Background(), claude, tt.model, "bound", usage, tt.ceiling)
			if err != nil {
				t.Fatal(err)
			}
			if tt.want == "" {
				if got != nil {
					t.Fatalf("got %s want none", got.ID)
				}
			} else if got == nil || got.ID != tt.want {
				t.Fatalf("got %v want %s", got, tt.want)
			}
		})
	}
}
