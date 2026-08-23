package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sqlutil"
	sqlite "modernc.org/sqlite"
	sqlitelib "modernc.org/sqlite/lib"
)

var _ AccountStore = (*SQLiteAccountStore)(nil)

// ErrAccountExists is returned by Create when the accounts table's
// UNIQUE(provider, label) constraint rejects the insert — the caller is
// re-adding a label that already exists for that provider. This is a normal
// client conflict, not a daemon failure; the server maps it to
// connect.CodeAlreadyExists.
var ErrAccountExists = errors.New("account with this provider and label already exists")

// isUniqueConstraintErr reports whether err is a SQLite UNIQUE constraint
// violation. modernc.org/sqlite surfaces these as *sqlite.Error with the
// extended result code SQLITE_CONSTRAINT_UNIQUE (or the primary
// SQLITE_CONSTRAINT).
func isUniqueConstraintErr(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	code := sqliteErr.Code()
	return code == sqlitelib.SQLITE_CONSTRAINT_UNIQUE || code == sqlitelib.SQLITE_CONSTRAINT
}

// SQLiteAccountStore implements AccountStore using SQLite. It persists
// account-registry metadata only — credential blobs live in the OS keyring
// (services/bossd/internal/accountcred), keyed by Account.ID.
type SQLiteAccountStore struct {
	db *sql.DB
}

// NewAccountStore creates a new SQLite-backed AccountStore.
func NewAccountStore(db *sql.DB) *SQLiteAccountStore {
	return &SQLiteAccountStore{db: db}
}

func (s *SQLiteAccountStore) Create(ctx context.Context, params CreateAccountParams) (*models.Account, error) {
	id, err := sqlutil.NewID()
	if err != nil {
		return nil, fmt.Errorf("new account id: %w", err)
	}
	now := sqlutil.TimeNow()
	// Insert every column explicitly, including status='active' and health='ok',
	// rather than relying on DB defaults — matches the store convention of not
	// depending on column DEFAULTs. cooldown_until and last_used_at are left NULL.
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO accounts (id, provider, label, status, priority, health, tier, allowed_models, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, string(params.Provider), params.Label,
		string(models.AccountStatusActive), params.Priority, string(models.AccountHealthOK),
		params.Tier, encodeAllowedModels(params.AllowedModels), now, now,
	)
	if err != nil {
		if isUniqueConstraintErr(err) {
			return nil, fmt.Errorf("%w: %s/%s", ErrAccountExists, params.Provider, params.Label)
		}
		return nil, fmt.Errorf("insert account: %w", err)
	}
	return s.Get(ctx, id)
}

func (s *SQLiteAccountStore) Get(ctx context.Context, id string) (*models.Account, error) {
	row := s.db.QueryRowContext(ctx, accountSelectSQL+" WHERE id = ?", id)
	return scanAccount(row)
}

func (s *SQLiteAccountStore) List(ctx context.Context) ([]*models.Account, error) {
	rows, err := s.db.QueryContext(ctx,
		accountSelectSQL+" ORDER BY provider ASC, priority ASC, created_at ASC")
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	return collectAccounts(rows)
}

func (s *SQLiteAccountStore) ListByProvider(ctx context.Context, p models.AccountProvider) ([]*models.Account, error) {
	rows, err := s.db.QueryContext(ctx,
		accountSelectSQL+" WHERE provider = ? ORDER BY provider ASC, priority ASC, created_at ASC", string(p))
	if err != nil {
		return nil, fmt.Errorf("list accounts by provider: %w", err)
	}
	return collectAccounts(rows)
}

