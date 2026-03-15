import { execSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import {
  getCurrentSha,
  getDefaultBranch,
  getGitCommonDir,
  getOriginUrl,
  isInsideWorktree,
} from '~/git/utils';

describe('git utils', () => {
  let tmpDir: string;
  let repoPath: string;

  beforeEach(() => {
    tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'boss-gitutils-test-'));
    repoPath = path.join(tmpDir, 'repo');

    fs.mkdirSync(repoPath);
    execSync('git init', { cwd: repoPath });
    execSync('git config user.email "test@test.com"', { cwd: repoPath });
    execSync('git config user.name "Test"', { cwd: repoPath });
    fs.writeFileSync(path.join(repoPath, 'README.md'), '# Test');
    execSync('git add . && git commit -m "init"', { cwd: repoPath });
  });

  afterEach(() => {
    try {
      execSync('git worktree prune', { cwd: repoPath });
    } catch {
      // Repo might already be gone
    }
    fs.rmSync(tmpDir, { recursive: true, force: true });
  });

  describe('getCurrentSha', () => {
    it('returns a 40-char hex SHA', async () => {
      const sha = await getCurrentSha(repoPath);
      expect(sha).toMatch(/^[0-9a-f]{40}$/);
    });
  });

  describe('getOriginUrl', () => {
    it('returns empty string when no remote', async () => {
      const url = await getOriginUrl(repoPath);
      expect(url).toBe('');
    });

    it('returns the origin URL when set', async () => {
      execSync('git remote add origin git@github.com:user/test.git', { cwd: repoPath });
      const url = await getOriginUrl(repoPath);
      expect(url).toBe('git@github.com:user/test.git');
    });
  });

  describe('getDefaultBranch', () => {
    it('returns "main" when main branch exists', async () => {
      // Our test repo has a main branch (default init branch name)
      const branch = await getDefaultBranch(repoPath);
      expect(['main', 'master']).toContain(branch);
    });
  });

  describe('isInsideWorktree', () => {
    it('returns false for the main working tree', async () => {
      const result = await isInsideWorktree(repoPath);
      expect(result).toBe(false);
    });

    it('returns true for a linked worktree', async () => {
      const wtPath = path.join(tmpDir, 'wt');
      execSync(`git worktree add ${wtPath} -b test-branch`, { cwd: repoPath });

      const result = await isInsideWorktree(wtPath);
      expect(result).toBe(true);
    });
  });

  describe('getGitCommonDir', () => {
    it('returns .git for the main working tree', async () => {
      const commonDir = await getGitCommonDir(repoPath);
      expect(commonDir).toBe('.git');
    });

    it('returns the shared .git path for a worktree', async () => {
      const wtPath = path.join(tmpDir, 'wt2');
      execSync(`git worktree add ${wtPath} -b test-branch-2`, { cwd: repoPath });

      const commonDir = await getGitCommonDir(wtPath);
      expect(commonDir).toContain(path.join(repoPath, '.git'));
    });
  });
});
