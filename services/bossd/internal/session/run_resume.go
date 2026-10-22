package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/recurser/bossd/internal/domain"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/resumepolicy"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/adapter/dotenv"
	"github.com/recurser/bossd/internal/db"
)

var ErrRunNotResumable = errors.New("session run cannot be resumed")

const droppedResumeSteeringNotice = "Your previous turn was interrupted. Verify current workspace/git/PR state before repeating any action (commits, pushes, comments may already exist)."

func (l *Lifecycle) SetRunResumeStore(store db.ResumeAttemptStore) { l.runResumeStore = store }

// CanResumeSessionRun is shared by the attention action producer and the resume
// entry point. A fault in an active working state is resumable too.
func CanResumeSessionRun(s *models.Session) bool {
	if s == nil || s.ArchivedAt != nil || strings.TrimSpace(s.Plan) == "" || s.WorktreePath == "" {
		return false
	}
	switch s.State {
	case machine.ImplementingPlan, machine.Blocked, machine.Orphaned, machine.AwaitingChecks, machine.FixingChecks, machine.GreenDraft, machine.ReadyForReview, machine.PushingBranch, machine.OpeningDraftPR:
		return true
	case machine.CreatingWorktree, machine.StartingAgent, machine.Merged, machine.Closed, machine.Finalizing:
		return false
	}
	return false
}

// firstChat is the chat that started the run. A replacement launched within
// the original's millisecond shares its CreatedAt, so insertion order decides
// before the random ID does; otherwise the run key would move to the
// replacement and its empty ledger would restart the backoff.
func firstChat(chats []*models.AgentChat) *models.AgentChat {
	var first *models.AgentChat
	for _, c := range chats {
		if c == nil {
			continue
		}
		if first == nil || c.CreatedAt.Before(first.CreatedAt) || (c.CreatedAt.Equal(first.CreatedAt) && (c.Seq < first.Seq || (c.Seq == first.Seq && c.ID < first.ID))) {
			first = c
		}
	}
	return first
}

// ResumeBudgetKey keeps one budget across replacement chats. The first chat stays
// as history; the cron id scopes it to its original fire, never a later one.
func ResumeBudgetKey(s *models.Session, chats []*models.AgentChat) string {
	first := firstChat(chats)
	key := ""
	if first != nil {
		key = first.AgentSessionID
	} else if s.AgentSessionID != nil {
		key = *s.AgentSessionID
	}
	if s.CronJobID != nil && *s.CronJobID != "" {
		key = *s.CronJobID + ":" + key
	}
	return key
}

// ResumeDroppedRuns is level-triggered over durable sessions and attempts. A
// process restart loses no timer or attempt budget.
func (l *Lifecycle) ResumeDroppedRuns(ctx context.Context) int {
	cfg, ok := l.currentRotationConfig()
	if !ok || !cfg.AutoResumeDroppedRunsEnabled() || l.runResumeStore == nil || l.agentChats == nil {
		return 0
	}
	rows, err := l.sessions.ListByStates(ctx, periodicReapStateInts())
	if err != nil {
		return 0
	}
	resumed := 0
	for _, s := range rows {
		if !isUnattendedSession(s) || !CanResumeSessionRun(s) || isZeroOutputSession(s) {
			continue
		}
		if l.resumeDroppedRun(ctx, s) {
			resumed++
		}
	}
	return resumed
}

// ErrRunResumeBusy reports that another resume of the same session is still
// running.
var ErrRunResumeBusy = errors.New("another resume of this session is in progress")

// lockRunResume serializes resumes of one session, so a manual and an automatic
// resume never both relaunch it. The gate is per session: a manual resume
// waiting on one repo's wedged start-path lock holds up only that session, not
// the automatic resume of every other one. A caller whose own context ends
// while it waits gets that context's error, not ErrRunResumeBusy.
func (l *Lifecycle) lockRunResume(ctx context.Context, sessionID string, wait time.Duration) (release func(), err error) {
	release, _, err = l.runResumeGates.Acquire(ctx, sessionID, wait)
	if err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %w", ErrRunResumeBusy, err)
	}
	return release, nil
}

