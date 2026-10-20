package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/recurser/bossalib/keyedgate"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sqlutil"
	"github.com/recurser/bossalib/vcs"

	"github.com/recurser/bossd/internal/reposecrets"
)

// ErrAmbiguousOrigin is returned by GetByOrigin when the canonical-URL
// fallback finds more than one repo whose stored origin maps to the same
// canonical web URL. Webhook routing must fail loud here rather than pick
// an arbitrary repo and silently refresh the wrong sessions.
var ErrAmbiguousOrigin = errors.New("multiple repos match canonical origin URL")

var _ RepoStore = (*SQLiteRepoStore)(nil)

// repoColumns is the column list every repo SELECT reads, in scanRepo order.
const repoColumns = `id, display_name, local_path, origin_url, default_base_branch, worktree_base_dir, setup_script, can_auto_merge, can_auto_merge_dependabot, can_auto_repair, can_auto_rotate, should_archive_sessions_after_merge, can_auto_delete_branches, should_keep_branches_current, merge_strategy, linear_api_key, sentry_api_key, sentry_org, is_trusted, has_keyring_secrets, created_at, updated_at`

// SQLiteRepoStore implements RepoStore using SQLite.
//
// Repo API keys (BOS-1587): a key write goes to the keyring item
// repo-secrets:<id> and the row keeps both key columns empty with
// has_keyring_secrets = 1; reads hydrate models.Repo's LinearAPIKey /
// SentryAPIKey from the keyring for flagged rows, so the many readers of those
// fields are unchanged. When the keyring cannot be opened — including a store
// built without WithRepoSecrets, whose secret store never opens — the columns
// stay authoritative (flag 0).
type SQLiteRepoStore struct {
	db      *sql.DB
	secrets *reposecrets.Store
	// keyringTimeout bounds Update's keyring phase; see keyringWriteTimeout.
	keyringTimeout time.Duration
	// readTimeout and listReadBudget bound keyring reads made while hydrating
	// repo reads; see keyringReadTimeout.
	readTimeout    time.Duration
	listReadBudget time.Duration
	// keyLocks serializes each repo's key updates; see updateRowWithKeys.
	keyLocks repoKeyLocker
	// commit ends updateInTx's write transaction; execCommit outside tests.
	commit commitFunc

	fallbackWarnOnce sync.Once
	hydrateWarnOnce  sync.Once
}

// keyringWriteTimeout bounds Update's keyring phase: the keyring read, write
// and undo it does while holding the repo's key lock. That phase never holds
// the SQLite write lock or a connection, so a stuck keychain holds up only
// other key updates (and the keyring removal of a Delete) of the same repo.
// A keychain call can hang (an unanswered approval prompt), and a request ctx
// need not carry a deadline, so without this bound one stuck keychain would
// hold that repo's key lock until it answered. 30s leaves room for a person to
// answer an approval prompt.
const keyringWriteTimeout = 30 * time.Second

// keyringReadTimeout bounds each keyring read made while hydrating a repo
// read, and listKeyringReadBudget bounds all of one listing's reads together.
// Repo reads run on paths with no deadline of their own (daemon startup, the
// pollers), so without these bounds an unanswered keychain prompt would block
// them indefinitely. A read that runs out of time degrades to empty keys and
// is logged, exactly like any other keyring read failure.
const (
	keyringReadTimeout    = 5 * time.Second
	listKeyringReadBudget = 10 * time.Second
)

// RepoStoreOption configures a SQLiteRepoStore.
type RepoStoreOption func(*SQLiteRepoStore)

// WithRepoSecrets keeps repo API keys in the given keyring-backed store. Without
// it (or given nil) the store uses reposecrets.NewUnavailable, so keys stay in
// the columns.
func WithRepoSecrets(secrets *reposecrets.Store) RepoStoreOption {
	return func(s *SQLiteRepoStore) {
		if secrets != nil {
			s.secrets = secrets
		}
	}
}

