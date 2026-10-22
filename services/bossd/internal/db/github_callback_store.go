package db

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/recurser/bossalib/githubcallback"
	"github.com/recurser/bossalib/sqlutil"
	"github.com/recurser/bossd/internal/db/sqlcgen"
	"github.com/recurser/bossd/internal/domain"
)

var _ GitHubCallbackStore = (*SQLiteGitHubCallbackStore)(nil)

// SQLiteGitHubCallbackStore implements GitHubCallbackStore using SQLite.
type SQLiteGitHubCallbackStore struct {
	db *sql.DB
}

// NewGitHubCallbackStore creates a new SQLite-backed GitHubCallbackStore.
func NewGitHubCallbackStore(db *sql.DB) *SQLiteGitHubCallbackStore {
	return &SQLiteGitHubCallbackStore{db: db}
}

// validGitHubCallbackTrigger reports whether t is a known trigger event. This
// store is the authority that rejects a registration, so it deliberately
// derives from githubcallback.ValidTriggers() — the same canonical list the CLI
// and MCP schemas validate against — rather than re-enumerating the vocabulary
// here. A hand-maintained third copy would let a newly added trigger be
// accepted by every other surface and rejected only at write time, with the
// whole suite still green. Membership is exact (no trimming or case folding) so
// the stored value is always a canonical trigger string.
func validGitHubCallbackTrigger(t domain.GitHubCallbackTrigger) bool {
	return slices.Contains(githubcallback.ValidTriggers(), t)
}

// mutuallyExclusiveGitHubCallbackTriggers records which trigger pairs cannot be
// satisfied by one and the same evaluation. It is the *instantaneous* relation,
// derived from callback.satisfiedTriggers: a pair belongs here only when no
// single (PRStatus, checks) observation can set both. It is deliberately not a
// statement about a PR's lifetime — checks that are green now can red later, so
// a lifetime reading would wrongly pair almost everything.
//
// Two consumers read this one table, with opposite polarity on an absent row:
//
//   - githubCallbackTriggersCoSatisfiable (grouping validation) treats an
//     unlisted pair as co-satisfiable and therefore REFUSES the grouping —
//     fail-closed.
//   - the cross-group conflict scan treats an unlisted pair as no conflict and
//     therefore emits NO warning — fail-open.
//
// So a trigger added to the vocabulary without rows here silently loses its
// warning *and* becomes ungroupable. Adding a trigger means adding its rows.
//
// Every entry must be symmetric; TestGitHubCallbackTriggerTableIsSymmetric
// pins that.
var mutuallyExclusiveGitHubCallbackTriggers = map[domain.GitHubCallbackTrigger]map[domain.GitHubCallbackTrigger]bool{
	// merged requires PRStateMerged, so it excludes every trigger that
	// requires the PR to be open or closed.
	domain.GitHubCallbackTriggerMerged: {
		domain.GitHubCallbackTriggerClosed:            true,
		domain.GitHubCallbackTriggerReadyForReview:    true,
		domain.GitHubCallbackTriggerChecksPassedReady: true,
	},
	// closed requires PRStateClosed, likewise.
	domain.GitHubCallbackTriggerClosed: {
		domain.GitHubCallbackTriggerMerged:            true,
		domain.GitHubCallbackTriggerReadyForReview:    true,
		domain.GitHubCallbackTriggerChecksPassedReady: true,
	},
	// checks_passed is set from the check verdict alone, with no PR-state
	// gate, so it is exclusive only with the opposite verdict. A closed or
	// merged PR whose checks are green satisfies checks_passed too — which is
	// why closed/merged are deliberately absent here.
	//
	// That absence is a deliberate departure from the change that introduced
	// this table, which listed checks_passed x closed and checks_passed x
	// merged as exclusive. They are not: callback.satisfiedTriggers sets
	// checks_passed from checkVerdict.IsGreen() alone, so one evaluation of a
	// closed (or merged) PR with green checks satisfies both. Listing them
	// would make the conflict scan warn about a pair that really can fire
	// together, and would permit grouping a pair that can co-fire — the two
	// failures this table exists to prevent.
	domain.GitHubCallbackTriggerChecksPassed: {
		domain.GitHubCallbackTriggerChecksFailed: true,
	},
	domain.GitHubCallbackTriggerChecksFailed: {
		domain.GitHubCallbackTriggerChecksPassed:      true,
		domain.GitHubCallbackTriggerChecksPassedReady: true,
	},
	// ready_for_review requires PRStateOpen && !Draft.
	domain.GitHubCallbackTriggerReadyForReview: {
		domain.GitHubCallbackTriggerMerged: true,
		domain.GitHubCallbackTriggerClosed: true,
	},
	// checks_passed_ready requires a green verdict AND PRStateOpen && !Draft,
	// so it carries both exclusions. It stays co-satisfiable with
	// ready_for_review and checks_passed, which it implies.
	domain.GitHubCallbackTriggerChecksPassedReady: {
		domain.GitHubCallbackTriggerChecksFailed: true,
		domain.GitHubCallbackTriggerMerged:       true,
		domain.GitHubCallbackTriggerClosed:       true,
	},
}

