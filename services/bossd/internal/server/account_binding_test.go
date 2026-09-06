package server

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/account"
	"github.com/recurser/bossd/internal/accountwiring"
	"github.com/recurser/bossd/internal/db"
)

// mustAddCodex adds a codex-provider account for provider-scoping tests.
func mustAddCodex(t *testing.T, srv *Server, label string, cred []byte) *pb.Account {
	t.Helper()
	resp, err := srv.AddAccount(context.Background(), connect.NewRequest(&pb.AddAccountRequest{
		Provider:   "codex",
		Label:      label,
		Priority:   3,
		Credential: cred,
	}))
	if err != nil {
		t.Fatalf("AddAccount codex: %v", err)
	}
	return resp.Msg.Account
}

func strptr(s string) *string { return &s }

type accountBindingStore struct {
	byProvider map[models.AccountProvider][]*models.Account
}

func (s accountBindingStore) Create(context.Context, db.CreateAccountParams) (*models.Account, error) {
	return nil, errors.New("not implemented")
}
func (s accountBindingStore) RecordInjectionFailure(context.Context, string, string) error {
	return errors.New("not implemented")
}
func (s accountBindingStore) ClearInjectionFailure(context.Context, string) error {
	return errors.New("not implemented")
}
func (s accountBindingStore) Get(context.Context, string) (*models.Account, error) {
	return nil, sql.ErrNoRows
}
func (s accountBindingStore) List(context.Context) ([]*models.Account, error) {
	return nil, errors.New("not implemented")
}
func (s accountBindingStore) ListByProvider(_ context.Context, p models.AccountProvider) ([]*models.Account, error) {
	return s.byProvider[p], nil
}
func (s accountBindingStore) Update(context.Context, string, db.UpdateAccountParams) (*models.Account, error) {
	return nil, errors.New("not implemented")
}
func (s accountBindingStore) Delete(context.Context, string) error {
	return errors.New("not implemented")
}
func (s accountBindingStore) RecordTestResult(context.Context, string, *time.Time, string) error {
	return errors.New("not implemented")
}
func (s accountBindingStore) RecordUsageProbe(context.Context, string, models.UsageSnapshot) error {
	return errors.New("not implemented")
}

// TestResolveSessionAccount_ExplicitID covers the explicit account_id path:
// unknown id and provider mismatch are InvalidArgument; a matching-provider id
// is returned verbatim.
func TestResolveSessionAccount_ExplicitID(t *testing.T) {
	s, _ := newAccountServer(t, newFakeCredStore(), nil)
	acct := mustAddClaude(t, s, "work", []byte("blob"))

	// Unknown id.
	if _, err := s.resolveSessionAccount(context.Background(), strptr("does-not-exist"), "claude"); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("unknown id: err = %v, want InvalidArgument", err)
	}

	// Provider mismatch: a claude account requested for a codex session.
	if _, err := s.resolveSessionAccount(context.Background(), strptr(acct.Id), "codex"); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("provider mismatch: err = %v, want InvalidArgument", err)
	}

	// Valid, matching provider.
	id, err := s.resolveSessionAccount(context.Background(), strptr(acct.Id), "claude")
	if err != nil {
		t.Fatalf("valid id: unexpected err %v", err)
	}
	if id != acct.Id {
		t.Errorf("valid id = %q, want %q", id, acct.Id)
	}
}

