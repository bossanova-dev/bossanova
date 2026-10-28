package accountwiring

import (
	"context"
	"testing"

	"github.com/recurser/bossd/internal/domain"
	"github.com/rs/zerolog"
)

func TestCredentialVerificationPublishesInventoryAfterWrite(t *testing.T) {
	store := newAuthStore(&domain.AgentAccount{ID: "local", Provider: domain.AgentAccountProviderCodex})
	changes := 0
	m, err := NewCredentialMaintainer(&fakeVerifier{}, store, nil, zerolog.Nop(), WithAuthCheckChanged(func() {
		store.mu.Lock()
		defer store.mu.Unlock()
		if len(store.writes) != 1 {
			t.Errorf("notification before auth outcome write: %+v", store.writes)
		}
		changes++
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	if err := m.Smoke(context.Background(), "local", "codex", nil); err != nil {
		t.Fatal(err)
	}
	if changes != 1 {
		t.Fatalf("changes=%d", changes)
	}
}
