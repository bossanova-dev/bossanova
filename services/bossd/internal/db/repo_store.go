package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/recurser/bossalib/contract/cloud/sqlutil"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db/sqlcgen"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/domain/keyedgate"
)

// ErrAmbiguousOrigin is returned by GetByOriginURL when the canonical-URL
// fallback finds more than one repo whose stored origin maps to the same
// canonical web URL. Webhook routing must fail loud here rather than pick
// an arbitrary repo and silently refresh the wrong sessions.
var ErrAmbiguousOrigin = domain.ErrAmbiguousOrigin

var _ RepoStore = (*SQLiteRepoStore)(nil)

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
	secrets RepoSecretVault
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
func WithRepoSecrets(secrets RepoSecretVault) RepoStoreOption {
	return func(s *SQLiteRepoStore) {
		if secrets != nil {
			s.secrets = secrets
		}
	}
}

// NewRepoStore creates a new SQLite-backed RepoStore.
func NewRepoStore(db *sql.DB, opts ...RepoStoreOption) *SQLiteRepoStore {
	s := &SQLiteRepoStore{
		db:             db,
		secrets:        unavailableRepoSecretVault{},
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

func (s *SQLiteRepoStore) Create(ctx context.Context, params CreateRepoParams) (*domain.Repo, error) {
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
func (s *SQLiteRepoStore) create(ctx context.Context, params CreateRepoParams) (*domain.Repo, bool, error) {
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
	row, err := sqlcgen.New(conn).CreateRepo(ctx, sqlcgen.CreateRepoParams{
		ID: id, DisplayName: params.DisplayName, LocalPath: params.LocalPath, OriginUrl: params.OriginURL,
		DefaultBaseBranch: params.DefaultBaseBranch, WorktreeBaseDir: params.WorktreeBaseDir, SetupScript: params.SetupScript,
		IsTrusted: params.IsTrusted, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		return nil, false, fmt.Errorf("insert repo: %w", err)
	}
	repo := repoFromRow(row)
	hasKeyringSecrets := row.HasKeyringSecrets
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return nil, false, fmt.Errorf("commit repo create: %w", err)
	}
	committed = true
	return repo, hasKeyringSecrets, nil
}

func (s *SQLiteRepoStore) Get(ctx context.Context, id string) (*domain.Repo, error) {
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
func getRow(ctx context.Context, q sqlcgen.DBTX, id string) (*domain.Repo, bool, error) {
	row, err := sqlcgen.New(q).GetRepo(ctx, id)
	if err != nil {
		return nil, false, notFound(err)
	}
	return repoFromRow(row), row.HasKeyringSecrets, nil
}

func (s *SQLiteRepoStore) GetByLocalPath(ctx context.Context, localPath string) (*domain.Repo, error) {
	row, err := sqlcgen.New(s.db).GetRepoByLocalPath(ctx, localPath)
	return s.hydrateRow(ctx, row, err)
}

func (s *SQLiteRepoStore) GetByOriginURL(ctx context.Context, originURL string) (*domain.Repo, error) {
	row, queryErr := sqlcgen.New(s.db).GetRepoByOriginURL(ctx, originURL)
	repo, err := s.hydrateRow(ctx, row, queryErr)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return nil, err
	}

	targetWebURL := vcs.NormalizeRepoURL(originURL)
	if targetWebURL == "" {
		if err == nil {
			return repo, nil
		}
		return nil, notFound(sql.ErrNoRows)
	}

	repos, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	var match *domain.Repo
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
		return nil, notFound(sql.ErrNoRows)
	}
	return match, nil
}

func (s *SQLiteRepoStore) List(ctx context.Context) ([]*domain.Repo, error) {
	return s.list(ctx, s.db)
}

func (s *SQLiteRepoStore) list(ctx context.Context, q sqlcgen.DBTX) ([]*domain.Repo, error) {
	return s.listRows(ctx, q, true)
}

// listRows lists every repo, hydrating keyring-held keys only when hydrate is
// set; the canonical-origin check needs no keys and must not touch the keyring.
func (s *SQLiteRepoStore) listRows(ctx context.Context, q sqlcgen.DBTX, hydrate bool) ([]*domain.Repo, error) {
	rows, err := sqlcgen.New(q).ListRepos(ctx)
	if err != nil {
		return nil, fmt.Errorf("list repos: %w", err)
	}
	var repos []*domain.Repo
	var inKeyring []*domain.Repo
	for _, row := range rows {
		r := repoFromRow(row)
		repos = append(repos, r)
		if row.HasKeyringSecrets {
			inKeyring = append(inKeyring, r)
		}
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

func (s *SQLiteRepoStore) Update(ctx context.Context, id string, params UpdateRepoParams) (*domain.Repo, error) {
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
func (s *SQLiteRepoStore) updateRow(ctx context.Context, id string, params UpdateRepoParams) (*domain.Repo, bool, error) {
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
// For a keyed update (keys.setKeys set) a COMMIT error is returned as an
// *uncertainCommitError carrying the row's key state before and after the
// UPDATE, so updateRowWithKeys can tell from a fresh read whether the commit
// landed before it compensates the keyring. It must make that read only after
// this returns: the connection is released here, and an in-memory database has
// only the one.
func (s *SQLiteRepoStore) updateInTx(ctx context.Context, id string, params UpdateRepoParams, keys keyClauses) (*domain.Repo, bool, error) {
	conn, err := beginImmediate(ctx, s.db, "repo")
	if err != nil {
		return nil, false, err
	}
	committed := false
	defer closeImmediate(ctx, conn, &committed)

	if err := s.checkUpdateGuards(ctx, conn, id, params); err != nil {
		return nil, false, err
	}
	keyed := keys.setKeys
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
func (s *SQLiteRepoStore) checkUpdateGuards(ctx context.Context, q sqlcgen.DBTX, id string, params UpdateRepoParams) error {
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
// round-trip drift). Returns domain.ErrNotFound when the row is gone and
// ErrStaleRepoUpdate on a token mismatch; in either case the caller does not
// commit, so no field is changed.
//
// Limitation: the token is millisecond-granular, so two writes landing in the
// same millisecond produce identical updated_at strings and a stale reader's
// token could still match — a narrow lost-update window. This is acceptable for
// human-paced repo-settings edits; a monotonic version column would close it if
// finer concurrency guarantees are ever needed.
func (s *SQLiteRepoStore) checkExpectedUpdatedAt(ctx context.Context, q sqlcgen.DBTX, id string, expected time.Time) error {
	row, err := sqlcgen.New(q).GetRepoKeyState(ctx, id)
	if err != nil {
		return fmt.Errorf("read repo updated_at: %w", notFound(err))
	}
	stored := row.UpdatedAt
	if stored != sqlutil.FormatTime(expected) {
		return ErrStaleRepoUpdate
	}
	return nil
}

// update writes params' non-key fields plus the key column clauses the key
// phase resolved; it never reads params.LinearAPIKey / SentryAPIKey itself. It
// returns the updated row unhydrated (see getRow).
func (s *SQLiteRepoStore) update(ctx context.Context, q sqlcgen.DBTX, id string, params UpdateRepoParams, keys keyClauses) (*domain.Repo, bool, error) {
	arg := sqlcgen.UpdateRepoParams{
		ID: id, UpdatedAt: nextUpdatedAt(sqlutil.TimeNow(), keys.updatedAfter),
		DisplayName: params.DisplayName, OriginUrl: params.OriginURL, DefaultBaseBranch: params.DefaultBaseBranch,
		WorktreeBaseDir: params.WorktreeBaseDir, SetSetupScript: params.SetupScript != nil,
		CanAutoMerge: params.CanAutoMerge, CanAutoMergeDependabot: params.CanAutoMergeDependabot,
		CanAutoRepair: params.CanAutoRepair, CanAutoRotate: params.CanAutoRotate,
		ShouldArchiveSessionsAfterMerge: params.ShouldArchiveSessionsAfterMerge,
		CanAutoDeleteBranches:           params.CanAutoDeleteBranches, ShouldKeepBranchesCurrent: params.ShouldKeepBranchesCurrent,
		SentryOrg: params.SentryOrg, IsTrusted: params.IsTrusted,
		SetKeys: keys.setKeys, LinearApiKey: keys.linearAPIKey, SentryApiKey: keys.sentryAPIKey, HasKeyringSecrets: keys.hasKeyringSecrets,
	}
	if params.SetupScript != nil {
		arg.SetupScript = *params.SetupScript
	}
	if params.MergeStrategy != nil {
		value := string(*params.MergeStrategy)
		arg.MergeStrategy = &value
	}
	row, err := sqlcgen.New(q).UpdateRepo(ctx, arg)
	if err != nil {
		return nil, false, fmt.Errorf("update repo: %w", notFound(err))
	}
	if keys.requestsScrub {
		if err := sqlcgen.New(q).RecordRepoSecretScrub(ctx); err != nil {
			return nil, false, fmt.Errorf("request secret scrub: %w", err)
		}
	}
	return repoFromRow(row), row.HasKeyringSecrets, nil
}

func (s *SQLiteRepoStore) ensureUniqueCanonicalOrigin(ctx context.Context, q sqlcgen.DBTX, originURL string, excludeID string) error {
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

	q := sqlcgen.New(tx)
	if err := q.DeleteRepoCheckSnapshots(ctx, id); err != nil {
		return fmt.Errorf("delete check snapshots: %w", err)
	}
	if err := q.DeleteRepoWorkflows(ctx, id); err != nil {
		return fmt.Errorf("delete workflows: %w", err)
	}
	if err := q.DeleteRepoTaskMappings(ctx, id); err != nil {
		return fmt.Errorf("delete task mappings: %w", err)
	}
	if err := q.DeleteRepoFixAttempts(ctx, id); err != nil {
		return fmt.Errorf("delete attempts: %w", err)
	}
	if err := q.DeleteRepoAgentChats(ctx, id); err != nil {
		return fmt.Errorf("delete agent chats: %w", err)
	}
	if err := q.SetRepoCronJobsLastRunSessionNull(ctx, id); err != nil {
		return fmt.Errorf("detach cron job last run sessions: %w", err)
	}
	if err := q.DeleteRepoCronJobs(ctx, id); err != nil {
		return fmt.Errorf("delete cron jobs: %w", err)
	}
	if err := q.DeleteRepoSessions(ctx, id); err != nil {
		return fmt.Errorf("delete sessions: %w", err)
	}
	n, err := q.DeleteRepo(ctx, id)
	if err != nil {
		return fmt.Errorf("delete repo: %w", err)
	}
	if n == 0 {
		return notFound(sql.ErrNoRows)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.deleteKeyringSecrets(ctx, id)
	return nil
}

// repoFromRow maps the persisted row without reading the keyring.
func repoFromRow(row sqlcgen.Repo) *domain.Repo {
	return &domain.Repo{
		ID: row.ID, DisplayName: row.DisplayName, LocalPath: row.LocalPath, OriginURL: row.OriginUrl,
		DefaultBaseBranch: row.DefaultBaseBranch, WorktreeBaseDir: row.WorktreeBaseDir, SetupScript: row.SetupScript,
		CanAutoMerge: row.CanAutoMerge, CanAutoMergeDependabot: row.CanAutoMergeDependabot,
		CanAutoRepair: row.CanAutoRepair, CanAutoRotate: row.CanAutoRotate,
		ShouldArchiveSessionsAfterMerge: row.ShouldArchiveSessionsAfterMerge, CanAutoDeleteBranches: row.CanAutoDeleteBranches,
		ShouldKeepBranchesCurrent: row.ShouldKeepBranchesCurrent, MergeStrategy: models.ParseMergeStrategy(row.MergeStrategy),
		LinearAPIKey: row.LinearApiKey, SentryAPIKey: row.SentryApiKey, SentryOrg: row.SentryOrg, IsTrusted: row.IsTrusted,
		CreatedAt: textTime(row.CreatedAt), UpdatedAt: textTime(row.UpdatedAt),
	}
}
