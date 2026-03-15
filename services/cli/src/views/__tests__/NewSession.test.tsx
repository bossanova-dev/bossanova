import { describe, expect, it, vi } from 'vitest';
import { render } from 'ink-testing-library';
import React from 'react';
import { NewSession } from '../NewSession.js';
import type { IpcClient, Repo, ContextResolveResult, Session } from '@bossanova/shared';

function makeRepo(overrides: Partial<Repo> = {}): Repo {
  return {
    id: 'repo-001',
    displayName: 'my-project',
    localPath: '/Users/test/my-project',
    originUrl: 'https://github.com/org/my-project.git',
    defaultBaseBranch: 'main',
    worktreeBaseDir: '/Users/test/my-project/.worktrees',
    setupScript: null,
    createdAt: new Date().toISOString(),
    updatedAt: new Date().toISOString(),
    ...overrides,
  };
}

function makeSession(overrides: Partial<Session> = {}): Session {
  return {
    id: 'sess-new',
    repoId: 'repo-001',
    title: 'New session',
    plan: 'fix the bug',
    worktreePath: null,
    branchName: null,
    baseBranch: 'main',
    state: 'creating_worktree',
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

function mockClient(
  repos: Repo[] = [makeRepo()],
  context: ContextResolveResult = { type: 'none' },
): IpcClient {
  const callFn = vi.fn().mockImplementation((method: string) => {
    switch (method) {
      case 'repo.list':
        return Promise.resolve(repos);
      case 'context.resolve':
        return Promise.resolve(context);
      case 'repo.listPrs':
        return Promise.resolve({ prs: [] });
      case 'session.create':
        return Promise.resolve(makeSession());
      default:
        return Promise.reject(new Error(`Unknown method: ${method}`));
    }
  });
  return { call: callFn, close: vi.fn() };
}

describe('NewSession', () => {
  it('shows repo picker when not inside a repo', async () => {
    const client = mockClient([makeRepo()], { type: 'none' });
    const { lastFrame } = render(
      <NewSession
        client={client}
        onDone={() => {}}
        onCancel={() => {}}
      />,
    );
    await vi.waitFor(() => {
      expect(lastFrame()).toContain('Select a repository');
    });
  });

  it('auto-selects repo when inside registered repo', async () => {
    const repo = makeRepo({ id: 'repo-001' });
    const context: ContextResolveResult = { type: 'repo', repoId: 'repo-001' };
    const client = mockClient([repo], context);
    const { lastFrame } = render(
      <NewSession
        client={client}
        onDone={() => {}}
        onCancel={() => {}}
      />,
    );
    // Should skip to mode selection
    await vi.waitFor(() => {
      expect(lastFrame()).toContain('How would you like to start');
    });
  });

  it('shows error when no repos registered', async () => {
    const client = mockClient([], { type: 'none' });
    const { lastFrame } = render(
      <NewSession
        client={client}
        onDone={() => {}}
        onCancel={() => {}}
      />,
    );
    await vi.waitFor(() => {
      expect(lastFrame()).toContain('No repositories registered');
    });
  });

  it('shows New PR and Existing PR options in mode step', async () => {
    const context: ContextResolveResult = { type: 'repo', repoId: 'repo-001' };
    const client = mockClient([makeRepo()], context);
    const { lastFrame } = render(
      <NewSession
        client={client}
        onDone={() => {}}
        onCancel={() => {}}
      />,
    );
    await vi.waitFor(() => {
      const frame = lastFrame();
      expect(frame).toContain('New PR');
      expect(frame).toContain('Existing PR');
    });
  });
});