// TestResolveSessionAccount_Label covers the Fix #3 label fallback: an
// account_id value that is not a known id is resolved as a provider-scoped
// label, returning the account's real id. Wrong-provider and unknown labels are
// InvalidArgument, and a real id still resolves.
func TestResolveSessionAccount_Label(t *testing.T) {
	s, _ := newAccountServer(t, newFakeCredStore(), nil)
	claude := mustAddClaude(t, s, "work", []byte("claude-work-blob"))
	// A codex account sharing no label with the claude one.
	mustAddCodex(t, s, "codex-only", []byte("codex-only-blob"))
	codexShared := mustAddCodex(t, s, "shared", []byte("codex-shared-blob"))
	claudeShared := mustAddClaude(t, s, "shared", []byte("claude-shared-blob"))

	// A valid claude label resolves to the claude account's real id.
	id, err := s.resolveSessionAccount(context.Background(), strptr("work"), "claude")
	if err != nil {
		t.Fatalf("valid label: unexpected err %v", err)
	}
	if id != claude.Id {
		t.Errorf("label %q resolved to %q, want %q (the real id, not the label)", "work", id, claude.Id)
	}

	// A label shared across providers resolves within the session provider.
	id, err = s.resolveSessionAccount(context.Background(), strptr("shared"), "claude")
	if err != nil {
		t.Fatalf("shared cross-provider label: unexpected err %v", err)
	}
	if id != claudeShared.Id || id == codexShared.Id {
		t.Errorf("shared label resolved to %q, want claude account %q and never codex account %q", id, claudeShared.Id, codexShared.Id)
	}

	// The codex label is not visible to a claude session (provider-scoped).
	if _, err := s.resolveSessionAccount(context.Background(), strptr("codex-only"), "claude"); connect.CodeOf(err) != connect.CodeInvalidArgument ||
		!strings.Contains(err.Error(), `provider "codex"`) ||
		!strings.Contains(err.Error(), `session runs "claude"`) {
		t.Errorf("wrong-provider label: err = %v, want InvalidArgument", err)
	}

	// An unknown label is not found.
	if _, err := s.resolveSessionAccount(context.Background(), strptr("nope"), "claude"); connect.CodeOf(err) != connect.CodeInvalidArgument ||
		!strings.Contains(err.Error(), `account "nope" not found`) {
		t.Errorf("unknown label: err = %v, want InvalidArgument", err)
	}

	// A real id still resolves via the id path (no label lookup needed).
	if got, err := s.resolveSessionAccount(context.Background(), strptr(claude.Id), "claude"); err != nil || got != claude.Id {
		t.Errorf("id path: got (%q,%v), want (%q,nil)", got, err, claude.Id)
	}
}

func TestResolveSessionAccount_DuplicateProviderLabelIsAmbiguous(t *testing.T) {
	s := &Server{
		accounts: accountBindingStore{byProvider: map[models.AccountProvider][]*models.Account{
			models.AccountProviderClaude: {
				{ID: "acct-1", Provider: models.AccountProviderClaude, Label: "work", Status: models.AccountStatusActive, Health: models.AccountHealthOK},
				{ID: "acct-2", Provider: models.AccountProviderClaude, Label: "work", Status: models.AccountStatusActive, Health: models.AccountHealthOK},
			},
		}},
		logger: zerolog.Nop(),
	}

	_, err := s.resolveSessionAccount(context.Background(), strptr("work"), "claude")
	if connect.CodeOf(err) != connect.CodeInvalidArgument ||
		!strings.Contains(err.Error(), `multiple claude accounts are labeled "work"`) ||
		!strings.Contains(err.Error(), "specify the account id") {
		t.Fatalf("duplicate label err = %v, want ambiguity InvalidArgument", err)
	}
}

// TestResolveSessionAccount_RejectsIneligible covers Fix #3: an explicitly
// requested account that the rotation engine would skip (disabled, failed
// health, or cooling down) is an InvalidArgument on BOTH the id-match and
// label-match branches, while an active/ok/non-cooling account still resolves.
func TestResolveSessionAccount_RejectsIneligible(t *testing.T) {
	ctx := context.Background()
	disabled := models.AccountStatusDisabled
	failed := models.AccountHealthFailed
	future := time.Now().Add(time.Hour)
	coolPtr := &future

	cases := []struct {
		name   string
		params db.UpdateAccountParams
	}{
		{"disabled", db.UpdateAccountParams{Status: &disabled}},
		{"failed-health", db.UpdateAccountParams{Health: &failed}},
		{"cooling", db.UpdateAccountParams{CooldownUntil: &coolPtr}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, accts := newAccountServer(t, newFakeCredStore(), nil)
			acct := mustAddClaude(t, s, "work", []byte("blob"))
			if _, err := accts.Update(ctx, acct.Id, tc.params); err != nil {
				t.Fatalf("Update: %v", err)
			}
			// id-match branch.
			if _, err := s.resolveSessionAccount(ctx, strptr(acct.Id), "claude"); connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Errorf("id path: err = %v, want InvalidArgument", err)
			}
			// label-match branch.
			if _, err := s.resolveSessionAccount(ctx, strptr("work"), "claude"); connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Errorf("label path: err = %v, want InvalidArgument", err)
			}
		})
	}

	// An active/ok/non-cooling account still resolves via both paths.
	s, _ := newAccountServer(t, newFakeCredStore(), nil)
	acct := mustAddClaude(t, s, "work", []byte("blob"))
	if id, err := s.resolveSessionAccount(ctx, strptr(acct.Id), "claude"); err != nil || id != acct.Id {
		t.Errorf("eligible id path: got (%q,%v), want (%q,nil)", id, err, acct.Id)
	}
	if id, err := s.resolveSessionAccount(ctx, strptr("work"), "claude"); err != nil || id != acct.Id {
		t.Errorf("eligible label path: got (%q,%v), want (%q,nil)", id, err, acct.Id)
	}
}

