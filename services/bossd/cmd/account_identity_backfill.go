package main

import (
	"context"

	"github.com/recurser/bossd/internal/account"
	"github.com/recurser/bossd/internal/domain"
	"github.com/rs/zerolog"
)

type accountIdentityStore interface {
	List(context.Context) ([]*domain.AgentAccount, error)
	SetIdentity(context.Context, string, domain.AgentAccountIdentity) error
}
type accountIdentityCredentials interface{ Load(string) ([]byte, error) }

// backfillAccountIdentities runs on the startup path, so it never touches the
// network: it passes no organization resolver, which leaves Claude rows none
// until backfillClaudeIdentities re-derives them in the background.
func backfillAccountIdentities(ctx context.Context, accounts accountIdentityStore, creds accountIdentityCredentials, logger zerolog.Logger) {
	rows, err := accounts.List(ctx)
	if err != nil {
		logger.Warn().Err(err).Msg("could not list accounts for identity backfill")
		return
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return
		}
		if row.Identity.Kind != "" {
			continue
		}
		credential, err := creds.Load(row.ID)
		if err != nil {
			logger.Warn().Str("account_id", row.ID).Msg("could not load credential for identity backfill")
			continue
		}
		identity := account.DeriveIdentity(ctx, string(row.Provider), row.Label, credential, nil)
		if err := accounts.SetIdentity(ctx, row.ID, identity); err != nil {
			logger.Warn().Err(err).Str("account_id", row.ID).Msg("could not backfill account identity")
		}
	}
}

// backfillClaudeIdentities re-derives Claude rows that have no confident
// identity, using the Anthropic organization resolver. It writes confident
// results only, so a failed lookup leaves the row for the next daemon start.
// When any identity changed it calls onChange, which republishes the account
// inventory so bosso links the rows without waiting for an unrelated change.
// It returns the number of identities it wrote.
func backfillClaudeIdentities(ctx context.Context, accounts accountIdentityStore, creds accountIdentityCredentials, org account.ClaudeOrganizationResolver, onChange func(), logger zerolog.Logger) int {
	rows, err := accounts.List(ctx)
	if err != nil {
		logger.Warn().Err(err).Msg("could not list accounts for Claude identity backfill")
		return 0
	}
	changed := 0
	for _, row := range rows {
		if ctx.Err() != nil {
			return changed
		}
		if !needsClaudeIdentity(row) {
			continue
		}
		credential, err := creds.Load(row.ID)
		if err != nil {
			logger.Debug().Str("account_id", row.ID).Msg("could not load credential for Claude identity backfill")
			continue
		}
		identity := account.DeriveIdentity(ctx, string(row.Provider), row.Label, credential, org)
		if !identity.IsConfident() {
			logger.Debug().Str("account_id", row.ID).Msg("Claude identity backfill: organization unavailable")
			continue
		}
		if err := accounts.SetIdentity(ctx, row.ID, identity); err != nil {
			logger.Warn().Err(err).Str("account_id", row.ID).Msg("could not record Claude account identity")
			continue
		}
		changed++
	}
	if changed > 0 && ctx.Err() == nil {
		logger.Info().Int("count", changed).Msg("Claude identity backfill: recorded account identities")
		if onChange != nil {
			onChange()
		}
	}
	return changed
}

func needsClaudeIdentity(row *domain.AgentAccount) bool {
	if row.Provider != domain.AgentAccountProviderClaude {
		return false
	}
	return row.Identity.Kind == "" || row.Identity.Kind == domain.AgentAccountIdentityKindNone
}
