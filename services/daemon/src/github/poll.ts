import type { Session } from '@bossanova/shared';
import type { RepoStore } from '~/db/repos';
import type { SessionStore } from '~/db/sessions';
import { getPrChecks, getPrStatus, summarizeChecks } from '~/github/client';
import { handlePrMerged, processReadyForReview } from '~/session/completion';

const DEFAULT_POLL_INTERVAL_MS = 60_000;

export interface PollResult {
  sessionId: string;
  checksOverall: 'pending' | 'passed' | 'failed';
  hasConflict: boolean;
  prState: 'open' | 'closed' | 'merged';
}

/**
 * Check the PR status for a single session.
 * Returns null if the session doesn't have a PR or worktree.
 */
export async function pollSession(
  session: Session,
  repos: RepoStore,
): Promise<PollResult | null> {
  if (!session.prNumber || !session.worktreePath) return null;

  const repo = repos.get(session.repoId);
  if (!repo) return null;

  const [status, checks] = await Promise.all([
    getPrStatus(session.worktreePath, session.prNumber),
    getPrChecks(session.worktreePath, session.prNumber),
  ]);

  return {
    sessionId: session.id,
    checksOverall: summarizeChecks(checks),
    hasConflict: status.mergeable === false,
    prState: status.state,
  };
}

/**
 * Process a poll result and update session state accordingly.
 * Returns the session if it was merged (for cleanup).
 */
export function processPollResult(
  sessions: SessionStore,
  result: PollResult,
): { merged: boolean; sessionId: string } {
  const session = sessions.get(result.sessionId);
  if (!session) return { merged: false, sessionId: result.sessionId };

  // Handle PR merged/closed
  if (result.prState === 'merged') {
    sessions.update(result.sessionId, { state: 'merged' });
    return { merged: true, sessionId: result.sessionId };
  }
  if (result.prState === 'closed') {
    sessions.update(result.sessionId, { state: 'closed' });
    return { merged: false, sessionId: result.sessionId };
  }

  const noMerge = { merged: false, sessionId: result.sessionId };

  // Only act on sessions in pollable states
  const pollableStates = ['awaiting_checks', 'green_draft', 'ready_for_review'];
  if (!pollableStates.includes(session.state)) return noMerge;

  // Update lastCheckState
  if (result.checksOverall !== 'pending') {
    sessions.update(result.sessionId, { lastCheckState: result.checksOverall });
  }

  // Handle conflict detection
  if (result.hasConflict && session.state === 'awaiting_checks') {
    sessions.update(result.sessionId, {
      state: 'fixing_checks',
      blockedReason: 'Merge conflict detected',
      attemptCount: session.attemptCount + 1,
    });
    return noMerge;
  }

  // Handle check results for awaiting_checks state
  if (session.state === 'awaiting_checks') {
    if (result.checksOverall === 'passed') {
      sessions.update(result.sessionId, {
        state: 'green_draft',
        lastCheckState: 'passed',
      });
    } else if (result.checksOverall === 'failed') {
      if (session.attemptCount + 1 >= 5) {
        sessions.update(result.sessionId, {
          state: 'blocked',
          lastCheckState: 'failed',
          blockedReason: 'Max fix attempts reached',
        });
      } else {
        sessions.update(result.sessionId, {
          state: 'fixing_checks',
          lastCheckState: 'failed',
          attemptCount: session.attemptCount + 1,
        });
      }
    }
  }

  return noMerge;
}

/**
 * Poll all sessions in pollable states, handle merged PRs, and
 * process ready-for-review transitions.
 * Returns the number of sessions polled.
 */
export async function pollAllSessions(
  sessions: SessionStore,
  repos: RepoStore,
): Promise<number> {
  const allSessions = sessions.list();
  const pollable = allSessions.filter(
    (s) =>
      ['awaiting_checks', 'green_draft', 'ready_for_review'].includes(s.state) &&
      s.prNumber != null &&
      s.worktreePath != null,
  );

  let polled = 0;
  for (const session of pollable) {
    try {
      const result = await pollSession(session, repos);
      if (result) {
        const { merged, sessionId } = processPollResult(sessions, result);
        polled++;

        // Clean up worktree for merged PRs
        if (merged) {
          const mergedSession = sessions.get(sessionId);
          if (mergedSession) {
            await handlePrMerged(sessions, repos, mergedSession).catch(() => {
              // Cleanup failure is non-fatal
            });
          }
        }
      }
    } catch {
      // Individual poll failure shouldn't stop others
    }
  }

  // Process ready-for-review transitions for any newly green sessions
  await processReadyForReview(sessions).catch(() => {
    // Non-fatal
  });

  return polled;
}

/**
 * Start a periodic polling loop. Returns a cleanup function.
 */
export function startPolling(
  sessions: SessionStore,
  repos: RepoStore,
  intervalMs = DEFAULT_POLL_INTERVAL_MS,
): () => void {
  const timer = setInterval(() => {
    pollAllSessions(sessions, repos).catch(() => {
      // Polling errors are non-fatal
    });
  }, intervalMs);

  return () => clearInterval(timer);
}
