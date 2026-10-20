package account

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// A suspended account is never the default, not even as the least-bad fallback
// that otherwise accepts an account in failed health.
func TestDefaultAccountIDSkipsSuspended(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	reg := &stubRegistry{accounts: []Meta{
		{ID: "suspended", Provider: "claude", Status: "active", Health: "failed", Priority: 0, Suspended: true},
	}}
	r := NewResolver(reg, nil, zerolog.Nop())
	got, err := r.DefaultAccountID(context.Background(), "claude", now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != SystemDefaultAccountID {
		t.Fatalf("selected %q, want no managed account; a suspended account must not be the fallback", got)
	}

	reg.accounts = append(reg.accounts, Meta{ID: "good", Provider: "claude", Status: "active", Health: "ok", Priority: 9})
	if got, _ := r.DefaultAccountID(context.Background(), "claude", now); got != "good" {
		t.Fatalf("selected %q, want good", got)
	}
}

// A bound account that is disabled or suspended is refused before any
// credential is materialized, in words the operator can read.
func TestResolveSpawnEnvRefusesOutOfServiceAccount(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta Meta
		want string
	}{
		{"disabled", Meta{ID: "acct-1", Provider: "claude", Status: "disabled", Health: "ok"}, "the account is disabled"},
		{"suspended", Meta{ID: "acct-1", Provider: "claude", Status: "active", Health: "failed", Suspended: true}, "the provider has suspended the account"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := &stubRegistry{accounts: []Meta{tc.meta}}
			mat := &stubMaterializer{supports: true, env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "tok"}}
			r := NewResolver(reg, mat, zerolog.Nop())

			env, err := r.ResolveSpawnEnv(context.Background(), "acct-1", "claude", time.Now())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ResolveSpawnEnv = (%v, %v), want a refusal containing %q", env, err, tc.want)
			}
			if mat.materializeCall != 0 {
				t.Fatalf("materialized %d times, want 0", mat.materializeCall)
			}
		})
	}
}
