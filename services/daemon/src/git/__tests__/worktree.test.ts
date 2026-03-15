import { execSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { buildBranchName, createWorktree, removeWorktree } from '~/git/worktree';

describe('buildBranchName', () => {
  it('creates a kebab-case branch name', () => {
    expect(buildBranchName('Fix auth bug', 'abcd1234-5678')).toBe('boss/fix-auth-bug-abcd1234');
  });

  it('strips non-alphanumeric characters', () => {
    expect(buildBranchName("What's up? (test)", 'abcd1234-5678')).toBe(
      'boss/what-s-up-test-abcd1234',
    );
  });

  it('truncates long titles to 30 chars for slug', () => {
    const longTitle = 'This is a really long title that should be truncated because it is too long';
    const branch = buildBranchName(longTitle, 'abcd1234-5678');
    // slug is max 30 chars + "boss/" prefix + "-" + 8-char short id
    const slug = branch.replace('boss/', '').replace(/-[a-f0-9]{8}$/, '');
    expect(slug.length).toBeLessThanOrEqual(30);
  });

  it('handles titles with leading/trailing special chars', () => {
    expect(buildBranchName('---hello---', 'abcd1234-5678')).toBe('boss/hello-abcd1234');
  });
});

describe('createWorktree', () => {
  let tmpDir: string;
  let repoPath: string;
  let worktreeBaseDir: string;

  beforeEach(() => {
    tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'boss-wt-test-'));
    repoPath = path.join(tmpDir, 'repo');
    worktreeBaseDir = path.join(tmpDir, 'worktrees');

    // Create a real git repo with an initial commit
    fs.mkdirSync(repoPath);
    execSync('git init', { cwd: repoPath });
    execSync('git config user.email "test@test.com"', { cwd: repoPath });
    execSync('git config user.name "Test"', { cwd: repoPath });
    fs.writeFileSync(path.join(repoPath, 'README.md'), '# Test');
    execSync('git add . && git commit -m "init"', { cwd: repoPath });

    fs.mkdirSync(worktreeBaseDir, { recursive: true });
  });

  afterEach(() => {
    // Clean up worktrees before removing the repo
    try {
      execSync('git worktree prune', { cwd: repoPath });
    } catch {
      // Repo might already be gone
    }
    fs.rmSync(tmpDir, { recursive: true, force: true });
  });

  it('creates a worktree at the expected path', async () => {
    const result = await createWorktree({
      repoPath,
      worktreeBaseDir,
      sessionId: 'test-session-1234',
      title: 'Fix the bug',
      setupScript: null,
    });

    expect(result.worktreePath).toBe(path.join(worktreeBaseDir, 'test-session-1234'));
    expect(result.branchName).toBe('boss/fix-the-bug-test-ses');
    expect(fs.existsSync(result.worktreePath)).toBe(true);

    // Verify git sees the worktree
    const worktrees = execSync('git worktree list', { cwd: repoPath, encoding: 'utf8' });
    expect(worktrees).toContain('test-session-1234');
  });

  it('creates a branch with the expected name', async () => {
    const result = await createWorktree({
      repoPath,
      worktreeBaseDir,
      sessionId: 'abc12345-session',
      title: 'Add feature',
      setupScript: null,
    });

    const branches = execSync('git branch', { cwd: repoPath, encoding: 'utf8' });
    expect(branches).toContain(result.branchName);
  });

  it('runs setupScript in the worktree', async () => {
    const result = await createWorktree({
      repoPath,
      worktreeBaseDir,
      sessionId: 'setup-test-1234',
      title: 'Setup test',
      setupScript: 'echo "setup done" > setup-marker.txt',
    });

    const marker = path.join(result.worktreePath, 'setup-marker.txt');
    expect(fs.existsSync(marker)).toBe(true);
    expect(fs.readFileSync(marker, 'utf8').trim()).toBe('setup done');
  });

  it('rejects when repo path is invalid', async () => {
    await expect(
      createWorktree({
        repoPath: '/nonexistent/path',
        worktreeBaseDir,
        sessionId: 'bad-test-1234567',
        title: 'Bad path',
        setupScript: null,
      }),
    ).rejects.toThrow();
  });
});

describe('removeWorktree', () => {
  let tmpDir: string;
  let repoPath: string;
  let worktreeBaseDir: string;

  beforeEach(() => {
    tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'boss-wt-rm-test-'));
    repoPath = path.join(tmpDir, 'repo');
    worktreeBaseDir = path.join(tmpDir, 'worktrees');

    fs.mkdirSync(repoPath);
    execSync('git init', { cwd: repoPath });
    execSync('git config user.email "test@test.com"', { cwd: repoPath });
    execSync('git config user.name "Test"', { cwd: repoPath });
    fs.writeFileSync(path.join(repoPath, 'README.md'), '# Test');
    execSync('git add . && git commit -m "init"', { cwd: repoPath });
    fs.mkdirSync(worktreeBaseDir, { recursive: true });
  });

  afterEach(() => {
    try {
      execSync('git worktree prune', { cwd: repoPath });
    } catch {
      // Repo might already be gone
    }
    fs.rmSync(tmpDir, { recursive: true, force: true });
  });

  it('removes an existing worktree', async () => {
    const result = await createWorktree({
      repoPath,
      worktreeBaseDir,
      sessionId: 'rm-test-12345678',
      title: 'Remove me',
      setupScript: null,
    });

    expect(fs.existsSync(result.worktreePath)).toBe(true);

    await removeWorktree(repoPath, result.worktreePath);

    expect(fs.existsSync(result.worktreePath)).toBe(false);
    const worktrees = execSync('git worktree list', { cwd: repoPath, encoding: 'utf8' });
    expect(worktrees).not.toContain('rm-test-12345678');
  });

  it('rejects when worktree path is invalid', async () => {
    await expect(removeWorktree(repoPath, '/nonexistent/worktree')).rejects.toThrow();
  });
});
