import { execFile } from 'node:child_process';
import { promisify } from 'node:util';

const execFileAsync = promisify(execFile);

/**
 * Push a branch to the origin remote and set upstream tracking.
 * Returns the HEAD SHA after pushing.
 */
export async function pushBranch(worktreePath: string, branchName: string): Promise<string> {
  await execFileAsync('git', ['push', '-u', 'origin', branchName], {
    cwd: worktreePath,
  });

  const { stdout } = await execFileAsync('git', ['rev-parse', 'HEAD'], {
    cwd: worktreePath,
  });

  return stdout.trim();
}
