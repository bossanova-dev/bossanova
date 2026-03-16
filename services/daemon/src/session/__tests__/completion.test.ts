import type { Session } from '@bossanova/shared';
import { type Mock, beforeEach, describe, expect, it, vi } from 'vitest';
import type { RepoStore } from '~/db/repos';
import type { SessionStore } from '~/db/sessions';

vi.mock('~/github/client', () => ({
  markReadyForReview: vi.fn().mockResolvedValue(undefined),
}));

vi.mock('~/git/worktree', () => ({
  removeWorktree: vi.fn().mockResolvedValue(undefined),
}));

import { removeWorktree } from '~/git/worktree';
import { markReadyForReview } from '~/github/client';
import {
  handlePrMerged,
  isReadyForReview,
  processReadyForReview,
  transitionToReadyForReview,
} from '~/session/completion';

const mockMarkReadyForReview = markReadyForReview as Mock;
const mockRemoveWorktree = removeWorktree as Mock;

function makeSession(overrides: Partial<Session> = {}): Session {
  return {
    id: 'sess-1',
    repoId: 'repo-1',
    title: 'Test',
    plan: 'Plan',
    worktreePath: '/tmp/wt',
    branchName: 'boss/test-123',
    baseBranch: 'main',
    state: 'green_draft',
    claudeSessionId: null,
    prNumber: 42,
    prUrl: 'https://github.com/owner/repo/pull/42',
    lastCheckState: 'passed',
    automationEnabled: true,
    attemptCount: 0,
    blockedReason: null,
    createdAt: '2026-01-01',
    updatedAt: '2026-01-01',
    ...overrides,
  };
}

describe('isReadyForReview', () => {
  it('returns true for green_draft with passed checks', () => {
    expect(isReadyForReview(makeSession())).toBe(true);
  });

  it('returns false for non-green_draft state', () => {
    expect(isReadyForReview(makeSession({ state: 'awaiting_checks' }))).toBe(false);
  });

  it('returns false when checks have not passed', () => {
    expect(isReadyForReview(makeSession({ lastCheckState: 'failed' }))).toBe(false);
    expect(isReadyForReview(makeSession({ lastCheckState: 'pending' }))).toBe(false);
  });
});

describe('transitionToReadyForReview', () => {
  const mockSessions = {
    update: vi.fn(),
  } as unknown as SessionStore & { update: Mock };

  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('marks PR as ready and updates state', async () => {
    const session = makeSession();
    await transitionToReadyForReview(mockSessions, session);

    expect(mockMarkReadyForReview).toHaveBeenCalledWith('/tmp/wt', 42);
    expect(mockSessions.update).toHaveBeenCalledWith('sess-1', { state: 'ready_for_review' });
  });

  it('does nothing if session has no PR', async () => {
    const session = makeSession({ prNumber: null });
    await transitionToReadyForReview(mockSessions, session);

    expect(mockMarkReadyForReview).not.toHaveBeenCalled();
    expect(mockSessions.update).not.toHaveBeenCalled();
  });

  it('does nothing if session has no worktree', async () => {
    const session = makeSession({ worktreePath: null });
    await transitionToReadyForReview(mockSessions, session);

    expect(mockMarkReadyForReview).not.toHaveBeenCalled();
  });
});

describe('handlePrMerged', () => {
  const mockSessions = {
    update: vi.fn(),
  } as unknown as SessionStore & { update: Mock };
  const mockRepos = {
    get: vi.fn().mockReturnValue({ id: 'repo-1', localPath: '/tmp/repo' }),
  } as unknown as RepoStore & { get: Mock };

  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('updates state to merged and cleans up worktree', async () => {
    const session = makeSession();
    await handlePrMerged(mockSessions, mockRepos, session);

    expect(mockSessions.update).toHaveBeenCalledWith('sess-1', { state: 'merged' });
    expect(mockRemoveWorktree).toHaveBeenCalledWith('/tmp/repo', '/tmp/wt');
  });

  it('still transitions even if worktree cleanup fails', async () => {
    mockRemoveWorktree.mockRejectedValueOnce(new Error('already gone'));

    const session = makeSession();
    await handlePrMerged(mockSessions, mockRepos, session);

    expect(mockSessions.update).toHaveBeenCalledWith('sess-1', { state: 'merged' });
  });

  it('handles sessions without worktree', async () => {
    const session = makeSession({ worktreePath: null });
    await handlePrMerged(mockSessions, mockRepos, session);

    expect(mockSessions.update).toHaveBeenCalledWith('sess-1', { state: 'merged' });
    expect(mockRemoveWorktree).not.toHaveBeenCalled();
  });
});

describe('processReadyForReview', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('transitions all green_draft sessions with passed checks', async () => {
    const mockSessions = {
      list: vi.fn().mockReturnValue([
        makeSession({ id: 'sess-1', state: 'green_draft', lastCheckState: 'passed' }),
        makeSession({ id: 'sess-2', state: 'awaiting_checks', lastCheckState: 'pending' }),
        makeSession({ id: 'sess-3', state: 'green_draft', lastCheckState: 'passed' }),
      ]),
      update: vi.fn(),
    } as unknown as SessionStore;

    const count = await processReadyForReview(mockSessions);
    expect(count).toBe(2);
    expect(mockMarkReadyForReview).toHaveBeenCalledTimes(2);
  });

  it('continues processing when individual transitions fail', async () => {
    mockMarkReadyForReview
      .mockRejectedValueOnce(new Error('fail'))
      .mockResolvedValueOnce(undefined);

    const mockSessions = {
      list: vi.fn().mockReturnValue([
        makeSession({ id: 'sess-1' }),
        makeSession({ id: 'sess-2' }),
      ]),
      update: vi.fn(),
    } as unknown as SessionStore;

    const count = await processReadyForReview(mockSessions);
    expect(count).toBe(1);
  });
});
