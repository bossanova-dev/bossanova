package server

import (
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/domain"
	goproto "google.golang.org/protobuf/proto"
)

func TestAccountToProto_UsageSnapshotRoundTrip_Accounts_AC5(t *testing.T) {
	reset5h := time.Now().Add(5 * time.Hour).UTC().Truncate(time.Millisecond)
	reset7d := time.Now().Add(7 * 24 * time.Hour).UTC().Truncate(time.Millisecond)
	fetchedAt := time.Now().UTC().Truncate(time.Millisecond)
	account := &domain.AgentAccount{
		ID:            "acct-usage",
		Provider:      domain.AgentAccountProviderClaude,
		Label:         "work",
		Status:        domain.AgentAccountStatusActive,
		Health:        domain.AgentAccountHealthOK,
		CreatedAt:     fetchedAt,
		UpdatedAt:     fetchedAt,
		LastTestError: "",
		Usage: &domain.UsageSnapshot{
			Util5h:       0.25,
			Util7d:       0.75,
			Reset5h:      &reset5h,
			Reset7d:      &reset7d,
			Status:       "warning",
			PlanTier:     "max",
			FetchedAt:    &fetchedAt,
			ModelWindows: []domain.UsageModelWindow{{ModelName: "Fable", Util: 0.7, ResetsAt: &reset7d}, {ModelName: "Opus", Util: 0.2}},
		},
	}

	got := accountToProto(account)
	if got.Usage == nil {
		t.Fatal("Usage = nil, want populated")
	}
	wire, err := goproto.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded pb.AgentAccount
	if err := goproto.Unmarshal(wire, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.GetUsage().GetUtil_5H() != account.Usage.Util5h || decoded.GetUsage().GetUtil_7D() != account.Usage.Util7d {
		t.Errorf("utils = %v/%v, want %v/%v", decoded.GetUsage().GetUtil_5H(), decoded.GetUsage().GetUtil_7D(), account.Usage.Util5h, account.Usage.Util7d)
	}
	if decoded.GetUsage().GetStatus() != account.Usage.Status || decoded.GetUsage().GetPlanTier() != account.Usage.PlanTier {
		t.Errorf("status/plan = %q/%q, want %q/%q", decoded.GetUsage().GetStatus(), decoded.GetUsage().GetPlanTier(), account.Usage.Status, account.Usage.PlanTier)
	}
	if !decoded.GetUsage().GetReset_5H().AsTime().Equal(reset5h) {
		t.Errorf("reset_5h = %v, want %v", decoded.GetUsage().GetReset_5H().AsTime(), reset5h)
	}
	if !decoded.GetUsage().GetReset_7D().AsTime().Equal(reset7d) {
		t.Errorf("reset_7d = %v, want %v", decoded.GetUsage().GetReset_7D().AsTime(), reset7d)
	}
	windows := decoded.GetUsage().GetModelWindows()
	if len(windows) != 2 {
		t.Fatalf("model windows = %v, want two", windows)
	}
	if windows[0].GetModelName() != "Fable" || windows[0].GetUtil() != 0.7 || !windows[0].GetResetsAt().AsTime().Equal(reset7d) || windows[1].GetModelName() != "Opus" || windows[1].GetUtil() != 0.2 || windows[1].GetResetsAt() != nil {
		t.Fatalf("model windows = %v", windows)
	}
	if !decoded.GetUsage().GetFetchedAt().AsTime().Equal(fetchedAt) {
		t.Errorf("fetched_at = %v, want %v", decoded.GetUsage().GetFetchedAt().AsTime(), fetchedAt)
	}
}
