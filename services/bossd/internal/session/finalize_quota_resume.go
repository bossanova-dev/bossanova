package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sessionreason"
)

const (
	quotaFinalizeGrace       = 30 * time.Second
	quotaFinalizeCooldown    = 5 * time.Minute
	quotaFinalizeMaxPerSweep = 3
)

func quotaFinalizeDue(sess *models.Session, now time.Time) bool {
	if sess == nil || sess.ArchivedAt != nil || sess.State != machine.Blocked || sess.RunBlockedReason != nil || sess.BlockedReason == nil {
		return false
	}
	reset, ok := persistedQuotaReset(sess)
	return ok && !now.Before(reset.Add(quotaFinalizeGrace))
}

// ResumeQuotaBlockedFinalizesPeriodic retries at most three finalizes per tick
// after their persisted quota reset. A restart forgets only the retry cooldown;
// the reason in the session row remains the authority for when to resume.
// The return count measures attempts, including failed attempts.
func (l *Lifecycle) ResumeQuotaBlockedFinalizesPeriodic(ctx context.Context) (int, error) {
	l.quotaFinalizeMu.Lock()
	defer l.quotaFinalizeMu.Unlock()
	if ctx.Err() != nil {
		return 0, nil
	}
	sessions, err := l.sessions.ListActive(ctx, "")
	if err != nil {
		if isContextError(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("list active sessions: %w", err)
	}
	now := l.now()
	eligible := make(map[string]bool)
	if l.quotaFinalizeAttempts == nil {
		l.quotaFinalizeAttempts = make(map[string]time.Time)
	}
	retried := 0
	for _, sess := range sessions {
		if ctx.Err() != nil {
			break
		}
		if _, waiting := persistedQuotaReset(sess); !waiting {
			continue
		}
		eligible[sess.ID] = true
		if !quotaFinalizeDue(sess, now) {
			continue
		}
		if retried >= quotaFinalizeMaxPerSweep {
			continue
		}
		if last, ok := l.quotaFinalizeAttempts[sess.ID]; ok && now.Sub(last) < quotaFinalizeCooldown {
			continue
		}
		result, err := l.finalizeSessionFrom(ctx, sess.ID, []int{int(machine.Blocked)}, finalizeTriggerQuotaResume)
		if result != nil && result.NoOp {
			continue
		}
		l.quotaFinalizeAttempts[sess.ID] = now
		retried++
		if err != nil {
			l.logger.Warn().Err(err).Str("session", sess.ID).Msg("quota finalize resume failed")
		} else {
			l.logger.Info().Str("session", sess.ID).Str("outcome", string(result.Outcome)).Msg("quota finalize resume completed")
		}
	}
	for id := range l.quotaFinalizeAttempts {
		if !eligible[id] {
			delete(l.quotaFinalizeAttempts, id)
		}
	}
	return retried, nil
}

func persistedQuotaReset(sess *models.Session) (time.Time, bool) {
	if sess == nil || sess.BlockedReason == nil {
		return time.Time{}, false
	}
	return sessionreason.ParseFinalizeQuotaWait(*sess.BlockedReason)
}

func finalizeFailureReason(result *FinalizeResult, runBlocked bool) string {
	if !runBlocked && errors.Is(result.Err, vcs.ErrRateLimited) {
		if reset, ok := vcs.RateLimitResetAt(result.Err); ok {
			return sessionreason.FinalizeQuotaWait(reset)
		}
	}
	return sessionreason.FinalizeFailure(string(result.Outcome), result.Err)
}
