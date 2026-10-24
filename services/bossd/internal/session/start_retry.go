package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/recurser/bossd/internal/account"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/domain"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	agentStartAttempts     = 3
	agentStartMinRemaining = 3 * time.Minute
)

// Kept variable like the other launch budgets so integration tests can avoid
// real backoff waits. Production never reassigns it.
var agentStartRetryBackoffs = []time.Duration{15 * time.Second, 30 * time.Second}

// Shared by the post-setup probe and tmux launch: a successful probe does not
// reset the bootstrap's retry budget. Other launch paths (paneless, quick chat,
// resurrect and account-switch respawn) deliberately do not use this policy.
type agentStartRetry struct {
	attempt  int
	backoffs []time.Duration
}

// failedAgentStart identifies only the pane/row created by this attempt.
// The original error remains visible to delivery and capability classifiers.
type failedAgentStart struct {
	err            error
	agentSessionID string
	tmuxName       string
}

func (e *failedAgentStart) Error() string { return e.err.Error() }
func (e *failedAgentStart) Unwrap() error { return e.err }

func isTransientAgentStartError(err error) bool {
	if err == nil {
		return false
	}
	var injection *redactedInjectionError
	var accountErr *account.InjectionError
	if errors.As(err, &injection) || errors.As(err, &accountErr) ||
		errors.Is(err, agent.ErrAgentNotLoaded) || errors.Is(err, agent.ErrHeadlessCapabilityProfileUnsupported) ||
		hasAgentStartStatus(err, codes.FailedPrecondition) {
		return false
	}
	// Delivery evidence takes precedence over a transport cause: an unconfirmed
	// submission must never be replayed merely because its capture RPC timed out.
	switch domain.OutcomeOf(err) {
	case domain.OutcomeNotSubmitted:
		return true
	case domain.OutcomeUnclassified:
	default:
		return false
	}
	return errors.Is(err, context.DeadlineExceeded) || hasAgentStartStatus(err, codes.DeadlineExceeded) || hasAgentStartStatus(err, codes.Unavailable)
}

// Joined errors can carry a status in a later branch. Refusals take precedence
// over transient statuses regardless of the order in that error tree.
func hasAgentStartStatus(err error, code codes.Code) bool {
	if err == nil {
		return false
	}
	if status.Code(err) == code {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, cause := range joined.Unwrap() {
			if hasAgentStartStatus(cause, code) {
				return true
			}
		}
		return false
	}
	return hasAgentStartStatus(errors.Unwrap(err), code)
}

func (l *Lifecycle) startAgentWithRetry(ctx context.Context, sessionID string, output io.Writer, retry *agentStartRetry, start func() (string, error)) (string, error) {
	if retry.attempt == 0 {
		retry.attempt = 1
	}
	backoffs := retry.backoffs
	if backoffs == nil {
		backoffs = agentStartRetryBackoffs
	}
	for {
		id, err := start()
		if err == nil {
			return id, nil
		}
		final := func() (string, error) {
			return "", &AgentStartFailedError{Cause: err, Attempts: retry.attempt}
		}
		if ctx.Err() != nil || retry.attempt >= agentStartAttempts || !isTransientAgentStartError(err) {
			return final()
		}
		backoff := backoffs[retry.attempt-1]
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < agentStartMinRemaining+backoff {
			return final()
		}

		reason, _, _ := strings.Cut(err.Error(), "\n")
		writeSetupProgress(output, fmt.Sprintf("agent start failed under load (attempt %d/%d): %s; retrying in %s", retry.attempt, agentStartAttempts, reason, backoff))
		l.logger.Warn().Str("session", sessionID).Int("attempt", retry.attempt).Err(err).Dur("backoff", backoff).Msg("agent start failed under load; retrying")
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return final()
		case <-timer.C:
		}
		// Cleanup/backoff can spend the remaining budget too.
		if deadline, ok := ctx.Deadline(); ctx.Err() != nil || (ok && time.Until(deadline) < agentStartMinRemaining) {
			return final()
		}
		var failed *failedAgentStart
		if errors.As(err, &failed) {
			// failStartBestEffort already killed it. Retry the kill only if necessary;
			// an ambiguous teardown must never permit a second pane to run the prompt.
			alive, probeErr := l.tmux.HasSessionStatus(ctx, failed.tmuxName)
			if probeErr != nil {
				err = fmt.Errorf("%w; inspect failed start pane: %v", err, probeErr)
				return final()
			}
			if alive {
				if killErr := l.tmux.KillSession(ctx, failed.tmuxName); killErr != nil {
					err = fmt.Errorf("%w; tear down failed start pane: %v", err, killErr)
					return final()
				}
			}
			if deleteErr := l.agentChats.DeleteByAgentSessionID(ctx, failed.agentSessionID); deleteErr != nil && !errors.Is(deleteErr, domain.ErrNotFound) {
				err = fmt.Errorf("%w; delete superseded chat: %v", err, deleteErr)
				return final()
			}
		}
		retry.attempt++
	}
}
