package db

import (
	"context"
	"sync"
	"testing"

	"github.com/recurser/bossd/internal/domain"
)

func TestAccountSyncConcurrentVersionsStayAtomic(t *testing.T) {
	store := NewAgentAccountStore(setupTestDB(t))
	ctx := context.Background()
	a, err := store.Create(ctx, CreateAgentAccountParams{Provider: domain.AgentAccountProviderCodex, Label: "sync"})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for v := int64(1); v <= 20; v++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.UpdateSync(ctx, domain.ApplyAgentAccountSync{LocalAgentAccountID: a.ID, SyncedAgentAccountID: "synced", Version: v, DesiredStatus: "disabled", Priority: int(v)})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got, err := store.Get(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SyncVersion != 20 || got.Priority != 20 || got.Status != domain.AgentAccountStatusDisabled || got.SyncedAgentAccountID != "synced" {
		t.Fatalf("account=%+v", got)
	}
	applied, err := store.UpdateSync(ctx, domain.ApplyAgentAccountSync{LocalAgentAccountID: a.ID, Version: 20, DesiredStatus: "active", Priority: 0})
	if err != nil || applied {
		t.Fatalf("duplicate applied=%v err=%v", applied, err)
	}
	got, err = store.Get(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Priority != 20 || got.Status != domain.AgentAccountStatusDisabled {
		t.Fatalf("duplicate overwrote state: %+v", got)
	}
}
