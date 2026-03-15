import { execSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { pushBranch } from '~/git/push';

describe('pushBranch', () => {
  let tmpDir: string;
  let bareRepoPath: string;
  let clonePath: string;
  let worktreePath: string;
  const branchName = 'boss/test-push-12345678';

  beforeEach(() => {
    tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'boss-push-test-'));
    bareRepoPath = path.join(tmpDir, 'bare.git');
    clonePath = path.join(tmpDir, 'clone');
    worktreePath = path.join(tmpDir, 'worktree');

    // Create a bare repo as the remote
    execSync(`git init --bare ${bareRepoPath}`);

    // Clone it to get a working copy
    execSync(`git clone ${bareRepoPath} ${clonePath}`);
    execSync('git config user.email "test@test.com"', { cwd: clonePath });
    execSync('git config user.name "Test"', { cwd: clonePath });
    fs.writeFileSync(path.join(clonePath, 'README.md'), '# Test');
    execSync('git add . && git commit -m "init" && git push', { cwd: clonePath });

    // Create a worktree with a new branch
    execSync(`git worktree add ${worktreePath} -b ${branchName}`, { cwd: clonePath });
    fs.writeFileSync(path.join(worktreePath, 'new-file.txt'), 'hello');
    execSync('git add . && git commit -m "add file"', { cwd: worktreePath });
  });

  afterEach(() => {
    try {
      execSync('git worktree prune', { cwd: clonePath });
    } catch {
      // Already gone
    }
    fs.rmSync(tmpDir, { recursive: true, force: true });
  });

  it('pushes a branch and returns the HEAD SHA', async () => {
    const sha = await pushBranch(worktreePath, branchName);

    expect(sha).toMatch(/^[0-9a-f]{40}$/);

    // Verify the branch exists on the remote
    const remoteBranches = execSync('git branch -r', { cwd: clonePath, encoding: 'utf8' });
    expect(remoteBranches).toContain(`origin/${branchName}`);
  });

  it('sets up upstream tracking', async () => {
    await pushBranch(worktreePath, branchName);

    const upstream = execSync('git rev-parse --abbrev-ref @{upstream}', {
      cwd: worktreePath,
      encoding: 'utf8',
    }).trim();

    expect(upstream).toBe(`origin/${branchName}`);
  });
});
