import { type Mock, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  type CheckResult,
  closePr,
  createDraftPr,
  getFailedCheckLogs,
  getPrChecks,
  getPrStatus,
  markReadyForReview,
  summarizeChecks,
} from '~/github/client';

// Mock child_process.execFile
vi.mock('node:child_process', () => ({
  execFile: vi.fn(),
}));

vi.mock('node:util', () => ({
  promisify: (fn: Mock) => fn,
}));

import { execFile } from 'node:child_process';

const mockExecFile = execFile as unknown as Mock;

describe('GitHub client', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  describe('createDraftPr', () => {
    it('creates a draft PR and returns number + url', async () => {
      mockExecFile.mockResolvedValueOnce({
        stdout: JSON.stringify({ number: 42, url: 'https://github.com/owner/repo/pull/42' }),
      });

      const result = await createDraftPr('/tmp/wt', 'My PR', 'Body text', 'main');

      expect(result).toEqual({ number: 42, url: 'https://github.com/owner/repo/pull/42' });
      expect(mockExecFile).toHaveBeenCalledWith(
        'gh',
        ['pr', 'create', '--draft', '--title', 'My PR', '--body', 'Body text', '--base', 'main', '--json', 'number,url'],
        { cwd: '/tmp/wt' },
      );
    });
  });

  describe('getPrStatus', () => {
    it('parses PR status from gh output', async () => {
      mockExecFile.mockResolvedValueOnce({
        stdout: JSON.stringify({
          state: 'OPEN',
          mergeable: 'MERGEABLE',
          title: 'Test PR',
          headRefName: 'boss/test-123',
          baseRefName: 'main',
        }),
      });

      const result = await getPrStatus('/tmp/wt', 42);

      expect(result).toEqual({
        state: 'open',
        mergeable: true,
        title: 'Test PR',
        headBranch: 'boss/test-123',
        baseBranch: 'main',
      });
    });

    it('handles conflicting mergeable state', async () => {
      mockExecFile.mockResolvedValueOnce({
        stdout: JSON.stringify({
          state: 'OPEN',
          mergeable: 'CONFLICTING',
          title: 'Test',
          headRefName: 'boss/test',
          baseRefName: 'main',
        }),
      });

      const result = await getPrStatus('/tmp/wt', 1);
      expect(result.mergeable).toBe(false);
    });

    it('handles unknown mergeable state', async () => {
      mockExecFile.mockResolvedValueOnce({
        stdout: JSON.stringify({
          state: 'OPEN',
          mergeable: 'UNKNOWN',
          title: 'Test',
          headRefName: 'boss/test',
          baseRefName: 'main',
        }),
      });

      const result = await getPrStatus('/tmp/wt', 1);
      expect(result.mergeable).toBe(null);
    });
  });

  describe('getPrChecks', () => {
    it('parses check results from gh output', async () => {
      mockExecFile.mockResolvedValueOnce({
        stdout: JSON.stringify([
          { name: 'CI', state: 'COMPLETED', conclusion: 'SUCCESS' },
          { name: 'lint', state: 'IN_PROGRESS', conclusion: '' },
        ]),
      });

      const result = await getPrChecks('/tmp/wt', 42);

      expect(result).toEqual([
        { name: 'CI', status: 'completed', conclusion: 'success' },
        { name: 'lint', status: 'in_progress', conclusion: null },
      ]);
    });
  });

  describe('summarizeChecks', () => {
    it('returns pending when no checks exist', () => {
      expect(summarizeChecks([])).toBe('pending');
    });

    it('returns pending when some checks are in progress', () => {
      const checks: CheckResult[] = [
        { name: 'CI', status: 'completed', conclusion: 'success' },
        { name: 'lint', status: 'in_progress', conclusion: null },
      ];
      expect(summarizeChecks(checks)).toBe('pending');
    });

    it('returns passed when all completed with success/neutral/skipped', () => {
      const checks: CheckResult[] = [
        { name: 'CI', status: 'completed', conclusion: 'success' },
        { name: 'optional', status: 'completed', conclusion: 'neutral' },
        { name: 'skip', status: 'completed', conclusion: 'skipped' },
      ];
      expect(summarizeChecks(checks)).toBe('passed');
    });

    it('returns failed when any check has failure conclusion', () => {
      const checks: CheckResult[] = [
        { name: 'CI', status: 'completed', conclusion: 'success' },
        { name: 'tests', status: 'completed', conclusion: 'failure' },
      ];
      expect(summarizeChecks(checks)).toBe('failed');
    });
  });

  describe('markReadyForReview', () => {
    it('calls gh pr ready', async () => {
      mockExecFile.mockResolvedValueOnce({ stdout: '' });

      await markReadyForReview('/tmp/wt', 42);

      expect(mockExecFile).toHaveBeenCalledWith('gh', ['pr', 'ready', '42'], { cwd: '/tmp/wt' });
    });
  });

  describe('closePr', () => {
    it('calls gh pr close', async () => {
      mockExecFile.mockResolvedValueOnce({ stdout: '' });

      await closePr('/tmp/wt', 42);

      expect(mockExecFile).toHaveBeenCalledWith('gh', ['pr', 'close', '42'], { cwd: '/tmp/wt' });
    });
  });

  describe('getFailedCheckLogs', () => {
    it('returns failed check info', async () => {
      mockExecFile.mockResolvedValueOnce({
        stdout: '{"name":"CI","conclusion":"FAILURE","link":"https://..."}\n',
      });

      const result = await getFailedCheckLogs('/tmp/wt', 42);
      expect(result).toContain('FAILURE');
    });

    it('returns empty string on error', async () => {
      mockExecFile.mockRejectedValueOnce(new Error('no checks'));

      const result = await getFailedCheckLogs('/tmp/wt', 42);
      expect(result).toBe('');
    });
  });
});
