import type { ContextResolveResult, IpcClient, Repo } from '@bossanova/shared';
import { render } from 'ink-testing-library';
import React from 'react';
import { describe, expect, it, vi } from 'vitest';
import { AddRepo } from '../AddRepo.js';
import { RepoList, RepoRemove } from '../RepoList.js';

function makeRepo(overrides: Partial<Repo> = {}): Repo {
  return {
    id: 'repo-001',
    displayName: 'my-project',
    localPath: '/Users/test/my-project',
    originUrl: 'https://github.com/org/my-project.git',
    defaultBaseBranch: 'main',
    worktreeBaseDir: '/Users/test/my-project/.worktrees',
    setupScript: 'pnpm install',
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
      case 'repo.register':
        return Promise.resolve(makeRepo());
      case 'repo.remove':
        return Promise.resolve({ removed: true });
      default:
        return Promise.reject(new Error(`Unknown method: ${method}`));
    }
  });
  return { call: callFn, close: vi.fn() };
}

describe('AddRepo', () => {
  it('shows path input on start', () => {
    const client = mockClient();
    const { lastFrame } = render(<AddRepo client={client} onDone={() => {}} onCancel={() => {}} />);
    expect(lastFrame()).toContain('Add a repository');
  });
});

describe('RepoList', () => {
  it('shows repos in a table', async () => {
    const repos = [
      makeRepo({ id: 'repo-001', displayName: 'my-project', setupScript: 'pnpm install' }),
      makeRepo({ id: 'repo-002', displayName: 'other-project', setupScript: null }),
    ];
    const client = mockClient(repos);
    const { lastFrame } = render(<RepoList client={client} />);
    await vi.waitFor(() => {
      const frame = lastFrame();
      expect(frame).toContain('my-project');
      expect(frame).toContain('other-project');
    });
  });

  it('shows empty message when no repos', async () => {
    const client = mockClient([]);
    const { lastFrame } = render(<RepoList client={client} />);
    await vi.waitFor(() => {
      expect(lastFrame()).toContain('No repositories registered');
    });
  });
});

describe('RepoRemove', () => {
  it('shows confirmation prompt', () => {
    const client = mockClient();
    const { lastFrame } = render(<RepoRemove client={client} repoId="repo-001" />);
    expect(lastFrame()).toContain('Remove repository');
    expect(lastFrame()).toContain('repo-001');
  });

  it('removes on y input', async () => {
    const client = mockClient();
    const { lastFrame, stdin } = render(<RepoRemove client={client} repoId="repo-001" />);
    stdin.write('y');
    await vi.waitFor(() => {
      expect(lastFrame()).toContain('removed');
    });
  });
});