func (s *SQLiteAccountStore) Update(ctx context.Context, id string, params UpdateAccountParams) (*models.Account, error) {
	now := sqlutil.TimeNow()
	sets := []string{"updated_at = ?"}
	args := []any{now}

	if params.Label != nil {
		sets = append(sets, "label = ?")
		args = append(args, *params.Label)
	}
	if params.Status != nil {
		sets = append(sets, "status = ?")
		args = append(args, string(*params.Status))
	}
	if params.Priority != nil {
		sets = append(sets, "priority = ?")
		args = append(args, *params.Priority)
	}
	if params.Health != nil {
		sets = append(sets, "health = ?")
		args = append(args, string(*params.Health))
	}
	if params.CooldownUntil != nil {
		if *params.CooldownUntil == nil {
			sets = append(sets, "cooldown_until = NULL")
		} else {
			sets = append(sets, "cooldown_until = ?")
			args = append(args, (*params.CooldownUntil).UTC().Format("2006-01-02T15:04:05.000Z"))
		}
	}
	if params.LastUsedAt != nil {
		if *params.LastUsedAt == nil {
			sets = append(sets, "last_used_at = NULL")
		} else {
			sets = append(sets, "last_used_at = ?")
			args = append(args, (*params.LastUsedAt).UTC().Format("2006-01-02T15:04:05.000Z"))
		}
	}
	if params.Tier != nil {
		sets = append(sets, "tier = ?")
		args = append(args, *params.Tier)
	}
	if params.AllowedModels != nil {
		sets = append(sets, "allowed_models = ?")
		args = append(args, encodeAllowedModels(*params.AllowedModels))
	}

	args = append(args, id)
	// #nosec G202 -- dynamic UPDATE...SET; concatenated fragments are code-literal `col = ?` set-clauses; every value is bound via ?, not user text
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	query := "UPDATE accounts SET " + strings.Join(sets, ", ") + " WHERE id = ?"
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("update account: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, sql.ErrNoRows
	}
	return s.Get(ctx, id)
}