type repoSQL interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// NewRepoStore creates a new SQLite-backed RepoStore.
func NewRepoStore(db *sql.DB, opts ...RepoStoreOption) *SQLiteRepoStore {
	s := &SQLiteRepoStore{
		db:             db,
		secrets:        reposecrets.NewUnavailable(),
		keyringTimeout: keyringWriteTimeout,
		readTimeout:    keyringReadTimeout,
		listReadBudget: listKeyringReadBudget,
		keyLocks:       &keyedgate.Registry{Name: "repo-key"},
		commit:         execCommit,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *SQLiteRepoStore) Create(ctx context.Context, params CreateRepoParams) (*models.Repo, error) {
	repo, hasKeyringSecrets, err := s.create(ctx, params)
	if err != nil {
		return nil, err
	}
	if hasKeyringSecrets {
		s.hydrate(ctx, repo)
	}
	return repo, nil
}

// create inserts the repo and reads its row back unhydrated: a keyring read
// must not run while the write lock is held.
func (s *SQLiteRepoStore) create(ctx context.Context, params CreateRepoParams) (*models.Repo, bool, error) {
	id, err := sqlutil.NewID()
	if err != nil {
		return nil, false, fmt.Errorf("new repo id: %w", err)
	}
	conn, err := beginImmediate(ctx, s.db, "repo")
	if err != nil {
		return nil, false, err
	}
	committed := false
	defer closeImmediate(ctx, conn, &committed)

	if err := s.ensureUniqueCanonicalOrigin(ctx, conn, params.OriginURL, ""); err != nil {
		return nil, false, err
	}

	now := sqlutil.TimeNow()
	// can_auto_delete_branches defaults OFF for new repos (BOS-424): an on-by-default
	// destructive auto-delete is the wrong default. This INSERT literal is authoritative
	// for app-created repos; the BOS-180 column DEFAULT 1 is a dormant fallback that Create
	// never exercises (SQLite can't ALTER a column default in place without a table rebuild).
	// Existing repos are intentionally left untouched (scope = new repos).
	_, err = conn.ExecContext(ctx,
		`INSERT INTO repos (id, display_name, local_path, origin_url, default_base_branch, worktree_base_dir, setup_script, can_auto_merge, can_auto_merge_dependabot, can_auto_repair, can_auto_rotate, should_archive_sessions_after_merge, can_auto_delete_branches, should_keep_branches_current, merge_strategy, linear_api_key, sentry_api_key, sentry_org, is_trusted, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 0, 1, 1, 1, 1, 0, 0, 'merge', '', '', '', ?, ?, ?)`,
		id, params.DisplayName, params.LocalPath, params.OriginURL,
		params.DefaultBaseBranch, params.WorktreeBaseDir, params.SetupScript,
		sqlutil.BoolToInt(params.IsTrusted), now, now,
	)
	if err != nil {
		return nil, false, fmt.Errorf("insert repo: %w", err)
	}
	repo, hasKeyringSecrets, err := getRow(ctx, conn, id)
	if err != nil {
		return nil, false, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return nil, false, fmt.Errorf("commit repo create: %w", err)
	}
	committed = true
	return repo, hasKeyringSecrets, nil
}

func (s *SQLiteRepoStore) Get(ctx context.Context, id string) (*models.Repo, error) {
	r, hasKeyringSecrets, err := getRow(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	if hasKeyringSecrets {
		s.hydrate(ctx, r)
	}
	return r, nil
}

// getRow reads one repo row without hydrating its keys, reporting whether they
// live in the keyring. Writers read back through it inside their transaction
// and hydrate only once the write lock is released.
func getRow(ctx context.Context, q repoSQL, id string) (*models.Repo, bool, error) {
	row := q.QueryRowContext(ctx,
		`SELECT `+repoColumns+`
		 FROM repos WHERE id = ?`, id)
	return scanRepo(row)
}

func (s *SQLiteRepoStore) GetByPath(ctx context.Context, localPath string) (*models.Repo, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+repoColumns+`
		 FROM repos WHERE local_path = ?`, localPath)
	return s.scanHydrated(ctx, row)
}

func (s *SQLiteRepoStore) GetByOrigin(ctx context.Context, originURL string) (*models.Repo, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+repoColumns+`
		 FROM repos WHERE origin_url = ?`, originURL)
	repo, err := s.scanHydrated(ctx, row)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}

	targetWebURL := vcs.NormalizeRepoURL(originURL)
	if targetWebURL == "" {
		if err == nil {
			return repo, nil
		}
		return nil, sql.ErrNoRows
	}

	repos, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	var match *models.Repo
	for _, repo := range repos {
		repoWebURL := vcs.NormalizeRepoURL(repo.OriginURL)
		if repoWebURL == "" || repoWebURL != targetWebURL {
			continue
		}
		if match != nil {
			return nil, fmt.Errorf("%w: %q", ErrAmbiguousOrigin, targetWebURL)
		}
		match = repo
	}
	if match == nil {
		if repo != nil {
			return repo, nil
		}
		return nil, sql.ErrNoRows
	}
	return match, nil
}

