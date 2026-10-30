package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/recurser/bossalib/contract/cloud/sqlutil"
	"github.com/recurser/bossd/internal/db/sqlcgen"
	"github.com/recurser/bossd/internal/domain"
	sqlite "modernc.org/sqlite"
	sqlitelib "modernc.org/sqlite/lib"
)

var _ AgentAccountStore = (*SQLiteAgentAccountStore)(nil)

// ErrAgentAccountExists is returned by Create when the accounts table's
// UNIQUE(provider, label) constraint rejects the insert — the caller is
// re-adding a label that already exists for that provider. This is a normal
// client conflict, not a daemon failure; the server maps it to
// connect.CodeAlreadyExists.
var ErrAgentAccountExists = domain.ErrAgentAccountExists

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

// SQLiteAgentAccountStore implements AgentAccountStore using SQLite. It persists
// account-registry metadata only — credential blobs live in the OS keyring
// (services/bossd/internal/adapter/accountcred), keyed by Account.ID.
type SQLiteAgentAccountStore struct {
	db *sql.DB
}

// NewAgentAccountStore creates a new SQLite-backed AgentAccountStore.
func NewAgentAccountStore(db *sql.DB) *SQLiteAgentAccountStore {
	return &SQLiteAgentAccountStore{db: db}
}

func (s *SQLiteAgentAccountStore) Create(ctx context.Context, params CreateAgentAccountParams) (*domain.AgentAccount, error) {
	id, err := sqlutil.NewID()
	if err != nil {
		return nil, fmt.Errorf("new account id: %w", err)
	}
	now := sqlutil.TimeNow()
	row, err := sqlcgen.New(s.db).CreateAgentAccount(ctx, sqlcgen.CreateAgentAccountParams{
		ID:            id,
		Provider:      string(params.Provider),
		Label:         params.Label,
		Status:        string(domain.AgentAccountStatusActive),
		Priority:      int64(params.Priority),
		Health:        string(domain.AgentAccountHealthOK),
		Tier:          params.Tier,
		AllowedModels: encodeAllowedModels(params.AllowedModels),
		CreatedAt:     now,
		UpdatedAt:     now,
	})
	if err != nil {
		if isUniqueConstraintErr(err) {
			return nil, fmt.Errorf("%w: %s/%s", ErrAgentAccountExists, params.Provider, params.Label)
		}
		return nil, fmt.Errorf("insert account: %w", err)
	}
	return agentAccountFromRow(row), nil
}

func (s *SQLiteAgentAccountStore) Get(ctx context.Context, id string) (*domain.AgentAccount, error) {
	row, err := sqlcgen.New(s.db).GetAgentAccount(ctx, id)
	if err != nil {
		return nil, notFound(err)
	}
	return agentAccountFromRow(row), nil
}

func (s *SQLiteAgentAccountStore) List(ctx context.Context) ([]*domain.AgentAccount, error) {
	rows, err := sqlcgen.New(s.db).ListAgentAccounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	return mapRows(rows, agentAccountFromRow), nil
}

func (s *SQLiteAgentAccountStore) ListByProvider(ctx context.Context, p domain.AgentAccountProvider) ([]*domain.AgentAccount, error) {
	rows, err := sqlcgen.New(s.db).ListAgentAccountsByProvider(ctx, string(p))
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	return mapRows(rows, agentAccountFromRow), nil
}

func (s *SQLiteAgentAccountStore) Update(ctx context.Context, id string, params UpdateAgentAccountParams) (*domain.AgentAccount, error) {
	arg := sqlcgen.UpdateAgentAccountParams{
		ID:               id,
		Label:            params.Label,
		Tier:             params.Tier,
		UpdatedAt:        sqlutil.TimeNow(),
		SetCooldownUntil: params.CooldownUntil != nil,
		SetLastUsedAt:    params.LastUsedAt != nil,
	}
	if params.Status != nil {
		v := string(*params.Status)
		arg.Status = &v
	}
	if params.Health != nil {
		v := string(*params.Health)
		arg.Health = &v
	}
	if params.Priority != nil {
		v := int64(*params.Priority)
		arg.Priority = &v
	}
	if params.CooldownUntil != nil {
		arg.CooldownUntil = timeText(*params.CooldownUntil)
	}
	if params.LastUsedAt != nil {
		arg.LastUsedAt = timeText(*params.LastUsedAt)
	}
	if params.AllowedModels != nil {
		v := encodeAllowedModels(*params.AllowedModels)
		arg.AllowedModels = &v
	}
	row, err := sqlcgen.New(s.db).UpdateAgentAccount(ctx, arg)
	if err != nil {
		return nil, fmt.Errorf("update account: %w", notFound(err))
	}
	return agentAccountFromRow(row), nil
}

