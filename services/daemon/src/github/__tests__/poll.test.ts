import type { Session } from '@bossanova/shared';
import { type Mock, beforeEach, describe, expect, it, vi } from 'vitest';
import type { RepoStore } from '~/db/repos';
import type { SessionStore } from '~/db/sessions';

vi.mock('~/github/client', () => ({
  getPrStatus: vi.fn(),
  getPrChecks: vi.fn(),
  summarizeChecks: vi.fn(),
}));

import { getPrChecks, getPrStatus, summarizeChecks } from '~/github/client';
import { pollAllSessions, pollSession, processPollResult, startPolling } from '~/github/poll';

const mockGetPrStatus = getPrStatus as Mock;
const mockGetPrChecks = getPrChecks as Mock;
const mockSummarizeChecks = summarizeChecks as Mock;

function makeSession(overrides: Partial<Session> = {}): Session {
  return {
    id: 'sess-1',
    repoId: 'repo-1',
    title: 'Test',
    plan: 'Plan',
    worktreePath: '/tmp/wt',
    branchName: 'boss/test-123',
    baseBranch: 'main',
    state: 'awaiting_checks',
    claudeSessionId: null,
    prNumber: 42,
    prUrl: 'https://github.com/owner/repo/pull/42',
    lastCheckState: 'pending',
    automationEnabled: true,
    attemptCount: 0,
    blockedReason: null,
    createdAt: '2026-01-01',
    updatedAt: '2026-01-01',
    ...overrides,
  };
}

describe('pollSession', () => {
  const mockRepos = {
    get: vi.fn().mockReturnValue({ id: 'repo-1' }),
  } as unknown as RepoStore & { get: Mock };

  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('returns null if session has no PR', async () => {
    const session = makeSession({ prNumber: null });
    const result = await pollSession(session, mockRepos);
    expect(result).toBeNull();
  });

  it('returns null if session has no worktree', async () => {
    const session = makeSession({ worktreePath: null });
    const result = await pollSession(session, mockRepos);
    expect(result).toBeNull();
  });

  it('polls PR status and checks', async () => {
    mockGetPrStatus.mockResolvedValueOnce({
      state: 'open',
      mergeable: true,
      title: 'Test',
      headBranch: 'boss/test',
      baseBranch: 'main',
    });
    mockGetPrChecks.mockResolvedValueOnce([
      { name: 'CI', status: 'completed', conclusion: 'success' },
    ]);
    mockSummarizeChecks.mockReturnValueOnce('passed');

    const session = makeSession();
    const result = await pollSession(session, mockRepos);

    expect(result).toEqual({
      sessionId: 'sess-1',
      checksOverall: 'passed',
      hasConflict: false,
      prState: 'open',
    });
  });

  it('detects conflicts', async () => {
    mockGetPrStatus.mockResolvedValueOnce({
      state: 'open',
      mergeable: false,
      title: 'Test',
      headBranch: 'boss/test',
      baseBranch: 'main',
    });
    mockGetPrChecks.mockResolvedValueOnce([]);
    mockSummarizeChecks.mockReturnValueOnce('pending');

    const session = makeSession();
    const result = await pollSession(session, mockRepos);

    expect(result?.hasConflict).toBe(true);
  });
});

