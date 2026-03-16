import type { SessionStore } from '~/db/sessions';
import { pushBranch } from '~/git/push';
import { createDraftPr } from '~/github/client';

/**
 * Push branch to origin and create a draft PR.
 * Transitions: pushing_branch → opening_draft_pr → awaiting_checks
 */
export async function pushAndCreatePr(
  sessions: SessionStore,
  sessionId: string,
  worktreePath: string,
  branchName: string,
  title: string,
  plan: string,
  baseBranch: string,
): Promise<void> {
  // 1. Push branch
  sessions.update(sessionId, { state: 'pushing_branch' });
  await pushBranch(worktreePath, branchName);

  // 2. Create draft PR
  sessions.update(sessionId, { state: 'opening_draft_pr' });
  const prInfo = await createDraftPr(worktreePath, title, plan, baseBranch);

  // 3. Store PR info and advance to awaiting_checks
  sessions.update(sessionId, {
    prNumber: prInfo.number,
    prUrl: prInfo.url,
    state: 'awaiting_checks',
    lastCheckState: 'pending',
  });
}
