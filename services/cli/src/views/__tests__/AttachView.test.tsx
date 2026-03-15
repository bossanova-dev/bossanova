import type { IpcClient, Session } from '@bossanova/shared';
import { render } from 'ink-testing-library';
import React from 'react';
import { describe, expect, it, vi } from 'vitest';
import { AttachView } from '../AttachView.js';

function makeSession(overrides: Partial<Session> = {}): Session {
  return {
    id: 'sess-001',
    repoId: 'repo-001',
    title: 'Fix login bug',
    plan: 'Fix the login validation',
    worktreePath: null,
    branchName: 'boss/fix-login-bug-abcd1234',
    baseBranch: 'main',
    state: 'implementing_plan',
    claudeSessionId: null,
    prNumber: null,
    prUrl: null,
    lastCheckState: null,
    automationEnabled: true,
    attemptCount: 0,
    blockedReason: null,
    createdAt: new Date().toISOString(),
    updatedAt: new Date().toISOString(),
    ...overrides,
  };
}

function mockClient(session: Session, logLines: string[] = []): IpcClient {
  return {
    call: vi.fn().mockImplementation((method: string) => {
      if (method === 'session.get') return Promise.resolve(session);
      if (method === 'session.logs') return Promise.resolve({ lines: logLines });
      return Promise.resolve({});
    }),
    close: vi.fn(),
  };
}

describe('AttachView', () => {
  it('shows loading state initially', () => {
    const session = makeSession();
    const client = mockClient(session);
    const { lastFrame } = render(<AttachView client={client} sessionId="sess-001" />);
    expect(lastFrame()).toContain('Attaching');
  });

  it('renders session header with title and state', async () => {
    const session = makeSession({ title: 'Fix login bug', state: 'implementing_plan' });
    const client = mockClient(session);
    const { lastFrame } = render(<AttachView client={client} sessionId="sess-001" />);
    await vi.waitFor(() => {
      const frame = lastFrame();
      expect(frame).toContain('Fix login bug');
      expect(frame).toContain('implementing plan');
    });
  });

  it('renders branch name', async () => {
    const session = makeSession({ branchName: 'boss/fix-login-bug-abcd1234' });
    const client = mockClient(session);
    const { lastFrame } = render(<AttachView client={client} sessionId="sess-001" />);
    await vi.waitFor(() => {
      expect(lastFrame()).toContain('boss/fix-login-bug-abcd1234');
    });
  });

  it('renders log lines', async () => {
    const session = makeSession();
    const logLines = ['[12:00:00] Starting work...', '[12:00:01] Reading file.ts'];
    const client = mockClient(session, logLines);
    const { lastFrame } = render(<AttachView client={client} sessionId="sess-001" />);
    await vi.waitFor(() => {
      const frame = lastFrame();
      expect(frame).toContain('Starting work...');
      expect(frame).toContain('Reading file.ts');
    });
  });

  it('shows empty message when no output', async () => {
    const session = makeSession();
    const client = mockClient(session, []);
    const { lastFrame } = render(<AttachView client={client} sessionId="sess-001" />);
    await vi.waitFor(() => {
      expect(lastFrame()).toContain('No output yet');
    });
  });

  it('shows error when daemon is not running', async () => {
    const err = new Error('Daemon not running');
    (err as { name: string }).name = 'DaemonNotRunningError';
    const client: IpcClient = {
      call: vi.fn().mockRejectedValue(err),
      close: vi.fn(),
    };
    const { lastFrame } = render(<AttachView client={client} sessionId="sess-001" />);
    await vi.waitFor(() => {
      expect(lastFrame()).toContain('bossd is not running');
    });
  });

  it('shows PR number when present', async () => {
    const session = makeSession({ prNumber: 42 });
    const client = mockClient(session);
    const { lastFrame } = render(<AttachView client={client} sessionId="sess-001" />);
    await vi.waitFor(() => {
      expect(lastFrame()).toContain('PR #42');
    });
  });
});
