package rotation

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/recurser/bossd/internal/credsync"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/dbtest"
	"github.com/recurser/bossd/internal/domain"
)

func TestCredentialSyncSelection_Accounts_AC224701(t *testing.T) {
	ctx := context.Background()
	for _, state := range []credsync.ReadinessState{credsync.ReadinessBootstrapUnpinned, credsync.ReadinessReady, credsync.ReadinessUnknown} {
		t.Run(string(state), func(t *testing.T) {
			synced := mkAcct("synced", 0, domain.AgentAccountHealthOK, domain.AgentAccountStatusActive, nil, nil)
			synced.Provider, synced.SyncedAgentAccountID = domain.AgentAccountProviderCodex, "cloud"
			local := mkAcct("local", 1, domain.AgentAccountHealthOK, domain.AgentAccountStatusActive, nil, nil)
			local.Provider = domain.AgentAccountProviderCodex
			readiness := credsync.Readiness{State: state}
			gate := readiness.AgentAccountBlockReason
			eng := newEngineForTest(newFakeStore(synced, local), WithCredentialSyncGate(gate))
			want := synced.ID
			if state == credsync.ReadinessBootstrapUnpinned {
				want = local.ID
			}
			for _, usage := range []map[string]domain.UsageSnapshot{nil, {synced.ID: {}, local.ID: {}}} {
				out, err := eng.Decide(ctx, Signal{Provider: "codex", RotationCapable: true, CandidateUsage: usage})
				if err != nil || out.NextAccount == nil || out.NextAccount.ID != want {
					t.Fatalf("Decide = %+v, %v; want %s", out, err, want)
				}
			}
			usage := map[string]domain.UsageSnapshot{synced.ID: {}, local.ID: {}}
			if got, err := eng.SelectProactiveCandidate(ctx, "codex", "", "", usage, 1); err != nil || got == nil || got.ID != want {
				t.Fatalf("proactive = %+v, %v; want %s", got, err, want)
			}
			if Selectable(synced, gate) != (want == synced.ID) || BindableNow(synced, base, gate) != (want == synced.ID) {
				t.Fatal("binding and rotation eligibility disagree")
			}
		})
	}
}

func TestCredentialSyncCooldownRecovery_Accounts_AC224701(t *testing.T) {
	blocked := mkAcct("blocked", 0, domain.AgentAccountHealthOK, domain.AgentAccountStatusActive, tp(time.Hour), nil)
	blocked.Provider, blocked.SyncedAgentAccountID = domain.AgentAccountProviderCodex, "cloud"
	gate := (credsync.Readiness{State: credsync.ReadinessBootstrapUnpinned}).AgentAccountBlockReason
	eng := newEngineForTest(newFakeStore(blocked), WithCredentialSyncGate(gate))
	out, err := eng.Decide(context.Background(), Signal{Provider: "codex", RotationCapable: true})
	if err != nil || out.Kind != OutcomeNoEligibleAgentAccount || !out.ResumeAt.IsZero() {
		t.Fatalf("blocked cooldown was treated as recovery: %+v, %v", out, err)
	}
}

func TestCredentialSyncSelectionDoesNotWriteAccount_Accounts_AC224701(t *testing.T) {
	ctx := context.Background()
	store := db.NewAgentAccountStore(dbtest.New(t))
	a, err := store.Create(ctx, db.CreateAgentAccountParams{Provider: domain.AgentAccountProviderCodex, Label: "synced"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetSync(ctx, a.ID, "cloud", 1); err != nil {
		t.Fatal(err)
	}
	before, err := store.Get(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeJSON, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	gate := (credsync.Readiness{State: credsync.ReadinessBootstrapUnpinned}).AgentAccountBlockReason
	out, err := NewEngine(store, WithCredentialSyncGate(gate)).Decide(ctx, Signal{Provider: "codex", RotationCapable: true})
	if err != nil || out.Kind != OutcomeNoEligibleAgentAccount {
		t.Fatalf("blocked selection = %+v, %v", out, err)
	}
	after, err := store.Get(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterJSON, err := json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	if string(beforeJSON) != string(afterJSON) {
		t.Fatal("blocked selection mutated persisted account")
	}
}