func (s *SQLiteAgentAccountStore) Delete(ctx context.Context, id string) error {
	n, err := sqlcgen.New(s.db).DeleteAgentAccount(ctx, id)
	if err != nil {
		return fmt.Errorf("delete account: %w", err)
	}
	if n == 0 {
		return notFound(sql.ErrNoRows)
	}
	return nil
}

// RecordTestResult updates only the last-test bookkeeping columns for a row.
// okAt nil clears last_test_ok_at to NULL (a failing test); a non-nil okAt
// records a passing test. testErr is written verbatim ("" clears the error).
// It leaves all other metadata untouched. Returns domain.ErrNotFound when the row
// does not exist.
func (s *SQLiteAgentAccountStore) RecordTestResult(ctx context.Context, id string, okAt *time.Time, testErr string) error {
	n, err := sqlcgen.New(s.db).RecordAgentAccountTestResult(ctx, sqlcgen.RecordAgentAccountTestResultParams{ID: id, LastTestOkAt: timeText(okAt), LastTestError: testErr, UpdatedAt: sqlutil.TimeNow()})
	if err != nil {
		return fmt.Errorf("record account test result: %w", err)
	}
	if n == 0 {
		return notFound(sql.ErrNoRows)
	}
	return nil
}

// MarkSuspended fails an account's health and records a legible reason in
// one write: it sets health=failed, writes reason to last_test_error, and clears
// last_test_ok_at (the account can no longer serve requests). It deliberately
// leaves status untouched — recovering a suspended account (e.g. after fixing
// billing) is an explicit operator action, mirroring any other health=failed
// account. Returns domain.ErrNotFound when the row does not exist.
func (s *SQLiteAgentAccountStore) MarkSuspended(ctx context.Context, id string, reason string) error {
	n, err := sqlcgen.New(s.db).MarkAgentAccountSuspended(ctx, sqlcgen.MarkAgentAccountSuspendedParams{ID: id,
		Health:        string(domain.AgentAccountHealthFailed),
		LastTestError: reason,
		UpdatedAt:     sqlutil.TimeNow()})
	if err != nil {
		return fmt.Errorf("mark account suspended: %w", err)
	}
	if n == 0 {
		return notFound(sql.ErrNoRows)
	}
	return nil
}

// InjectionFailureReasonPrefix marks a last_test_error that was written by the
// credential-injection path rather than by an operator-run account test. It is
// the ONLY thing that makes the failure self-clearing: MarkInjectionFailureCleared
// scopes its guarded UPDATE to this prefix, so a successful materialize can
// never erase a genuine RecordTestResult failure. Changing the string changes
// that contract — a previously recorded failure would stop clearing itself.
const InjectionFailureReasonPrefix = domain.InjectionFailureReasonPrefix

// RecordInjectionFailure fails an account's health because its credentials
// could not be materialized for a spawn. Without it the spawn silently falls
// back to the agent CLI's ambient login and nothing durable says so (BOS-973);
// with it the account row that every operator surface already renders — the TUI
// Accounts list, `boss account ls`, and the rotation-eligibility hint — reports
// the downgrade. reason is the materialize error string, prefixed with
// InjectionFailureReasonPrefix; it carries filesystem paths, never credential
// material, and callers must not widen what they pass. Returns domain.ErrNotFound
// when the row does not exist.
//
// Unlike MarkSuspended this write is NON-DESTRUCTIVE, in two ways, and
// both exist so MarkInjectionFailureCleared can put the row back exactly as it found
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
//     leave the row failed with a reason MarkInjectionFailureCleared can never match,
//     and the account would stay out of rotation permanently — reintroducing the
//     stuck-account class this whole change exists to remove. A reason on a
//     healthy row is history, not protection.
func (s *SQLiteAgentAccountStore) RecordInjectionFailure(ctx context.Context, id string, reason string) error {
	n, err := sqlcgen.New(s.db).RecordAgentAccountInjectionFailure(ctx, sqlcgen.RecordAgentAccountInjectionFailureParams{ID: id,
		FailedHealth: string(domain.AgentAccountHealthFailed),
		Prefix:       InjectionFailureReasonPrefix + "%",
		Reason:       InjectionFailureReasonPrefix + reason,
		UpdatedAt:    sqlutil.TimeNow()})
	if err != nil {
		return fmt.Errorf("record account injection failure: %w", err)
	}
	if n == 0 {
		return notFound(sql.ErrNoRows)
	}
	return nil
}

