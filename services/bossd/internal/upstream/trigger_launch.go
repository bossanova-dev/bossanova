package upstream

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/recurser/bossalib/createframe"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"github.com/rs/zerolog"
)

// defaultTriggerLaunchBudget bounds one trigger launch's create when the wiring
// supplies no budget. It covers the worst legitimate create: a wait for the
// per-target start lock plus a full bootstrap (session.TargetStartLockTimeout +
// session.BootstrapTimeout, 12m + 10m today), with slack. cmd/main.go wires the
// derived value; this constant only keeps a zero-value launcher bounded.
const defaultTriggerLaunchBudget = 25 * time.Minute

// defaultTriggerLaunchPollInterval is how often a launch that lost the claim
// race re-reads the winner's row for its session id.
const defaultTriggerLaunchPollInterval = 250 * time.Millisecond

// triggerLaunchRetention is how long an idempotency row is kept. Far beyond
// any bosso retry window; pruned opportunistically on each launch.
const triggerLaunchRetention = 30 * 24 * time.Hour

// triggerLaunchStaleMargin is how far past Budget a session-less claim must age
// before another launch may take it over. The create is bounded by Budget
// measured from slightly after the claim, and its bookkeeping lands after
// that, so a takeover at exactly Budget could race a still-finishing owner.
const triggerLaunchStaleMargin = 2 * time.Minute

// TriggerRepoResolver resolves the repo a trigger launch targets by its origin
// URL. *db.SQLiteRepoStore satisfies it; GetByOrigin already falls back to the
// canonical web URL, so callers pass the origin as received.
type TriggerRepoResolver interface {
	GetByOrigin(ctx context.Context, originURL string) (*models.Repo, error)
}

// TriggerSessionLauncher answers LaunchTriggerSessionCommand (BOS-1418): it
// starts one unattended, detached, defer_pr session for an inbound trigger
// invocation and is idempotent on the invocation id, so a bosso retry after a
// lost acknowledgement never launches twice.
//
// Idempotency lives in the trigger_launches table: a launch claims the
// invocation id before creating anything (the PRIMARY KEY elects exactly one
// winner among concurrent launches), records the session id once the session
// settles, and releases the claim on a failed create so a retry can try again.
type TriggerSessionLauncher struct {
	Launches db.TriggerLaunchStore
	Repos    TriggerRepoResolver
	// Creator is the daemon's StreamCreateSession core — the same one
	// SessionCreatorAdapter drives. Wired post-hoc in cmd/main.go once the
	// server exists.
	Creator StreamCreateSessioner
	Logger  zerolog.Logger
	// Budget bounds the create once it has started; zero means
	// defaultTriggerLaunchBudget. A claim older than Budget plus
	// triggerLaunchStaleMargin that still has no session is treated as
	// abandoned (a daemon crash mid-launch).
	Budget time.Duration
	// PollInterval is the losing launch's re-read cadence; zero means
	// defaultTriggerLaunchPollInterval.
	PollInterval time.Duration
}

func (l *TriggerSessionLauncher) budget() time.Duration {
	if l.Budget > 0 {
		return l.Budget
	}
	return defaultTriggerLaunchBudget
}

func (l *TriggerSessionLauncher) pollInterval() time.Duration {
	if l.PollInterval > 0 {
		return l.PollInterval
	}
	return defaultTriggerLaunchPollInterval
}

