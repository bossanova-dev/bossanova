import { execSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { DatabaseService } from '~/db/database';
import { RepoStore } from '~/db/repos';
import { SessionStore } from '~/db/sessions';
import { removeSession, startSession } from '~/session/lifecycle';

function createTestDb(): DatabaseService {
  const config = { dbPath: ':memory:', socketPath: '', logLevel: 'info' as const };
  const logger = { debug: () => {}, info: () => {}, warn: () => {}, error: () => {} };
  const db = new DatabaseService(config, logger);
  db.initialize(':memory:');
  return db;
}

describe('session lifecycle', () => {
  let tmpDir: string;
  let repoPath: string;
  let worktreeBaseDir: string;
  let db: DatabaseService;
  let repoStore: RepoStore;
  let sessionStore: SessionStore;
  let repoId: string;

  beforeEach(() => {
    tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'boss-lifecycle-test-'));
    repoPath = path.join(tmpDir, 'repo');
    worktreeBaseDir = path.join(tmpDir, 'worktrees');

    // Create a real git repo
    fs.mkdirSync(repoPath);
    execSync('git init', { cwd: repoPath });
    execSync('git config user.email "test@test.com"', { cwd: repoPath });
    execSync('git config user.name "Test"', { cwd: repoPath });
    fs.writeFileSync(path.join(repoPath, 'README.md'), '# Test');
    execSync('git add . && git commit -m "init"', { cwd: repoPath });

    fs.mkdirSync(worktreeBaseDir, { recursive: true });

    // Set up DB
    db = createTestDb();
    repoStore = new RepoStore(db);
    sessionStore = new SessionStore(db);

    const repo = repoStore.register({
      displayName: 'Test Repo',
      localPath: repoPath,
      originUrl: 'git@github.com:user/test.git',
      worktreeBaseDir,
    });
    repoId = repo.id;
  });

  afterEach(() => {
    try {
      execSync('git worktree prune', { cwd: repoPath });
    } catch {
      // Repo might already be gone
    }
    db.close();
    fs.rmSync(tmpDir, { recursive: true, force: true });
  });

  describe('startSession', () => {
    it('creates session, worktree, and advances to starting_claude', async () => {
      const session = await startSession(repoStore, sessionStore, {
        repoId,
        title: 'Fix auth bug',
        plan: 'Fix the failing test',
      });

      expect(session.state).toBe('starting_claude');
      expect(session.worktreePath).toContain(session.id);
      expect(session.branchName).toMatch(/^boss\/fix-auth-bug-/);
      expect(fs.existsSync(session.worktreePath ?? '')).toBe(true);

      // Verify git sees the worktree
      const worktrees = execSync('git worktree list', { cwd: repoPath, encoding: 'utf8' });
      expect(worktrees).toContain(session.id);
    });

    it('throws for unknown repo', async () => {
      await expect(
        startSession(repoStore, sessionStore, {
          repoId: 'nonexistent',
          title: 'Test',
          plan: 'Plan',
        }),
      ).rejects.toThrow('Repo not found');
    });

    it('runs setup script during worktree creation', async () => {
      // Update repo to have a setup script
      repoStore.remove(repoId);
      const repo = repoStore.register({
        displayName: 'Test Repo',
        localPath: repoPath,
        originUrl: 'git@github.com:user/test.git',
        worktreeBaseDir,
        setupScript: 'echo "ready" > .setup-done',
      });

      const session = await startSession(repoStore, sessionStore, {
        repoId: repo.id,
        title: 'Setup test',
        plan: 'Test setup script',
      });

      const marker = path.join(session.worktreePath ?? '', '.setup-done');
      expect(fs.existsSync(marker)).toBe(true);
      expect(fs.readFileSync(marker, 'utf8').trim()).toBe('ready');
    });
  });

  describe('removeSession', () => {
    it('removes worktree and deletes session record', async () => {
      const session = await startSession(repoStore, sessionStore, {
        repoId,
        title: 'To remove',
        plan: 'Will be removed',
      });

      const wtPath = session.worktreePath ?? '';
      expect(fs.existsSync(wtPath)).toBe(true);

      const removed = await removeSession(repoStore, sessionStore, session.id);
      expect(removed).toBe(true);
      expect(fs.existsSync(wtPath)).toBe(false);
      expect(sessionStore.get(session.id)).toBeNull();
    });

    it('returns false for nonexistent session', async () => {
      const removed = await removeSession(repoStore, sessionStore, 'nonexistent');
      expect(removed).toBe(false);
    });

    it('still deletes session even if worktree is already gone', async () => {
      const session = await startSession(repoStore, sessionStore, {
        repoId,
        title: 'Already cleaned',
        plan: 'Worktree will be pre-removed',
      });

      // Manually remove the worktree
      if (session.worktreePath) {
        execSync(`git worktree remove ${session.worktreePath} --force`, { cwd: repoPath });
      }

      const removed = await removeSession(repoStore, sessionStore, session.id);
      expect(removed).toBe(true);
      expect(sessionStore.get(session.id)).toBeNull();
    });
  });
});