// MarkInjectionFailureCleared withdraws a previously recorded injection failure after
// a successful materialize, restoring health=ok and emptying last_test_error.
// The UPDATE is guarded on the prefix so it is a strict no-op against a row
// whose failure came from anywhere else — an operator's `boss account test`
// failure or a confirmed suspension both survive it — and the guard lives in
// the WHERE clause rather than a read-modify-write, so there is no race. It
// runs on every successful spawn and therefore matches no row in the normal
// case: an unmatched row (including an unknown id) is success, NOT
// domain.ErrNotFound.
//
// It restores nothing beyond health and the reason because
// RecordInjectionFailure destroys nothing else: it preserves last_test_ok_at
// and never overwrites a foreign last_test_error, so the pair leaves the row
// exactly as it found it. That is what makes "self-clearing" mean
// self-clearing — and it is why a row this record never wrote to (health failed
// for another reason) is untouched at both ends.
func (s *SQLiteAgentAccountStore) MarkInjectionFailureCleared(ctx context.Context, id string) error {
	err := sqlcgen.New(s.db).MarkAgentAccountInjectionFailureCleared(ctx, sqlcgen.MarkAgentAccountInjectionFailureClearedParams{ID: id,
		HealthyHealth: string(domain.AgentAccountHealthOK),
		FailedHealth:  string(domain.AgentAccountHealthFailed),
		Prefix:        InjectionFailureReasonPrefix + "%",
		UpdatedAt:     sqlutil.TimeNow()})
	if err != nil {
		return fmt.Errorf("clear account injection failure: %w", err)
	}
	return nil
}

// IsSelfClearingInjectionFailure reports whether a row's failed health is the
// SELF-CLEARING kind RecordInjectionFailure writes and MarkInjectionFailureCleared
// withdraws — health=failed carrying a last_test_error under
// InjectionFailureReasonPrefix. It is the in-memory twin of
// MarkInjectionFailureCleared's WHERE clause, so a caller asking "will the next
// successful materialize heal this row?" gets the same answer the UPDATE would
// give, and the two cannot drift.
//
// It is deliberately the NARROWEST possible reading of failed health. An
// operator's `boss account test` failure, a MarkSuspended row, and any
// non-prefixed reason all report false — exactly the rows MarkInjectionFailureCleared
// refuses to touch — so a caller that exempts this class exempts only failures
// a local materialization outage produced and a successful materialization
// erases. It says nothing about status, cooldown, or auth-check state; a caller
// still has to evaluate those itself.
func IsSelfClearingInjectionFailure(a *domain.AgentAccount) bool {
	return domain.IsSelfClearingInjectionFailure(a)
}

// RecordUsageProbe overwrites only the cached usage-snapshot metadata columns
// for a row. It never stores credential material. Returns domain.ErrNotFound when
// the row does not exist.
func (s *SQLiteAgentAccountStore) RecordUsageProbe(ctx context.Context, id string, snap domain.UsageSnapshot) error {
	now := sqlutil.TimeNow()
	fetchedAt := snap.FetchedAt
	if fetchedAt == nil {
		t := sqlutil.ParseTime(now)
		fetchedAt = &t
	}
	modelWindows, err := json.Marshal(snap.ModelWindows)
	if err != nil {
		return fmt.Errorf("encode account usage model windows: %w", err)
	}
	encodedWindows := ""
	if len(snap.ModelWindows) != 0 {
		encodedWindows = string(modelWindows)
	}
	n, err := sqlcgen.New(s.db).RecordAgentAccountUsageProbe(ctx, sqlcgen.RecordAgentAccountUsageProbeParams{ID: id,
		UsageUtil5h:       snap.Util5h,
		UsageUtil7d:       snap.Util7d,
		UsageReset5h:      timeText(snap.Reset5h),
		UsageReset7d:      timeText(snap.Reset7d),
		UsageStatus:       snap.Status,
		UsagePlanTier:     snap.PlanTier,
		UsageFetchedAt:    timeText(fetchedAt),
		UsageModelWindows: encodedWindows,
		UpdatedAt:         now})
	if err != nil {
		return fmt.Errorf("record account usage probe: %w", err)
	}
	if n == 0 {
		return notFound(sql.ErrNoRows)
	}
	return nil
}

