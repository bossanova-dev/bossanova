import type { Session, SessionCreateParams } from '@bossanova/shared';
import type { RepoStore } from '~/db/repos';
import type { SessionStore } from '~/db/sessions';
import { createWorktree, removeWorktree } from '~/git/worktree';

/**
 * Create a new session: insert DB record, create worktree, update session
 * with worktree path and branch name, advance state to starting_claude.
 */
export async function startSession(
  repos: RepoStore,
  sessions: SessionStore,
  params: SessionCreateParams,
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

  // biome-ignore lint/style/noNonNullAssertion: row was just updated
  return sessions.get(session.id)!;
}

/**
 * Remove a session: clean up worktree if present, delete DB record.
 */
export async function removeSession(
  repos: RepoStore,
  sessions: SessionStore,
  sessionId: string,
): Promise<boolean> {
  const session = sessions.get(sessionId);
  if (!session) return false;

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
