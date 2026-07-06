package server

import (
	"context"
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
		Email:      "dev@example.com",
		Priority:   3,
		Credential: cred,
	}))
	if err != nil {
		t.Fatalf("AddAccount codex: %v", err)
	}
	return resp.Msg.Account
}

func strptr(s string) *string { return &s }

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
	claude := mustAddClaude(t, s, "work", []byte("blob"))
	// A codex account sharing no label with the claude one.
	mustAddCodex(t, s, "codex-only", []byte("blob"))

	// A valid claude label resolves to the claude account's real id.
	id, err := s.resolveSessionAccount(context.Background(), strptr("work"), "claude")
	if err != nil {
		t.Fatalf("valid label: unexpected err %v", err)
	}
	if id != claude.Id {
		t.Errorf("label %q resolved to %q, want %q (the real id, not the label)", "work", id, claude.Id)
	}

	// The codex label is not visible to a claude session (provider-scoped).
	if _, err := s.resolveSessionAccount(context.Background(), strptr("codex-only"), "claude"); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("wrong-provider label: err = %v, want InvalidArgument", err)
	}

	// An unknown label is not found.
	if _, err := s.resolveSessionAccount(context.Background(), strptr("nope"), "claude"); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("unknown label: err = %v, want InvalidArgument", err)
	}

	// A real id still resolves via the id path (no label lookup needed).
	if got, err := s.resolveSessionAccount(context.Background(), strptr(claude.Id), "claude"); err != nil || got != claude.Id {
		t.Errorf("id path: got (%q,%v), want (%q,nil)", got, err, claude.Id)
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
