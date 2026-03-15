import { execSync } from 'node:child_process';
import { resolve } from 'node:path';
import type { ContextResolveResult } from '@bossanova/shared';
import type { RepoStore } from '~/db/repos';
import type { SessionStore } from '~/db/sessions';

/**
 * Resolve cwd to the appropriate context:
 * 1. Inside an active session worktree? → session context
 * 2. Inside a registered repo? → repo context
 * 3. Inside an unregistered Git repo? → unregistered_repo context
 * 4. None of the above → none
 */
export function resolveContext(
  cwd: string,
  repos: RepoStore,
  sessions: SessionStore,
): ContextResolveResult {
  // Get the git repo root for the cwd, if any
  let gitRoot: string;
  try {
    gitRoot = execSync('git rev-parse --show-toplevel', {
      cwd,
      encoding: 'utf8',
      stdio: ['pipe', 'pipe', 'pipe'],
    }).trim();
  } catch {
    return { type: 'none' };
  }

  // Check if this is a worktree (linked to a main repo)
  let commonDir: string;
  try {
    commonDir = execSync('git rev-parse --git-common-dir', {
      cwd,
      encoding: 'utf8',
      stdio: ['pipe', 'pipe', 'pipe'],
    }).trim();
  } catch {
    commonDir = '';
  }

  // If commonDir differs from the normal .git, this is a worktree
  // The main repo path is the parent of the common dir
  const isWorktree = commonDir !== '' && commonDir !== '.git' && !commonDir.endsWith('/.git');

  let mainRepoPath = gitRoot;
  if (isWorktree) {
    // Resolve the absolute path of the common dir, then find its repo root
    const absCommonDir = resolve(gitRoot, commonDir);
    // Common dir is the .git dir of the main repo — its parent is the repo root
    mainRepoPath = resolve(absCommonDir, '..');
  }

  // 1. Check if cwd is inside a session worktree
  const allSessions = sessions.list();
  for (const session of allSessions) {
    if (session.worktreePath && gitRoot.startsWith(session.worktreePath)) {
      return { type: 'session', sessionId: session.id, repoId: session.repoId };
    }
  }

  // 2. Check if cwd is inside a registered repo
  const repo = repos.findByPath(mainRepoPath) ?? repos.findByPath(gitRoot);
  if (repo) {
    return { type: 'repo', repoId: repo.id };
  }

  // 3. Unregistered git repo
  let originUrl = '';
  try {
    originUrl = execSync('git remote get-url origin', {
      cwd: gitRoot,
      encoding: 'utf8',
      stdio: ['pipe', 'pipe', 'pipe'],
    }).trim();
  } catch {
    // No remote
  }

  return { type: 'unregistered_repo', localPath: gitRoot, originUrl };
}