func (s *SQLiteRepoStore) List(ctx context.Context) ([]*models.Repo, error) {
	return s.list(ctx, s.db)
}

func (s *SQLiteRepoStore) list(ctx context.Context, q repoSQL) ([]*models.Repo, error) {
	return s.listRows(ctx, q, true)
}

// listRows lists every repo, hydrating keyring-held keys only when hydrate is
// set; the canonical-origin check needs no keys and must not touch the keyring.
func (s *SQLiteRepoStore) listRows(ctx context.Context, q repoSQL, hydrate bool) ([]*models.Repo, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT `+repoColumns+`
		 FROM repos ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list repos: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var repos []*models.Repo
	var inKeyring []*models.Repo
	for rows.Next() {
		r, hasKeyringSecrets, err := scanRepo(rows)
		if err != nil {
			return nil, err
		}
		repos = append(repos, r)
		if hasKeyringSecrets {
			inKeyring = append(inKeyring, r)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Hydrate after the cursor is drained: a keyring read must not hold the
	// SQLite read open.
	if hydrate && len(inKeyring) > 0 {
		budgetCtx, cancel := context.WithTimeout(ctx, s.listReadBudget)
		defer cancel()
		for _, r := range inKeyring {
			s.hydrate(budgetCtx, r)
		}
	}
	return repos, nil
}

func (s *SQLiteRepoStore) Update(ctx context.Context, id string, params UpdateRepoParams) (*models.Repo, error) {
	repo, hasKeyringSecrets, err := s.updateRow(ctx, id, params)
	if err != nil {
		return nil, err
	}
	// Hydrate only now: the write lock (and, for the transactional path, its
	// connection) is released, so a slow keychain read holds up no writer.
	if hasKeyringSecrets {
		s.hydrate(ctx, repo)
	}
	return repo, nil
}

// updateRow applies params and returns the updated row unhydrated, reporting
// whether its keys live in the keyring.
func (s *SQLiteRepoStore) updateRow(ctx context.Context, id string, params UpdateRepoParams) (*models.Repo, bool, error) {
	if params.LinearAPIKey != nil || params.SentryAPIKey != nil {
		return s.updateRowWithKeys(ctx, id, params)
	}
	// The non-transactional fast path only applies when there is neither a
	// canonical-origin uniqueness check nor an optimistic-concurrency guard to
	// run atomically with the write.
	if params.OriginURL == nil && params.ExpectedUpdatedAt == nil {
		return s.update(ctx, s.db, id, params, keyClauses{})
	}
	return s.updateInTx(ctx, id, params, keyClauses{})
}

// updateInTx runs params' guards and the UPDATE (carrying keys' column
// clauses) in one write transaction and commits it.
//
// For a keyed update (keys.sets non-empty) a COMMIT error is returned as an
// *uncertainCommitError carrying the row's key state before and after the
// UPDATE, so updateRowWithKeys can tell from a fresh read whether the commit
// landed before it compensates the keyring. It must make that read only after
// this returns: the connection is released here, and an in-memory database has
// only the one.
func (s *SQLiteRepoStore) updateInTx(ctx context.Context, id string, params UpdateRepoParams, keys keyClauses) (*models.Repo, bool, error) {
	conn, err := beginImmediate(ctx, s.db, "repo")
	if err != nil {
		return nil, false, err
	}
	committed := false
	defer closeImmediate(ctx, conn, &committed)

	if err := s.checkUpdateGuards(ctx, conn, id, params); err != nil {
		return nil, false, err
	}
	keyed := len(keys.sets) > 0
	var before repoKeyState
	if keyed {
		// Read inside the write transaction, this is exactly the state a
		// COMMIT that does not land leaves behind.
		before, err = readRepoKeyState(ctx, conn, id)
		if err != nil {
			return nil, false, err
		}
		keys.updatedAfter = before.updatedAt
	}
	repo, hasKeyringSecrets, err := s.update(ctx, conn, id, params, keys)
	if err != nil {
		return nil, false, err
	}
	if err := s.commit(ctx, conn); err != nil {
		err = fmt.Errorf("commit repo update: %w", err)
		if !keyed {
			return nil, false, err
		}
		return nil, false, &uncertainCommitError{
			err:               err,
			before:            before,
			after:             repoKeyState{updatedAt: sqlutil.FormatTime(repo.UpdatedAt), flagged: hasKeyringSecrets},
			repo:              repo,
			hasKeyringSecrets: hasKeyringSecrets,
		}
	}
	committed = true
	return repo, hasKeyringSecrets, nil
}

// checkUpdateGuards runs params' canonical-origin uniqueness check and
// optimistic-concurrency guard against q.
func (s *SQLiteRepoStore) checkUpdateGuards(ctx context.Context, q repoSQL, id string, params UpdateRepoParams) error {
	if params.OriginURL != nil {
		if err := s.ensureUniqueCanonicalOrigin(ctx, q, *params.OriginURL, id); err != nil {
			return err
		}
	}
	if params.ExpectedUpdatedAt != nil {
		if err := s.checkExpectedUpdatedAt(ctx, q, id, *params.ExpectedUpdatedAt); err != nil {
			return err
		}
	}
	return nil
}

// checkExpectedUpdatedAt enforces the optimistic-concurrency guard inside the
// caller's open transaction. It reads the repo's current updated_at TEXT and
// compares it string-to-string against expected formatted with the canonical
// storage layout, so the comparison is at the stored millisecond granularity
// (raw time.Time equality would spuriously fail on sub-millisecond client
// round-trip drift). Returns sql.ErrNoRows when the row is gone and
// ErrStaleRepoUpdate on a token mismatch; in either case the caller does not
// commit, so no field is changed.
//
// Limitation: the token is millisecond-granular, so two writes landing in the
// same millisecond produce identical updated_at strings and a stale reader's
// token could still match — a narrow lost-update window. This is acceptable for
// human-paced repo-settings edits; a monotonic version column would close it if
// finer concurrency guarantees are ever needed.
func (s *SQLiteRepoStore) checkExpectedUpdatedAt(ctx context.Context, q repoSQL, id string, expected time.Time) error {
	var stored string
	if err := q.QueryRowContext(ctx, `SELECT updated_at FROM repos WHERE id = ?`, id).Scan(&stored); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sql.ErrNoRows
		}
		return fmt.Errorf("read repo updated_at: %w", err)
	}
	if stored != sqlutil.FormatTime(expected) {
		return ErrStaleRepoUpdate
	}
	return nil
}

