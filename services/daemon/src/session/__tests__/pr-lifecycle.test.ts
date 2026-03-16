import { type Mock, beforeEach, describe, expect, it, vi } from 'vitest';
import type { SessionStore } from '~/db/sessions';

// Mock external dependencies
vi.mock('~/git/push', () => ({
  pushBranch: vi.fn().mockResolvedValue('abc123'),
}));

vi.mock('~/github/client', () => ({
  createDraftPr: vi.fn().mockResolvedValue({ number: 42, url: 'https://github.com/owner/repo/pull/42' }),
}));

import { pushBranch } from '~/git/push';
import { createDraftPr } from '~/github/client';
import { pushAndCreatePr } from '~/session/pr-lifecycle';

const mockPushBranch = pushBranch as Mock;
const mockCreateDraftPr = createDraftPr as Mock;

describe('pushAndCreatePr', () => {
  const mockSessions = {
    update: vi.fn(),
    get: vi.fn(),
  } as unknown as SessionStore & { update: Mock; get: Mock };

  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('pushes branch, creates draft PR, and updates session through states', async () => {
    await pushAndCreatePr(
      mockSessions,
      'sess-1',
      '/tmp/wt',
      'boss/my-branch',
      'Fix the bug',
      'Fix the failing test in auth module',
      'main',
    );

    // Verify state transitions
    expect(mockSessions.update).toHaveBeenCalledWith('sess-1', { state: 'pushing_branch' });
    expect(mockPushBranch).toHaveBeenCalledWith('/tmp/wt', 'boss/my-branch');

    expect(mockSessions.update).toHaveBeenCalledWith('sess-1', { state: 'opening_draft_pr' });
    expect(mockCreateDraftPr).toHaveBeenCalledWith(
      '/tmp/wt',
      'Fix the bug',
      'Fix the failing test in auth module',
      'main',
    );

    expect(mockSessions.update).toHaveBeenCalledWith('sess-1', {
      prNumber: 42,
      prUrl: 'https://github.com/owner/repo/pull/42',
      state: 'awaiting_checks',
      lastCheckState: 'pending',
    });
  });

  it('transitions through correct order', async () => {
    const callOrder: string[] = [];

    mockSessions.update.mockImplementation((_id: string, fields: Record<string, unknown>) => {
      if (fields.state) callOrder.push(fields.state as string);
    });

    await pushAndCreatePr(
      mockSessions,
      'sess-1',
      '/tmp/wt',
      'boss/branch',
      'Title',
      'Plan',
      'main',
    );

    expect(callOrder).toEqual(['pushing_branch', 'opening_draft_pr', 'awaiting_checks']);
  });

  it('propagates push errors', async () => {
    mockPushBranch.mockRejectedValueOnce(new Error('push failed'));

    await expect(
      pushAndCreatePr(mockSessions, 'sess-1', '/tmp/wt', 'boss/b', 'T', 'P', 'main'),
    ).rejects.toThrow('push failed');
  });

  it('propagates PR creation errors', async () => {
    mockCreateDraftPr.mockRejectedValueOnce(new Error('gh not found'));

    await expect(
      pushAndCreatePr(mockSessions, 'sess-1', '/tmp/wt', 'boss/b', 'T', 'P', 'main'),
    ).rejects.toThrow('gh not found');
  });
});
