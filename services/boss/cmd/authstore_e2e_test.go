//go:build e2e

package main

import (
	"testing"

	"github.com/recurser/boss/internal/auth"
)

// TestResolveE2ETokenStoreSeedsReloginMarker covers the BOS-659 fixture seed:
// BOSS_AUTH_E2E_NEEDS_RELOGIN must flag the seeded record, retain a non-empty
// email, and stay off entirely when the var is unset.
func TestResolveE2ETokenStoreSeedsReloginMarker(t *testing.T) {
	tests := []struct {
		name        string
		email       string
		needs       string
		wantSeeded  bool
		wantFlagged bool
		wantReason  string
		wantEmail   string
	}{
		{
			name:       "unset leaves an empty store",
			wantSeeded: false,
		},
		{
			name:       "email alone seeds a clean record",
			email:      "proof@example.com",
			wantSeeded: true,
			wantEmail:  "proof@example.com",
		},
		{
			name:        "unknown-outcome flags the seeded email",
			email:       "proof@example.com",
			needs:       auth.ReloginReasonRefreshOutcomeUnknown,
			wantSeeded:  true,
			wantFlagged: true,
			wantReason:  auth.ReloginReasonRefreshOutcomeUnknown,
			wantEmail:   "proof@example.com",
		},
		{
			name:        "rejected reason is selectable by name",
			email:       "proof@example.com",
			needs:       auth.ReloginReasonRefreshTokenRejected,
			wantSeeded:  true,
			wantFlagged: true,
			wantReason:  auth.ReloginReasonRefreshTokenRejected,
			wantEmail:   "proof@example.com",
		},
		{
			name:        "any other value means unknown outcome and keeps an email",
			needs:       "1",
			wantSeeded:  true,
			wantFlagged: true,
			wantReason:  auth.ReloginReasonRefreshOutcomeUnknown,
			wantEmail:   e2eReloginEmail,
		},
		{
			// An operator who exports the var as "0" plainly means off, and
			// a seed that read that as on would be a nasty surprise.
			name:       "an explicit falsey value means off",
			needs:      "0",
			wantSeeded: false,
		},
		{
			name:       "false with surrounding space and case still means off",
			needs:      "  FALSE ",
			wantSeeded: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("BOSS_AUTH_E2E_EMAIL", tt.email)
			t.Setenv("BOSS_AUTH_E2E_NEEDS_RELOGIN", tt.needs)

			tokens, err := resolveE2ETokenStore().Load()
			if !tt.wantSeeded {
				if err == nil {
					t.Fatalf("expected an empty store, got tokens %+v", tokens)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v, want seeded tokens", err)
			}
			if tokens.Email != tt.wantEmail {
				t.Fatalf("Email = %q, want %q", tokens.Email, tt.wantEmail)
			}
			if tokens.NeedsRelogin != tt.wantFlagged {
				t.Fatalf("NeedsRelogin = %v, want %v", tokens.NeedsRelogin, tt.wantFlagged)
			}
			if tokens.ReloginReason != tt.wantReason {
				t.Fatalf("ReloginReason = %q, want %q", tokens.ReloginReason, tt.wantReason)
			}
			// A flagged record is retained but not usable: Status must report
			// signed-out while keeping the identity, which is what drives the
			// Home warning the proof scenario captures.
			if tt.wantFlagged && tokens.Valid() {
				t.Fatal("a flagged record must not report Valid()")
			}
		})
	}
}
