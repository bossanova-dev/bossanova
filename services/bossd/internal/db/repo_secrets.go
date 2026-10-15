package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/recurser/bossalib/models"

	"github.com/recurser/bossd/internal/reposecrets"
)

// Repo API keys and the keyring (BOS-1587). Secret VALUES never reach a log
// line or an error string from this file; failures are reported by repo id.

// scanHydrated scans one repo row and fills its keys from the keyring when the
// row is flagged has_keyring_secrets.
func (s *SQLiteRepoStore) scanHydrated(ctx context.Context, row *sql.Row) (*models.Repo, error) {
	r, hasKeyringSecrets, err := scanRepo(row)
	if err != nil {
		return nil, err
	}
	if hasKeyringSecrets {
		s.hydrate(ctx, r)
	}
	return r, nil
}

// hydrate fills r's keys from the keyring. A read failure leaves the keys
// empty and logs, rather than failing the read: every repo listing goes through
// here, and a keychain hiccup must degrade to "no key configured" for that repo
// instead of taking the whole repo list down. Each read is bounded by
// keyringReadTimeout (and a listing by listKeyringReadBudget).
func (s *SQLiteRepoStore) hydrate(ctx context.Context, r *models.Repo) {
	readCtx, cancel := context.WithTimeout(ctx, s.readTimeout)
	defer cancel()
	sec, err := s.secrets.Get(readCtx, r.ID)
	if err != nil {
		if errors.Is(err, reposecrets.ErrUnavailable) {
			s.hydrateWarnOnce.Do(func() {
				log.Warn().Err(err).Msg("keyring unavailable; repo keys stored there read as empty")
			})
			return
		}
		log.Warn().Err(err).Str("repo_id", r.ID).Msg("read repo keys from keyring failed; keys read as empty")
		return
	}
	r.LinearAPIKey = sec.LinearAPIKey
	r.SentryAPIKey = sec.SentryAPIKey
}

// noUndo is the compensation for a key write that touched nothing outside the
// SQLite transaction.
func noUndo() {}

// repoKeyLocker is one lock per repo, held across a key update's whole
// read-merge-write of the keyring item and the UPDATE that records it, so two
// concurrent key updates of one repo cannot interleave: each reads the keys the
// previous one left, and each queues its undo before the next one starts. It
// is in-process: every writer of a repo's key columns and flag goes through
// SQLiteRepoStore except the startup move, which runs before any store exists.
// NewRepoStore uses a *keyedgate.Registry; it is an interface only so tests can
// observe who reaches the lock and when it is released.
type repoKeyLocker interface {
	Acquire(ctx context.Context, key string, timeout time.Duration) (release func(), waited time.Duration, err error)
}

// deleteKeyLockWait bounds how long Delete waits for a repo's key lock before
// queuing the keyring removal anyway. A key update holds the lock for at most
// its key phase (keyringTimeout) and one write transaction, so twice the key
// phase is generous; the bound is there so that a holder wedged past it (a
// write transaction stuck on the database, say) cannot hold up Delete forever.
func (s *SQLiteRepoStore) deleteKeyLockWait() time.Duration {
	return 2 * s.keyringTimeout
}