// TestResolveSessionAccount_RejectsAuthInvalid covers the case the
// status/health/cooldown predicate cannot see. Recording an auth-invalid
// verdict deliberately leaves Status active and Health ok, so an explicit
// CreateSession was accepted and the binding returned; the refusal then
// happened invisibly at spawn time, where a nil credential overlay means the
// session runs on the agent CLI's ambient login instead.
func TestResolveSessionAccount_RejectsAuthInvalid(t *testing.T) {
	ctx := context.Background()
	s, accts := newAccountServer(t, newFakeCredStore(), nil)
	acct := mustAddClaude(t, s, "work", []byte("blob"))

	authStore, ok := accts.(*db.SQLiteAccountStore)
	if !ok {
		t.Fatalf("harness store is %T, want *db.SQLiteAccountStore", accts)
	}
	checkedAt := time.Now().Add(-time.Minute).UTC()
	if err := authStore.RecordAuthCheck(ctx, acct.Id, models.AuthCheck{
		CheckedAt:    &checkedAt,
		Outcome:      models.AuthCheckOutcomeAuthInvalid,
		FailureClass: "auth_invalidated",
	}); err != nil {
		t.Fatalf("seed auth-invalid: %v", err)
	}
	// Precondition: the account still looks fine to a status/health predicate.
	got, err := accts.Get(ctx, acct.Id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != models.AccountStatusActive || got.Health != models.AccountHealthOK {
		t.Fatalf("precondition: want active/ok, got %q/%q", got.Status, got.Health)
	}

	if _, err := s.resolveSessionAccount(ctx, strptr(acct.Id), "claude"); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("id path: err = %v, want InvalidArgument", err)
	} else if !strings.Contains(err.Error(), "re-authenticate") {
		t.Errorf("id path message = %q, want it to name the operator action", err.Error())
	}
	if _, err := s.resolveSessionAccount(ctx, strptr("work"), "claude"); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("label path: err = %v, want InvalidArgument", err)
	}
}

// TestResolveSessionAccount_ExplicitSystemDefault covers the P1 case: a
// present-but-empty account_id is an explicit "account 0" opt-out and must NOT
// invoke the default-account policy, even when the resolver would otherwise pick
// a real account.
func TestResolveSessionAccount_ExplicitSystemDefault(t *testing.T) {
	s, accts := newAccountServer(t, newFakeCredStore(), nil)
	mustAddClaude(t, s, "work", []byte("blob"))
	// A resolver that WOULD select the account above if the policy ran.
	s.resolver = account.NewResolver(accountwiring.NewRegistry(accts), nil, zerolog.Nop())

	id, err := s.resolveSessionAccount(context.Background(), strptr(""), "claude")
	if err != nil {
		t.Fatalf("explicit system default: unexpected err %v", err)
	}
	if id != "" {
		t.Errorf("explicit empty account_id = %q, want \"\" (account 0, policy skipped)", id)
	}
}