// update writes params' non-key fields plus the key column clauses the key
// phase resolved; it never reads params.LinearAPIKey / SentryAPIKey itself. It
// returns the updated row unhydrated (see getRow).
func (s *SQLiteRepoStore) update(ctx context.Context, q repoSQL, id string, params UpdateRepoParams, keys keyClauses) (*models.Repo, bool, error) {
	now := nextUpdatedAt(sqlutil.TimeNow(), keys.updatedAfter)
	sets := []string{"updated_at = ?"}
	args := []any{now}

	if params.DisplayName != nil {
		sets = append(sets, "display_name = ?")
		args = append(args, *params.DisplayName)
	}
	if params.OriginURL != nil {
		sets = append(sets, "origin_url = ?")
		args = append(args, *params.OriginURL)
	}
	if params.DefaultBaseBranch != nil {
		sets = append(sets, "default_base_branch = ?")
		args = append(args, *params.DefaultBaseBranch)
	}
	if params.WorktreeBaseDir != nil {
		sets = append(sets, "worktree_base_dir = ?")
		args = append(args, *params.WorktreeBaseDir)
	}
	if params.SetupScript != nil {
		sets = append(sets, "setup_script = ?")
		args = append(args, *params.SetupScript)
	}
	if params.CanAutoMerge != nil {
		sets = append(sets, "can_auto_merge = ?")
		args = append(args, sqlutil.BoolToInt(*params.CanAutoMerge))
	}
	if params.CanAutoMergeDependabot != nil {
		sets = append(sets, "can_auto_merge_dependabot = ?")
		args = append(args, sqlutil.BoolToInt(*params.CanAutoMergeDependabot))
	}
	if params.CanAutoRepair != nil {
		sets = append(sets, "can_auto_repair = ?")
		args = append(args, sqlutil.BoolToInt(*params.CanAutoRepair))
	}
	if params.CanAutoRotate != nil {
		sets = append(sets, "can_auto_rotate = ?")
		args = append(args, sqlutil.BoolToInt(*params.CanAutoRotate))
	}
	if params.ShouldArchiveSessionsAfterMerge != nil {
		sets = append(sets, "should_archive_sessions_after_merge = ?")
		args = append(args, sqlutil.BoolToInt(*params.ShouldArchiveSessionsAfterMerge))
	}
	if params.CanAutoDeleteBranches != nil {
		sets = append(sets, "can_auto_delete_branches = ?")
		args = append(args, sqlutil.BoolToInt(*params.CanAutoDeleteBranches))
	}
	if params.ShouldKeepBranchesCurrent != nil {
		sets = append(sets, "should_keep_branches_current = ?")
		args = append(args, sqlutil.BoolToInt(*params.ShouldKeepBranchesCurrent))
	}
	if params.MergeStrategy != nil {
		sets = append(sets, "merge_strategy = ?")
		args = append(args, string(*params.MergeStrategy))
	}
	sets = append(sets, keys.sets...)
	args = append(args, keys.args...)
	if params.SentryOrg != nil {
		sets = append(sets, "sentry_org = ?")
		args = append(args, *params.SentryOrg)
	}
	if params.IsTrusted != nil {
		sets = append(sets, "is_trusted = ?")
		args = append(args, sqlutil.BoolToInt(*params.IsTrusted))
	}
	args = append(args, id)
	query := "UPDATE repos SET " + strings.Join(sets, ", ") + " WHERE id = ?"
	res, err := q.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("update repo: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, false, sql.ErrNoRows
	}
	if keys.requestsScrub {
		if _, err := q.ExecContext(ctx, requestSecretScrubSQL); err != nil {
			return nil, false, fmt.Errorf("request secret scrub: %w", err)
		}
	}
	return getRow(ctx, q, id)
}

