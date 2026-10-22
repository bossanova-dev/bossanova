package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/recurser/bossd/internal/db/sqlcgen"
)

// ScrubFreedSecretPages removes stale copies of moved repo keys from the
// database file and its WAL when requested. Call at startup before stores or
// workers exist. A failure leaves a request for the next start.
func ScrubFreedSecretPages(ctx context.Context, sqlDB *sql.DB) (ran bool, err error) {
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return false, fmt.Errorf("open secret scrub connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	return scrubFreedSecretPages(ctx, conn)
}

func scrubFreedSecretPages(ctx context.Context, conn sqlcgen.DBTX) (ran bool, err error) {
	q := sqlcgen.New(conn)
	requested, err := q.CountRepoSecretScrubs(ctx)
	if err != nil {
		return false, fmt.Errorf("read secret scrub request: %w", err)
	}
	if requested == 0 {
		return false, nil
	}
	defer func() {
		_, restoreErr := conn.ExecContext(context.WithoutCancel(ctx), `PRAGMA secure_delete = FAST`)
		if restoreErr != nil {
			err = errors.Join(err, fmt.Errorf("restore secure_delete: %w", restoreErr))
		}
		if err != nil {
			// DELETE may already have committed when the final checkpoint fails.
			// Keep the request retryable even if the caller cancelled.
			requestErr := q.RecordRepoSecretScrub(context.WithoutCancel(ctx))
			if requestErr != nil {
				err = errors.Join(err, fmt.Errorf("restore secret scrub request: %w", requestErr))
			}
			ran = false
		}
	}()
	if _, err := conn.ExecContext(ctx, `PRAGMA secure_delete = ON`); err != nil {
		return false, fmt.Errorf("enable secret scrub: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `VACUUM`); err != nil {
		return false, fmt.Errorf("vacuum secret pages: %w", err)
	}
	if err := truncateSecretWAL(ctx, conn); err != nil {
		return false, err
	}
	if err := q.DeleteRepoSecretScrubs(ctx); err != nil {
		return false, fmt.Errorf("clear secret scrub request: %w", err)
	}
	if err := truncateSecretWAL(ctx, conn); err != nil {
		return false, err
	}
	return true, nil
}

// SQLite maintenance statements (PRAGMA and VACUUM) bypass sqlc: they control the
// checked-out connection rather than query application rows.
func truncateSecretWAL(ctx context.Context, conn sqlcgen.DBTX) error {
	var busy, frames, checkpointed int
	if err := conn.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &frames, &checkpointed); err != nil {
		return fmt.Errorf("truncate secret WAL: %w", err)
	}
	if busy != 0 {
		return fmt.Errorf("truncate secret WAL: checkpoint busy (%d frames, %d checkpointed)", frames, checkpointed)
	}
	return nil
}
