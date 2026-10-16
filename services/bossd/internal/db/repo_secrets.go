package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sqlutil"

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
// keyringUndo) before the next key update of that repo can start. A COMMIT
// error is not taken at its word: it is resolved from a fresh read of the row
// (resolveKeyCommit), and only a commit shown not to have landed is undone. A
// commit that landed despite the error returns the committed row and a nil
// error, with a warning logged by repo id.
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
	// affected, or a COMMIT error that a fresh read shows did not land (one
	// that landed, or whose outcome is unknown, is not undone; see
	// resolveKeyCommit). Deferred last, it runs before unlock,
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
	var uncertain *uncertainCommitError
	if errors.As(err, &uncertain) {
		// updateInTx has released its connection, so the fresh read can run.
		switch resolveKeyCommit(ctx, s.db, id, uncertain) {
		case commitLanded:
			committed = true
			log.Warn().Err(uncertain.err).Str("repo_id", id).
				Msg("repo key update reported a commit error but landed; keeping its keyring item")
			return uncertain.repo, uncertain.hasKeyringSecrets, nil
		case commitNotLanded:
			return nil, false, err
		default:
			// Never undo on a guess: undoing a write that landed loses the
			// keys, while keeping one that did not is overwritten by the next
			// key write (or, for an unflagged row, the next startup move).
			committed = true
			log.Warn().Err(uncertain.err).Str("repo_id", id).
				Msg("repo key update reported a commit error and its outcome is unknown; keyring item left as written")
			return nil, false, fmt.Errorf("repo key update outcome unknown: %w", err)
		}
	}
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
	// updatedAfter, when set, is the row's updated_at before the UPDATE;
	// update moves updated_at strictly past it (see nextUpdatedAt). updateInTx
	// sets it so a keyed commit's outcome can be read back from the row.
	updatedAfter string
}

// nextUpdatedAt is the updated_at an UPDATE writes: now, or 1ms past
// updatedAfter when now would not be later than it (two writes in one
// millisecond, or a clock that stepped back). Moving updated_at on every keyed
// update is what lets classifyKeyCommit tell whether a flagged row's commit
// landed. An updatedAfter that is empty or not in sqlutil.TimeLayout is ignored.
func nextUpdatedAt(now, updatedAfter string) string {
	if updatedAfter == "" || now > updatedAfter {
		return now
	}
	prev, err := time.Parse(sqlutil.TimeLayout, updatedAfter)
	if err != nil {
		return now
	}
	return sqlutil.FormatTime(prev.Add(time.Millisecond))
}

// repoKeyState is the part of a repo row that records where its keys live:
// the flag, and updated_at, which every keyed update moves.
type repoKeyState struct {
	updatedAt string
	flagged   bool
}

// readRepoKeyState reads id's repoKeyState from q. A missing row is
// sql.ErrNoRows, unwrapped.
func readRepoKeyState(ctx context.Context, q repoSQL, id string) (repoKeyState, error) {
	var st repoKeyState
	var flag int
	if err := q.QueryRowContext(ctx,
		`SELECT updated_at, has_keyring_secrets FROM repos WHERE id = ?`, id,
	).Scan(&st.updatedAt, &flag); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return repoKeyState{}, sql.ErrNoRows
		}
		return repoKeyState{}, fmt.Errorf("read repo key state: %w", err)
	}
	st.flagged = flag != 0
	return st, nil
}

// commitOutcome is what a fresh read says about a COMMIT that reported an
// error.
type commitOutcome int

const (
	// commitUnknown: the read cannot tell. Nothing is compensated.
	commitUnknown commitOutcome = iota
	// commitLanded: the transaction is durable despite the error.
	commitLanded
	// commitNotLanded: the transaction was discarded.
	commitNotLanded
)

// classifyKeyCommit decides a keyed write's COMMIT error from the row's key
// state before and after the write (as seen inside the transaction) and a
// fresh read made after the transaction's connection was released:
//
//   - a failed fresh read (including sql.ErrNoRows: a concurrent Delete owns
//     the keyring item's removal) is unknown;
//   - when the write changed the flag, the flag decides. The flag and key
//     columns change only under the repo's key lock, or before any store
//     exists for the startup move, so no other writer can forge it;
//   - otherwise updated_at decides: the write's value means landed, the
//     previous one means not landed, and anything else (a concurrent non-key
//     update moved it), or a write that did not move it, is unknown.
func classifyKeyCommit(before, after, fresh repoKeyState, freshErr error) commitOutcome {
	switch {
	case freshErr != nil:
		return commitUnknown
	case before.flagged != after.flagged:
		if fresh.flagged == after.flagged {
			return commitLanded
		}
		return commitNotLanded
	case before.updatedAt == after.updatedAt:
		return commitUnknown
	case fresh.updatedAt == after.updatedAt:
		return commitLanded
	case fresh.updatedAt == before.updatedAt:
		return commitNotLanded
	default:
		return commitUnknown
	}
}