// RecordAuthCheck atomically stores the redacted result of one daemon-owned
// credential verification (BOS-1141). It writes ONLY the four auth_check_*
// columns plus updated_at: usage_* and last_test_* are deliberately untouched,
// so a scheduled verification can never clobber a usage probe or a manual
// TestAgentAccount result. Nothing written here is derived from credential bytes —
// check.Outcome and check.FailureClass are closed-set classification tokens.
func (s *SQLiteAgentAccountStore) RecordAuthCheck(ctx context.Context, id string, check domain.AuthCheck) error {
	now := sqlutil.TimeNow()
	checkedAt := check.CheckedAt
	if checkedAt == nil {
		t := sqlutil.ParseTime(now)
		checkedAt = &t
	}
	n, err := sqlcgen.New(s.db).RecordAgentAccountAuthCheck(ctx, sqlcgen.RecordAgentAccountAuthCheckParams{ID: id,
		AuthCheckedAt:         timeText(checkedAt),
		AuthCheckOutcome:      string(check.Outcome),
		AuthCheckFailureClass: check.FailureClass,
		AuthCheckNextRetryAt:  timeText(check.NextRetryAt),
		UpdatedAt:             now})
	if err != nil {
		return fmt.Errorf("record account auth check: %w", err)
	}
	if n == 0 {
		return notFound(sql.ErrNoRows)
	}
	return nil
}

// MarkAuthCheckCleared withdraws a row's durable credential-verification state after
// its credential is replaced (BOS-1141). It writes ONLY the four auth_check_*
// columns plus updated_at, leaving usage_* and last_test_* exactly as it found
// them, and is a no-op returning nil when no state is recorded.
//
// auth_checked_at is set to NULL rather than to now. That is load-bearing and
// is why this cannot be expressed as RecordAuthCheck with a zero AuthCheck:
// RecordAuthCheck defaults a nil CheckedAt to the current time, which would
// satisfy the maintainer's staleness gate and leave the replacement credential
// unverified for a full interval. A NULL auth_checked_at is immediately due.
//
// Returns domain.ErrNotFound when the account does not exist.
func (s *SQLiteAgentAccountStore) MarkAuthCheckCleared(ctx context.Context, id string) error {
	n, err := sqlcgen.New(s.db).MarkAgentAccountAuthCheckCleared(ctx, sqlcgen.MarkAgentAccountAuthCheckClearedParams{ID: id, UpdatedAt: sqlutil.TimeNow()})
	if err != nil {
		return fmt.Errorf("clear account auth check: %w", err)
	}
	if n == 0 {
		return notFound(sql.ErrNoRows)
	}
	return nil
}

const accountSelectSQL = `SELECT id, provider, label, status, priority, health,
	cooldown_until, last_used_at, tier, allowed_models,
	last_test_ok_at, last_test_error,
	usage_util_5h, usage_util_7d, usage_reset_5h, usage_reset_7d,
	usage_status, usage_plan_tier, usage_fetched_at, usage_model_windows,
	auth_checked_at, auth_check_outcome, auth_check_failure_class, auth_check_next_retry_at,
	created_at, updated_at, identity_kind, identity_digest, synced_agent_account_id, sync_version, synced_credential_version
	FROM agent_accounts`

func collectAccounts(rows *sql.Rows) ([]*domain.AgentAccount, error) {
	defer func() { _ = rows.Close() }()
	var accounts []*domain.AgentAccount
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, a)
	}
	return accounts, rows.Err()
}