func (l *Lifecycle) resumeDroppedRun(ctx context.Context, s *models.Session) bool {
	release, err := l.lockRunResume(ctx, s.ID, RevivalLockTimeout)
	if err != nil {
		return false
	}
	defer release()
	current, err := l.sessions.Get(ctx, s.ID)
	if err != nil || !CanResumeSessionRun(current) {
		return false
	}
	s = current
	if l.liveness != nil && l.liveness.SessionLiveness(ctx, s.ID) == LivenessParked {
		return false
	}
	chats, err := l.agentChats.ListBySession(ctx, s.ID)
	if err != nil || len(chats) == 0 {
		return false
	}
	if seen, receiptErr := l.runBuildReceipt(ctx, s); seen || receiptErr != nil {
		return false
	}
	auth := l.SessionChatAuthFailed(s)
	// A live human chat, unknown pane probe, or still-running unattended agent
	// always wins over recovery. Authentication wedges may leave an idle pane.
	for _, c := range chats {
		if l.tmux != nil && isUnattendedSession(s) && s.AgentSessionID != nil && c.AgentSessionID == *s.AgentSessionID && c.TmuxSessionName == nil && !c.IsRetired() {
			return false
		}
		if c.TmuxSessionName != nil && *c.TmuxSessionName != "" {
			if l.tmux == nil {
				return false
			}
			alive, e := l.tmux.HasSessionStatus(ctx, *c.TmuxSessionName)
			if e != nil {
				return false
			}
			if alive {
				dead, e := l.tmux.PaneDead(ctx, *c.TmuxSessionName)
				if e != nil {
					return false
				}
				if !dead && (!auth || s.AgentSessionID == nil || c.AgentSessionID != *s.AgentSessionID) {
					return false
				}
			}
		} else if l.agentRunner != nil && l.agentRunner.IsRunningByAgent(c.AgentName, c.AgentSessionID) {
			return false
		}
	}
	// Explicit terminal telemetry is stronger than process death.
	var diedAt time.Time
	if l.agentRuns != nil {
		runs, e := l.agentRuns.List(ctx, domain.AgentRunFilter{SessionID: s.ID, IncludeOpen: true, IncludeAllReasons: true})
		if e != nil {
			return false
		}
		for _, r := range runs {
			if s.AgentSessionID != nil && r.AgentSessionID == *s.AgentSessionID {
				if r.TerminalState != "" {
					return false
				}
				if r.StoppedAt != nil {
					diedAt = *r.StoppedAt
				}
			}
		}
	}
	if diedAt.IsZero() {
		observer, ok := l.sessions.(db.RunDeathObserver)
		if !ok || s.AgentSessionID == nil {
			return false
		}
		diedAt, err = observer.RecordAgentRunDeath(ctx, s.ID, *s.AgentSessionID, int(s.State), l.now())
		if err != nil {
			return false
		}
	}
	key := ResumeBudgetKey(s, chats)
	if key == "" {
		return false
	}
	rows, err := l.runResumeStore.ListByBudget(ctx, s.ID, key)
	if err != nil {
		return false
	}
	tail := ""
	if s.AgentSessionID != nil && l.agentLogsDir != "" {
		tail = readFileTail(agentLogPathFor(l.agentLogsDir, *s.AgentSessionID), 8192)
	}
	for _, c := range chats {
		if s.AgentSessionID != nil && c.AgentSessionID == *s.AgentSessionID && c.TmuxSessionName != nil && l.tmux != nil {
			if pane, e := l.tmux.CapturePane(ctx, *c.TmuxSessionName); e == nil && pane != "" {
				tail = pane
			}
		}
	}
	detail := ""
	if s.BlockedReason != nil {
		detail = *s.BlockedReason
	}
	class := resumepolicy.ClassifyFailure(tail, detail, auth)
	if len(rows) > 0 {
		last := rows[len(rows)-1]
		if class != resumepolicy.FailureUnknown && last.FailureClass != string(class) {
			if err := l.runResumeStore.SetFailure(ctx, last.ID, string(class)); err != nil {
				return false
			}
			last.FailureClass = string(class)
		}
	}
	attempts := resumePolicyAttempts(rows)
	decision := resumepolicy.Next(diedAt, attempts, l.now())
	if decision.Action == resumepolicy.ActionWait {
		return false
	}
	if decision.Action == resumepolicy.ActionGiveUp {
		reason := "automatic resume ledger: " + decision.Reason
		giver, ok := l.sessions.(db.RunResumeGiveUpper)
		if !ok {
			return false
		}
		err = giver.MarkAgentRunResumeAbandoned(ctx, s.ID, int(s.State), s.AgentSessionID, reason)
		if err != nil {
			l.logger.Warn().Err(err).Str("session", s.ID).Msg("run resume give-up failed")
		}
		return false
	}
	attempt, err := l.runResumeStore.RecordResume(ctx, db.RecordResumeAttemptParams{SessionID: s.ID, BudgetKey: key, AttemptedAt: l.now(), FailureClass: string(resumepolicy.FailureUnknown)})
	if err != nil {
		return false
	}
	// Recording first intentionally consumes an attempt even if launch fails.
	if _, err = l.relaunchDroppedRun(ctx, s, chats, attempt.AttemptNumber); err != nil {
		l.logger.Warn().Err(err).Str("session", s.ID).Msg("run resume launch failed")
		return false
	}
	l.logger.Info().Str("session", s.ID).Int("attempt", attempt.AttemptNumber).Msg("resumed dropped run")
	return true
}

