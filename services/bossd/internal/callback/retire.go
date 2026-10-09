package callback

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
	"github.com/rs/zerolog"
)

type retireStore interface {
	List(context.Context, db.ListGithubCallbacksFilter) ([]*models.GithubCallback, error)
	TriggerGroup(context.Context, string, string, time.Time) (*models.GithubCallback, error)
	CancelUnreachable(context.Context, string, string, time.Time) error
	CancelTriggered(context.Context, string, string, time.Time) error
}
type retirementChatLister interface {
	ListBySession(context.Context, string) ([]*models.AgentChat, error)
	GetByAgentSessionID(context.Context, string) (*models.AgentChat, error)
}
type retirementSessionGetter interface {
	Get(context.Context, string) (*models.Session, error)
}
type retirementRepoGetter interface {
	Get(context.Context, string) (*models.Repo, error)
}

// Retirer reconciles a terminal session's own watches. The transition observer
// and evaluator safety net share this DB-only path; delivery checks the same scope.
type Retirer struct {
	store      retireStore
	chats      retirementChatLister
	sessions   retirementSessionGetter
	repos      retirementRepoGetter
	recomputer db.SessionRecomputer
	now        func() time.Time
	logger     zerolog.Logger
}

// NewRetirer constructs the retirement observer and delivery gate. Recomputer
// and clock are optional; all stores are required.
func NewRetirer(store retireStore, chats retirementChatLister, sessions retirementSessionGetter, repos retirementRepoGetter, recomputer db.SessionRecomputer, now func() time.Time, logger zerolog.Logger) *Retirer {
	if now == nil {
		now = time.Now
	}
	return &Retirer{store: store, chats: chats, sessions: sessions, repos: repos, recomputer: recomputer, now: now, logger: logger}
}

func terminalRetirement(state machine.State) bool {
	return state == machine.Merged || state == machine.Closed
}

func (r *Retirer) ownPR(ctx context.Context, session *models.Session) (string, error) {
	if session.PRNumber == nil {
		return "", nil
	}
	repo, err := r.repos.Get(ctx, session.RepoID)
	if err != nil {
		return "", err
	}
	return vcs.GitHubNWO(repo.OriginURL), nil
}
func callbackOnOwnPR(cb *models.GithubCallback, session *models.Session, nwo string) bool {
	return nwo != "" && session.PRNumber != nil && cb.PRNumber == *session.PRNumber && strings.EqualFold(repoPath(cb.RepoOwner, cb.RepoName), nwo)
}
func benignRetirementRace(err error) bool {
	return errors.Is(err, db.ErrGithubCallbackTriggerConflict) || errors.Is(err, sql.ErrNoRows)
}

// ReconcileSession fires the intended own-PR merge wake before retiring the
// other active watches on that PR. Closed sessions retire all active/triggered
// watches; leases are left for the delivery gate on their next attempt.
func (r *Retirer) ReconcileSession(ctx context.Context, sessionID string) (changed int, err error) {
	session, err := r.sessions.Get(ctx, sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("get retirement session %s: %w", sessionID, err)
	}
	if !terminalRetirement(session.State) {
		return 0, nil
	}
	nwo := ""
	if session.State == machine.Merged {
		nwo, err = r.ownPR(ctx, session)
		if err != nil {
			return 0, fmt.Errorf("get retirement repo: %w", err)
		}
		if nwo == "" {
			return 0, nil
		}
	}
	chats, err := r.chats.ListBySession(ctx, sessionID)
	if err != nil {
		return 0, fmt.Errorf("list retirement chats: %w", err)
	}
	// Recompute even when a later operation fails after earlier writes succeeded.
	defer func() {
		if changed > 0 && r.recomputer != nil {
			err = errors.Join(err, r.recomputer.Recompute(ctx, sessionID))
		}
	}()
	var callbacks []*models.GithubCallback
	for _, chat := range chats {
		cbs, listErr := r.store.List(ctx, db.ListGithubCallbacksFilter{TargetChatID: &chat.AgentSessionID})
		if listErr != nil {
			return changed, fmt.Errorf("list retirement callbacks: %w", listErr)
		}
		callbacks = append(callbacks, cbs...)
	}
	now := r.now()
	// Two passes are necessary: list ordering must never let a group sibling
	// cancel an intended merge wake before it has fired.
	if session.State == machine.Merged {
		for _, cb := range callbacks {
			if cb.State != models.GithubCallbackStateActive || cb.Trigger != models.GithubCallbackTriggerMerged || !callbackOnOwnPR(cb, session, nwo) || (cb.ShouldRequireTransition && !cb.HasObservedBaseline) {
				continue
			}
			_, triggerErr := r.store.TriggerGroup(ctx, cb.ID, "merged", now)
			if benignRetirementRace(triggerErr) {
				continue
			}
			if triggerErr != nil {
				return changed, r.callbackError(cb.ID, triggerErr)
			}
			changed++
		}
	}
	for _, cb := range callbacks {
		var cancelErr error
		event := "retired: session closed"
		if session.State == machine.Merged {
			if !callbackOnOwnPR(cb, session, nwo) || (cb.Trigger == models.GithubCallbackTriggerMerged && (!cb.ShouldRequireTransition || cb.HasObservedBaseline)) {
				continue
			}
			event = "retired: session merged"
		}
		switch cb.State {
		case models.GithubCallbackStateActive:
			cancelErr = r.store.CancelUnreachable(ctx, cb.ID, event, now)
		case models.GithubCallbackStateTriggered:
			if session.State != machine.Closed {
				continue
			}
			cancelErr = r.store.CancelTriggered(ctx, cb.ID, event, now)
		default:
			continue
		}
		if benignRetirementRace(cancelErr) {
			continue
		}
		if cancelErr != nil {
			return changed, r.callbackError(cb.ID, cancelErr)
		}
		changed++
	}
	return changed, nil
}
func (r *Retirer) callbackError(id string, err error) error {
	r.logger.Warn().Err(err).Str("callback_id", id).Msg("callback retirement failed")
	return fmt.Errorf("retire callback %s: %w", id, err)
}

