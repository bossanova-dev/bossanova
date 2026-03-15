import { execFile } from 'node:child_process';
import { promisify } from 'node:util';

const execFileAsync = promisify(execFile);

export interface CreateWorktreeParams {
  repoPath: string;
  worktreeBaseDir: string;
  sessionId: string;
  title: string;
  setupScript: string | null;
}

export interface CreateWorktreeResult {
  worktreePath: string;
  branchName: string;
}

/**
 * Generate a branch name from a session title.
 * Format: boss/<slug>-<short-id> where slug is kebab-case, max 30 chars total for slug portion.
 */
export function buildBranchName(title: string, sessionId: string): string {
  const shortId = sessionId.slice(0, 8);
  const slug = title
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-|-$/g, '')
    .slice(0, 30);
  return `boss/${slug}-${shortId}`;
}

/**
 * Create a git worktree for a session.
 * Runs `git worktree add <path> -b <branch>` from the repo root.
 * If the repo has a setupScript configured, executes it in the new worktree.
 */
export async function createWorktree(params: CreateWorktreeParams): Promise<CreateWorktreeResult> {
  const { repoPath, worktreeBaseDir, sessionId, title, setupScript } = params;
  const branchName = buildBranchName(title, sessionId);
  const worktreePath = `${worktreeBaseDir}/${sessionId}`;

  await execFileAsync('git', ['worktree', 'add', worktreePath, '-b', branchName], {
    cwd: repoPath,
  });

  if (setupScript) {
    await execFileAsync('sh', ['-c', setupScript], {
      cwd: worktreePath,
      timeout: 120_000,
    });
  }

  return { worktreePath, branchName };
}

/**
 * Remove a git worktree and prune the reference.
 * Runs `git worktree remove <path> --force` from the repo root.
 */
export async function removeWorktree(repoPath: string, worktreePath: string): Promise<void> {
  await execFileAsync('git', ['worktree', 'remove', worktreePath, '--force'], {
    cwd: repoPath,
  });
}