func resumePolicyAttempts(rows []*domain.ResumeAttempt) []resumepolicy.Attempt {
	out := make([]resumepolicy.Attempt, 0, len(rows))
	for _, r := range rows {
		class, _ := resumepolicy.ParseFailureClass(r.FailureClass)
		out = append(out, resumepolicy.Attempt{At: r.AttemptedAt, Class: class})
	}
	return out
}

func (l *Lifecycle) ResumeSessionRun(ctx context.Context, id string) (*models.Session, error) {
	unlock, err := l.lockRunResume(ctx, id, StartPathLockTimeout)
	if err != nil {
		return nil, err
	}
	defer unlock()
	s, err := l.sessions.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !CanResumeSessionRun(s) || l.agentChats == nil {
		return nil, ErrRunNotResumable
	}
	chats, err := l.agentChats.ListBySession(ctx, id)
	if err != nil {
		return nil, err
	}
	if l.hasActiveOtherChat(ctx, s, chats) {
		return nil, ErrRunNotResumable
	}
	// Refuse before stopping or spawning anything; relaunchDroppedRun checks
	// again, under the lock, as it commits.
	release, err := l.lockRunRevival(ctx, s)
	if err != nil {
		return nil, err
	}
	release()
	// A manual retry is immediate and gets a fresh automatic budget.
	if l.runResumeStore != nil {
		if _, err = l.runResumeStore.Delete(ctx, id, ResumeBudgetKey(s, chats)); err != nil {
			return nil, err
		}
	}
	return l.relaunchDroppedRun(ctx, s, chats, 1)
}

