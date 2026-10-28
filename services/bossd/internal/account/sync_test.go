package account

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/recurser/bossd/internal/domain"
	"github.com/rs/zerolog"
)

type syncAccountMemory struct {
	mu      sync.Mutex
	account domain.AgentAccount
	checks  int
}

func (s *syncAccountMemory) UpdateSync(_ context.Context, cmd domain.ApplyAgentAccountSync) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cmd.Version <= s.account.SyncVersion {
		return false, nil
	}
	s.account.SyncVersion = cmd.Version
	s.account.SyncedAgentAccountID = cmd.SyncedAgentAccountID
	s.account.Priority = cmd.Priority
	if cmd.DesiredStatus != "" {
		s.account.Status = domain.AgentAccountStatus(cmd.DesiredStatus)
	}
	return true, nil
}
func (s *syncAccountMemory) Get(context.Context, string) (*domain.AgentAccount, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.account
	return &a, nil
}
func (s *syncAccountMemory) Smoke(context.Context, string, string, []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checks++
	s.account.AuthCheck.Outcome = domain.AuthCheckOutcomeHealthy
	return nil
}

func TestSyncApplierVersionAndVerification(t *testing.T) {
	store := &syncAccountMemory{account: domain.AgentAccount{ID: "local", Provider: domain.AgentAccountProviderCodex, Status: domain.AgentAccountStatusActive}}
	changed := 0
	applier := NewSyncApplier(store, store, func() { changed++ }, zerolog.Nop())
	defer applier.Stop()
	cmd := domain.ApplyAgentAccountSync{LocalAgentAccountID: "local", SyncedAgentAccountID: "synced", Version: 2, DesiredStatus: "disabled", Priority: 9, IsQuarantined: true, ShouldVerifyAuth: true}
	if err := applier.Apply(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	if err := applier.Apply(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	cmd.Version = 1
	cmd.Priority = 1
	cmd.DesiredStatus = "active"
	if err := applier.Apply(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	// Stop joins the accepted command's verification before reading its result.
	applier.Stop()
	a, _ := store.Get(context.Background(), "local")
	if a.Status != domain.AgentAccountStatusDisabled || a.Priority != 9 || a.SyncVersion != 2 || a.SyncedAgentAccountID != "synced" {
		t.Fatalf("applied account = %+v", a)
	}
	if store.checks != 1 || changed != 1 {
		t.Fatalf("checks=%d changes=%d", store.checks, changed)
	}
	if a.IsAuthInvalid() {
		t.Fatal("remote quarantine benched a working local credential")
	}
}

func TestSyncApplierUnknownStatusPreservesLocalStatus(t *testing.T) {
	store := &syncAccountMemory{account: domain.AgentAccount{Status: domain.AgentAccountStatusActive}}
	applier := NewSyncApplier(store, nil, nil, zerolog.Nop())
	defer applier.Stop()
	if err := applier.Apply(context.Background(), domain.ApplyAgentAccountSync{Version: 1, DesiredStatus: "future-status", Priority: 4}); err != nil {
		t.Fatal(err)
	}
	a, _ := store.Get(context.Background(), "")
	if a.Status != domain.AgentAccountStatusActive || a.Priority != 4 || a.SyncVersion != 1 {
		t.Fatalf("account=%+v", a)
	}
}

type rejectingSyncVerifier struct{ store *syncAccountMemory }

func (v rejectingSyncVerifier) Smoke(context.Context, string, string, []byte) error {
	v.store.mu.Lock()
	defer v.store.mu.Unlock()
	v.store.checks++
	v.store.account.AuthCheck.Outcome = domain.AuthCheckOutcomeAuthInvalid
	return errors.New("local credential rejected")
}

func TestSyncApplierFailedVerificationOnlyBenchesLocalCopy(t *testing.T) {
	store := &syncAccountMemory{account: domain.AgentAccount{ID: "local", Status: domain.AgentAccountStatusActive}}
	applier := NewSyncApplier(store, rejectingSyncVerifier{store}, nil, zerolog.Nop())
	defer applier.Stop()
	if err := applier.Apply(context.Background(), domain.ApplyAgentAccountSync{LocalAgentAccountID: "local", Version: 1, DesiredStatus: "active", IsQuarantined: true, ShouldVerifyAuth: true}); err != nil {
		t.Fatal(err)
	}
	applier.Stop()
	a, _ := store.Get(context.Background(), "local")
	if !a.IsAuthInvalid() || a.Status != domain.AgentAccountStatusActive || store.checks != 1 {
		t.Fatalf("account=%+v checks=%d", a, store.checks)
	}
}