// updateRowWithKeys is Update for params that write a key. A keychain call can
// block for as long as an approval prompt goes unanswered, so the key phase
// runs before, and outside, the SQLite write transaction: holding the write
// lock across it would fail every other bossd writer at the busy timeout. The
// repo's key lock takes the write lock's place for what the key phase needs:
// the read-merge-write of the two keys and the UPDATE recording it are one
// step per repo, and an update that does not commit queues its undo (see
// keyringUndo) before the next key update of that repo can start.
//
// The wait for the key lock is bounded by keyringTimeout, as the key phase is:
// a holder releases after at most its own key phase and one write
// transaction, so a waiter that gives up first reports an error the caller
// can retry rather than queue indefinitely behind a stuck keychain.
//
// The guards run twice: once before the key phase, so a stale or conflicting
// update fails without touching the keyring, and again in the write
// transaction, which is authoritative.
func (s *SQLiteRepoStore) updateRowWithKeys(ctx context.Context, id string, params UpdateRepoParams) (*models.Repo, bool, error) {
	unlock, _, err := s.keyLocks.Acquire(ctx, id, s.keyringTimeout)
	if err != nil {
		return nil, false, fmt.Errorf("wait for repo key update: %w", err)
	}
	defer unlock()
	if err := s.checkUpdateGuards(ctx, s.db, id, params); err != nil {
		return nil, false, err
	}

	// The key phase is bounded by keyringTimeout even when ctx has no
	// deadline, so a hung keychain cannot hold the key lock indefinitely.
	keyCtx, endKeys := context.WithTimeout(ctx, s.keyringTimeout)
	defer endKeys()
	keys, undo, err := s.writeKeysForUpdate(keyCtx, id, params)
	committed := false
	// undo puts back a keyring write the UPDATE did not keep, on every path
	// that does not commit: a failed key phase, guard, UPDATE, zero rows
	// affected, or the COMMIT itself. Deferred last, it runs before unlock,
	// and it waits on keyCtx at most, so it returns at once when ctx has
	// ended; endKeys releases keyCtx only after it.
	defer func() {
		if !committed {
			undo()
		}
	}()
	if err != nil {
		return nil, false, err
	}
	repo, hasKeyringSecrets, err := s.updateInTx(ctx, id, params, keys)
	if err != nil {
		return nil, false, err
	}
	committed = true
	return repo, hasKeyringSecrets, nil
}

// keyClauses are the repos-row SET clauses (and their args) for an update's
// LinearAPIKey / SentryAPIKey, resolved by writeKeysForUpdate.
type keyClauses struct {
	sets          []string
	args          []any
	requestsScrub bool
}

// writeKeysForUpdate is Update's key phase. It is not a pure clause builder: it
// WRITES the merged keys to the keyring item before the caller's UPDATE runs,
// and returns clauses that clear both columns and set the flag. The keyring is
// outside the SQLite transaction, so it also returns an undo func the caller
// must run whenever the update does not commit, including when this returns
// an error (a keyring write abandoned on ctx may still land); undo puts the
// keyring item back as it was (the previous item for a flagged row, no item for
// an unflagged one). undo is never nil. With no keyring available it returns
// clauses that keep the keys in the columns with the flag clear. The caller
// must hold id's key lock, so the read-merge-write of the two keys cannot
// interleave with another key update of the repo. It takes no SQLite lock.
// ctx bounds every keyring call here and how long undo waits; Update gives it
// keyringWriteTimeout.
func (s *SQLiteRepoStore) writeKeysForUpdate(ctx context.Context, id string, params UpdateRepoParams) (keyClauses, func(), error) {
	var current reposecrets.Secrets
	var hasKeyringSecrets int
	if err := s.db.QueryRowContext(ctx,
		`SELECT linear_api_key, sentry_api_key, has_keyring_secrets FROM repos WHERE id = ?`, id,
	).Scan(&current.LinearAPIKey, &current.SentryAPIKey, &hasKeyringSecrets); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return keyClauses{}, noUndo, sql.ErrNoRows
		}
		return keyClauses{}, noUndo, fmt.Errorf("read repo keys: %w", err)
	}
	if hasKeyringSecrets != 0 {
		// The keyring holds the key this update does not touch. If it cannot be
		// read, fail rather than silently drop that key.
		sec, err := s.secrets.Get(ctx, id)
		if err != nil {
			return keyClauses{}, noUndo, fmt.Errorf("read repo keys from keyring: %w", err)
		}
		current = sec
	}

	merged := current
	if params.LinearAPIKey != nil {
		merged.LinearAPIKey = *params.LinearAPIKey
	}
	if params.SentryAPIKey != nil {
		merged.SentryAPIKey = *params.SentryAPIKey
	}

	err := s.secrets.Set(ctx, id, merged)
	switch {
	case err == nil:
		return keyClauses{
				sets:          []string{"linear_api_key = ''", "sentry_api_key = ''", "has_keyring_secrets = 1"},
				requestsScrub: hasKeyringSecrets == 0 && !current.IsEmpty(),
			},
			s.keyringUndo(ctx, id, current, hasKeyringSecrets != 0), nil
	case errors.Is(err, reposecrets.ErrUnavailable) && hasKeyringSecrets == 0:
		// No keyring on this host (e.g. headless Linux without a secret
		// service): keep today's column storage rather than lock the user out
		// of Linear and Sentry.
		s.fallbackWarnOnce.Do(func() {
			log.Warn().Err(err).Msg("keyring unavailable; repo API keys are stored in the database instead")
		})
		return keyClauses{
			sets: []string{"linear_api_key = ?", "sentry_api_key = ?", "has_keyring_secrets = 0"},
			args: []any{merged.LinearAPIKey, merged.SentryAPIKey},
		}, noUndo, nil
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		// The Store gave up waiting, but a keyring call already in progress
		// may still land. Hand back the undo so the caller puts the item back;
		// the Store queues it after that call.
		return keyClauses{}, s.keyringUndo(ctx, id, current, hasKeyringSecrets != 0),
			fmt.Errorf("store repo keys in keyring: %w", err)
	default:
		return keyClauses{}, noUndo, fmt.Errorf("store repo keys in keyring: %w", err)
	}
}

