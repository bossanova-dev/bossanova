import { Box, Text, useApp, useInput } from 'ink';
import React, { useEffect, useState } from 'react';
import type { IpcClient, Repo } from '@bossanova/shared';

// --- Column widths ---

const COL = {
  id: 12,
  name: 20,
  path: 35,
  branch: 10,
  setup: 20,
} as const;

function pad(str: string, width: number): string {
  return str.length >= width ? str.slice(0, width) : str + ' '.repeat(width - str.length);
}

// --- RepoList (non-interactive, for `boss repo ls`) ---

export interface RepoListProps {
  client: IpcClient;
}

export function RepoList({ client }: RepoListProps) {
  const { exit } = useApp();
  const [repos, setRepos] = useState<Repo[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    (async () => {
      try {
        const result = await client.call('repo.list', {});
        setRepos(result);
      } catch (err) {
        if ((err as { name?: string }).name === 'DaemonNotRunningError') {
          setError('bossd is not running. Start it with: boss daemon start');
        } else {
          setError(`Failed to list repos: ${(err as Error).message}`);
        }
      }
    })();
  }, [client]);

  useEffect(() => {
    if (repos !== null || error !== null) {
      const timer = setTimeout(() => exit(), 0);
      return () => clearTimeout(timer);
    }
  }, [repos, error, exit]);

  if (error) {
    return <Text color="red">{error}</Text>;
  }

  if (repos === null) {
    return <Text dimColor>Loading…</Text>;
  }

  if (repos.length === 0) {
    return <Text dimColor>No repositories registered. Run: boss repo add</Text>;
  }

  return (
    <Box flexDirection="column">
      <Box>
        <Text bold dimColor>
          {pad('ID', COL.id)}
          {pad('Name', COL.name)}
          {pad('Path', COL.path)}
          {pad('Branch', COL.branch)}
          {pad('Setup Script', COL.setup)}
        </Text>
      </Box>
      {repos.map((r) => (
        <Box key={r.id}>
          <Text>
            {pad(r.id.slice(0, COL.id), COL.id)}
            {pad(r.displayName, COL.name)}
            {pad(r.localPath, COL.path)}
            {pad(r.defaultBaseBranch, COL.branch)}
            {pad(r.setupScript ?? '—', COL.setup)}
          </Text>
        </Box>
      ))}
    </Box>
  );
}

// --- RepoRemove (for `boss repo remove <id>`) ---

export interface RepoRemoveProps {
  client: IpcClient;
  repoId: string;
}

export function RepoRemove({ client, repoId }: RepoRemoveProps) {
  const { exit } = useApp();
  const [status, setStatus] = useState<'confirming' | 'removing' | 'done' | 'error'>('confirming');
  const [error, setError] = useState<string | null>(null);

  useInput((input, key) => {
    if (status !== 'confirming') return;
    if (input === 'y' || input === 'Y') {
      setStatus('removing');
      (async () => {
        try {
          await client.call('repo.remove', { repoId });
          setStatus('done');
        } catch (err) {
          setError(`Failed to remove: ${(err as Error).message}`);
          setStatus('error');
        }
      })();
    } else if (input === 'n' || input === 'N' || key.escape) {
      exit();
    }
  });

  useEffect(() => {
    if (status === 'done' || status === 'error') {
      const timer = setTimeout(() => exit(), 0);
      return () => clearTimeout(timer);
    }
  }, [status, exit]);

  if (status === 'error') {
    return <Text color="red">{error}</Text>;
  }

  if (status === 'done') {
    return <Text color="green">Repository {repoId} removed.</Text>;
  }

  if (status === 'removing') {
    return <Text dimColor>Removing…</Text>;
  }

  return (
    <Text>
      Remove repository <Text bold>{repoId}</Text>? (y/n)
    </Text>
  );
}