func (s *SQLiteAccountStore) Delete(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM accounts WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete account: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// RecordTestResult updates only the last-test bookkeeping columns for a row.
// okAt nil clears last_test_ok_at to NULL (a failing test); a non-nil okAt
// records a passing test. testErr is written verbatim ("" clears the error).
// It leaves all other metadata untouched. Returns sql.ErrNoRows when the row
// does not exist.
func (s *SQLiteAccountStore) RecordTestResult(ctx context.Context, id string, okAt *time.Time, testErr string) error {
	now := sqlutil.TimeNow()
	var res sql.Result
	var err error
	if okAt == nil {
		res, err = s.db.ExecContext(ctx,
			`UPDATE accounts SET last_test_ok_at = NULL, last_test_error = ?, updated_at = ? WHERE id = ?`,
			testErr, now, id)
	} else {
		res, err = s.db.ExecContext(ctx,
			`UPDATE accounts SET last_test_ok_at = ?, last_test_error = ?, updated_at = ? WHERE id = ?`,
			okAt.UTC().Format("2006-01-02T15:04:05.000Z"), testErr, now, id)
	}
	if err != nil {
		return fmt.Errorf("record account test result: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// MarkAccountSuspended fails an account's health and records a legible reason in
// one write: it sets health=failed, writes reason to last_test_error, and clears
// last_test_ok_at (the account can no longer serve requests). It deliberately
// leaves status untouched — recovering a suspended account (e.g. after fixing
// billing) is an explicit operator action, mirroring any other health=failed
// account. Returns sql.ErrNoRows when the row does not exist.
func (s *SQLiteAccountStore) MarkAccountSuspended(ctx context.Context, id string, reason string) error {
	now := sqlutil.TimeNow()
	res, err := s.db.ExecContext(ctx,
		`UPDATE accounts
		 SET health = ?, last_test_error = ?, last_test_ok_at = NULL, updated_at = ?
		 WHERE id = ?`,
		string(models.AccountHealthFailed), reason, now, id)
	if err != nil {
		return fmt.Errorf("mark account suspended: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// InjectionFailureReasonPrefix marks a last_test_error that was written by the
// credential-injection path rather than by an operator-run account test. It is
// the ONLY thing that makes the failure self-clearing: ClearInjectionFailure
// scopes its guarded UPDATE to this prefix, so a successful materialize can
// never erase a genuine RecordTestResult failure. Changing the string changes
// that contract — a previously recorded failure would stop clearing itself.
const InjectionFailureReasonPrefix = "credential injection failed: "

// RecordInjectionFailure fails an account's health because its credentials
// could not be materialized for a spawn. Without it the spawn silently falls
// back to the agent CLI's ambient login and nothing durable says so (BOS-973);
// with it the account row that every operator surface already renders — the TUI
// Accounts list, `boss account ls`, and the rotation-eligibility hint — reports
// the downgrade. reason is the materialize error string, prefixed with
// InjectionFailureReasonPrefix; it carries filesystem paths, never credential
// material, and callers must not widen what they pass. Returns sql.ErrNoRows
// when the row does not exist.
//
// Unlike MarkAccountSuspended this write is NON-DESTRUCTIVE, in two ways, and
// both exist so ClearInjectionFailure can put the row back exactly as it found
// it. Anything this record destroys is destroyed permanently, because the
// withdrawal has nothing to restore it from.
//
//   - last_test_ok_at is PRESERVED. A suspension means the credential itself is
//     dead, so the last-verified-good timestamp is a stale claim; an injection
//     failure is a LOCAL materialization failure that says nothing about the
//     credential, which usually still tests fine. Clearing it would leave a
//     self-healed account reporting health=ok while every surface rendered it
//     as never tested.
//
//   - A last_test_error written by anything else is PRESERVED, and health still
//     goes to failed — but ONLY while that failure is still LIVE, i.e. the row's
//     pre-update health is already failed. Overwriting a live one would convert a
//     protected failure into a self-clearing one: an operator's failing
//     `boss account test` result, or a confirmed suspension, would be replaced by
//     this prefixed reason, and the next successful materialize would then clear
//     it and return a known-bad-credential account to rotation looking healthy.
//     Materialization is a local filesystem operation and never re-validates the
//     credential, so it may not be read as evidence that a live failure is
//     resolved.
//
//     The health precondition is load-bearing, not defensive. restoreAccountHealth
//     (services/bossd/internal/server/account.go) sets health=ok WITHOUT clearing
//     last_test_error, so a healthy row routinely carries a stale, non-prefixed
//     reason from a failure that is already resolved. Preserving that string would
//     leave the row failed with a reason ClearInjectionFailure can never match,
//     and the account would stay out of rotation permanently — reintroducing the
//     stuck-account class this whole change exists to remove. A reason on a
//     healthy row is history, not protection.
func (s *SQLiteAccountStore) RecordInjectionFailure(ctx context.Context, id string, reason string) error {
	now := sqlutil.TimeNow()
	res, err := s.db.ExecContext(ctx,
		// SQLite evaluates every SET expression against the ORIGINAL row, so the
		// `health = ?` inside the CASE reads the PRE-update health.
		`UPDATE accounts
		 SET health = ?,
		     last_test_error = CASE
		       WHEN health = ? AND last_test_error <> '' AND last_test_error NOT LIKE ?
		         THEN last_test_error
		       ELSE ?
		     END,
		     updated_at = ?
		 WHERE id = ?`,
		string(models.AccountHealthFailed),
		string(models.AccountHealthFailed), InjectionFailureReasonPrefix+"%",
		InjectionFailureReasonPrefix+reason,
		now, id)
	if err != nil {
		return fmt.Errorf("record account injection failure: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// sql.ErrNoRows travels unwrapped: call sites compare it with ==.
		return sql.ErrNoRows
	}
	return nil
}

// ClearInjectionFailure withdraws a previously recorded injection failure after
// a successful materialize, restoring health=ok and emptying last_test_error.
// The UPDATE is guarded on the prefix so it is a strict no-op against a row
// whose failure came from anywhere else — an operator's `boss account test`
// failure or a confirmed suspension both survive it — and the guard lives in
// the WHERE clause rather than a read-modify-write, so there is no race. It
// runs on every successful spawn and therefore matches no row in the normal
// case: an unmatched row (including an unknown id) is success, NOT
// sql.ErrNoRows.
//
// It restores nothing beyond health and the reason because
// RecordInjectionFailure destroys nothing else: it preserves last_test_ok_at
// and never overwrites a foreign last_test_error, so the pair leaves the row
// exactly as it found it. That is what makes "self-clearing" mean
// self-clearing — and it is why a row this record never wrote to (health failed
// for another reason) is untouched at both ends.
func (s *SQLiteAccountStore) ClearInjectionFailure(ctx context.Context, id string) error {
	now := sqlutil.TimeNow()
	_, err := s.db.ExecContext(ctx,
		`UPDATE accounts
		 SET health = ?, last_test_error = '', updated_at = ?
		 WHERE id = ? AND health = ? AND last_test_error LIKE ?`,
		string(models.AccountHealthOK), now, id,
		string(models.AccountHealthFailed), InjectionFailureReasonPrefix+"%")
	if err != nil {
		return fmt.Errorf("clear account injection failure: %w", err)
	}
	return nil
}

// RecordUsageProbe overwrites only the cached usage-snapshot metadata columns
// for a row. It never stores credential material. Returns sql.ErrNoRows when
// the row does not exist.
func (s *SQLiteAccountStore) RecordUsageProbe(ctx context.Context, id string, snap models.UsageSnapshot) error {
	now := sqlutil.TimeNow()
	fetchedAt := snap.FetchedAt
	if fetchedAt == nil {
		t := sqlutil.ParseTime(now)
		fetchedAt = &t
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE accounts
		 SET usage_util_5h = ?,
		     usage_util_7d = ?,
		     usage_reset_5h = ?,
		     usage_reset_7d = ?,
		     usage_status = ?,
		     usage_plan_tier = ?,
		     usage_fetched_at = ?,
		     updated_at = ?
		 WHERE id = ?`,
		snap.Util5h,
		snap.Util7d,
		formatNullableTime(snap.Reset5h),
		formatNullableTime(snap.Reset7d),
		snap.Status,
		snap.PlanTier,
		fetchedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		now,
		id,
	)
	if err != nil {
		return fmt.Errorf("record account usage probe: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func formatNullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

const accountSelectSQL = `SELECT id, provider, label, status, priority, health,
	cooldown_until, last_used_at, tier, allowed_models,
	last_test_ok_at, last_test_error,
	usage_util_5h, usage_util_7d, usage_reset_5h, usage_reset_7d,
	usage_status, usage_plan_tier, usage_fetched_at,
	created_at, updated_at
	FROM accounts`

func collectAccounts(rows *sql.Rows) ([]*models.Account, error) {
	defer func() { _ = rows.Close() }()
	var accounts []*models.Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, a)
	}
	return accounts, rows.Err()
}

func scanAccount(s sqlutil.Scanner) (*models.Account, error) {
	var a models.Account
	var providerStr, statusStr, healthStr, allowedModelsStr string
	var cooldownUntil, lastUsedAt, lastTestOkAt sql.NullString
	var usageReset5h, usageReset7d, usageFetchedAt sql.NullString
	var usageStatus, usagePlanTier string
	var usageUtil5h, usageUtil7d float64
	var createdAt, updatedAt string
	err := s.Scan(
		&a.ID, &providerStr, &a.Label, &statusStr, &a.Priority, &healthStr,
		&cooldownUntil, &lastUsedAt, &a.Tier, &allowedModelsStr,
		&lastTestOkAt, &a.LastTestError,
		&usageUtil5h, &usageUtil7d, &usageReset5h, &usageReset7d,
		&usageStatus, &usagePlanTier, &usageFetchedAt,
		&createdAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}
	a.Provider = models.AccountProvider(providerStr)
	a.Status = models.AccountStatus(statusStr)
	a.Health = models.AccountHealth(healthStr)
	a.AllowedModels = decodeAllowedModels(allowedModelsStr)
	if cooldownUntil.Valid {
		t := sqlutil.ParseTime(cooldownUntil.String)
		if !t.IsZero() {
			a.CooldownUntil = &t
		}
	}
	if lastUsedAt.Valid {
		t := sqlutil.ParseTime(lastUsedAt.String)
		if !t.IsZero() {
			a.LastUsedAt = &t
		}
	}
	if lastTestOkAt.Valid {
		t := sqlutil.ParseTime(lastTestOkAt.String)
		if !t.IsZero() {
			a.LastTestOkAt = &t
		}
	}
	if usageFetchedAt.Valid {
		fetchedAt := sqlutil.ParseTime(usageFetchedAt.String)
		if !fetchedAt.IsZero() {
			usage := &models.UsageSnapshot{
				Util5h:    usageUtil5h,
				Util7d:    usageUtil7d,
				Status:    usageStatus,
				PlanTier:  usagePlanTier,
				FetchedAt: &fetchedAt,
			}
			if usageReset5h.Valid {
				t := sqlutil.ParseTime(usageReset5h.String)
				if !t.IsZero() {
					usage.Reset5h = &t
				}
			}
			if usageReset7d.Valid {
				t := sqlutil.ParseTime(usageReset7d.String)
				if !t.IsZero() {
					usage.Reset7d = &t
				}
			}
			a.Usage = usage
		}
	}
	a.CreatedAt = sqlutil.ParseTime(createdAt)
	a.UpdatedAt = sqlutil.ParseTime(updatedAt)
	return &a, nil
}

func encodeAllowedModels(list []string) string {
	if len(list) == 0 {
		return "" // canonical "unspecified"; column default is '' not '[]'
	}
	b, err := json.Marshal(list)
	if err != nil {
		return "" // metadata-only; never fail a write over affinity hints
	}
	return string(b)
}

func decodeAllowedModels(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}