// keyringUndo returns the compensation for a keyring write whose SQLite
// UPDATE did not commit. A row already flagged has_keyring_secrets gets its
// previous item back; an unflagged row's columns are still authoritative, so
// the item is removed.
//
// The undo is queued with reposecrets.Store.Restore, which lands it after the
// write it undoes — even one still stuck in the keychain — and before any
// keyring op queued later. Update runs it while it still holds the repo's key
// lock, so every later key write of the repo is queued behind it and cannot be
// clobbered. It then waits for the undo to land for as long as ctx allows: a
// prompt keychain leaves the item restored by the time Update returns, while a
// cancelled or expired ctx (the abandoned-write case, where the keychain is
// stuck) returns at once and lets the undo land on its own. A failed undo is
// logged by repo id, never with values.
func (s *SQLiteRepoStore) keyringUndo(ctx context.Context, id string, previous reposecrets.Secrets, wasFlagged bool) func() {
	restore := previous
	if !wasFlagged {
		restore = reposecrets.Secrets{}
	}
	return func() {
		landed := s.secrets.Restore(id, restore)
		select {
		case <-landed:
		case <-ctx.Done():
			log.Warn().Str("repo_id", id).
				Msg("repo update did not commit; restoring repo keys in keyring continues in the background")
		}
	}
}

// deleteKeyringSecrets removes a deleted repo's keyring item. The repo row is
// already gone, so a failure is logged (by the Store) rather than returned.
//
// The removal must land whatever happens to ctx: a removal skipped because
// Delete's caller gave up while the item was busy would leave a deleted repo's
// keys in the keychain indefinitely. So it is queued with Restore, which is
// never skipped, and queued under the repo's key lock, so it lands after the
// undo of any key update that was in flight when the row went (an undo puts
// the previous item back). Delete then waits for it at most until ctx ends or
// keyringTimeout passes; the removal lands either way.
func (s *SQLiteRepoStore) deleteKeyringSecrets(ctx context.Context, id string) {
	// The lock wait ignores ctx, since the removal must be queued anyway, and
	// is bounded by deleteKeyLockWait instead. Past it the removal is queued
	// without the lock: it may then land before a stuck holder's undo, which
	// is no worse than not queuing it at all.
	unlock, _, err := s.keyLocks.Acquire(context.WithoutCancel(ctx), id, s.deleteKeyLockWait())
	if err != nil {
		log.Warn().Err(err).Str("repo_id", id).
			Msg("delete repo keys from keyring: key lock still held; removing without it")
		unlock = func() {}
	}
	removed := s.secrets.Restore(id, reposecrets.Secrets{})
	unlock()
	waitCtx, cancel := context.WithTimeout(ctx, s.keyringTimeout)
	defer cancel()
	select {
	case <-removed:
	case <-waitCtx.Done():
		log.Warn().Str("repo_id", id).
			Msg("repo deleted; removing its keys from the keyring continues in the background")
	}
}

