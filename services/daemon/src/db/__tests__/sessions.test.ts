import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { DatabaseService } from '~/db/database';
import { RepoStore } from '~/db/repos';
import { SessionStore } from '~/db/sessions';

function createTestDb(): DatabaseService {
  const config = { dbPath: ':memory:', socketPath: '', logLevel: 'info' as const };
  const logger = { debug: () => {}, info: () => {}, warn: () => {}, error: () => {} };
  const db = new DatabaseService(config, logger);
  db.initialize(':memory:');
  return db;
}

describe('SessionStore', () => {
  let db: DatabaseService;
  let repoStore: RepoStore;
  let sessionStore: SessionStore;
  let repoId: string;

  beforeEach(() => {
    db = createTestDb();
    repoStore = new RepoStore(db);
    sessionStore = new SessionStore(db);

    const repo = repoStore.register({
      displayName: 'Test Repo',
      localPath: '/test/repo',
      originUrl: 'git@github.com:user/test.git',
      worktreeBaseDir: '/tmp/wt',
    });
    repoId = repo.id;
  });

  afterEach(() => {
    db.close();
  });

  it('creates a session with defaults', () => {
    const session = sessionStore.create({
      repoId,
      title: 'Fix bug #123',
      plan: 'Fix the failing test in auth module',
      baseBranch: 'main',
    });

    expect(session.id).toBeDefined();
    expect(session.repoId).toBe(repoId);
    expect(session.title).toBe('Fix bug #123');
    expect(session.plan).toBe('Fix the failing test in auth module');
    expect(session.baseBranch).toBe('main');
    expect(session.state).toBe('creating_worktree');
    expect(session.automationEnabled).toBe(true);
    expect(session.attemptCount).toBe(0);
    expect(session.worktreePath).toBeNull();
    expect(session.branchName).toBeNull();
    expect(session.prNumber).toBeNull();
  });

  it('lists all sessions', () => {
    sessionStore.create({ repoId, title: 'S1', plan: 'P1', baseBranch: 'main' });
    sessionStore.create({ repoId, title: 'S2', plan: 'P2', baseBranch: 'main' });

    const sessions = sessionStore.list();
    expect(sessions).toHaveLength(2);
  });

  it('lists sessions filtered by repoId', () => {
    const otherRepo = repoStore.register({
      displayName: 'Other',
      localPath: '/other/repo',
      originUrl: 'git@github.com:user/other.git',
      worktreeBaseDir: '/tmp/wt2',
    });

    sessionStore.create({ repoId, title: 'S1', plan: 'P1', baseBranch: 'main' });
    sessionStore.create({ repoId: otherRepo.id, title: 'S2', plan: 'P2', baseBranch: 'main' });

    const filtered = sessionStore.list(repoId);
    expect(filtered).toHaveLength(1);
    expect(filtered[0].title).toBe('S1');
  });

  it('gets a session by id', () => {
    const created = sessionStore.create({
      repoId,
      title: 'Test',
      plan: 'Plan',
      baseBranch: 'main',
    });

    const found = sessionStore.get(created.id);
    expect(found).not.toBeNull();
    expect(found?.title).toBe('Test');
  });

  it('returns null for unknown id', () => {
    expect(sessionStore.get('nonexistent')).toBeNull();
  });

  it('updates session fields', () => {
    const session = sessionStore.create({
      repoId,
      title: 'Test',
      plan: 'Plan',
      baseBranch: 'main',
    });

    sessionStore.update(session.id, {
      state: 'implementing_plan',
      worktreePath: '/tmp/wt/test',
      branchName: 'boss/test-123',
      claudeSessionId: 'claude-abc',
    });

    const updated = sessionStore.get(session.id);
    expect(updated?.state).toBe('implementing_plan');
    expect(updated?.worktreePath).toBe('/tmp/wt/test');
    expect(updated?.branchName).toBe('boss/test-123');
    expect(updated?.claudeSessionId).toBe('claude-abc');
  });

  it('updates automation_enabled boolean correctly', () => {
    const session = sessionStore.create({
      repoId,
      title: 'Test',
      plan: 'Plan',
      baseBranch: 'main',
    });

    expect(sessionStore.get(session.id)?.automationEnabled).toBe(true);

    sessionStore.update(session.id, { automationEnabled: false });
    expect(sessionStore.get(session.id)?.automationEnabled).toBe(false);

    sessionStore.update(session.id, { automationEnabled: true });
    expect(sessionStore.get(session.id)?.automationEnabled).toBe(true);
  });

  it('updates PR info', () => {
    const session = sessionStore.create({
      repoId,
      title: 'Test',
      plan: 'Plan',
      baseBranch: 'main',
    });

    sessionStore.update(session.id, {
      prNumber: 42,
      prUrl: 'https://github.com/test/pr/42',
      lastCheckState: 'pending',
    });

    const updated = sessionStore.get(session.id);
    expect(updated?.prNumber).toBe(42);
    expect(updated?.prUrl).toBe('https://github.com/test/pr/42');
    expect(updated?.lastCheckState).toBe('pending');
  });

  it('deletes a session', () => {
    const session = sessionStore.create({
      repoId,
      title: 'ToDelete',
      plan: 'Plan',
      baseBranch: 'main',
    });

    sessionStore.delete(session.id);
    expect(sessionStore.get(session.id)).toBeNull();
  });

  it('cascades delete when repo is removed', () => {
    const session = sessionStore.create({
      repoId,
      title: 'Orphan',
      plan: 'Plan',
      baseBranch: 'main',
    });

    repoStore.remove(repoId);
    expect(sessionStore.get(session.id)).toBeNull();
  });

  it('does nothing when updating with empty fields', () => {
    const session = sessionStore.create({
      repoId,
      title: 'Test',
      plan: 'Plan',
      baseBranch: 'main',
    });

    // Should not throw
    sessionStore.update(session.id, {});
    expect(sessionStore.get(session.id)?.title).toBe('Test');
  });
});
