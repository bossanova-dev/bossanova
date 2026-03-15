import { describe, expect, it, vi, beforeEach } from 'vitest';
import { render } from 'ink-testing-library';
import React from 'react';
import { HomeScreen, SessionList } from '../HomeScreen.js';
import type { IpcClient, Session } from '@bossanova/shared';

function mockClient(sessions: Session[] = []): IpcClient {
  return {
    call: vi.fn().mockResolvedValue(sessions),
    close: vi.fn(),
  };
}

function makeSession(overrides: Partial<Session> = {}): Session {
  return {
    id: 'sess-001',
    repoId: 'repo-001',
    title: 'Fix login bug',
    plan: 'Fix the login validation',
    worktreePath: null,
    branchName: 'fix/login-bug',
    baseBranch: 'main',
    state: 'implementing_plan',
    claudeSessionId: null,
    prNumber: 42,
    prUrl: 'https://github.com/org/repo/pull/42',
    lastCheckState: null,
    automationEnabled: true,
    attemptCount: 0,
    blockedReason: null,
    createdAt: new Date().toISOString(),
    updatedAt: new Date().toISOString(),
    ...overrides,
  };
}

describe('HomeScreen', () => {
  it('shows loading state initially', () => {
    const client = mockClient();
    const { lastFrame } = render(
      <HomeScreen
        client={client}
        onNewSession={() => {}}
        onAddRepo={() => {}}
        onAttach={() => {}}
      />,
    );
    expect(lastFrame()).toContain('Loading');
  });

  it('shows empty state when no sessions', async () => {
    const client = mockClient([]);
    const { lastFrame } = render(
      <HomeScreen
        client={client}
        onNewSession={() => {}}
        onAddRepo={() => {}}
        onAttach={() => {}}
      />,
    );
    // Wait for async fetch
    await vi.waitFor(() => {
      expect(lastFrame()).toContain('No sessions');
    });
  });

  it('renders session rows', async () => {
    const sessions = [
      makeSession({ id: 'sess-001', title: 'Fix login bug', state: 'implementing_plan' }),
      makeSession({ id: 'sess-002', title: 'Add dark mode', state: 'green_draft' }),
    ];
    const client = mockClient(sessions);
    const { lastFrame } = render(
      <HomeScreen
        client={client}
        onNewSession={() => {}}
        onAddRepo={() => {}}
        onAttach={() => {}}
      />,
    );
    await vi.waitFor(() => {
      const frame = lastFrame();
      expect(frame).toContain('Fix login');
      expect(frame).toContain('Add dark');
    });
  });

  it('shows action bar with shortcuts', async () => {
    const client = mockClient([]);
    const { lastFrame } = render(
      <HomeScreen
        client={client}
        onNewSession={() => {}}
        onAddRepo={() => {}}
        onAttach={() => {}}
      />,
    );
    await vi.waitFor(() => {
      const frame = lastFrame();
      expect(frame).toContain('New Session');
      expect(frame).toContain('Add Repo');
      expect(frame).toContain('Quit');
    });
  });

  it('shows error when daemon is not running', async () => {
    const err = new Error('Daemon not running');
    (err as { name: string }).name = 'DaemonNotRunningError';
    const client: IpcClient = {
      call: vi.fn().mockRejectedValue(err),
      close: vi.fn(),
    };
    const { lastFrame } = render(
      <HomeScreen
        client={client}
        onNewSession={() => {}}
        onAddRepo={() => {}}
        onAttach={() => {}}
      />,
    );
    await vi.waitFor(() => {
      expect(lastFrame()).toContain('bossd is not running');
    });
  });
});

describe('SessionList (non-interactive)', () => {
  it('renders sessions and exits', async () => {
    const sessions = [
      makeSession({ id: 'sess-001', title: 'Fix login bug' }),
    ];
    const client = mockClient(sessions);
    const { lastFrame } = render(
      <SessionList client={client} />,
    );
    await vi.waitFor(() => {
      expect(lastFrame()).toContain('Fix login');
    });
  });

  it('shows empty message when no sessions', async () => {
    const client = mockClient([]);
    const { lastFrame } = render(
      <SessionList client={client} />,
    );
    await vi.waitFor(() => {
      expect(lastFrame()).toContain('No sessions');
    });
  });
});
