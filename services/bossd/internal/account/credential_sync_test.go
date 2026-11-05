package account

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestCredentialSyncBlockedSkippedInBothTiers_Accounts_AC224701(t *testing.T) {
	for _, health := range []string{"ok", "failed"} {
		t.Run(health, func(t *testing.T) {
			blocked := Meta{ID: "blocked", Provider: "codex", Status: "active", Health: health, CredentialSyncBlocked: "bootstrap_unpinned"}
			reg := &stubRegistry{accounts: []Meta{blocked}}
			r := NewResolver(reg, nil, zerolog.Nop())
			if id, err := r.DefaultAgentAccountID(context.Background(), "codex", "", time.Now()); err != nil || id != SystemDefaultAgentAccountID {
				t.Fatalf("only blocked account: id=%q err=%v", id, err)
			}
			reg.accounts = append(reg.accounts, Meta{ID: "local", Provider: "codex", Status: "active", Health: health, Priority: 1})
			if id, err := r.DefaultAgentAccountID(context.Background(), "codex", "", time.Now()); err != nil || id != "local" {
				t.Fatalf("local alternative: id=%q err=%v", id, err)
			}
		})
	}
}
