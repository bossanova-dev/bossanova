import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { AttemptStore } from '~/db/attempts';
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

describe('AttemptStore', () => {
  let db: DatabaseService;
  let attemptStore: AttemptStore;
  let sessionId: string;

  beforeEach(() => {
    db = createTestDb();
    const repoStore = new RepoStore(db);
    const sessionStore = new SessionStore(db);
    attemptStore = new AttemptStore(db);

    const repo = repoStore.register({
      displayName: 'Test Repo',
      localPath: '/test/repo',
      originUrl: 'git@github.com:user/test.git',
      worktreeBaseDir: '/tmp/wt',
    });

    const session = sessionStore.create({
      repoId: repo.id,
      title: 'Test Session',
      plan: 'Fix the bug',
      baseBranch: 'main',
    });
    sessionId = session.id;
  });

  afterEach(() => {
    db.close();
  });

  it('records an attempt', () => {
    const attempt = attemptStore.record(sessionId, 'check_failed');

    expect(attempt.id).toBeDefined();
    expect(attempt.sessionId).toBe(sessionId);
    expect(attempt.trigger).toBe('check_failed');
    expect(attempt.startedAt).toBeDefined();
    expect(attempt.completedAt).toBeNull();
    expect(attempt.result).toBeNull();
    expect(attempt.error).toBeNull();
  });

  it('completes an attempt with success', () => {
    const attempt = attemptStore.record(sessionId, 'check_failed');
    attemptStore.complete(attempt.id, 'success');

    const updated = attemptStore.get(attempt.id);
    expect(updated?.result).toBe('success');
    expect(updated?.completedAt).toBeDefined();
    expect(updated?.error).toBeNull();
  });

  it('completes an attempt with failure and error', () => {
    const attempt = attemptStore.record(sessionId, 'conflict');
    attemptStore.complete(attempt.id, 'failure', 'Merge conflict unresolvable');

    const updated = attemptStore.get(attempt.id);
    expect(updated?.result).toBe('failure');
    expect(updated?.error).toBe('Merge conflict unresolvable');
  });

  it('lists attempts for a session', () => {
    attemptStore.record(sessionId, 'check_failed');
    attemptStore.record(sessionId, 'review_feedback');
    attemptStore.record(sessionId, 'conflict');

    const attempts = attemptStore.listBySession(sessionId);
    expect(attempts).toHaveLength(3);
  });

  it('returns null for unknown attempt id', () => {
    expect(attemptStore.get('nonexistent')).toBeNull();
  });

  it('supports all trigger types', () => {
    const triggers = ['check_failed', 'conflict', 'review_feedback', 'manual'] as const;
    for (const trigger of triggers) {
      const attempt = attemptStore.record(sessionId, trigger);
      expect(attempt.trigger).toBe(trigger);
    }
  });

  it('cascades delete when session is removed', () => {
    const attempt = attemptStore.record(sessionId, 'check_failed');

    // Delete the session — attempts should cascade
    db.getDb().prepare('DELETE FROM sessions WHERE id = ?').run(sessionId);
    expect(attemptStore.get(attempt.id)).toBeNull();
  });
});
