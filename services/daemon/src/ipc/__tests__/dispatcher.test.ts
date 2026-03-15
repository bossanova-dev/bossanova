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
  let db: DatabaseService;
  let repos: RepoStore;
  let sessions: SessionStore;
  let attempts: AttemptStore;
  let dispatcher: Dispatcher;

  beforeEach(() => {
    db = createTestDb();
    repos = new RepoStore(db);
    sessions = new SessionStore(db);
    attempts = new AttemptStore(db);
    dispatcher = new Dispatcher(repos, sessions, attempts, noopLogger);
  });

  afterEach(() => {
    db.close();
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

  it('session.create creates a session for existing repo', async () => {
    const repo = repos.register({
      displayName: 'Test',
      localPath: '/test/repo',
      originUrl: 'git@github.com:user/test.git',
      worktreeBaseDir: '/tmp/wt',
    });

    const res = await dispatcher.dispatch(
      rpc('session.create', {
        repoId: repo.id,
        title: 'Fix bug',
        plan: 'Do the thing',
      }),
    );

    expect(res.error).toBeUndefined();
    const session = res.result as { id: string; title: string; repoId: string };
    expect(session.title).toBe('Fix bug');
    expect(session.repoId).toBe(repo.id);
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

  it('session.remove removes existing session', async () => {
    const repo = repos.register({
      displayName: 'Test',
      localPath: '/test/remove',
      originUrl: 'git@github.com:user/test.git',
      worktreeBaseDir: '/tmp/wt',
    });

    const session = sessions.create({
      repoId: repo.id,
      title: 'To remove',
      plan: 'plan',
      baseBranch: 'main',
    });

    const res = await dispatcher.dispatch(rpc('session.remove', { sessionId: session.id }));
    expect(res.result).toEqual({ removed: true });
    expect(sessions.get(session.id)).toBeNull();
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
