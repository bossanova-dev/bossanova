package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"testing"

	"github.com/recurser/bossd/internal/domain"
	"github.com/rs/zerolog"
)

type identityBackfillAccounts struct{ rows []*domain.AgentAccount }

func (s *identityBackfillAccounts) List(context.Context) ([]*domain.AgentAccount, error) {
	return s.rows, nil
}
func (s *identityBackfillAccounts) SetIdentity(_ context.Context, id string, identity domain.AgentAccountIdentity) error {
	for _, row := range s.rows {
		if row.ID == id {
			row.Identity = identity
			return nil
		}
	}
	return domain.ErrNotFound
}

type identityBackfillCredentials struct{ loads []string }

func (s *identityBackfillCredentials) Load(id string) ([]byte, error) {
	s.loads = append(s.loads, id)
	if id == "locked" {
		return nil, errors.New("locked")
	}
	if id == "codex" {
		token := "h." + base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"workspace","chatgpt_user_id":"user"}}`)) + ".s"
		return []byte(fmt.Sprintf(`{"id_token":%q}`, token)), nil
	}
	return []byte("claude-token"), nil
}
func TestBackfillAccountIdentities(t *testing.T) {
	none := domain.AgentAccountIdentity{Kind: domain.AgentAccountIdentityKindNone}
	store := &identityBackfillAccounts{rows: []*domain.AgentAccount{
		{ID: "new", Provider: domain.AgentAccountProviderClaude},
		{ID: "done", Provider: domain.AgentAccountProviderClaude, Identity: none},
		{ID: "locked", Provider: domain.AgentAccountProviderCodex},
	}}
	creds := &identityBackfillCredentials{}
	backfillAccountIdentities(context.Background(), store, creds, zerolog.Nop())
	if store.rows[0].Identity != none || store.rows[1].Identity != none || store.rows[2].Identity.Kind != "" {
		t.Fatalf("wrong backfill: %+v", store.rows)
	}
	if len(creds.loads) != 2 {
		t.Fatalf("loaded derived account: %v", creds.loads)
	}
	backfillAccountIdentities(context.Background(), store, creds, zerolog.Nop())
	if len(creds.loads) != 3 || creds.loads[2] != "locked" {
		t.Fatalf("did not retry load failure only: %v", creds.loads)
	}
}

func TestBackfillCodexIdentity(t *testing.T) {
	store := &identityBackfillAccounts{rows: []*domain.AgentAccount{{ID: "codex", Provider: domain.AgentAccountProviderCodex}}}
	backfillAccountIdentities(context.Background(), store, &identityBackfillCredentials{}, zerolog.Nop())
	got := store.rows[0].Identity
	if got.Kind != domain.AgentAccountIdentityKindCodexUser || len(got.Digest) != 64 {
		t.Fatalf("missing Codex identity: %+v", got)
	}
}