// TestResolveSessionAccount_DefaultPolicy covers the absent-account_id (nil)
// path: the resolver's default-account policy selects an eligible account of the
// agent's provider. A nil resolver leaves the session on account 0.
func TestResolveSessionAccount_DefaultPolicy(t *testing.T) {
	s, accts := newAccountServer(t, newFakeCredStore(), nil)
	acct := mustAddClaude(t, s, "work", []byte("blob"))
	s.resolver = account.NewResolver(accountwiring.NewRegistry(accts), nil, zerolog.Nop())

	id, err := s.resolveSessionAccount(context.Background(), nil, "claude")
	if err != nil {
		t.Fatalf("default policy: unexpected err %v", err)
	}
	if id != acct.Id {
		t.Errorf("default policy id = %q, want %q", id, acct.Id)
	}

	// A different provider with no accounts falls back to account 0.
	if id, err := s.resolveSessionAccount(context.Background(), nil, "codex"); err != nil || id != "" {
		t.Errorf("no codex account: got (%q,%v), want (\"\",nil)", id, err)
	}

	// Nil resolver + absent id ⇒ account 0, never an error.
	s.resolver = nil
	if id, err := s.resolveSessionAccount(context.Background(), nil, "claude"); err != nil || id != "" {
		t.Errorf("nil resolver: got (%q,%v), want (\"\",nil)", id, err)
	}
}

// TestResolveSessionAccount_DefaultPolicyBindsRegardlessOfRotationFlag proves
// creation-time binding is decoupled from the rotation kill-switch (BOS-305):
// the absent-account_id path binds the resolved managed account rather than
// collapsing to account 0. resolveSessionAccount no longer reads any rotation
// config, so binding is unconditional on the rotation flag.
func TestResolveSessionAccount_DefaultPolicyBindsRegardlessOfRotationFlag(t *testing.T) {
	s, accts := newAccountServer(t, newFakeCredStore(), nil)
	acct := mustAddClaude(t, s, "work", []byte("blob"))
	s.resolver = account.NewResolver(accountwiring.NewRegistry(accts), nil, zerolog.Nop())

	id, err := s.resolveSessionAccount(context.Background(), nil, "claude")
	if err != nil {
		t.Fatalf("rotation disabled default policy: unexpected err %v", err)
	}
	if id != acct.Id {
		t.Errorf("rotation disabled default policy id = %q, want %q bound even with rotation off", id, acct.Id)
	}

	got, err := s.resolveSessionAccount(context.Background(), strptr(acct.Id), "claude")
	if err != nil {
		t.Fatalf("explicit id with rotation disabled: %v", err)
	}
	if got != acct.Id {
		t.Errorf("explicit id with rotation disabled = %q, want %q", got, acct.Id)
	}
}

// policyEligibilityStore lets a test make the authoritative store disagree with
// the registry projection the default-account policy ranked from. That
// divergence is the whole point of the pre-worktree re-check: without it the two
// reads always agree and the seam is untestable. Unimplemented methods panic on
// the nil embedded interface, so a test that reaches past Get fails loudly
// rather than silently exercising a different path.
type policyEligibilityStore struct {
	db.AccountStore
	byID map[string]*models.Account
	err  error
}

func (s policyEligibilityStore) Get(_ context.Context, id string) (*models.Account, error) {
	if s.err != nil {
		return nil, s.err
	}
	if a, ok := s.byID[id]; ok {
		return a, nil
	}
	return nil, sql.ErrNoRows
}

// TestResolveSessionAccount_DefaultPolicyRefusesIneligibleAccount pins the
// BOS-1142 seam: the policy path is eligibility-checked, not just the explicit
// path. Before this, an account the policy picked but the store knows is
// auth-invalid was bound, given a worktree and a branch, and only refused at
// spawn time — paying for a worktree to learn something the store already knew.
func TestResolveSessionAccount_DefaultPolicyRefusesIneligibleAccount(t *testing.T) {
	s, accts := newAccountServer(t, newFakeCredStore(), nil)
	acct := mustAddClaude(t, s, "work", []byte("blob"))
	s.resolver = account.NewResolver(accountwiring.NewRegistry(accts), nil, zerolog.Nop())

	checked := time.Now().Add(-time.Minute)
	s.accounts = policyEligibilityStore{byID: map[string]*models.Account{
		acct.Id: {
			ID:       acct.Id,
			Provider: models.AccountProviderClaude,
			Status:   models.AccountStatusActive,
			Health:   models.AccountHealthOK,
			AuthCheck: models.AuthCheck{
				CheckedAt: &checked,
				Outcome:   models.AuthCheckOutcomeAuthInvalid,
			},
		},
	}}

	id, err := s.resolveSessionAccount(context.Background(), nil, "claude")
	if err == nil {
		t.Fatalf("default policy on an auth-invalid account: want a refusal, got id %q", id)
	}
	if got := connect.CodeOf(err); got != connect.CodeFailedPrecondition {
		// InvalidArgument would blame the caller for a request that named no
		// account at all.
		t.Errorf("code = %v, want %v", got, connect.CodeFailedPrecondition)
	}
	// The refusal must name the skip class, not collapse to an unexplained
	// no-op (run-eligibility-before-ownership-side-effects.md).
	if !strings.Contains(err.Error(), "re-authenticate") {
		t.Errorf("refusal %q does not name the skip class", err)
	}
	if id != "" {
		t.Errorf("refused binding returned id %q, want empty", id)
	}
}

