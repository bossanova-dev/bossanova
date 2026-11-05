package accountwiring

import (
	"context"
	"testing"

	"github.com/recurser/bossd/internal/credsync"
	"github.com/recurser/bossd/internal/domain"
)

func TestRegistryCredentialSyncGateRecoversLive(t *testing.T) {
	ctx := context.Background()
	acct := newClaudeAccount()
	acct.Provider, acct.SyncedAgentAccountID = domain.AgentAccountProviderCodex, "cloud"
	store := &spyStore{accounts: map[string]*domain.AgentAccount{acct.ID: acct}}
	var holder credsync.ReadinessHolder
	registry := NewRegistry(store, WithCredentialSyncGate(func(a *domain.AgentAccount) string { return holder.Load().AgentAccountBlockReason(a) }))
	for _, state := range []credsync.ReadinessState{credsync.ReadinessBootstrapUnpinned, credsync.ReadinessReady, credsync.ReadinessUnknown} {
		holder.Store(credsync.Readiness{State: state})
		want := ""
		if state == credsync.ReadinessBootstrapUnpinned {
			want = string(state)
		}
		rows, err := registry.List(ctx)
		if err != nil || len(rows) != 1 || rows[0].CredentialSyncBlocked != want {
			t.Fatalf("List for %s = %+v, %v", state, rows, err)
		}
		got, found, err := registry.Get(ctx, acct.ID)
		if err != nil || !found || got.CredentialSyncBlocked != want {
			t.Fatalf("Get for %s = %+v, %v, %v", state, got, found, err)
		}
	}
	if store.updateCalls != 0 || store.injectCalls != 0 {
		t.Fatal("readiness gate wrote account state")
	}
}