func scanAccount(scanner sqlutil.Scanner) (*domain.AgentAccount, error) {
	var row sqlcgen.AgentAccount
	err := scanner.Scan(
		&row.ID, &row.Provider, &row.Label, &row.Status, &row.Priority, &row.Health, &row.CooldownUntil, &row.LastUsedAt, &row.Tier, &row.AllowedModels, &row.LastTestOkAt, &row.LastTestError, &row.UsageUtil5h, &row.UsageUtil7d, &row.UsageReset5h, &row.UsageReset7d, &row.UsageStatus, &row.UsagePlanTier, &row.UsageFetchedAt, &row.UsageModelWindows, &row.AuthCheckedAt, &row.AuthCheckOutcome, &row.AuthCheckFailureClass, &row.AuthCheckNextRetryAt, &row.CreatedAt, &row.UpdatedAt, &row.IdentityKind, &row.IdentityDigest, &row.SyncedAgentAccountID, &row.SyncVersion, &row.SyncedCredentialVersion,
	)
	if err != nil {
		return nil, notFound(err)
	}
	return agentAccountFromRow(row), nil
}

func agentAccountFromRow(row sqlcgen.AgentAccount) *domain.AgentAccount {
	a := &domain.AgentAccount{
		ID:                      row.ID,
		SyncedAgentAccountID:    row.SyncedAgentAccountID,
		SyncedCredentialVersion: row.SyncedCredentialVersion,
		SyncVersion:             row.SyncVersion,
		Identity:                domain.AgentAccountIdentity{Kind: domain.AgentAccountIdentityKind(row.IdentityKind), Digest: row.IdentityDigest},
		Provider:                domain.AgentAccountProvider(row.Provider),
		Label:                   row.Label,
		Status:                  domain.AgentAccountStatus(row.Status),
		Priority:                int(row.Priority),
		Health:                  domain.AgentAccountHealth(row.Health),
		CooldownUntil:           optionalTextTime(row.CooldownUntil),
		LastUsedAt:              optionalTextTime(row.LastUsedAt),
		Tier:                    row.Tier,
		AllowedModels:           decodeAllowedModels(row.AllowedModels),
		LastTestOkAt:            optionalTextTime(row.LastTestOkAt),
		LastTestError:           row.LastTestError,
		CreatedAt:               textTime(row.CreatedAt),
		UpdatedAt:               textTime(row.UpdatedAt),
		AuthCheck: domain.AuthCheck{
			CheckedAt:    optionalTextTime(row.AuthCheckedAt),
			Outcome:      domain.AuthCheckOutcome(row.AuthCheckOutcome),
			FailureClass: row.AuthCheckFailureClass,
			NextRetryAt:  optionalTextTime(row.AuthCheckNextRetryAt),
		},
	}
	if fetchedAt := optionalTextTime(row.UsageFetchedAt); fetchedAt != nil {
		a.Usage = &domain.UsageSnapshot{
			Util5h:       row.UsageUtil5h,
			Util7d:       row.UsageUtil7d,
			Status:       row.UsageStatus,
			PlanTier:     row.UsagePlanTier,
			FetchedAt:    fetchedAt,
			ModelWindows: decodeUsageModelWindows(row.UsageModelWindows),
			Reset5h:      optionalTextTime(row.UsageReset5h),
			Reset7d:      optionalTextTime(row.UsageReset7d),
		}
	}
	return a
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

// SetIdentity updates only identity metadata and its modification timestamp.
func (s *SQLiteAgentAccountStore) SetIdentity(ctx context.Context, id string, identity domain.AgentAccountIdentity) error {
	n, err := sqlcgen.New(s.db).SetAgentAccountIdentity(ctx, sqlcgen.SetAgentAccountIdentityParams{
		ID: id, IdentityKind: string(identity.Kind), IdentityDigest: identity.Digest, UpdatedAt: sqlutil.TimeNow(),
	})
	if err != nil {
		return fmt.Errorf("set account identity: %w", err)
	}
	if n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func decodeUsageModelWindows(value string) []domain.UsageModelWindow {
	if value == "" {
		return nil
	}
	var windows []domain.UsageModelWindow
	if err := json.Unmarshal([]byte(value), &windows); err != nil {
		return nil
	}
	for _, window := range windows {
		if window.ModelName == "" {
			return nil
		}
	}
	return windows
}
