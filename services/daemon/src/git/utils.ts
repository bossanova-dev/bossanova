import { execFile } from 'node:child_process';
import { promisify } from 'node:util';

const execFileAsync = promisify(execFile);

/** Get the current HEAD SHA. */
export async function getCurrentSha(cwd: string): Promise<string> {
  const { stdout } = await execFileAsync('git', ['rev-parse', 'HEAD'], { cwd });
  return stdout.trim();
}

/** Get the origin remote URL, or empty string if no remote. */
export async function getOriginUrl(cwd: string): Promise<string> {
  try {
    const { stdout } = await execFileAsync('git', ['remote', 'get-url', 'origin'], { cwd });
    return stdout.trim();
  } catch {
    return '';
  }
}

/** Get the default branch name (e.g. "main" or "master"). */
export async function getDefaultBranch(cwd: string): Promise<string> {
  try {
    const { stdout } = await execFileAsync(
      'git',
      ['symbolic-ref', 'refs/remotes/origin/HEAD', '--short'],
      { cwd },
    );
    // Returns "origin/main" → strip "origin/"
    return stdout.trim().replace(/^origin\//, '');
  } catch {
    // Fallback: check common branch names
    try {
      await execFileAsync('git', ['rev-parse', '--verify', 'refs/heads/main'], { cwd });
      return 'main';
    } catch {
      return 'master';
    }
  }
}

/** Check whether cwd is inside a git worktree (not the main working tree). */
export async function isInsideWorktree(cwd: string): Promise<boolean> {
  try {
    const { stdout } = await execFileAsync('git', ['rev-parse', '--git-common-dir'], { cwd });
    const commonDir = stdout.trim();
    return commonDir !== '.git';
  } catch {
    return false;
  }
}

/** Get the git common dir (shared .git directory for worktrees). */
export async function getGitCommonDir(cwd: string): Promise<string> {
  const { stdout } = await execFileAsync('git', ['rev-parse', '--git-common-dir'], { cwd });
  return stdout.trim();
}

/** Fetch the latest from origin. */
export async function fetchLatest(cwd: string): Promise<void> {
  await execFileAsync('git', ['fetch', 'origin'], { cwd, timeout: 30_000 });
}

/** Check if the current branch has merge conflicts with a base branch. */
export async function hasConflictsWithBase(cwd: string, baseBranch: string): Promise<boolean> {
  try {
    // Try a merge-tree check (dry run)
    const { stdout: headSha } = await execFileAsync('git', ['rev-parse', 'HEAD'], { cwd });
    const { stdout: baseSha } = await execFileAsync('git', ['rev-parse', `origin/${baseBranch}`], {
      cwd,
    });
    const { stdout: mergeBaseSha } = await execFileAsync(
      'git',
      ['merge-base', headSha.trim(), baseSha.trim()],
      { cwd },
    );

    const { stdout } = await execFileAsync(
      'git',
      ['merge-tree', mergeBaseSha.trim(), headSha.trim(), baseSha.trim()],
      { cwd },
    );

    // If merge-tree output contains conflict markers, there are conflicts
    return stdout.includes('<<<<<<<');
  } catch {
    // If we can't determine, assume no conflicts
    return false;
  }
}
