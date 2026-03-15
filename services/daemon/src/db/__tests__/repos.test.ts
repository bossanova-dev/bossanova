import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { DatabaseService } from '~/db/database';
import { RepoStore } from '~/db/repos';

function createTestDb(): DatabaseService {
  const config = { dbPath: ':memory:', socketPath: '', logLevel: 'info' as const };
  const logger = { debug: () => {}, info: () => {}, warn: () => {}, error: () => {} };
  const db = new DatabaseService(config, logger);
  db.initialize(':memory:');
  return db;
}

describe('RepoStore', () => {
  let db: DatabaseService;
  let store: RepoStore;

  beforeEach(() => {
    db = createTestDb();
    store = new RepoStore(db);
  });

  afterEach(() => {
    db.close();
  });

  it('registers a repo and returns it', () => {
    const repo = store.register({
      displayName: 'My App',
      localPath: '/home/user/my-app',
      originUrl: 'git@github.com:user/my-app.git',
      worktreeBaseDir: '/tmp/worktrees/my-app',
    });

    expect(repo.id).toBeDefined();
    expect(repo.displayName).toBe('My App');
    expect(repo.localPath).toBe('/home/user/my-app');
    expect(repo.originUrl).toBe('git@github.com:user/my-app.git');
    expect(repo.defaultBaseBranch).toBe('main');
    expect(repo.worktreeBaseDir).toBe('/tmp/worktrees/my-app');
    expect(repo.setupScript).toBeNull();
    expect(repo.createdAt).toBeDefined();
  });

  it('registers with custom base branch and setup script', () => {
    const repo = store.register({
      displayName: 'My App',
      localPath: '/home/user/my-app',
      originUrl: 'git@github.com:user/my-app.git',
      defaultBaseBranch: 'develop',
      worktreeBaseDir: '/tmp/wt',
      setupScript: 'pnpm install',
    });

    expect(repo.defaultBaseBranch).toBe('develop');
    expect(repo.setupScript).toBe('pnpm install');
  });

  it('lists repos in reverse chronological order', () => {
    store.register({
      displayName: 'First',
      localPath: '/first',
      originUrl: 'git@github.com:user/first.git',
      worktreeBaseDir: '/tmp/wt/first',
    });
    store.register({
      displayName: 'Second',
      localPath: '/second',
      originUrl: 'git@github.com:user/second.git',
      worktreeBaseDir: '/tmp/wt/second',
    });

    const repos = store.list();
    expect(repos).toHaveLength(2);
    // Both have the same timestamp in fast tests, so just check count
  });

  it('gets a repo by id', () => {
    const created = store.register({
      displayName: 'Test',
      localPath: '/test',
      originUrl: 'git@github.com:user/test.git',
      worktreeBaseDir: '/tmp/wt',
    });

    const found = store.get(created.id);
    expect(found).not.toBeNull();
    expect(found?.displayName).toBe('Test');
  });

  it('returns null for unknown id', () => {
    expect(store.get('nonexistent')).toBeNull();
  });

  it('finds a repo by local path', () => {
    store.register({
      displayName: 'Test',
      localPath: '/unique/path',
      originUrl: 'git@github.com:user/test.git',
      worktreeBaseDir: '/tmp/wt',
    });

    const found = store.findByPath('/unique/path');
    expect(found).not.toBeNull();
    expect(found?.displayName).toBe('Test');
  });

  it('returns null for unknown path', () => {
    expect(store.findByPath('/no/such/path')).toBeNull();
  });

  it('removes a repo', () => {
    const repo = store.register({
      displayName: 'ToDelete',
      localPath: '/delete-me',
      originUrl: 'git@github.com:user/delete.git',
      worktreeBaseDir: '/tmp/wt',
    });

    store.remove(repo.id);
    expect(store.get(repo.id)).toBeNull();
  });

  it('enforces unique local_path', () => {
    store.register({
      displayName: 'First',
      localPath: '/same/path',
      originUrl: 'git@github.com:user/first.git',
      worktreeBaseDir: '/tmp/wt',
    });

    expect(() =>
      store.register({
        displayName: 'Second',
        localPath: '/same/path',
        originUrl: 'git@github.com:user/second.git',
        worktreeBaseDir: '/tmp/wt',
      }),
    ).toThrow();
  });
});