func (l *Lifecycle) relaunchDroppedRun(ctx context.Context, s *models.Session, chats []*models.AgentChat, n int) (*models.Session, error) {
	if l.agentRunner == nil {
		return nil, ErrRunNotResumable
	}
	spawn, chat, err := l.InServiceSpawnTarget(ctx, s)
	if err != nil {
		return nil, err
	}
	tmuxHosted := l.tmux != nil && ((chat != nil && chat.TmuxSessionName != nil) || isUnattendedSession(s))
	title := s.Title
	if chat != nil {
		title = chat.Title
	}
	if original := firstChat(chats); original != nil && original.Title != "" {
		title = original.Title
	}
	title = fmt.Sprintf("%s (resumed %d)", title, n)
	prompt := droppedResumeSteeringNotice + "\n\n" + s.Plan
	// Keep the original chat row and its dead-pane pointer for run identity and
	// completion evidence. A cleared pointer means deliberately parked, so a
	// failed replacement must not turn the original crash into that verdict.
	for _, c := range chats {
		if s.AgentSessionID != nil && c.AgentSessionID != *s.AgentSessionID {
			continue
		}
		if c.TmuxSessionName != nil && l.tmux != nil {
			alive, probeErr := l.tmux.HasSessionStatus(ctx, *c.TmuxSessionName)
			if probeErr != nil {
				return nil, probeErr
			}
			if alive {
				if err := l.tmux.KillSession(ctx, *c.TmuxSessionName); err != nil {
					return nil, err
				}
			}
		}
		if l.agentRunner.IsRunningByAgent(c.AgentName, c.AgentSessionID) {
			if err := l.agentRunner.StopByAgent(ctx, c.AgentName, c.AgentSessionID); err != nil {
				return nil, err
			}
		}
	}
	var newID string
	if tmuxHosted {
		// This path already checked all live chats. Preserve dead chat pointers
		// instead of running generic stale-row cleanup, which means parked.
		newID, err = l.StartTmuxChat(ctx, s.ID, ChatInput{Prompt: prompt, Delivery: DeliverySubmit, AccountID: spawn.AccountID, AgentName: &spawn.AgentName, Model: &spawn.Model, Role: models.AgentChatRoleBuild}, title, HookOpts{AllowSiblingChat: true})
	} else {
		repo := RepoForSessionEnv(ctx, l.repos, s.RepoID, s.ID, "dropped run resume", l.logger)
		env, e := l.resolveAccountEnvForChat(ctx, spawn, chat)
		if e != nil {
			return nil, e
		}
		env = dotenv.OverlayWithRepo(mergeEnv(env, l.resolveProofEnvFor(repo)), s.WorktreePath, repo)
		newID, err = l.startHeadlessReplacementRun(ctx, spawn, repo, s.WorktreePath, prompt, nil, env)
		if err == nil {
			_, err = l.agentChats.Create(ctx, db.CreateAgentChatParams{SessionID: s.ID, AgentSessionID: newID, AgentName: spawn.AgentName, Model: spawn.Model, AccountID: spawn.AccountID, Title: title, Role: models.AgentChatRoleBuild})
			if err != nil {
				l.stopDroppedReplacement(ctx, spawn.AgentName, newID, s.ID)
			}
		}
	}
	if err != nil {
		return nil, err
	}
	committer, ok := l.sessions.(db.RunResumeCommitter)
	if !ok {
		l.stopDroppedReplacement(ctx, spawn.AgentName, newID, s.ID)
		return nil, ErrRunNotResumable
	}
	release, err := l.lockRunRevival(ctx, s)
	if err != nil {
		l.stopDroppedReplacement(ctx, spawn.AgentName, newID, s.ID)
		return nil, err
	}
	updated, err := committer.MarkAgentRunResumed(ctx, s.ID, int(s.State), s.AgentSessionID, newID)
	release()
	if err != nil {
		l.stopDroppedReplacement(ctx, spawn.AgentName, newID, s.ID)
		return nil, err
	}
	if err = l.sessions.SetRepairAlertCleared(ctx, s.ID); err != nil {
		return nil, err
	}
	l.rearmCompletionForRotatedRun(s.ID, newID, spawn)
	return updated, nil
}

// lockRunRevival takes the start-path lock for a run resume that would bring a
// Blocked or Orphaned session back to life, and refuses, naming the holder,
// when another live session now holds its PR or branch. A session already in
// a live state holds its own target, so it takes no lock.
func (l *Lifecycle) lockRunRevival(ctx context.Context, s *models.Session) (release func(), err error) {
	if StateBlocksDuplicateTarget(s.State) {
		return func() {}, nil
	}
	_, release, err = AcquireRevival(ctx, l.sessions, s, StartPathLockTimeout, func(fresh *models.Session) bool {
		return fresh.State == s.State
	})
	if errors.Is(err, ErrRevivalStale) {
		return nil, fmt.Errorf("%w: %w", ErrRunNotResumable, err)
	}
	if err != nil {
		return nil, RevivalRefusal(err)
	}
	return release, nil
}

func stringPtrPtr(s string) **string { p := &s; return &p }

