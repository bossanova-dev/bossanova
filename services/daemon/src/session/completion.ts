import type { Session } from '@bossanova/shared';
import type { RepoStore } from '~/db/repos';
import type { SessionStore } from '~/db/sessions';
import { removeWorktree } from '~/git/worktree';
import { markReadyForReview } from '~/github/client';

/**
 * Check if a session is ready to be marked for review.
 * Conditions: state is green_draft (checks passed while plan is complete).
 */
export function isReadyForReview(session: Session): boolean {
  return session.state === 'green_draft' && session.lastCheckState === 'passed';
}

/**
 * Mark a session's PR as ready for review.
 * Transitions: green_draft → ready_for_review
 */
export async function transitionToReadyForReview(
  sessions: SessionStore,
  session: Session,
): Promise<void> {
  if (!session.worktreePath || !session.prNumber) return;

  await markReadyForReview(session.worktreePath, session.prNumber);
  sessions.update(session.id, { state: 'ready_for_review' });
}

/**
 * Handle a merged PR: clean up worktree and update state.
 */
export async function handlePrMerged(
  sessions: SessionStore,
  repos: RepoStore,
  session: Session,
): Promise<void> {
  sessions.update(session.id, { state: 'merged' });

  // Clean up worktree
  if (session.worktreePath) {
    const repo = repos.get(session.repoId);
    if (repo) {
      try {
        await removeWorktree(repo.localPath, session.worktreePath);
      } catch {
        // Worktree may already be gone
      }
    }
  }
}

/**
 * Check all sessions in green_draft state and mark as ready for review.
 * Returns number of sessions transitioned.
 */
export async function processReadyForReview(sessions: SessionStore): Promise<number> {
  const allSessions = sessions.list();
  let transitioned = 0;

  for (const session of allSessions) {
    if (isReadyForReview(session)) {
      try {
        await transitionToReadyForReview(sessions, session);
        transitioned++;
      } catch {
        // Individual transition failure shouldn't stop others
      }
    }
  }

  return transitioned;
}