func (s *SQLiteRepoStore) ensureUniqueCanonicalOrigin(ctx context.Context, q repoSQL, originURL string, excludeID string) error {
	targetWebURL := vcs.NormalizeRepoURL(originURL)
	if targetWebURL == "" {
		return nil
	}

	repos, err := s.listRows(ctx, q, false)
	if err != nil {
		return err
	}
	for _, repo := range repos {
		if repo.ID == excludeID {
			continue
		}
		repoWebURL := vcs.NormalizeRepoURL(repo.OriginURL)
		if repoWebURL == "" || repoWebURL != targetWebURL {
			continue
		}
		return fmt.Errorf("%w: %q", ErrAmbiguousOrigin, targetWebURL)
	}
	return nil
}

func (s *SQLiteRepoStore) Delete(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM session_check_snapshots WHERE session_id IN (SELECT id FROM sessions WHERE repo_id = ?)`, id); err != nil {
		return fmt.Errorf("delete check snapshots: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM workflows WHERE repo_id = ?`, id); err != nil {
		return fmt.Errorf("delete workflows: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM task_mappings WHERE repo_id = ?`, id); err != nil {
		return fmt.Errorf("delete task mappings: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM fix_attempts WHERE session_id IN (SELECT id FROM sessions WHERE repo_id = ?)`, id); err != nil {
		return fmt.Errorf("delete attempts: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM agent_chats WHERE session_id IN (SELECT id FROM sessions WHERE repo_id = ?)`, id); err != nil {
		return fmt.Errorf("delete agent chats: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE cron_jobs SET last_run_session_id = NULL WHERE last_run_session_id IN (SELECT id FROM sessions WHERE repo_id = ?)`, id); err != nil {
		return fmt.Errorf("detach cron job last run sessions: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM cron_jobs WHERE repo_id = ?`, id); err != nil {
		return fmt.Errorf("delete cron jobs: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE repo_id = ?`, id); err != nil {
		return fmt.Errorf("delete sessions: %w", err)
	}

	res, err := tx.ExecContext(ctx, `DELETE FROM repos WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete repo: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.deleteKeyringSecrets(ctx, id)
	return nil
}

// scanRepo scans a repo from any scanner (*sql.Row or *sql.Rows), reporting
// whether the row's keys live in the keyring (has_keyring_secrets).
func scanRepo(s sqlutil.Scanner) (*models.Repo, bool, error) {
	var r models.Repo
	var hasKeyringSecrets int
	var createdAt, updatedAt string
	var canAutoMerge, canAutoMergeDependabot, canAutoRepair, canAutoRotate int
	var archiveSessionsAfterMerge int
	var canAutoDeleteBranches int
	var shouldKeepBranchesCurrent int
	var isTrusted int
	var mergeStrategy string
	err := s.Scan(&r.ID, &r.DisplayName, &r.LocalPath, &r.OriginURL,
		&r.DefaultBaseBranch, &r.WorktreeBaseDir, &r.SetupScript,
		&canAutoMerge, &canAutoMergeDependabot, &canAutoRepair, &canAutoRotate,
		&archiveSessionsAfterMerge,
		&canAutoDeleteBranches,
		&shouldKeepBranchesCurrent,
		&mergeStrategy,
		&r.LinearAPIKey,
		&r.SentryAPIKey, &r.SentryOrg,
		&isTrusted,
		&hasKeyringSecrets,
		&createdAt, &updatedAt)
	if err != nil {
		return nil, false, err
	}
	r.CanAutoMerge = canAutoMerge != 0
	r.CanAutoMergeDependabot = canAutoMergeDependabot != 0
	r.CanAutoRepair = canAutoRepair != 0
	r.CanAutoRotate = canAutoRotate != 0
	r.ShouldArchiveSessionsAfterMerge = archiveSessionsAfterMerge != 0
	r.CanAutoDeleteBranches = canAutoDeleteBranches != 0
	r.ShouldKeepBranchesCurrent = shouldKeepBranchesCurrent != 0
	r.IsTrusted = isTrusted != 0
	r.MergeStrategy = models.ParseMergeStrategy(mergeStrategy)
	r.CreatedAt = sqlutil.ParseTime(createdAt)
	r.UpdatedAt = sqlutil.ParseTime(updatedAt)
	return &r, hasKeyringSecrets != 0, nil
}
