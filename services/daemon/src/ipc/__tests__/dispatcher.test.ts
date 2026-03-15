import { execSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { RpcErrorCode } from '@bossanova/shared';
import type { JsonRpcRequest, Repo } from '@bossanova/shared';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { AttemptStore } from '~/db/attempts';
import { DatabaseService } from '~/db/database';
import { RepoStore } from '~/db/repos';
import { SessionStore } from '~/db/sessions';
import { Dispatcher } from '~/ipc/dispatcher';

const noopLogger = { debug: () => {}, info: () => {}, warn: () => {}, error: () => {} };

function createTestDb(): DatabaseService {
  const config = { dbPath: ':memory:', socketPath: '', logLevel: 'info' as const };
  const db = new DatabaseService(config, noopLogger);
  db.initialize(':memory:');
  return db;
}

function rpc(method: string, params: unknown = {}, id: number | string = 1): JsonRpcRequest {
  return { jsonrpc: '2.0', method, params, id };
}

describe('Dispatcher', () => {
  let tmpDir: string;
  let repoPath: string;
  let worktreeBaseDir: string;
  let db: DatabaseService;
  let repos: RepoStore;
  let sessions: SessionStore;
  let attempts: AttemptStore;
  let dispatcher: Dispatcher;

  beforeEach(() => {
    tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'boss-dispatch-test-'));
    repoPath = path.join(tmpDir, 'repo');
    worktreeBaseDir = path.join(tmpDir, 'worktrees');

    // Create a real git repo for session.create tests
    fs.mkdirSync(repoPath);
    execSync('git init', { cwd: repoPath });
    execSync('git config user.email "test@test.com"', { cwd: repoPath });
    execSync('git config user.name "Test"', { cwd: repoPath });
    fs.writeFileSync(path.join(repoPath, 'README.md'), '# Test');
    execSync('git add . && git commit -m "init"', { cwd: repoPath });
    fs.mkdirSync(worktreeBaseDir, { recursive: true });

    db = createTestDb();
    repos = new RepoStore(db);
    sessions = new SessionStore(db);
    attempts = new AttemptStore(db);
    dispatcher = new Dispatcher(repos, sessions, attempts, noopLogger);
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

  it('returns method not found for unknown method', async () => {
    const res = await dispatcher.dispatch(rpc('unknown.method'));
    expect(res.error?.code).toBe(RpcErrorCode.MethodNotFound);
    expect(res.error?.message).toContain('unknown.method');
  });

  it('lists registered methods', () => {
    const methods = dispatcher.getMethodNames();
    expect(methods).toContain('repo.list');
    expect(methods).toContain('repo.register');
    expect(methods).toContain('repo.remove');
    expect(methods).toContain('session.list');
    expect(methods).toContain('session.create');
    expect(methods).toContain('session.get');
    expect(methods).toContain('session.remove');
    expect(methods).toContain('session.attempts');
    expect(methods).toContain('context.resolve');
  });

  // --- Repo handlers ---

  it('repo.list returns empty array initially', async () => {
    const res = await dispatcher.dispatch(rpc('repo.list'));
    expect(res.result).toEqual([]);
  });

  it('repo.remove returns removed: false for unknown id', async () => {
    const res = await dispatcher.dispatch(rpc('repo.remove', { repoId: 'nonexistent' }));
    expect(res.result).toEqual({ removed: false });
  });

  // --- Session handlers ---

  it('session.list returns empty array initially', async () => {
    const res = await dispatcher.dispatch(rpc('session.list'));
    expect(res.result).toEqual([]);
  });

  it('session.create creates a session with worktree for existing repo', async () => {
    const repo = repos.register({
      displayName: 'Test',
      localPath: repoPath,
      originUrl: 'git@github.com:user/test.git',
      worktreeBaseDir,
    });

    const res = await dispatcher.dispatch(
      rpc('session.create', {
        repoId: repo.id,
        title: 'Fix bug',
        plan: 'Do the thing',
      }),
    );

    expect(res.error).toBeUndefined();
    const session = res.result as {
      id: string;
      title: string;
      repoId: string;
      state: string;
      worktreePath: string;
      branchName: string;
    };
    expect(session.title).toBe('Fix bug');
    expect(session.repoId).toBe(repo.id);
    expect(session.state).toBe('starting_claude');
    expect(session.worktreePath).toBeTruthy();
    expect(session.branchName).toMatch(/^boss\/fix-bug-/);
  });

  it('session.create returns error for unknown repo', async () => {
    const res = await dispatcher.dispatch(
      rpc('session.create', {
        repoId: 'nonexistent',
        title: 'Fix',
        plan: 'plan',
      }),
    );

    expect(res.error?.code).toBe(RpcErrorCode.InternalError);
    expect(res.error?.message).toContain('Repo not found');
  });

  it('session.get returns session by id', async () => {
    const repo = repos.register({
      displayName: 'Test',
      localPath: '/test/get',
      originUrl: 'git@github.com:user/test.git',
      worktreeBaseDir: '/tmp/wt',
    });

    const created = sessions.create({
      repoId: repo.id,
      title: 'Session',
      plan: 'plan',
      baseBranch: 'main',
    });

    const res = await dispatcher.dispatch(rpc('session.get', { sessionId: created.id }));
    expect(res.error).toBeUndefined();
    expect((res.result as { id: string }).id).toBe(created.id);
  });

  it('session.get returns error for unknown session', async () => {
    const res = await dispatcher.dispatch(rpc('session.get', { sessionId: 'nonexistent' }));
    expect(res.error?.code).toBe(RpcErrorCode.InternalError);
    expect(res.error?.message).toContain('Session not found');
  });

  it('session.remove removes existing session and cleans up worktree', async () => {
    const repo = repos.register({
      displayName: 'Test',
      localPath: repoPath,
      originUrl: 'git@github.com:user/test.git',
      worktreeBaseDir,
    });

    // Create session via lifecycle (creates worktree)
    const createRes = await dispatcher.dispatch(
      rpc('session.create', {
        repoId: repo.id,
        title: 'To remove',
        plan: 'plan',
      }),
    );
    const created = createRes.result as { id: string; worktreePath: string };
    expect(fs.existsSync(created.worktreePath)).toBe(true);

    const res = await dispatcher.dispatch(rpc('session.remove', { sessionId: created.id }));
    expect(res.result).toEqual({ removed: true });
    expect(sessions.get(created.id)).toBeNull();
    expect(fs.existsSync(created.worktreePath)).toBe(false);
  });

  it('session.remove returns removed: false for unknown session', async () => {
    const res = await dispatcher.dispatch(rpc('session.remove', { sessionId: 'nonexistent' }));
    expect(res.result).toEqual({ removed: false });
  });

  // --- Attempts ---

  it('session.attempts returns attempts for a session', async () => {
    const repo = repos.register({
      displayName: 'Test',
      localPath: '/test/attempts',
      originUrl: 'git@github.com:user/test.git',
      worktreeBaseDir: '/tmp/wt',
    });

    const session = sessions.create({
      repoId: repo.id,
      title: 'Attempts test',
      plan: 'plan',
      baseBranch: 'main',
    });

    attempts.record(session.id, 'check_failed');

    const res = await dispatcher.dispatch(rpc('session.attempts', { sessionId: session.id }));
    expect(res.error).toBeUndefined();
    expect(res.result).toHaveLength(1);
  });
});