// DeferDroppedRunFinalization keeps unfinished runs in the resume lane instead
// of converting their process death into a successful finalize.
func (l *Lifecycle) DeferDroppedRunFinalization(s *models.Session) bool {
	cfg, ok := l.currentRotationConfig()
	if !ok || !cfg.AutoResumeDroppedRunsEnabled() || l.runResumeStore == nil || !CanResumeSessionRun(s) || isZeroOutputSession(s) {
		return false
	}
	// A completed unattended turn can leave its agent alive and idle. Normal
	// completion owns that case, including prompts that emit no build telemetry.
	// Recovery only owns process death; an unreadable probe keeps its veto.
	if l.agentChats != nil && s.AgentSessionID != nil {
		chats, err := l.agentChats.ListBySession(context.Background(), s.ID)
		if err != nil {
			return true
		}
		for _, chat := range chats {
			if chat.AgentSessionID != *s.AgentSessionID {
				continue
			}
			if chat.TmuxSessionName != nil && *chat.TmuxSessionName != "" && l.tmux != nil {
				alive, err := l.tmux.HasSessionStatus(context.Background(), *chat.TmuxSessionName)
				if err != nil {
					return true
				}
				if alive {
					dead, err := l.tmux.PaneDead(context.Background(), *chat.TmuxSessionName)
					if err != nil {
						return true
					}
					if !dead {
						return false
					}
				}
			} else if l.agentRunner != nil && l.agentRunner.IsRunningByAgent(chat.AgentName, chat.AgentSessionID) {
				return false
			}
		}
	}
	if seen, receiptErr := l.runBuildReceipt(context.Background(), s); receiptErr != nil {
		return true
	} else if seen {
		return false
	}
	if l.agentRuns == nil {
		return true
	}
	runs, err := l.agentRuns.List(context.Background(), domain.AgentRunFilter{SessionID: s.ID, IncludeOpen: true, IncludeAllReasons: true})
	if err != nil {
		return true
	}
	for _, r := range runs {
		if s.AgentSessionID != nil && r.AgentSessionID == *s.AgentSessionID && r.TerminalState != "" {
			return false
		}
	}
	return true
}

func (l *Lifecycle) runBuildReceipt(ctx context.Context, s *models.Session) (bool, error) {
	if s.PRNumber == nil || l.provider == nil {
		return false, nil
	}
	checks, err := vcs.ReadCheckSet(ctx, l.provider, s.WorktreePath, *s.PRNumber)
	// An unreadable remote is not permission to relaunch a possibly finished run.
	return checks.HasBuildReceipt, err
}

func (l *Lifecycle) hasActiveOtherChat(ctx context.Context, s *models.Session, chats []*models.AgentChat) bool {
	for _, c := range chats {
		if s.AgentSessionID != nil && c.AgentSessionID == *s.AgentSessionID {
			continue
		}
		if c.TmuxSessionName != nil && *c.TmuxSessionName != "" {
			if l.tmux == nil {
				return true
			}
			alive, err := l.tmux.HasSessionStatus(ctx, *c.TmuxSessionName)
			if err != nil {
				return true
			}
			if alive {
				dead, err := l.tmux.PaneDead(ctx, *c.TmuxSessionName)
				if err != nil || !dead {
					return true
				}
			}
		} else if l.agentRunner != nil && l.agentRunner.IsRunningByAgent(c.AgentName, c.AgentSessionID) {
			return true
		}
	}
	return false
}

// RunResumeAvailable includes live-chat protection for attention hydration.
func (l *Lifecycle) RunResumeAvailable(ctx context.Context, s *models.Session, chats []*models.AgentChat) bool {
	return CanResumeSessionRun(s) && l.agentRunner != nil && !l.hasActiveOtherChat(ctx, s, chats)
}

func (l *Lifecycle) stopDroppedReplacement(ctx context.Context, agentName, newID, sessionID string) {
	l.stopOrphanedRestart(ctx, agentName, newID, sessionID)
	if l.agentChats == nil {
		return
	}
	chat, err := l.agentChats.GetByAgentSessionID(ctx, newID)
	if err == nil && chat != nil && chat.TmuxSessionName != nil {
		l.killTmuxChatBestEffort(ctx, sessionID, newID, *chat.TmuxSessionName)
	}
}
