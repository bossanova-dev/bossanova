import { execFile } from 'node:child_process';
import { promisify } from 'node:util';

const execFileAsync = promisify(execFile);

// --- Types ---

export interface PrInfo {
  number: number;
  url: string;
}

export interface PrStatus {
  state: 'open' | 'closed' | 'merged';
  mergeable: boolean | null;
  title: string;
  headBranch: string;
  baseBranch: string;
}

export interface CheckResult {
  name: string;
  status: 'completed' | 'in_progress' | 'queued';
  conclusion: 'success' | 'failure' | 'neutral' | 'cancelled' | 'skipped' | 'timed_out' | null;
}

export type ChecksOverall = 'pending' | 'passed' | 'failed';

// --- GitHub API via gh CLI ---

/**
 * Create a draft pull request.
 * Runs from the worktree directory so `gh` picks up the correct repo.
 */
export async function createDraftPr(
  worktreePath: string,
  title: string,
  body: string,
  baseBranch: string,
): Promise<PrInfo> {
  const { stdout } = await execFileAsync(
    'gh',
    ['pr', 'create', '--draft', '--title', title, '--body', body, '--base', baseBranch, '--json', 'number,url'],
    { cwd: worktreePath },
  );

  const result = JSON.parse(stdout) as { number: number; url: string };
  return { number: result.number, url: result.url };
}

/**
 * Get the status of a pull request.
 */
export async function getPrStatus(worktreePath: string, prNumber: number): Promise<PrStatus> {
  const { stdout } = await execFileAsync(
    'gh',
    [
      'pr',
      'view',
      String(prNumber),
      '--json',
      'state,mergeable,title,headRefName,baseRefName',
    ],
    { cwd: worktreePath },
  );

  const raw = JSON.parse(stdout) as {
    state: string;
    mergeable: string;
    title: string;
    headRefName: string;
    baseRefName: string;
  };

  return {
    state: raw.state.toLowerCase() as PrStatus['state'],
    mergeable: raw.mergeable === 'MERGEABLE' ? true : raw.mergeable === 'CONFLICTING' ? false : null,
    title: raw.title,
    headBranch: raw.headRefName,
    baseBranch: raw.baseRefName,
  };
}

/**
 * Get the check results for a pull request.
 */
export async function getPrChecks(worktreePath: string, prNumber: number): Promise<CheckResult[]> {
  const { stdout } = await execFileAsync(
    'gh',
    ['pr', 'checks', String(prNumber), '--json', 'name,state,conclusion'],
    { cwd: worktreePath },
  );

  const raw = JSON.parse(stdout) as Array<{
    name: string;
    state: string;
    conclusion: string;
  }>;

  return raw.map((check) => ({
    name: check.name,
    status: check.state.toLowerCase() as CheckResult['status'],
    conclusion: (check.conclusion?.toLowerCase() || null) as CheckResult['conclusion'],
  }));
}

/**
 * Summarize the overall check state from individual check results.
 */
export function summarizeChecks(checks: CheckResult[]): ChecksOverall {
  if (checks.length === 0) return 'pending';

  const allCompleted = checks.every((c) => c.status === 'completed');
  if (!allCompleted) return 'pending';

  const allPassed = checks.every(
    (c) => c.conclusion === 'success' || c.conclusion === 'neutral' || c.conclusion === 'skipped',
  );
  return allPassed ? 'passed' : 'failed';
}

/**
 * Mark a draft PR as ready for review.
 */
export async function markReadyForReview(worktreePath: string, prNumber: number): Promise<void> {
  await execFileAsync('gh', ['pr', 'ready', String(prNumber)], {
    cwd: worktreePath,
  });
}

/**
 * Close a pull request.
 */
export async function closePr(worktreePath: string, prNumber: number): Promise<void> {
  await execFileAsync('gh', ['pr', 'close', String(prNumber)], {
    cwd: worktreePath,
  });
}

/**
 * Get the failed check run logs for a PR.
 * Used by the fix loop to provide Claude with failure context.
 */
export async function getFailedCheckLogs(
  worktreePath: string,
  prNumber: number,
): Promise<string> {
  try {
    const { stdout } = await execFileAsync(
      'gh',
      ['pr', 'checks', String(prNumber), '--json', 'name,state,conclusion,link', '--jq', '.[] | select(.conclusion == "FAILURE")'],
      { cwd: worktreePath },
    );
    return stdout.trim();
  } catch {
    return '';
  }
}
