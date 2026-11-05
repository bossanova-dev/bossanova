package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
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

// identityBackfillOrganizations resolves the fake Claude token to a fake org.
type identityBackfillOrganizations struct{ calls int }

const (
	backfillClaudeToken = "claude-token"
	backfillClaudeOrg   = "org-7f3a-secret"
)

func (o *identityBackfillOrganizations) OrganizationID(_ context.Context, token string) (string, error) {
	o.calls++
	if token != backfillClaudeToken {
		return "", errors.New("unknown token")
	}
	return backfillClaudeOrg, nil
}

func TestBackfillClaudeIdentities_Accounts_AC225501(t *testing.T) {
	none := domain.AgentAccountIdentity{Kind: domain.AgentAccountIdentityKindNone}
	store := &identityBackfillAccounts{rows: []*domain.AgentAccount{
		{ID: "claude-none", Provider: domain.AgentAccountProviderClaude, Label: "Work", Identity: none},
		{ID: "claude-new", Provider: domain.AgentAccountProviderClaude, Label: "work"},
		{ID: "codex", Provider: domain.AgentAccountProviderCodex, Label: "work", Identity: none},
		{ID: "locked", Provider: domain.AgentAccountProviderClaude, Label: "work", Identity: none},
	}}
	orgs := &identityBackfillOrganizations{}
	changes := 0
	var logs bytes.Buffer
	logger := zerolog.New(&logs).Level(zerolog.TraceLevel)
	got := backfillClaudeIdentities(context.Background(), store, &identityBackfillCredentials{}, orgs, func() { changes++ }, logger)
	if got != 2 || changes != 1 {
		t.Fatalf("changed = %d, onChange calls = %d; want 2, 1", got, changes)
	}
	first, second := store.rows[0].Identity, store.rows[1].Identity
	if first.Kind != domain.AgentAccountIdentityKindClaudeOrganizationLabel || !first.IsConfident() || first != second {
		t.Fatalf("Claude identities: %+v %+v", first, second)
	}
	if store.rows[2].Identity != none || store.rows[3].Identity != none {
		t.Fatalf("touched Codex or unloadable rows: %+v %+v", store.rows[2].Identity, store.rows[3].Identity)
	}
	if out := logs.String(); strings.Contains(out, backfillClaudeToken) || strings.Contains(out, backfillClaudeOrg) {
		t.Fatalf("backfill logged a secret: %s", out)
	}

	// A second pass finds nothing left to derive and does not republish.
	calls := orgs.calls
	if got := backfillClaudeIdentities(context.Background(), store, &identityBackfillCredentials{}, orgs, func() { changes++ }, logger); got != 0 || changes != 1 {
		t.Fatalf("idle pass changed = %d, onChange calls = %d", got, changes)
	}
	if orgs.calls != calls {
		t.Fatal("idle pass re-resolved confident rows")
	}
}

func TestBackfillClaudeIdentitiesFailedLookupStaysNone_Accounts_AC225502(t *testing.T) {
	none := domain.AgentAccountIdentity{Kind: domain.AgentAccountIdentityKindNone}
	store := &identityBackfillAccounts{rows: []*domain.AgentAccount{{ID: "claude", Provider: domain.AgentAccountProviderClaude, Label: "work", Identity: none}}}
	creds := credentialFunc(func(string) ([]byte, error) { return []byte("revoked-token"), nil })
	changes := 0
	if got := backfillClaudeIdentities(context.Background(), store, creds, &identityBackfillOrganizations{}, func() { changes++ }, zerolog.Nop()); got != 0 || changes != 0 {
		t.Fatalf("changed = %d, onChange calls = %d", got, changes)
	}
	if store.rows[0].Identity != none {
		t.Fatalf("failed lookup wrote %+v", store.rows[0].Identity)
	}
}

func TestBackfillClaudeIdentitiesStopsOnCancel(t *testing.T) {
	store := &identityBackfillAccounts{rows: []*domain.AgentAccount{{ID: "claude", Provider: domain.AgentAccountProviderClaude, Label: "work"}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	orgs := &identityBackfillOrganizations{}
	changes := 0
	if got := backfillClaudeIdentities(ctx, store, &identityBackfillCredentials{}, orgs, func() { changes++ }, zerolog.Nop()); got != 0 || changes != 0 || orgs.calls != 0 {
		t.Fatalf("cancelled pass: changed = %d, onChange = %d, lookups = %d", got, changes, orgs.calls)
	}
}

func TestBackfillAccountIdentitiesStaysOffline(t *testing.T) {
	store := &identityBackfillAccounts{rows: []*domain.AgentAccount{{ID: "claude", Provider: domain.AgentAccountProviderClaude, Label: "work"}}}
	backfillAccountIdentities(context.Background(), store, &identityBackfillCredentials{}, zerolog.Nop())
	if got := store.rows[0].Identity; got.Kind != domain.AgentAccountIdentityKindNone {
		t.Fatalf("startup pass derived a Claude identity without the network: %+v", got)
	}
}

type credentialFunc func(string) ([]byte, error)

func (f credentialFunc) Load(id string) ([]byte, error) { return f(id) }