// TestResolveSessionAccount_DefaultPolicySkipsToNextBest pins the other half of
// that doc's rule: an explicit id has no alternative and stops, but the policy
// path has runner-up candidates and continues past the ineligible one.
func TestResolveSessionAccount_DefaultPolicySkipsToNextBest(t *testing.T) {
	s, accts := newAccountServer(t, newFakeCredStore(), nil)
	first := mustAddClaude(t, s, "one", []byte("blob-1"))
	second := mustAddClaude(t, s, "two", []byte("blob-2"))
	s.resolver = account.NewResolver(accountwiring.NewRegistry(accts), nil, zerolog.Nop())

	// Ask the policy which account it ranks first rather than assuming the
	// ordering, then make exactly that one ineligible in the store.
	picked, err := s.resolver.DefaultAccountID(context.Background(), "claude", time.Now())
	if err != nil {
		t.Fatalf("DefaultAccountID: %v", err)
	}
	other := first.Id
	if picked == first.Id {
		other = second.Id
	}

	checked := time.Now().Add(-time.Minute)
	s.accounts = policyEligibilityStore{byID: map[string]*models.Account{
		picked: {
			ID:       picked,
			Provider: models.AccountProviderClaude,
			Status:   models.AccountStatusActive,
			Health:   models.AccountHealthOK,
			AuthCheck: models.AuthCheck{
				CheckedAt: &checked,
				Outcome:   models.AuthCheckOutcomeAuthInvalid,
			},
		},
		other: {
			ID:       other,
			Provider: models.AccountProviderClaude,
			Status:   models.AccountStatusActive,
			Health:   models.AccountHealthOK,
		},
	}}

	id, err := s.resolveSessionAccount(context.Background(), nil, "claude")
	if err != nil {
		t.Fatalf("default policy with a runner-up: unexpected err %v", err)
	}
	if id != other {
		t.Errorf("id = %q, want the runner-up %q", id, other)
	}
}

// TestResolveSessionAccount_DefaultPolicyStoreReadFailureKeepsBinding pins the
// deliberate asymmetry: an unreadable row is not evidence of an unusable
// credential. Refusing on it would let one store hiccup block every session on
// the daemon, and the spawn path still fails closed if the credentials really
// cannot be injected.
func TestResolveSessionAccount_DefaultPolicyStoreReadFailureKeepsBinding(t *testing.T) {
	s, accts := newAccountServer(t, newFakeCredStore(), nil)
	acct := mustAddClaude(t, s, "work", []byte("blob"))
	s.resolver = account.NewResolver(accountwiring.NewRegistry(accts), nil, zerolog.Nop())
	s.accounts = policyEligibilityStore{err: errors.New("store unavailable")}

	id, err := s.resolveSessionAccount(context.Background(), nil, "claude")
	if err != nil {
		t.Fatalf("store read failure: unexpected err %v", err)
	}
	if id != acct.Id {
		t.Errorf("id = %q, want the policy's choice %q kept", id, acct.Id)
	}
}

