import { describe, expect, it } from 'vitest';
import { createActor } from 'xstate';
import { SessionState, sessionMachine } from '../session-machine.js';

function createTestActor(overrides?: { maxAttempts?: number }) {
  return createActor(sessionMachine, {
    input: {
      repoId: 'test-repo',
      title: 'Test Session',
      plan: 'Test plan',
      baseBranch: 'main',
      ...overrides,
    },
  });
}

function walkToAwaitingChecks(actor: ReturnType<typeof createTestActor>) {
  actor.start();
  actor.send({ type: 'WORKTREE_CREATED', worktreePath: '/tmp/wt', branchName: 'boss/test' });
  actor.send({ type: 'CLAUDE_STARTED', claudeSessionId: 'claude-abc' });
  actor.send({ type: 'PLAN_COMPLETE' });
  actor.send({ type: 'BRANCH_PUSHED', sha: 'abc123' });
  actor.send({ type: 'PR_OPENED', prNumber: 42, prUrl: 'https://github.com/test/pr/42' });
}

describe('sessionMachine', () => {
  it('has all 12 states', () => {
    const stateValues = Object.values(SessionState);
    expect(stateValues).toHaveLength(12);
  });

  it('starts in creating_worktree', () => {
    const actor = createTestActor();
    actor.start();
    expect(actor.getSnapshot().value).toBe('creating_worktree');
  });

  it('walks through happy path to merged', () => {
    const actor = createTestActor();
    walkToAwaitingChecks(actor);
    expect(actor.getSnapshot().value).toBe('awaiting_checks');

    actor.send({ type: 'CHECKS_PASSED' });
    expect(actor.getSnapshot().value).toBe('green_draft');

    actor.send({ type: 'PR_MERGED' });
    expect(actor.getSnapshot().value).toBe('merged');
    expect(actor.getSnapshot().status).toBe('done');
  });

  it('stores context from events', () => {
    const actor = createTestActor();
    walkToAwaitingChecks(actor);
    const ctx = actor.getSnapshot().context;
    expect(ctx.worktreePath).toBe('/tmp/wt');
    expect(ctx.branchName).toBe('boss/test');
    expect(ctx.claudeSessionId).toBe('claude-abc');
    expect(ctx.prNumber).toBe(42);
    expect(ctx.prUrl).toBe('https://github.com/test/pr/42');
  });

  it('handles fix loop: awaiting_checks <-> fixing_checks', () => {
    const actor = createTestActor();
    walkToAwaitingChecks(actor);

    actor.send({ type: 'CHECKS_FAILED' });
    expect(actor.getSnapshot().value).toBe('fixing_checks');
    expect(actor.getSnapshot().context.attemptCount).toBe(1);

    actor.send({ type: 'FIX_COMPLETE' });
    expect(actor.getSnapshot().value).toBe('awaiting_checks');

    actor.send({ type: 'CHECKS_PASSED' });
    expect(actor.getSnapshot().value).toBe('green_draft');
  });

  it('blocks after max attempts', () => {
    const actor = createTestActor({ maxAttempts: 2 });
    walkToAwaitingChecks(actor);

    // First failure: attempt 1
    actor.send({ type: 'CHECKS_FAILED' });
    expect(actor.getSnapshot().value).toBe('fixing_checks');
    expect(actor.getSnapshot().context.attemptCount).toBe(1);

    actor.send({ type: 'FIX_COMPLETE' });
    expect(actor.getSnapshot().value).toBe('awaiting_checks');

    // Second failure: at max, should block
    actor.send({ type: 'CHECKS_FAILED' });
    expect(actor.getSnapshot().value).toBe('blocked');
    expect(actor.getSnapshot().context.blockedReason).toBe('Max fix attempts reached');
  });

  it('ignores invalid events in wrong states', () => {
    const actor = createTestActor();
    actor.start();

    // PLAN_COMPLETE is invalid in creating_worktree
    actor.send({ type: 'PLAN_COMPLETE' });
    expect(actor.getSnapshot().value).toBe('creating_worktree');
  });

  it('transitions to blocked on BLOCK event', () => {
    const actor = createTestActor();
    actor.start();
    actor.send({ type: 'BLOCK', reason: 'Git auth failed' });
    expect(actor.getSnapshot().value).toBe('blocked');
    expect(actor.getSnapshot().context.blockedReason).toBe('Git auth failed');
  });

  it('unblocks from blocked state', () => {
    const actor = createTestActor();
    walkToAwaitingChecks(actor);
    actor.send({ type: 'BLOCK', reason: 'test' });
    expect(actor.getSnapshot().value).toBe('blocked');

    actor.send({ type: 'UNBLOCK' });
    expect(actor.getSnapshot().value).toBe('awaiting_checks');
    expect(actor.getSnapshot().context.blockedReason).toBeNull();
  });

  it('handles review and conflict in awaiting_checks', () => {
    const actor = createTestActor();
    walkToAwaitingChecks(actor);

    actor.send({ type: 'REVIEW_SUBMITTED' });
    expect(actor.getSnapshot().value).toBe('fixing_checks');
    expect(actor.getSnapshot().context.attemptCount).toBe(1);
  });

  it('transitions to closed on PR_CLOSED', () => {
    const actor = createTestActor();
    walkToAwaitingChecks(actor);
    actor.send({ type: 'PR_CLOSED' });
    expect(actor.getSnapshot().value).toBe('closed');
    expect(actor.getSnapshot().status).toBe('done');
  });
});