// Launch implements the LaunchTriggerSession command. ctx is the command
// context: it bounds validation, the claim and a losing launch's wait, but NOT
// the create — once the create has started it runs on a context detached from
// ctx and bounded by Budget, so a launch that outlives bosso's dispatch
// deadline still finishes and records its session id for the retry to find.
func (l *TriggerSessionLauncher) Launch(ctx context.Context, cmd *pb.LaunchTriggerSessionCommand) (*pb.LaunchTriggerSessionResult, pb.CommandResult_ErrorCode, error) {
	invocationID := strings.TrimSpace(cmd.GetInvocationId())
	originURL := strings.TrimSpace(cmd.GetRepoOriginUrl())
	if invocationID == "" || originURL == "" || strings.TrimSpace(cmd.GetPrompt()) == "" {
		return nil, pb.CommandResult_ERROR_CODE_INVALID_ARGUMENT,
			errors.New("launch_trigger_session: invocation_id, repo_origin_url and prompt are required")
	}
	if l.Launches == nil || l.Repos == nil || l.Creator == nil {
		return nil, pb.CommandResult_ERROR_CODE_UNSPECIFIED, errors.New("launch_trigger_session: launcher not wired")
	}

	if n, err := l.Launches.PruneOlderThan(ctx, time.Now().Add(-triggerLaunchRetention)); err != nil {
		l.Logger.Warn().Err(err).Msg("launch_trigger_session: prune trigger_launches failed")
	} else if n > 0 {
		l.Logger.Debug().Int64("pruned", n).Msg("launch_trigger_session: pruned old trigger_launches")
	}

	// Idempotency read: a finished launch is replayed without touching the
	// repo or the creator.
	if row, err := l.Launches.Get(ctx, invocationID); err != nil {
		return nil, pb.CommandResult_ERROR_CODE_UNSPECIFIED, fmt.Errorf("launch_trigger_session: %w", err)
	} else if sessionID := l.replaySession(ctx, row); sessionID != "" {
		return &pb.LaunchTriggerSessionResult{SessionId: sessionID, IsReplay: true}, pb.CommandResult_ERROR_CODE_UNSPECIFIED, nil
	}

	repo, code, err := resolveTriggerRepo(ctx, l.Repos, originURL)
	if err != nil {
		return nil, code, fmt.Errorf("launch_trigger_session: %w", err)
	}

	token, replay, code, err := l.claim(ctx, invocationID, cmd.GetTriggerId())
	if err != nil || replay != nil {
		return replay, code, err
	}

	sessionID, runningID, err := l.create(ctx, repo.ID, token, cmd)
	// From here on the command context may already be gone; the bookkeeping
	// must still land or a retry would wedge on (or relaunch past) the claim.
	bookkeeping := context.WithoutCancel(ctx)
	if err != nil && runningID != "" {
		// The create ran out of budget after the daemon accepted the session:
		// its bootstrap keeps running in the background (BOS-720), so releasing
		// the claim would let bosso's retry launch a duplicate. Record the
		// accepted id instead, so the retry replays it.
		if setErr := l.setSession(bookkeeping, invocationID, token, runningID); setErr != nil {
			l.Logger.Error().Err(setErr).Str("invocation_id", invocationID).Str("session", runningID).
				Msg("launch_trigger_session: record still-bootstrapping session on claim")
		}
		return nil, pb.CommandResult_ERROR_CODE_FAILED_PRECONDITION,
			fmt.Errorf("launch_trigger_session: session %s did not settle within the launch budget: %w", runningID, err)
	}
	if err != nil {
		if relErr := l.Launches.Release(bookkeeping, invocationID, token); relErr != nil {
			l.Logger.Error().Err(relErr).Str("invocation_id", invocationID).
				Msg("launch_trigger_session: release claim after failed create")
		}
		return nil, pb.CommandResult_ERROR_CODE_FAILED_PRECONDITION, fmt.Errorf("launch_trigger_session: %w", err)
	}
	if err := l.setSession(bookkeeping, invocationID, token, sessionID); err != nil {
		// The session exists, so report it rather than invite a duplicate.
		// Its accepted record lets a later stale replay recover the id.
		l.Logger.Error().Err(err).Str("invocation_id", invocationID).Str("session", sessionID).
			Msg("launch_trigger_session: record session id on claim")
	}
	return &pb.LaunchTriggerSessionResult{SessionId: sessionID}, pb.CommandResult_ERROR_CODE_UNSPECIFIED, nil
}

// claim wins the invocation's claim row, or — when a concurrent launch already
// holds it — waits for that launch's session id and returns it as a replay. A
// nil result with a nil error means this caller won and must create.
func (l *TriggerSessionLauncher) claim(ctx context.Context, invocationID, triggerID string) (string, *pb.LaunchTriggerSessionResult, pb.CommandResult_ErrorCode, error) {
	ticker := time.NewTicker(l.pollInterval())
	defer ticker.Stop()
	for {
		token, won, err := l.Launches.Claim(ctx, invocationID, triggerID, l.staleBefore())
		if err != nil {
			return "", nil, pb.CommandResult_ERROR_CODE_UNSPECIFIED, fmt.Errorf("launch_trigger_session: %w", err)
		}
		if won {
			return token, nil, pb.CommandResult_ERROR_CODE_UNSPECIFIED, nil
		}
		// Lost the race: poll the winner's row. It either records a session
		// (replay), vanishes because the winner's create failed and released it
		// (claim again), or outlasts this command's budget.
		for {
			row, err := l.Launches.Get(ctx, invocationID)
			if err != nil {
				return "", nil, pb.CommandResult_ERROR_CODE_UNSPECIFIED, fmt.Errorf("launch_trigger_session: %w", err)
			}
			if row == nil {
				break
			}
			if sessionID := l.replaySession(ctx, row); sessionID != "" {
				return "", &pb.LaunchTriggerSessionResult{SessionId: sessionID, IsReplay: true}, pb.CommandResult_ERROR_CODE_UNSPECIFIED, nil
			}
			select {
			case <-ctx.Done():
				// UNSPECIFIED (bosso: Aborted) on purpose: the launch is
				// idempotent, so a retry is exactly what should happen — it
				// will replay the winner's session once it settles.
				return "", nil, pb.CommandResult_ERROR_CODE_UNSPECIFIED,
					fmt.Errorf("launch_trigger_session: invocation %s is still launching: %w", invocationID, ctx.Err())
			case <-ticker.C:
			}
		}
	}
}