// seedInjectionFailure fails id's health the way a spawn-time materialization
// outage does — through RecordInjectionFailure, so last_test_error carries
// db.InjectionFailureReasonPrefix and ClearInjectionFailure can withdraw it.
// Going through the real store (rather than an Update with a hand-written
// reason) is what makes these tests pin the actual self-clearing contract.
func seedInjectionFailure(t *testing.T, accts db.AccountStore, id string) {
	t.Helper()
	if err := accts.RecordInjectionFailure(context.Background(), id, "keyring unavailable"); err != nil {
		t.Fatalf("RecordInjectionFailure: %v", err)
	}
	got, err := accts.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get after RecordInjectionFailure: %v", err)
	}
	if got.Health != models.AccountHealthFailed {
		t.Fatalf("seed health = %q, want %q", got.Health, models.AccountHealthFailed)
	}
	if !strings.HasPrefix(got.LastTestError, db.InjectionFailureReasonPrefix) {
		t.Fatalf("seed last_test_error = %q, want the self-clearing prefix", got.LastTestError)
	}
}

// TestResolveSessionAccount_SelfClearingInjectionFailureStaysBindable pins the
// deadlock this exemption exists to break. RecordInjectionFailure marks a
// transient materialization outage as failed health, and ONLY a later
// successful materialize (ClearInjectionFailure) withdraws it — so rejecting the
// row at bind time makes the withdrawal unreachable and one plugin/keyring blip
// wedges every subsequent create for a provider with no other account.
//
// Both entry points are covered: the default-account policy re-check (the
// resolver's tier-2 fallback hands failed-health accounts back by design, see
// account.selectDefault) and the explicit id/label path, which wedges the same
// way and has no runner-up at all.
func TestResolveSessionAccount_SelfClearingInjectionFailureStaysBindable(t *testing.T) {
	ctx := context.Background()

	t.Run("default-policy path", func(t *testing.T) {
		s, accts := newAccountServer(t, newFakeCredStore(), nil)
		acct := mustAddClaude(t, s, "work", []byte("blob"))
		s.resolver = account.NewResolver(accountwiring.NewRegistry(accts), nil, zerolog.Nop())
		seedInjectionFailure(t, accts, acct.Id)

		id, err := s.resolveSessionAccount(ctx, nil, "claude")
		if err != nil {
			t.Fatalf("policy path: unexpected refusal %v; the self-heal is now unreachable", err)
		}
		if id != acct.Id {
			t.Errorf("policy path id = %q, want %q", id, acct.Id)
		}
	})

	t.Run("explicit id and label paths", func(t *testing.T) {
		s, accts := newAccountServer(t, newFakeCredStore(), nil)
		acct := mustAddClaude(t, s, "work", []byte("blob"))
		seedInjectionFailure(t, accts, acct.Id)

		if id, err := s.resolveSessionAccount(ctx, strptr(acct.Id), "claude"); err != nil || id != acct.Id {
			t.Errorf("id path: got (%q,%v), want (%q,nil)", id, err, acct.Id)
		}
		if id, err := s.resolveSessionAccount(ctx, strptr("work"), "claude"); err != nil || id != acct.Id {
			t.Errorf("label path: got (%q,%v), want (%q,nil)", id, err, acct.Id)
		}
	})
}