// OnSessionState implements db.SessionTransitionObserver. Errors are logged
// here because the transition seam intentionally discards observer failures.
func (r *Retirer) OnSessionState(ctx context.Context, sessionID string, to machine.State) error {
	if !terminalRetirement(to) {
		return nil
	}
	_, err := r.ReconcileSession(ctx, sessionID)
	if err != nil {
		r.logger.Warn().Err(err).Str("session_id", sessionID).Msg("callback retirement observer failed")
	}
	return err
}

// retiresMergedBaseline leaves an unobserved own-PR merge watch for the
// post-pass to cancel. Observing it first would make reconciliation fire the
// initially satisfied watch in the very same evaluation.
func (r *Retirer) retiresMergedBaseline(ctx context.Context, cb *models.GithubCallback) (bool, error) {
	chat, err := r.chats.GetByAgentSessionID(ctx, cb.TargetChatID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	session, err := r.sessions.Get(ctx, chat.SessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if session.State != machine.Merged {
		return false, nil
	}
	nwo, err := r.ownPR(ctx, session)
	if err != nil {
		return false, err
	}
	return callbackOnOwnPR(cb, session, nwo), nil
}

// DeliveryVerdict drops stale terminal-session deliveries and missing targets.
// A transient lookup failure preserves today's delivery/retry behavior.
func (r *Retirer) DeliveryVerdict(ctx context.Context, cb *models.GithubCallback) (deliver bool, reason string) {
	chat, err := r.chats.GetByAgentSessionID(ctx, cb.TargetChatID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "target chat missing"
	}
	if err != nil {
		return true, ""
	}
	session, err := r.sessions.Get(ctx, chat.SessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "target session missing"
	}
	if err != nil {
		return true, ""
	}
	if session.State == machine.Closed {
		return false, "session closed"
	}
	if session.State == machine.Merged && cb.Trigger != models.GithubCallbackTriggerMerged {
		nwo, err := r.ownPR(ctx, session)
		if err == nil && callbackOnOwnPR(cb, session, nwo) {
			return false, "session merged"
		}
	}
	return true, ""
}

// DropDelivery owns stale delivery cancellation and its cached waiting projection.
func (r *Retirer) DropDelivery(ctx context.Context, cb *models.GithubCallback, reason string) error {
	if err := r.store.CancelTriggered(ctx, cb.ID, "dropped: "+reason, r.now()); err != nil {
		return err
	}
	if r.recomputer == nil {
		return nil
	}
	chat, err := r.chats.GetByAgentSessionID(ctx, cb.TargetChatID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return r.recomputer.Recompute(ctx, chat.SessionID)
}

// reconcilePR reconciles each owning session before provider reads and after
// the trigger pass. Closed owners settle independently of the watched PR.
func (r *Retirer) reconcilePR(ctx context.Context, callbacks []*models.GithubCallback) error {
	seen := make(map[string]bool)
	var errs []error
	for _, cb := range callbacks {
		chat, err := r.chats.GetByAgentSessionID(ctx, cb.TargetChatID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("get callback owner: %w", err))
			continue
		}
		if seen[chat.SessionID] {
			continue
		}
		seen[chat.SessionID] = true
		if _, err := r.ReconcileSession(ctx, chat.SessionID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