describe('processPollResult', () => {
  let mockSessions: SessionStore & { update: Mock; get: Mock };

  beforeEach(() => {
    vi.clearAllMocks();
    mockSessions = {
      update: vi.fn(),
      get: vi.fn(),
    } as unknown as SessionStore & { update: Mock; get: Mock };
  });

  it('transitions to merged when PR is merged', () => {
    mockSessions.get.mockReturnValueOnce(makeSession());

    processPollResult(mockSessions, {
      sessionId: 'sess-1',
      checksOverall: 'passed',
      hasConflict: false,
      prState: 'merged',
    });

    expect(mockSessions.update).toHaveBeenCalledWith('sess-1', { state: 'merged' });
  });

  it('transitions to closed when PR is closed', () => {
    mockSessions.get.mockReturnValueOnce(makeSession());

    processPollResult(mockSessions, {
      sessionId: 'sess-1',
      checksOverall: 'pending',
      hasConflict: false,
      prState: 'closed',
    });

    expect(mockSessions.update).toHaveBeenCalledWith('sess-1', { state: 'closed' });
  });

  it('transitions awaiting_checks to green_draft on checks passed', () => {
    mockSessions.get.mockReturnValueOnce(makeSession({ state: 'awaiting_checks' }));

    processPollResult(mockSessions, {
      sessionId: 'sess-1',
      checksOverall: 'passed',
      hasConflict: false,
      prState: 'open',
    });

    expect(mockSessions.update).toHaveBeenCalledWith('sess-1', {
      state: 'green_draft',
      lastCheckState: 'passed',
    });
  });

  it('transitions awaiting_checks to fixing_checks on checks failed', () => {
    mockSessions.get.mockReturnValueOnce(makeSession({ state: 'awaiting_checks', attemptCount: 0 }));

    processPollResult(mockSessions, {
      sessionId: 'sess-1',
      checksOverall: 'failed',
      hasConflict: false,
      prState: 'open',
    });

    expect(mockSessions.update).toHaveBeenCalledWith('sess-1', {
      state: 'fixing_checks',
      lastCheckState: 'failed',
      attemptCount: 1,
    });
  });

  it('transitions to blocked when max attempts reached', () => {
    mockSessions.get.mockReturnValueOnce(makeSession({ state: 'awaiting_checks', attemptCount: 4 }));

    processPollResult(mockSessions, {
      sessionId: 'sess-1',
      checksOverall: 'failed',
      hasConflict: false,
      prState: 'open',
    });

    expect(mockSessions.update).toHaveBeenCalledWith('sess-1', {
      state: 'blocked',
      lastCheckState: 'failed',
      blockedReason: 'Max fix attempts reached',
    });
  });

  it('handles conflict detection', () => {
    mockSessions.get.mockReturnValueOnce(makeSession({ state: 'awaiting_checks', attemptCount: 1 }));

    processPollResult(mockSessions, {
      sessionId: 'sess-1',
      checksOverall: 'pending',
      hasConflict: true,
      prState: 'open',
    });

    expect(mockSessions.update).toHaveBeenCalledWith('sess-1', {
      state: 'fixing_checks',
      blockedReason: 'Merge conflict detected',
      attemptCount: 2,
    });
  });

  it('ignores non-pollable states', () => {
    mockSessions.get.mockReturnValueOnce(makeSession({ state: 'implementing_plan' }));

    processPollResult(mockSessions, {
      sessionId: 'sess-1',
      checksOverall: 'passed',
      hasConflict: false,
      prState: 'open',
    });

    // Should not update state
    expect(mockSessions.update).not.toHaveBeenCalledWith('sess-1', expect.objectContaining({ state: expect.any(String) }));
  });
});

describe('pollAllSessions', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('polls sessions in awaiting_checks state', async () => {
    const sessions = {
      list: vi.fn().mockReturnValue([
        makeSession({ id: 'sess-1', state: 'awaiting_checks' }),
        makeSession({ id: 'sess-2', state: 'implementing_plan', prNumber: null }),
      ]),
      get: vi.fn().mockReturnValue(makeSession()),
      update: vi.fn(),
    } as unknown as SessionStore;

    const repos = {
      get: vi.fn().mockReturnValue({ id: 'repo-1' }),
    } as unknown as RepoStore;

    mockGetPrStatus.mockResolvedValue({
      state: 'open',
      mergeable: true,
      title: 'Test',
      headBranch: 'boss/test',
      baseBranch: 'main',
    });
    mockGetPrChecks.mockResolvedValue([]);
    mockSummarizeChecks.mockReturnValue('pending');

    const polled = await pollAllSessions(sessions, repos);
    expect(polled).toBe(1);
  });
});

describe('startPolling', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  it('returns a cleanup function that stops polling', () => {
    const sessions = {
      list: vi.fn().mockReturnValue([]),
    } as unknown as SessionStore;
    const repos = {} as RepoStore;

    const stop = startPolling(sessions, repos, 1000);

    vi.advanceTimersByTime(3000);
    expect(sessions.list).toHaveBeenCalledTimes(3);

    stop();
    vi.advanceTimersByTime(3000);
    expect(sessions.list).toHaveBeenCalledTimes(3);

    vi.useRealTimers();
  });
});