// create runs the daemon's StreamCreateSession core for the trigger and returns
// the settled session's id. The settled frame is identified with the same three
// terminal tests ProxyCreateSession documents (attach flag, quick chat, a
// populated worktree_path) rather than a new heuristic: since BOS-720 the
// bootstrapping path emits `created` twice, and the first (accepted) frame is
// not the session the trigger launched until its bootstrap succeeds.
//
// The same rule covers a zero-output launch (BOS-1477). It is not a quick chat
// on the request — it runs through StartSession and its bootstrap runner — and
// StartSession persists the repo checkout as its worktree path, so it emits an
// accepted frame with an empty worktree_path and then settles with the
// checkout's path.
//
// On error, runningID is the accepted session's id when the create ran out of
// budget after the accepted frame — the daemon keeps bootstrapping that session
// in the background — and empty otherwise.
func (l *TriggerSessionLauncher) create(ctx context.Context, repoID, token string, cmd *pb.LaunchTriggerSessionCommand) (settledID, runningID string, err error) {
	title := strings.TrimSpace(cmd.GetTitle())
	if title == "" {
		// bosso sends the trigger's name; without one the daemon only knows
		// the id, and StreamCreateSession rejects an empty title.
		title = "Trigger: " + cmd.GetTriggerId()
	}
	zeroOutput := cmd.GetIsZeroOutput()
	baseBranch := cmd.GetBaseBranch()
	if zeroOutput {
		// A zero-output run cuts no branch; sending a base branch would only
		// invite the daemon and bosso to disagree about what it means.
		baseBranch = ""
	}
	req := &pb.CreateSessionRequest{
		RepoId:           repoID,
		Title:            title,
		Plan:             cmd.GetPrompt(),
		BaseBranch:       baseBranch,
		IsZeroOutput:     zeroOutput,
		Model:            cmd.Model,
		Effort:           cmd.Effort,
		Detach:           true,
		IsTmuxUnattended: true,
		DeferPr:          true,
		// A trigger launch carries no tracker fields and must never attach to
		// an existing session for the same target.
		Force: true,
	}
	if name := strings.TrimSpace(cmd.GetAgentName()); name != "" {
		req.AgentName = &name
	}
	// Presence is the signal: absent (a bosso that predates the field) must
	// stay absent so the daemon keeps running setup.
	if cmd.ShouldRunSetupCommand != nil {
		runSetup := cmd.GetShouldRunSetupCommand()
		req.ShouldRunSetupCommand = &runSetup
	}

	createCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), l.budget())
	defer cancel()

	var acceptedID string
	sawAccepted := false
	emit := func(resp *pb.CreateSessionResponse) error {
		created := resp.GetSessionCreated()
		if created == nil {
			return nil
		}
		if !sawAccepted && createframe.IsAccepted(created.GetAttachedExisting(), req.GetIsQuickChat(), created.GetSession().GetWorktreePath()) {
			sawAccepted = true
			acceptedID = created.GetSession().GetId()
			if err := l.Launches.MarkAccepted(context.WithoutCancel(ctx), strings.TrimSpace(cmd.GetInvocationId()), token, acceptedID); err != nil {
				message := "launch_trigger_session: record accepted session on claim"
				if errors.Is(err, db.ErrTriggerLaunchClaimLost) {
					message = "launch_trigger_session: claim taken over mid-create"
				}
				// Never abort emit: the bootstrap keeps running even if this stream stops.
				l.Logger.Error().Err(err).Str("invocation_id", cmd.GetInvocationId()).Str("session", acceptedID).Msg(message)
			}
			return nil
		}
		settledID = created.GetSession().GetId()
		return nil
	}
	if err := l.Creator.StreamCreateSession(createCtx, req, emit); err != nil {
		if settledID == "" && createCtx.Err() != nil {
			return "", acceptedID, err
		}
		return "", "", err
	}
	if settledID == "" {
		return "", "", errors.New("create ended without a settled session")
	}
	return settledID, "", nil
}

// staleBefore is shared by takeover and recovery of an accepted claim.
func (l *TriggerSessionLauncher) staleBefore() time.Time {
	return time.Now().Add(-(l.budget() + triggerLaunchStaleMargin))
}

// replaySession recovers a silent owner's durable accepted id after the stale
// threshold. Before then, bootstrap may still fail and release the claim.
func (l *TriggerSessionLauncher) replaySession(ctx context.Context, row *db.TriggerLaunch) string {
	if row == nil {
		return ""
	}
	if row.SessionID != "" {
		return row.SessionID
	}
	staleBefore := l.staleBefore()
	if row.AcceptedSessionID == "" || !row.CreatedAt.Before(staleBefore) {
		return ""
	}
	if _, err := l.Launches.PromoteAccepted(ctx, row.InvocationID, staleBefore); err != nil {
		l.Logger.Error().Err(err).Str("invocation_id", row.InvocationID).Msg("launch_trigger_session: promote accepted session")
	}
	return row.AcceptedSessionID
}

// setSession retries transient bookkeeping errors without repeating creation.
func (l *TriggerSessionLauncher) setSession(ctx context.Context, invocationID, token, sessionID string) error {
	var err error
	for attempt := range 3 {
		err = l.Launches.SetSession(ctx, invocationID, token, sessionID)
		if err == nil || errors.Is(err, db.ErrTriggerLaunchClaimLost) {
			return err
		}
		if attempt < 2 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	return err
}
