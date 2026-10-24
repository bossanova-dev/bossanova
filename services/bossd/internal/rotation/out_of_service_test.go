package rotation

import (
	"context"
	"testing"
	"time"

	"github.com/recurser/bossd/internal/domain"
)

// The failover proxy replays a refused request on whatever Decide returns. A
// disabled account and a suspended one must never be that target, even when
// they outrank the only usable account on priority and recency.
func TestDecideReplayTargetSkipsOutOfServiceAccounts(t *testing.T) {
	ok := domain.AgentAccountHealthOK
	active := domain.AgentAccountStatusActive
	refused := mkAcct("refused", 0, ok, domain.AgentAccountStatusDisabled, nil, tp(-4*time.Hour))
	disabled := mkAcct("disabled", 1, ok, domain.AgentAccountStatusDisabled, nil, nil)
	suspended := mkAcct("suspended", 2, domain.AgentAccountHealthFailed, active, nil, nil)
	suspended.LastTestError = "account suspended: organization disabled Claude subscription access"
	usable := mkAcct("usable", 9, ok, active, nil, tp(-time.Minute))
	eng := newEngineForTest(newFakeStore(refused, disabled, suspended, usable))

	out, err := eng.Decide(context.Background(), Signal{
		Provider: claude, CappedAccountID: "refused", Kind: AuthInvalidated, RotationCapable: true,
	})
	if err != nil {
		t.Fatalf("Decide error: %v", err)
	}
	if out.Kind != OutcomeRotate || out.NextAccount == nil || out.NextAccount.ID != "usable" {
		t.Fatalf("Decide = %+v, want a rotation to usable", out)
	}
}