// githubCallbackTriggersCoSatisfiable drives the fail-closed grouping check in
// Create: a group may only hold pairs this reports false for.
//
// Growing the exclusion table above therefore RELAXES that check. Pairs that
// used to be refused with ErrGitHubCallbackInvalid and are now accepted into
// one group: merged x ready_for_review, merged x checks_passed_ready,
// closed x ready_for_review, closed x checks_passed_ready, and
// checks_failed x checks_passed_ready. Each really is instantaneously
// exclusive, so refusing them was the bug; the relaxation is recorded here
// rather than version-gated because it is strict — no registration that
// previously succeeded starts failing — and a success-to-error down-convert is
// not something the response-transform seam can express.
func githubCallbackTriggersCoSatisfiable(a, b domain.GitHubCallbackTrigger) bool {
	if a == b {
		return true
	}
	return !mutuallyExclusiveGitHubCallbackTriggers[a][b]
}

// Create inserts an active callback and, unless suppressed, reports whether an
// instantaneously mutually exclusive callback is already live for the same chat
// and PR under a different group. See GitHubCallbackStore.Create.
func (s *SQLiteGitHubCallbackStore) Create(ctx context.Context, params CreateGitHubCallbackParams) (*domain.GitHubCallback, *GitHubCallbackConflict, error) {
	owner := strings.ToLower(strings.TrimSpace(params.RepoOwner))
	name := strings.ToLower(strings.TrimSpace(params.RepoName))
	chatID := strings.TrimSpace(params.TargetChatID)

	if chatID == "" {
		return nil, nil, fmt.Errorf("%w: target chat id is required", ErrGitHubCallbackInvalid)
	}
	if owner == "" || name == "" {
		return nil, nil, fmt.Errorf("%w: repository owner and name are required", ErrGitHubCallbackInvalid)
	}
	if params.PRNumber <= 0 {
		return nil, nil, fmt.Errorf("%w: pr number must be positive, got %d", ErrGitHubCallbackInvalid, params.PRNumber)
	}
	if !validGitHubCallbackTrigger(params.Trigger) {
		return nil, nil, fmt.Errorf("%w: unknown trigger %q", ErrGitHubCallbackInvalid, params.Trigger)
	}
	if strings.TrimSpace(params.Message) == "" {
		return nil, nil, fmt.Errorf("%w: message is required", ErrGitHubCallbackInvalid)
	}

	now := time.Now().UTC()
	expiresAt := now.Add(GitHubCallbackDefaultExpiry)
	if params.ExpiresAt != nil {
		expiresAt = params.ExpiresAt.UTC()
		if !expiresAt.After(now) {
			return nil, nil, fmt.Errorf("%w: expiry must be in the future", ErrGitHubCallbackInvalid)
		}
		if expiresAt.Sub(now) > GitHubCallbackMaxExpiry {
			return nil, nil, fmt.Errorf("%w: expiry must be within %s", ErrGitHubCallbackInvalid, GitHubCallbackMaxExpiry)
		}
	}

	id, err := sqlutil.NewID()
	if err != nil {
		return nil, nil, fmt.Errorf("new github callback id: %w", err)
	}
	nowStr := sqlutil.FormatTime(now)

	var groupID string
	if params.GroupID != nil {
		groupID = strings.TrimSpace(*params.GroupID)
	}

	conn, err := beginImmediate(ctx, s.db, "github callback create")
	if err != nil {
		return nil, nil, err
	}
	committed := false
	defer closeImmediate(ctx, conn, &committed)

	if groupID != "" {
		rows, err := sqlcgen.New(conn).ListGitHubCallbackGroupTriggers(ctx, sqlcgen.ListGitHubCallbackGroupTriggersParams{
			GroupID:     optionalText(groupID),
			ActiveState: string(domain.GitHubCallbackStateActive),
			LeasedState: string(domain.GitHubCallbackStateLeased),
		})
		if err != nil {
			return nil, nil, fmt.Errorf("validate github callback group: %w", err)
		}
		for _, existing := range rows {
			existingTrigger := domain.GitHubCallbackTrigger(existing)
			if githubCallbackTriggersCoSatisfiable(existingTrigger, params.Trigger) {
				return nil, nil, fmt.Errorf("%w: group %q already has co-satisfiable trigger %q; cannot add %q", ErrGitHubCallbackInvalid, groupID, existingTrigger, params.Trigger)
			}
		}
	}

	conflict, err := findCrossGroupConflict(ctx, conn, params, chatID, owner, name, groupID, nowStr)
	if err != nil {
		return nil, nil, err
	}

	created, err := sqlcgen.New(conn).CreateGitHubCallback(ctx, sqlcgen.CreateGitHubCallbackParams{
		ID:                      id,
		GroupID:                 optionalText(groupID),
		TargetChatID:            chatID,
		RepoOwner:               owner,
		RepoName:                name,
		PrNumber:                int64(params.PRNumber),
		TriggerEvent:            string(params.Trigger),
		State:                   string(domain.GitHubCallbackStateActive),
		Message:                 params.Message,
		ShouldRequireTransition: params.ShouldRequireTransition,
		ExpiresAt:               sqlutil.FormatTime(expiresAt),
		CreatedAt:               nowStr,
		UpdatedAt:               nowStr,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("insert github callback: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, nil, fmt.Errorf("commit github callback create: %w", err)
	}
	committed = true
	return githubCallbackFromRow(created), conflict, nil
}

// findCrossGroupConflict reports the first live callback for the same chat and
// PR, in a *different* group, whose trigger cannot be satisfied by the same
// evaluation as params.Trigger — the shape where the winner cancels nothing and
// the loser sits armed until it expires.
//
// It must run on the caller's checked-out conn rather than s.List/s.Get: the
// pool may hold exactly one connection for an in-memory DB, so touching s.db
// while the write transaction is open deadlocks instead of failing (see
// beginImmediate in helpers.go).
//
// "Different group" is not `group_id != ?`. A NULL group_id is a group of one,
// so an ungrouped row is in a different group from every other row — including
// another ungrouped row. SQLite would evaluate `group_id != ?` to NULL (falsy)
// for those, silently skipping exactly the rows that need reporting, so the
// group predicate is built explicitly instead:
//
//   - new callback is grouped   -> match rows that are ungrouped or in another group
//   - new callback is ungrouped -> it is its own group of one, so every other
//     live row for this chat and PR is in a different group; no predicate at all
func findCrossGroupConflict(
	ctx context.Context,
	conn *sql.Conn,
	params CreateGitHubCallbackParams,
	chatID, owner, name string,
	groupID string,
	nowStr string,
) (*GitHubCallbackConflict, error) {
	if params.IndependentWatch {
		return nil, nil
	}

	// The expires_at > now guard matches SetLease and MarkDelivered: expiry
	// is swept lazily (list RPC / MarkExpired) and the create path sweeps
	// nothing, so an overdue-but-unreaped row is still active/leased here.
	// Without the guard the notice would report a dead row as a live conflict,
	// printing a *past* expiry while asserting both legs stay armed.
	// expires_at is stored in the fixed-width UTC layout, so the string compare
	// is chronological, matching MarkExpired's expires_at <= ? predicate.
	rows, err := sqlcgen.New(conn).ListGitHubCallbackCrossGroupConflicts(ctx, sqlcgen.ListGitHubCallbackCrossGroupConflictsParams{
		TargetChatID: chatID, RepoOwner: owner, RepoName: name, PrNumber: int64(params.PRNumber), GroupID: optionalText(groupID), ExpiresAt: nowStr,
	})
	if err != nil {
		return nil, fmt.Errorf("scan github callback cross-group conflicts: %w", err)
	}
	for _, row := range rows {
		trigger := domain.GitHubCallbackTrigger(row.TriggerEvent)
		if githubCallbackTriggersCoSatisfiable(trigger, params.Trigger) {
			continue
		}
		return &GitHubCallbackConflict{ID: row.ID, GroupID: row.GroupID, Trigger: trigger, ExpiresAt: sqlutil.ParseTime(row.ExpiresAt)}, nil
	}
	return nil, nil
}

func (s *SQLiteGitHubCallbackStore) Get(ctx context.Context, id string) (*domain.GitHubCallback, error) {
	row, err := sqlcgen.New(s.db).GetGitHubCallback(ctx, id)
	if err != nil {
		return nil, notFound(err)
	}
	return githubCallbackFromRow(row), nil
}

func (s *SQLiteGitHubCallbackStore) List(ctx context.Context, filter ListGitHubCallbacksFilter) ([]*domain.GitHubCallback, error) {
	arg := sqlcgen.ListGitHubCallbacksParams{
		TargetChatID: filter.TargetChatID,
	}
	if filter.RepoOwner != nil {
		v := strings.ToLower(strings.TrimSpace(*filter.RepoOwner))
		arg.RepoOwner = &v
	}
	if filter.RepoName != nil {
		v := strings.ToLower(strings.TrimSpace(*filter.RepoName))
		arg.RepoName = &v
	}
	if filter.PRNumber != nil {
		v := int64(*filter.PRNumber)
		arg.PrNumber = &v
	}
	if filter.Trigger != nil {
		v := string(*filter.Trigger)
		arg.TriggerEvent = &v
	}
	if filter.State != nil {
		v := string(*filter.State)
		arg.State = &v
	}
	rows, err := sqlcgen.New(s.db).ListGitHubCallbacks(ctx, arg)
	if err != nil {
		return nil, fmt.Errorf("list github callbacks: %w", err)
	}
	return mapRows(rows, githubCallbackFromRow), nil
}

func (s *SQLiteGitHubCallbackStore) Delete(ctx context.Context, id, expectTargetChatID string) (DeleteGitHubCallbackOutcome, error) {
	conn, err := beginImmediate(ctx, s.db, "github callback delete")
	if err != nil {
		return "", err
	}
	committed := false
	defer closeImmediate(ctx, conn, &committed)

	res, err := sqlcgen.New(conn).DeleteGitHubCallback(ctx, sqlcgen.DeleteGitHubCallbackParams{
		ID:           id,
		TargetChatID: expectTargetChatID,
	})
	if err != nil {
		return "", fmt.Errorf("delete github callback: %w", err)
	}
	if res > 0 {
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return "", fmt.Errorf("commit github callback delete: %w", err)
		}
		committed = true
		return DeleteGitHubCallbackOutcomeDeleted, nil
	}

	_, err = sqlcgen.New(conn).GetGitHubCallbackTargetChat(ctx, id)
	if err == sql.ErrNoRows {
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return "", fmt.Errorf("commit github callback not-found delete: %w", err)
		}
		committed = true
		return DeleteGitHubCallbackOutcomeNotFound, nil
	}
	if err != nil {
		return "", fmt.Errorf("select github callback after delete miss: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return "", fmt.Errorf("commit github callback not-owned delete: %w", err)
	}
	committed = true
	return DeleteGitHubCallbackOutcomeNotOwned, ErrGitHubCallbackNotOwned
}

func (s *SQLiteGitHubCallbackStore) MarkBaselineObserved(ctx context.Context, id string, now time.Time) error {
	nowStr := sqlutil.FormatTime(now)
	res, err := sqlcgen.New(s.db).MarkGitHubCallbackBaselineObserved(ctx, sqlcgen.MarkGitHubCallbackBaselineObservedParams{
		UpdatedAt: nowStr,
		ID:        id,
		State:     string(domain.GitHubCallbackStateActive),
	})
	if err != nil {
		return fmt.Errorf("observe github callback baseline: %w", err)
	}
	if res == 0 {
		if _, gerr := s.Get(ctx, id); gerr != nil {
			return gerr
		}
		return ErrGitHubCallbackTriggerConflict
	}
	return nil
}

// MarkUnreachable implements GitHubCallbackStore.
func (s *SQLiteGitHubCallbackStore) MarkUnreachable(ctx context.Context, id, event string, now time.Time) error {
	nowStr := sqlutil.FormatTime(now)
	res, err := sqlcgen.New(s.db).MarkGitHubCallbackUnreachable(ctx, sqlcgen.MarkGitHubCallbackUnreachableParams{
		State:         string(domain.GitHubCallbackStateCanceled),
		LastEvent:     &event,
		UpdatedAt:     nowStr,
		ID:            id,
		ExpectedState: string(domain.GitHubCallbackStateActive),
	})
	if err != nil {
		return fmt.Errorf("cancel unreachable github callback: %w", err)
	}
	if res == 0 {
		if _, gerr := s.Get(ctx, id); gerr != nil {
			return gerr
		}
		return ErrGitHubCallbackTriggerConflict
	}
	return nil
}

// MarkCanceled implements GitHubCallbackStore.
func (s *SQLiteGitHubCallbackStore) MarkCanceled(ctx context.Context, id, event string, now time.Time) error {
	nowStr := sqlutil.FormatTime(now)
	res, err := sqlcgen.New(s.db).MarkGitHubCallbackCanceled(ctx, sqlcgen.MarkGitHubCallbackCanceledParams{
		State:         string(domain.GitHubCallbackStateCanceled),
		LastEvent:     &event,
		UpdatedAt:     nowStr,
		ID:            id,
		ExpectedState: string(domain.GitHubCallbackStateTriggered),
	})
	if err != nil {
		return fmt.Errorf("cancel triggered github callback: %w", err)
	}
	if res == 0 {
		if _, gerr := s.Get(ctx, id); gerr != nil {
			return gerr
		}
		return ErrGitHubCallbackTriggerConflict
	}
	return nil
}

// ExpiredGitHubCallback is a callback made terminal by one expiry sweep.
// It contains only the bounded fields needed for terminal-outcome telemetry.
type ExpiredGitHubCallback struct {
	Trigger      domain.GitHubCallbackTrigger
	AttemptCount int
}

// MarkExpiredCallbacks expires overdue callbacks and returns only rows that
// transitioned in this statement, allowing callers to emit each outcome once.
func (s *SQLiteGitHubCallbackStore) MarkExpiredCallbacks(ctx context.Context, now time.Time) (int, []ExpiredGitHubCallback, error) {
	nowStr := sqlutil.FormatTime(now)
	rows, err := sqlcgen.New(s.db).MarkGitHubCallbacksExpired(ctx, sqlcgen.MarkGitHubCallbacksExpiredParams{
		State:          string(domain.GitHubCallbackStateExpired),
		UpdatedAt:      nowStr,
		ActiveState:    string(domain.GitHubCallbackStateActive),
		LeasedState:    string(domain.GitHubCallbackStateLeased),
		TriggeredState: string(domain.GitHubCallbackStateTriggered),
		ExpiresAt:      nowStr,
	})
	if err != nil {
		return 0, nil, fmt.Errorf("expire overdue github callbacks: %w", err)
	}
	expired := mapRows(rows, func(row sqlcgen.MarkGitHubCallbacksExpiredRow) ExpiredGitHubCallback {
		return ExpiredGitHubCallback{Trigger: domain.GitHubCallbackTrigger(row.TriggerEvent), AttemptCount: int(row.AttemptCount)}
	})
	return len(expired), expired, nil
}

func (s *SQLiteGitHubCallbackStore) MarkExpired(ctx context.Context, now time.Time) (int, error) {
	n, _, err := s.MarkExpiredCallbacks(ctx, now)
	return n, err
}

func (s *SQLiteGitHubCallbackStore) SetLease(ctx context.Context, id, owner string, now time.Time, leaseFor time.Duration) (*domain.GitHubCallback, error) {
	nowStr := sqlutil.FormatTime(now)
	deadline := sqlutil.FormatTime(now.Add(leaseFor))
	// Claimable when: not terminal (active/leased/triggered), not past expiry, the
	// retry backoff has elapsed, and the lease is free — unset, already ours, or
	// past its deadline (crash recovery). Promote active -> leased; keep
	// leased/triggered as-is so a recovered claim does not lose its triggered status.
	//
	// The expires_at > now guard keeps acquisition consistent with the terminal
	// expired invariant enforced in MarkGroupTriggered and MarkDelivered: expiry is only
	// swept lazily (list RPC / MarkExpired), so an overdue-but-unswept row is
	// still active/triggered here. Without this guard a delivery worker could lease
	// and send an expired callback before MarkDelivered rejects the state change.
	// expires_at is stored in the fixed-width UTC layout, so the string compare is
	// chronological, matching MarkExpired's expires_at <= ? predicate.
	res, err := sqlcgen.New(s.db).SetGitHubCallbackLease(ctx, sqlcgen.SetGitHubCallbackLeaseParams{
		ActiveState:          string(domain.GitHubCallbackStateActive),
		LeasedState:          string(domain.GitHubCallbackStateLeased),
		LeaseOwner:           &owner,
		LeaseDeadlineAt:      &deadline,
		UpdatedAt:            nowStr,
		ID:                   id,
		TriggeredState:       string(domain.GitHubCallbackStateTriggered),
		ExpiresAt:            nowStr,
		NextAttemptAt:        &nowStr,
		LeaseDeadlineAtCheck: &nowStr,
	})
	if err != nil {
		return nil, fmt.Errorf("acquire github callback lease: %w", err)
	}
	if res == 0 {
		// Distinguish an absent row from a live-but-unclaimable one.
		if _, gerr := s.Get(ctx, id); gerr != nil {
			return nil, gerr
		}
		return nil, ErrGitHubCallbackLeaseConflict
	}
	return s.Get(ctx, id)
}

func (s *SQLiteGitHubCallbackStore) MarkLeaseReleased(ctx context.Context, id, owner string, now time.Time) error {
	nowStr := sqlutil.FormatTime(now)
	res, err := sqlcgen.New(s.db).MarkGitHubCallbackLeaseReleased(ctx, sqlcgen.MarkGitHubCallbackLeaseReleasedParams{
		LeasedState: string(domain.GitHubCallbackStateLeased),
		ActiveState: string(domain.GitHubCallbackStateActive),
		UpdatedAt:   nowStr,
		ID:          id,
		LeaseOwner:  &owner,
	})
	if err != nil {
		return fmt.Errorf("release github callback lease: %w", err)
	}
	if res == 0 {
		if _, gerr := s.Get(ctx, id); gerr != nil {
			return gerr
		}
		return ErrGitHubCallbackLeaseConflict
	}
	return nil
}

func (s *SQLiteGitHubCallbackStore) MarkGroupTriggered(ctx context.Context, id, event string, now time.Time) (*domain.GitHubCallback, error) {
	nowStr := sqlutil.FormatTime(now)
	conn, err := beginImmediate(ctx, s.db, "github callback")
	if err != nil {
		return nil, err
	}
	committed := false
	defer closeImmediate(ctx, conn, &committed)

	current, err := sqlcgen.New(conn).GetGitHubCallbackTriggerState(ctx, id)
	if err != nil {
		return nil, notFound(err)
	}
	state, groupID, expiresAt := current.State, current.GroupID, current.ExpiresAt

	if state != string(domain.GitHubCallbackStateActive) && state != string(domain.GitHubCallbackStateLeased) {
		return nil, ErrGitHubCallbackTriggerConflict
	}

	// Reject overdue callbacks before mutating: expiry is only swept lazily (on
	// the list RPC / MarkExpired), so a row past expires_at can still be active
	// or leased here. Triggering it would fire a callback that should be terminally
	// expired and would cancel its siblings. Guarding on state alone is not enough.
	if !now.Before(sqlutil.ParseTime(expiresAt)) {
		return nil, ErrGitHubCallbackTriggerConflict
	}

	if err := sqlcgen.New(conn).MarkGitHubCallbackTriggered(ctx, sqlcgen.MarkGitHubCallbackTriggeredParams{
		State:       string(domain.GitHubCallbackStateTriggered),
		TriggeredAt: &nowStr,
		LastEvent:   &event,
		UpdatedAt:   nowStr,
		ID:          id,
	}); err != nil {
		return nil, fmt.Errorf("mark github callback triggered: %w", err)
	}

	if groupID != nil && *groupID != "" {
		if err := sqlcgen.New(conn).MarkGitHubCallbackSiblingsCanceled(ctx, sqlcgen.MarkGitHubCallbackSiblingsCanceledParams{
			State:       string(domain.GitHubCallbackStateCanceled),
			UpdatedAt:   nowStr,
			GroupID:     groupID,
			ID:          id,
			ActiveState: string(domain.GitHubCallbackStateActive),
			LeasedState: string(domain.GitHubCallbackStateLeased),
		}); err != nil {
			return nil, fmt.Errorf("cancel github callback siblings: %w", err)
		}
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, fmt.Errorf("commit github callback trigger: %w", err)
	}
	committed = true
	// Read back on the same connection: it is still checked out (the deferred
	// closeImmediate has not run yet), so calling s.Get here would deadlock on a
	// single-connection (in-memory) pool. Post-COMMIT the conn is in autocommit.
	row, err := sqlcgen.New(conn).GetGitHubCallback(ctx, id)
	if err != nil {
		return nil, notFound(err)
	}
	return githubCallbackFromRow(row), nil
}

func (s *SQLiteGitHubCallbackStore) MarkDelivered(ctx context.Context, id, owner string, now time.Time) error {
	nowStr := sqlutil.FormatTime(now)
	// The expires_at > now guard keeps delivery consistent with the terminal
	// expired state: if expiry passes while the worker still holds the lease,
	// the row must not be marked delivered (it belongs to MarkExpired's sweep).
	// expires_at is stored in the fixed-width UTC layout, so the string compare is
	// chronological, matching MarkExpired's expires_at <= ? predicate.
	res, err := sqlcgen.New(s.db).MarkGitHubCallbackDelivered(ctx, sqlcgen.MarkGitHubCallbackDeliveredParams{
		State:         string(domain.GitHubCallbackStateDelivered),
		DeliveredAt:   &nowStr,
		UpdatedAt:     nowStr,
		ID:            id,
		ExpectedState: string(domain.GitHubCallbackStateTriggered),
		LeaseOwner:    &owner,
		ExpiresAt:     nowStr,
	})
	if err != nil {
		return fmt.Errorf("mark github callback delivered: %w", err)
	}
	if res == 0 {
		if _, gerr := s.Get(ctx, id); gerr != nil {
			return gerr
		}
		return ErrGitHubCallbackLeaseConflict
	}
	return nil
}

func (s *SQLiteGitHubCallbackStore) SetRetry(ctx context.Context, id, owner string, params ScheduleGitHubCallbackRetryParams) error {
	now := sqlutil.TimeNow()
	attemptIncrement := 1
	if params.PreserveAttemptCount {
		attemptIncrement = 0
	}
	var lastEventVal *string
	if params.LastEvent != "" {
		lastEventVal = &params.LastEvent
	}
	res, err := sqlcgen.New(s.db).SetGitHubCallbackRetry(ctx, sqlcgen.SetGitHubCallbackRetryParams{
		AttemptIncrement: int64(attemptIncrement),
		NextAttemptAt:    timeText(&params.NextAttemptAt),
		LastError:        &params.LastError,
		LastEvent:        lastEventVal,
		UpdatedAt:        now,
		ID:               id,
		LeaseOwner:       &owner,
	})
	if err != nil {
		return fmt.Errorf("schedule github callback retry: %w", err)
	}
	if res == 0 {
		if _, gerr := s.Get(ctx, id); gerr != nil {
			return gerr
		}
		return ErrGitHubCallbackLeaseConflict
	}
	return nil
}

func githubCallbackFromRow(row sqlcgen.GithubCallback) *domain.GitHubCallback {
	return &domain.GitHubCallback{
		ID: row.ID, GroupID: row.GroupID, TargetChatID: row.TargetChatID,
		RepoOwner: row.RepoOwner, RepoName: row.RepoName, PRNumber: int(row.PrNumber),
		Trigger: domain.GitHubCallbackTrigger(row.TriggerEvent), State: domain.GitHubCallbackState(row.State), Message: row.Message,
		ShouldRequireTransition: row.ShouldRequireTransition, HasObservedBaseline: row.HasObservedBaseline,
		LeaseOwner: row.LeaseOwner, LeaseDeadlineAt: optionalTextTime(row.LeaseDeadlineAt), AttemptCount: int(row.AttemptCount),
		NextAttemptAt: optionalTextTime(row.NextAttemptAt), TriggeredAt: optionalTextTime(row.TriggeredAt), DeliveredAt: optionalTextTime(row.DeliveredAt),
		LastError: row.LastError, LastEvent: row.LastEvent, ExpiresAt: sqlutil.ParseTime(row.ExpiresAt),
		CreatedAt: sqlutil.ParseTime(row.CreatedAt), UpdatedAt: sqlutil.ParseTime(row.UpdatedAt),
	}
}
