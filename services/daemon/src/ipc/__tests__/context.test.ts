import { execSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { DatabaseService } from '~/db/database';
import { RepoStore } from '~/db/repos';
import { SessionStore } from '~/db/sessions';
import { resolveContext } from '~/ipc/handlers/context';

const noopLogger = { debug: () => {}, info: () => {}, warn: () => {}, error: () => {} };

function createTestDb(): DatabaseService {
  const config = { dbPath: ':memory:', socketPath: '', logLevel: 'info' as const };
  const db = new DatabaseService(config, noopLogger);
  db.initialize(':memory:');
  return db;
}

describe('resolveContext', () => {
  let db: DatabaseService;
  let repos: RepoStore;
  let sessions: SessionStore;
  let tmpDir: string;

  beforeEach(() => {
    db = createTestDb();
    repos = new RepoStore(db);
    sessions = new SessionStore(db);
    // Resolve real path to handle macOS /var → /private/var symlink
    tmpDir = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), 'bossd-ctx-')));
  });

  afterEach(() => {
    db.close();
    fs.rmSync(tmpDir, { recursive: true, force: true });
  });

  it('returns "none" for a non-git directory', () => {
    const nonGitDir = fs.mkdtempSync(path.join(os.tmpdir(), 'bossd-nongit-'));
    try {
      const result = resolveContext(nonGitDir, repos, sessions);
      expect(result.type).toBe('none');
    } finally {
      fs.rmSync(nonGitDir, { recursive: true, force: true });
    }
  });

  it('returns "unregistered_repo" for git repo not in database', () => {
    // Create a temp git repo
    execSync('git init', { cwd: tmpDir, stdio: 'pipe' });

    const result = resolveContext(tmpDir, repos, sessions);
    expect(result.type).toBe('unregistered_repo');
    if (result.type === 'unregistered_repo') {
      expect(result.localPath).toBe(tmpDir);
    }
  });

  it('returns "repo" for a registered repo', () => {
    execSync('git init', { cwd: tmpDir, stdio: 'pipe' });

    const repo = repos.register({
      displayName: 'Test',
      localPath: tmpDir,
      originUrl: '',
      worktreeBaseDir: `${tmpDir}/.worktrees`,
    });

    const result = resolveContext(tmpDir, repos, sessions);
    expect(result.type).toBe('repo');
    if (result.type === 'repo') {
      expect(result.repoId).toBe(repo.id);
    }
  });

  it('returns "repo" from a subdirectory of registered repo', () => {
    execSync('git init', { cwd: tmpDir, stdio: 'pipe' });
    const subDir = path.join(tmpDir, 'src');
    fs.mkdirSync(subDir);

    repos.register({
      displayName: 'Test',
      localPath: tmpDir,
      originUrl: '',
      worktreeBaseDir: `${tmpDir}/.worktrees`,
    });

    const result = resolveContext(subDir, repos, sessions);
    expect(result.type).toBe('repo');
  });

  it('returns "session" when cwd matches a session worktree', () => {
    execSync('git init', { cwd: tmpDir, stdio: 'pipe' });

    const repo = repos.register({
      displayName: 'Test',
      localPath: tmpDir,
      originUrl: '',
      worktreeBaseDir: `${tmpDir}/.worktrees`,
    });

    const session = sessions.create({
      repoId: repo.id,
      title: 'Fix bug',
      plan: 'plan',
      baseBranch: 'main',
    });

    // Set the session's worktree to the tmpDir (simulating it being inside a worktree)
    sessions.update(session.id, { worktreePath: tmpDir });

    const result = resolveContext(tmpDir, repos, sessions);
    expect(result.type).toBe('session');
    if (result.type === 'session') {
      expect(result.sessionId).toBe(session.id);
      expect(result.repoId).toBe(repo.id);
    }
  });
});