// uncertainCommitError is a keyed write's COMMIT error, with what
// resolveKeyCommit needs to decide whether the write landed anyway. Its
// message is the wrapped COMMIT error's.
type uncertainCommitError struct {
	err           error
	before, after repoKeyState
	// repo and hasKeyringSecrets are the row the write read back inside its
	// transaction (unhydrated), returned when the commit turns out to have
	// landed.
	repo              *models.Repo
	hasKeyringSecrets bool
}

func (e *uncertainCommitError) Error() string { return e.err.Error() }
func (e *uncertainCommitError) Unwrap() error { return e.err }

// commitOutcomeReadTimeout bounds resolveKeyCommit's fresh read.
const commitOutcomeReadTimeout = 5 * time.Second

// resolveKeyCommit decides e from a fresh read of id through sqlDB. The caller
// must already have released the write transaction's connection. The read
// ignores ctx's cancellation, which is one of the ways a COMMIT fails, and is
// bounded by commitOutcomeReadTimeout instead.
func resolveKeyCommit(ctx context.Context, sqlDB *sql.DB, id string, e *uncertainCommitError) commitOutcome {
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commitOutcomeReadTimeout)
	defer cancel()
	fresh, err := readRepoKeyState(readCtx, sqlDB, id)
	return classifyKeyCommit(e.before, e.after, fresh, err)
}

// writeKeysForUpdate is Update's key phase. It is not a pure clause builder: it
// WRITES the merged keys to the keyring item before the caller's UPDATE runs,
// and returns clauses that clear both columns and set the flag. The keyring is
// outside the SQLite transaction, so it also returns an undo func the caller
// must run whenever the update does not commit (for a COMMIT error, only once a
// fresh read shows it did not land), including when this returns
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
// UPDATE did not commit (never one whose COMMIT error might have landed; see
// updateRowWithKeys). A row already flagged has_keyring_secrets gets its
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
// fails at any step is left exactly as it was and retried on the next start,
// except that a COMMIT error the row shows landed counts as moved.
//
// It returns how many rows moved. The error joins every per-row failure; an
// unavailable keyring stops the move at the first row and is returned wrapping
// reposecrets.ErrUnavailable. Values are never logged.
func MigrateRepoSecretsFromSQLite(ctx context.Context, sqlDB *sql.DB, secrets *reposecrets.Store) (int, error) {
	return migrateRepoSecrets(ctx, sqlDB, secrets, execCommit)
}

// migrateRepoSecrets is MigrateRepoSecretsFromSQLite with each move's COMMIT
// made by commit, so tests can make it fail.
func migrateRepoSecrets(ctx context.Context, sqlDB *sql.DB, secrets *reposecrets.Store, commit commitFunc) (int, error) {
	ids, err := plaintextRepoKeyIDs(ctx, sqlDB)
	if err != nil {
		return 0, err
	}
	moved := 0
	var errs []error
	for _, id := range ids {
		ok, err := moveRepoSecrets(ctx, sqlDB, secrets, id, commit)
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
// A COMMIT error is resolved from a fresh read of the row, made once the move's
// connection is released: a move that landed anyway reports true with a nil
// error (and logs a warning by repo id); otherwise the COMMIT error is
// returned. No failure path compensates the keyring: until the row is flagged
// its columns stay authoritative, and the item the move wrote mirrors them and
// is overwritten by the next start's retry.
func moveRepoSecrets(ctx context.Context, sqlDB *sql.DB, secrets *reposecrets.Store, id string, commit commitFunc) (bool, error) {
	moved, err := moveRepoSecretsInTx(ctx, sqlDB, secrets, id, commit)
	var uncertain *uncertainCommitError
	if !errors.As(err, &uncertain) {
		return moved, err
	}
	if resolveKeyCommit(ctx, sqlDB, id, uncertain) == commitLanded {
		log.Warn().Err(uncertain.err).Str("repo_id", id).
			Msg("repo key move reported a commit error but landed; counting it as moved")
		return true, nil
	}
	return false, err
}

// moveRepoSecretsInTx is moveRepoSecrets' write transaction. A COMMIT error is
// returned as an *uncertainCommitError for moveRepoSecrets to resolve after
// this has released the connection.
//
// Unlike Update, it holds the write transaction across its keyring write and
// read-back. That starves no writer: it runs at daemon startup under bossd's
// exclusive daemon lock, after migrations and before any store, server or
// worker is built, and no other process opens the database, so there is no
// other writer to wait on it; its ctx bounds the whole move.
func moveRepoSecretsInTx(ctx context.Context, sqlDB *sql.DB, secrets *reposecrets.Store, id string, commit commitFunc) (bool, error) {
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
	if err := commit(ctx, conn); err != nil {
		// The move's UPDATE sets the flag and leaves updated_at alone, so the
		// flag alone decides the outcome.
		return false, &uncertainCommitError{
			err:    fmt.Errorf("commit: %w", err),
			before: repoKeyState{flagged: false},
			after:  repoKeyState{flagged: true},
		}
	}
	committed = true
	return true, nil
}