// TestResolveSessionAccount_RejectsFailedHealthThatIsNotSelfClearing is the
// counterweight: it proves the exemption did not simply widen the gate to all
// failed health. Only the exact pair ClearInjectionFailure heals (failed +
// prefixed reason) is exempt; an operator's `boss account test` failure, a
// suspension, and a bare failed flag with no reason at all are all still
// refused, because nothing on the spawn path will ever clear them.
func TestResolveSessionAccount_RejectsFailedHealthThatIsNotSelfClearing(t *testing.T) {
	ctx := context.Background()
	failed := models.AccountHealthFailed

	cases := []struct {
		name string
		seed func(t *testing.T, accts db.AccountStore, id string)
	}{
		{"operator test failure", func(t *testing.T, accts db.AccountStore, id string) {
			t.Helper()
			if err := accts.RecordTestResult(ctx, id, nil, "401 unauthorized"); err != nil {
				t.Fatalf("RecordTestResult: %v", err)
			}
			if _, err := accts.Update(ctx, id, db.UpdateAccountParams{Health: &failed}); err != nil {
				t.Fatalf("Update health: %v", err)
			}
		}},
		{"suspension", func(t *testing.T, accts db.AccountStore, id string) {
			t.Helper()
			store, ok := accts.(*db.SQLiteAccountStore)
			if !ok {
				t.Fatalf("harness store is %T, want *db.SQLiteAccountStore", accts)
			}
			if err := store.MarkAccountSuspended(ctx, id, "billing suspended"); err != nil {
				t.Fatalf("MarkAccountSuspended: %v", err)
			}
		}},
		{"failed with no reason", func(t *testing.T, accts db.AccountStore, id string) {
			t.Helper()
			if _, err := accts.Update(ctx, id, db.UpdateAccountParams{Health: &failed}); err != nil {
				t.Fatalf("Update health: %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, accts := newAccountServer(t, newFakeCredStore(), nil)
			acct := mustAddClaude(t, s, "work", []byte("blob"))
			s.resolver = account.NewResolver(accountwiring.NewRegistry(accts), nil, zerolog.Nop())
			tc.seed(t, accts, acct.Id)

			got, err := accts.Get(ctx, acct.Id)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if got.Health != models.AccountHealthFailed {
				t.Fatalf("precondition health = %q, want %q", got.Health, models.AccountHealthFailed)
			}
			if strings.HasPrefix(got.LastTestError, db.InjectionFailureReasonPrefix) {
				t.Fatalf("precondition last_test_error %q carries the self-clearing prefix", got.LastTestError)
			}

			if _, err := s.resolveSessionAccount(ctx, strptr(acct.Id), "claude"); connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Errorf("id path: err = %v, want InvalidArgument", err)
			}
			if _, err := s.resolveSessionAccount(ctx, strptr("work"), "claude"); connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Errorf("label path: err = %v, want InvalidArgument", err)
			}
			if _, err := s.resolveSessionAccount(ctx, nil, "claude"); connect.CodeOf(err) != connect.CodeFailedPrecondition {
				t.Errorf("policy path: err = %v, want FailedPrecondition", err)
			}
		})
	}
}

// TestResolveSessionAccount_InjectionFailureExemptsOnlyHealth proves the
// exemption is scoped to the health clause alone. A row that also carries the
// injection-failure marker is still refused when it is disabled, cooling, or
// auth-invalid — the marker says a local materialization failed, and it is not a
// licence to bind an account sidelined for any other reason. Auth-invalid
// matters most: that is a confirmed provider rejection of the credential, which
// no number of materialize retries fixes and only re-authentication clears.
func TestResolveSessionAccount_InjectionFailureExemptsOnlyHealth(t *testing.T) {
	ctx := context.Background()
	disabled := models.AccountStatusDisabled
	future := time.Now().Add(time.Hour)
	coolPtr := &future

	cases := []struct {
		name string
		seed func(t *testing.T, accts db.AccountStore, id string)
	}{
		{"disabled", func(t *testing.T, accts db.AccountStore, id string) {
			t.Helper()
			if _, err := accts.Update(ctx, id, db.UpdateAccountParams{Status: &disabled}); err != nil {
				t.Fatalf("Update status: %v", err)
			}
		}},
		{"cooling", func(t *testing.T, accts db.AccountStore, id string) {
			t.Helper()
			if _, err := accts.Update(ctx, id, db.UpdateAccountParams{CooldownUntil: &coolPtr}); err != nil {
				t.Fatalf("Update cooldown: %v", err)
			}
		}},
		{"auth-invalid", func(t *testing.T, accts db.AccountStore, id string) {
			t.Helper()
			store, ok := accts.(*db.SQLiteAccountStore)
			if !ok {
				t.Fatalf("harness store is %T, want *db.SQLiteAccountStore", accts)
			}
			checkedAt := time.Now().Add(-time.Minute).UTC()
			if err := store.RecordAuthCheck(ctx, id, models.AuthCheck{
				CheckedAt:    &checkedAt,
				Outcome:      models.AuthCheckOutcomeAuthInvalid,
				FailureClass: "auth_invalidated",
			}); err != nil {
				t.Fatalf("RecordAuthCheck: %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, accts := newAccountServer(t, newFakeCredStore(), nil)
			acct := mustAddClaude(t, s, "work", []byte("blob"))
			seedInjectionFailure(t, accts, acct.Id)
			tc.seed(t, accts, acct.Id)

			if _, err := s.resolveSessionAccount(ctx, strptr(acct.Id), "claude"); connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Errorf("id path: err = %v, want InvalidArgument", err)
			}
			if _, err := s.resolveSessionAccount(ctx, strptr("work"), "claude"); connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Errorf("label path: err = %v, want InvalidArgument", err)
			}
		})
	}
}