// MigrateRepoSecretsFromSQLite is the one-time move of plaintext repo keys into
// the keyring, run at daemon startup after migrations. Each row with a
// non-empty key column and has_keyring_secrets = 0 is moved under its own
// write transaction: write the keyring item, read it back from the keyring and
// compare, then clear the columns and set the flag with an UPDATE guarded on
// the values it moved, so a concurrent edit is never clobbered. A row that
// fails at any step is left exactly as it was and retried on the next start.
//
// It returns how many rows moved. The error joins every per-row failure; an
// unavailable keyring stops the move at the first row and is returned wrapping
// reposecrets.ErrUnavailable. Values are never logged.
func MigrateRepoSecretsFromSQLite(ctx context.Context, sqlDB *sql.DB, secrets *reposecrets.Store) (int, error) {
	ids, err := plaintextRepoKeyIDs(ctx, sqlDB)
	if err != nil {
		return 0, err
	}
	moved := 0
	var errs []error
	for _, id := range ids {
		ok, err := moveRepoSecrets(ctx, sqlDB, secrets, id)
		if err != nil {
			if errors.Is(err, reposecrets.ErrUnavailable) {
				return moved, err
			}
			errs = append(errs, fmt.Errorf("repo %s: %w", id, err))
			continue
		}
		if ok {
			moved++
		}
	}
	return moved, errors.Join(errs...)
}

func plaintextRepoKeyIDs(ctx context.Context, sqlDB *sql.DB) ([]string, error) {
	rows, err := sqlDB.QueryContext(ctx,
		`SELECT id FROM repos WHERE has_keyring_secrets = 0 AND (linear_api_key != '' OR sentry_api_key != '') ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list repos with plaintext keys: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan repo id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// moveRepoSecrets moves one row's keys. It reports false with a nil error when
// the row no longer needs moving (deleted, cleared or moved since listing).
//
// Unlike Update, it holds the write transaction across its keyring write and
// read-back. That starves no writer: it runs at daemon startup under bossd's
// exclusive daemon lock, after migrations and before any store, server or
// worker is built, and no other process opens the database, so there is no
// other writer to wait on it; its ctx bounds the whole move.
func moveRepoSecrets(ctx context.Context, sqlDB *sql.DB, secrets *reposecrets.Store, id string) (bool, error) {
	conn, err := beginImmediate(ctx, sqlDB, "repo secrets move")
	if err != nil {
		return false, err
	}
	committed := false
	defer closeImmediate(ctx, conn, &committed)

	var sec reposecrets.Secrets
	var hasKeyringSecrets int
	err = conn.QueryRowContext(ctx,
		`SELECT linear_api_key, sentry_api_key, has_keyring_secrets FROM repos WHERE id = ?`, id,
	).Scan(&sec.LinearAPIKey, &sec.SentryAPIKey, &hasKeyringSecrets)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read keys: %w", err)
	}
	if hasKeyringSecrets != 0 || sec.IsEmpty() {
		return false, nil
	}

	if err := secrets.Set(ctx, id, sec); err != nil {
		return false, err
	}
	// Read back from the keyring itself, not the cache Set just filled.
	secrets.Forget(id)
	readBack, err := secrets.Get(ctx, id)
	if err != nil {
		return false, fmt.Errorf("read back keyring item: %w", err)
	}
	if readBack != sec {
		return false, errors.New("keyring item read back different from the stored keys")
	}

	res, err := conn.ExecContext(ctx,
		`UPDATE repos SET linear_api_key = '', sentry_api_key = '', has_keyring_secrets = 1
		 WHERE id = ? AND has_keyring_secrets = 0 AND linear_api_key = ? AND sentry_api_key = ?`,
		id, sec.LinearAPIKey, sec.SentryAPIKey)
	if err != nil {
		return false, fmt.Errorf("clear key columns: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	if _, err := conn.ExecContext(ctx, requestSecretScrubSQL); err != nil {
		return false, fmt.Errorf("request secret scrub: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	committed = true
	return true, nil
}
