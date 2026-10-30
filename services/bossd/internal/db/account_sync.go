package db

import (
	"context"
	"fmt"

	"github.com/recurser/bossalib/contract/cloud/sqlutil"
	"github.com/recurser/bossd/internal/db/sqlcgen"
	"github.com/recurser/bossd/internal/domain"
)

// SetSync records a newer cloud association without changing local metadata.
func (s *SQLiteAgentAccountStore) SetSync(ctx context.Context, id, syncedID string, version int64) error {
	n, err := sqlcgen.New(s.db).SetAgentAccountSync(ctx, sqlcgen.SetAgentAccountSyncParams{ID: id, SyncedAgentAccountID: syncedID, SyncVersion: version, UpdatedAt: sqlutil.TimeNow()})
	if err != nil {
		return fmt.Errorf("set account sync: %w", err)
	}
	if n == 0 {
		_, err = s.Get(ctx, id)
	}
	return err
}

// UpdateSync atomically accepts metadata and its version, rejecting stale writers.
func (s *SQLiteAgentAccountStore) UpdateSync(ctx context.Context, cmd domain.ApplyAgentAccountSync) (bool, error) {
	arg := sqlcgen.UpdateAgentAccountSyncParams{ID: cmd.LocalAgentAccountID, SyncedAgentAccountID: cmd.SyncedAgentAccountID, SyncVersion: cmd.Version, Priority: int64(cmd.Priority), UpdatedAt: sqlutil.TimeNow()}
	if cmd.DesiredStatus != "" {
		arg.Status = &cmd.DesiredStatus
	}
	n, err := sqlcgen.New(s.db).UpdateAgentAccountSync(ctx, arg)
	if err != nil {
		return false, fmt.Errorf("update account sync: %w", err)
	}
	if n == 0 {
		if _, err := s.Get(ctx, cmd.LocalAgentAccountID); err != nil {
			return false, err
		}
	}
	return n != 0, nil
}

// SetCredentialVersion records an installed encrypted blob without accepting rollback.
func (s *SQLiteAgentAccountStore) SetCredentialVersion(ctx context.Context, id string, version int64) error {
	n, err := sqlcgen.New(s.db).SetAgentAccountCredentialVersion(ctx, sqlcgen.SetAgentAccountCredentialVersionParams{ID: id, SyncedCredentialVersion: version, UpdatedAt: sqlutil.TimeNow()})
	if err != nil {
		return fmt.Errorf("set credential version: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("credential version rollback or missing account")
	}
	return nil
}
