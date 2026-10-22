package callback

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/domain"
	"github.com/rs/zerolog"
)

type retireStore interface {
	List(context.Context, db.ListGitHubCallbacksFilter) ([]*models.GitHubCallback, error)
	MarkGroupTriggered(context.Context, string, string, time.Time) (*models.GitHubCallback, error)
	MarkUnreachable(context.Context, string, string, time.Time) error
	MarkCanceled(context.Context, string, string, time.Time) error
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

type sessionRecomputer interface {
	Recompute(context.Context, string) error
}

// Retirer reconciles a terminal session's own watches. The transition observer
// and evaluator safety net share this DB-only path; delivery checks the same scope.
type Retirer struct {
	store      retireStore
	chats      retirementChatLister
	sessions   retirementSessionGetter
	repos      retirementRepoGetter
	recomputer sessionRecomputer
	now        func() time.Time
	logger     zerolog.Logger
}

// NewRetirer constructs the retirement observer and delivery gate. Recomputer
// and clock are optional; all stores are required.
func NewRetirer(store retireStore, chats retirementChatLister, sessions retirementSessionGetter, repos retirementRepoGetter, recomputer sessionRecomputer, now func() time.Time, logger zerolog.Logger) *Retirer {
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
func callbackOnOwnPR(cb *models.GitHubCallback, session *models.Session, nwo string) bool {
	return nwo != "" && session.PRNumber != nil && cb.PRNumber == *session.PRNumber && strings.EqualFold(repoPath(cb.RepoOwner, cb.RepoName), nwo)
}
func benignRetirementRace(err error) bool {
	return errors.Is(err, db.ErrGitHubCallbackTriggerConflict) || errors.Is(err, domain.ErrNotFound)
}

// ReconcileSession fires the intended own-PR merge wake before retiring the
// other active watches on that PR. Closed sessions retire all active/triggered
// watches; leases are left for the delivery gate on their next attempt.
func (r *Retirer) ReconcileSession(ctx context.Context, sessionID string) (changed int, err error) {
	session, err := r.sessions.Get(ctx, sessionID)
	if errors.Is(err, domain.ErrNotFound) {
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
	var callbacks []*models.GitHubCallback
	for _, chat := range chats {
		cbs, listErr := r.store.List(ctx, db.ListGitHubCallbacksFilter{TargetChatID: &chat.AgentSessionID})
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
			if cb.State != models.GitHubCallbackStateActive || cb.Trigger != models.GitHubCallbackTriggerMerged || !callbackOnOwnPR(cb, session, nwo) || (cb.ShouldRequireTransition && !cb.HasObservedBaseline) {
				continue
			}
			_, triggerErr := r.store.MarkGroupTriggered(ctx, cb.ID, "merged", now)
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
			if !callbackOnOwnPR(cb, session, nwo) || (cb.Trigger == models.GitHubCallbackTriggerMerged && (!cb.ShouldRequireTransition || cb.HasObservedBaseline)) {
				continue
			}
			event = "retired: session merged"
		}
		switch cb.State {
		case models.GitHubCallbackStateActive:
			cancelErr = r.store.MarkUnreachable(ctx, cb.ID, event, now)
		case models.GitHubCallbackStateTriggered:
			if session.State != machine.Closed {
				continue
			}
			cancelErr = r.store.MarkCanceled(ctx, cb.ID, event, now)
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

// OnSessionState implements the session transition observer. Errors are logged
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

// armedBeforeReclaim reports a watch armed by a chat that automation has since
// replaced. Delivery would wake that chat beside its replacement, so two agents
// would react to one PR failure in one worktree. A watch armed after the reclaim
// comes from a chat the operator revived.
func armedBeforeReclaim(cb *models.GitHubCallback, chat *models.AgentChat) bool {
	return chat.ReclaimedAt != nil && !cb.CreatedAt.After(*chat.ReclaimedAt)
}

// ownMergeWake reports a merge watch on the session's own PR. Repair stops once
// that PR merges, so waking a reclaimed chat for it raises no rival agent. A
// failed repo lookup keeps the wake, matching the gate's transient-error rule.
func (r *Retirer) ownMergeWake(ctx context.Context, cb *models.GitHubCallback, session *models.Session) bool {
	if cb.Trigger != models.GitHubCallbackTriggerMerged {
		return false
	}
	nwo, err := r.ownPR(ctx, session)
	return err != nil || callbackOnOwnPR(cb, session, nwo)
}

// DeliveryVerdict drops stale terminal-session deliveries, watches armed by a
// since-reclaimed chat, and missing targets. A transient lookup failure
// preserves today's delivery/retry behavior.
func (r *Retirer) DeliveryVerdict(ctx context.Context, cb *models.GitHubCallback) (deliver bool, reason string) {
	chat, err := r.chats.GetByAgentSessionID(ctx, cb.TargetChatID)
	if errors.Is(err, domain.ErrNotFound) {
		return false, "target chat missing"
	}
	if err != nil {
		return true, ""
	}
	session, err := r.sessions.Get(ctx, chat.SessionID)
	if errors.Is(err, domain.ErrNotFound) {
		return false, "target session missing"
	}
	if err != nil {
		return true, ""
	}
	if armedBeforeReclaim(cb, chat) && !r.ownMergeWake(ctx, cb, session) {
		return false, "target chat reclaimed"
	}
	if session.State == machine.Closed {
		return false, "session closed"
	}
	if session.State == machine.Merged && cb.Trigger != models.GitHubCallbackTriggerMerged {
		nwo, err := r.ownPR(ctx, session)
		if err == nil && callbackOnOwnPR(cb, session, nwo) {
			return false, "session merged"
		}
	}
	return true, ""
}

// DropDelivery owns stale delivery cancellation and its cached waiting projection.
func (r *Retirer) DropDelivery(ctx context.Context, cb *models.GitHubCallback, reason string) error {
	if err := r.store.MarkCanceled(ctx, cb.ID, "dropped: "+reason, r.now()); err != nil {
		return err
	}
	if r.recomputer == nil {
		return nil
	}
	chat, err := r.chats.GetByAgentSessionID(ctx, cb.TargetChatID)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return r.recomputer.Recompute(ctx, chat.SessionID)
}

// reconcilePR reconciles each owning session before provider reads and after
// the trigger pass. Closed owners settle independently of the watched PR.
func (r *Retirer) reconcilePR(ctx context.Context, callbacks []*models.GitHubCallback) error {
	seen := make(map[string]bool)
	var errs []error
	for _, cb := range callbacks {
		chat, err := r.chats.GetByAgentSessionID(ctx, cb.TargetChatID)
		if errors.Is(err, domain.ErrNotFound) {
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
