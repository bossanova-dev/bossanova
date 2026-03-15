import type { Session, SessionCreateParams } from '@bossanova/shared';
import { appendToLog } from '~/claude/logger';
import type { ClaudeSupervisor } from '~/claude/supervisor';
import type { RepoStore } from '~/db/repos';
import type { SessionStore } from '~/db/sessions';
import { createWorktree, removeWorktree } from '~/git/worktree';

/**
 * Create a new session: insert DB record, create worktree, start Claude,
 * update session state through the lifecycle.
 */
export async function startSession(
  repos: RepoStore,
  sessions: SessionStore,
  params: SessionCreateParams,
  supervisor?: ClaudeSupervisor,
): Promise<Session> {
  const repo = repos.get(params.repoId);
  if (!repo) {
    throw new Error(`Repo not found: ${params.repoId}`);
  }

  // 1. Create the session record (state: creating_worktree)
  const session = sessions.create({
    repoId: params.repoId,
    title: params.title,
    plan: params.plan,
    baseBranch: repo.defaultBaseBranch,
  });

  // 2. Create the worktree
  const { worktreePath, branchName } = await createWorktree({
    repoPath: repo.localPath,
    worktreeBaseDir: repo.worktreeBaseDir,
    sessionId: session.id,
    title: session.title,
    setupScript: repo.setupScript,
  });

  // 3. Update session with worktree info and advance state
  sessions.update(session.id, {
    worktreePath,
    branchName,
    state: 'starting_claude',
  });

  // 4. Start Claude session if supervisor provided
  if (supervisor) {
    supervisor.setMessageCallback((sessionId, message) => {
      appendToLog(sessionId, message);

      // Update session state on Claude completion
      if (message.type === 'result') {
        if ('errors' in message && Array.isArray(message.errors)) {
          sessions.update(sessionId, {
            state: 'blocked',
            blockedReason: message.errors.join('; '),
          });
        } else {
          sessions.update(sessionId, { state: 'pushing_branch' });
        }
      }

      // Capture Claude SDK session ID from init
      if (message.type === 'system' && 'subtype' in message && message.subtype === 'init') {
        sessions.update(sessionId, { claudeSessionId: message.session_id });
      }
    });

    await supervisor.start(session.id, worktreePath, params.plan);
    sessions.update(session.id, { state: 'implementing_plan' });
  }

  // biome-ignore lint/style/noNonNullAssertion: row was just updated
  return sessions.get(session.id)!;
}

/**
 * Remove a session: stop Claude, clean up worktree, delete DB record.
 */
export async function removeSession(
  repos: RepoStore,
  sessions: SessionStore,
  sessionId: string,
  supervisor?: ClaudeSupervisor,
): Promise<boolean> {
  const session = sessions.get(sessionId);
  if (!session) return false;

  // Stop Claude session if running
  if (supervisor) {
    supervisor.remove(sessionId);
  }

  // Clean up worktree if one was created
  if (session.worktreePath) {
    const repo = repos.get(session.repoId);
    if (repo) {
      try {
        await removeWorktree(repo.localPath, session.worktreePath);
      } catch {
        // Worktree might already be gone — continue with removal
      }
    }
  }

  sessions.delete(sessionId);
  return true;
}