// TestResolveSessionAccount_InjectionFailureSelfHealsEndToEnd walks the whole
// loop the exemption exists to reopen: a previously injection-failed account is
// bound at create time, its credentials materialize on the next spawn, and
// ResolveSpawnEnv's ClearInjectionFailure puts the row back to healthy with an
// empty reason. Without the exemption the walk stops at step one and the row
// stays failed forever.
func TestResolveSessionAccount_InjectionFailureSelfHealsEndToEnd(t *testing.T) {
	ctx := context.Background()
	s, accts := newAccountServer(t, newFakeCredStore(), nil)
	acct := mustAddClaude(t, s, "work", []byte("blob"))
	mat := &fakeMaterializer{supports: true, env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "x"}}
	s.resolver = account.NewResolver(accountwiring.NewRegistry(accts), mat, zerolog.Nop())
	seedInjectionFailure(t, accts, acct.Id)

	id, err := s.resolveSessionAccount(ctx, nil, "claude")
	if err != nil {
		t.Fatalf("bind: unexpected refusal %v", err)
	}
	if id != acct.Id {
		t.Fatalf("bound id = %q, want %q", id, acct.Id)
	}

	sess := &models.Session{ID: "sess-1", AgentName: "claude", AccountID: &id}
	if _, err := s.resolveAccountEnv(ctx, sess); err != nil {
		t.Fatalf("resolveAccountEnv: %v", err)
	}

	healed, err := accts.Get(ctx, acct.Id)
	if err != nil {
		t.Fatalf("get after spawn: %v", err)
	}
	if healed.Health != models.AccountHealthOK {
		t.Errorf("health = %q after a successful materialize, want %q", healed.Health, models.AccountHealthOK)
	}
	if healed.LastTestError != "" {
		t.Errorf("last_test_error = %q after the self-heal, want empty", healed.LastTestError)
	}
}

// TestResolveSessionAccount_AdmitsRefreshChainUnproven is the BOS-1174
// acceptance criterion at the gate that actually benches accounts.
//
// refresh_chain_unproven reports that a credential's refresh chain has not been
// observed working — it does not report that the provider rejected it. The
// account demonstrably still authenticates, so the pre-worktree bind gate must
// keep admitting it. Collapsing a warning onto auth_invalid here would bench a
// working account, which this package states is the more expensive error.
func TestResolveSessionAccount_AdmitsRefreshChainUnproven(t *testing.T) {
	ctx := context.Background()
	s, accts := newAccountServer(t, newFakeCredStore(), nil)
	acct := mustAddClaude(t, s, "work", []byte("blob"))

	authStore, ok := accts.(*db.SQLiteAccountStore)
	if !ok {
		t.Fatalf("harness store is %T, want *db.SQLiteAccountStore", accts)
	}
	checkedAt := time.Now().Add(-time.Minute).UTC()
	if err := authStore.RecordAuthCheck(ctx, acct.Id, models.AuthCheck{
		CheckedAt:    &checkedAt,
		Outcome:      models.AuthCheckOutcomeRefreshChainUnproven,
		FailureClass: "refresh_not_observed",
	}); err != nil {
		t.Fatalf("seed refresh-chain-unproven: %v", err)
	}
	// Precondition: the verdict really did land on the row, so a pass below is
	// the gate admitting the state rather than the state never being recorded.
	got, err := accts.Get(ctx, acct.Id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.AuthCheck.Outcome != models.AuthCheckOutcomeRefreshChainUnproven {
		t.Fatalf("precondition: outcome = %q, want %q", got.AuthCheck.Outcome, models.AuthCheckOutcomeRefreshChainUnproven)
	}

	if _, err := s.resolveSessionAccount(ctx, strptr(acct.Id), "claude"); err != nil {
		t.Errorf("id path: err = %v, want the account to stay bindable", err)
	}
	if _, err := s.resolveSessionAccount(ctx, strptr("work"), "claude"); err != nil {
		t.Errorf("label path: err = %v, want the account to stay bindable", err)
	}
}
