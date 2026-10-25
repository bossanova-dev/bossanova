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
		if err := accounts.SetIdentity(ctx, row.ID, account.DeriveIdentity(string(row.Provider), credential)); err != nil {
			logger.Warn().Err(err).Str("account_id", row.ID).Msg("could not backfill account identity")
		}
	}
}
