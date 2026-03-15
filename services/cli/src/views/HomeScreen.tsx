import { Box, Text, useApp, useInput } from 'ink';
import React, { useCallback, useEffect, useState } from 'react';
import type { IpcClient, Session, DaemonNotRunningError } from '@bossanova/shared';

// --- State color mapping ---

type StateColor = 'green' | 'yellow' | 'red' | 'gray' | 'cyan';

const stateColors: Record<string, StateColor> = {
  green_draft: 'green',
  ready_for_review: 'green',
  merged: 'green',
  implementing_plan: 'yellow',
  awaiting_checks: 'yellow',
  fixing_checks: 'red',
  blocked: 'red',
  closed: 'gray',
  creating_worktree: 'cyan',
  starting_claude: 'cyan',
  pushing_branch: 'cyan',
  opening_draft_pr: 'cyan',
};

function stateColor(state: string): StateColor {
  return stateColors[state] ?? 'gray';
}

function formatState(state: string): string {
  return state.replace(/_/g, ' ');
}

function shortId(id: string): string {
  return id.length > 8 ? id.slice(0, 8) : id;
}

function relativeTime(dateStr: string): string {
  const now = Date.now();
  const then = new Date(dateStr).getTime();
  const diffMs = now - then;
  const diffMins = Math.floor(diffMs / 60_000);
  if (diffMins < 1) return 'just now';
  if (diffMins < 60) return `${diffMins}m ago`;
  const diffHours = Math.floor(diffMins / 60);
  if (diffHours < 24) return `${diffHours}h ago`;
  const diffDays = Math.floor(diffHours / 24);
  return `${diffDays}d ago`;
}

// --- Column widths ---

const COL = {
  id: 10,
  title: 30,
  state: 18,
  branch: 20,
  pr: 6,
  updated: 10,
} as const;

function pad(str: string, width: number): string {
  return str.length >= width ? str.slice(0, width) : str + ' '.repeat(width - str.length);
}

// --- SessionRow component ---

function SessionRow({
  session,
  selected,
}: {
  session: Session;
  selected: boolean;
}) {
  const color = stateColor(session.state);
  const prefix = selected ? '▸ ' : '  ';
  return (
    <Box>
      <Text inverse={selected} bold={selected}>
        {prefix}
        {pad(shortId(session.id), COL.id)}
        {pad(session.title.slice(0, COL.title), COL.title)}
      </Text>
      <Text color={color} inverse={selected} bold={selected}>
        {pad(formatState(session.state), COL.state)}
      </Text>
      <Text inverse={selected} dimColor={!selected}>
        {pad(session.branchName ?? '—', COL.branch)}
        {pad(session.prNumber ? `#${session.prNumber}` : '—', COL.pr)}
        {pad(relativeTime(session.updatedAt), COL.updated)}
      </Text>
    </Box>
  );
}

// --- Header ---

function Header() {
  return (
    <Box>
      <Text bold dimColor>
        {'  '}
        {pad('ID', COL.id)}
        {pad('Title', COL.title)}
        {pad('State', COL.state)}
        {pad('Branch', COL.branch)}
        {pad('PR', COL.pr)}
        {pad('Updated', COL.updated)}
      </Text>
    </Box>
  );
}

// --- ActionBar ---

function ActionBar() {
  return (
    <Box marginTop={1}>
      <Text dimColor>
        <Text bold color="cyan">n</Text> New Session{'  '}
        <Text bold color="cyan">r</Text> Add Repo{'  '}
        <Text bold color="cyan">q</Text> Quit
      </Text>
    </Box>
  );
}

// --- HomeScreen ---

export interface HomeScreenProps {
  client: IpcClient;
  repoId?: string;
  onNewSession: () => void;
  onAddRepo: () => void;
  onAttach: (sessionId: string) => void;
}

export function HomeScreen({
  client,
  repoId,
  onNewSession,
  onAddRepo,
  onAttach,
}: HomeScreenProps) {
  const { exit } = useApp();
  const [sessions, setSessions] = useState<Session[]>([]);
  const [selectedIndex, setSelectedIndex] = useState(0);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  const fetchSessions = useCallback(async () => {
    try {
      const params = repoId ? { repoId } : {};
      const result = await client.call('session.list', params as never);
      setSessions(result);
      setError(null);
    } catch (err) {
      if ((err as { name?: string }).name === 'DaemonNotRunningError') {
        setError('bossd is not running. Start it with: boss daemon start');
      } else {
        setError(`Failed to connect: ${(err as Error).message}`);
      }
    } finally {
      setLoading(false);
    }
  }, [client, repoId]);

  // Initial fetch + polling every 2s
  useEffect(() => {
    fetchSessions();
    const interval = setInterval(fetchSessions, 2000);
    return () => clearInterval(interval);
  }, [fetchSessions]);

  // Keep selectedIndex in bounds
  useEffect(() => {
    if (selectedIndex >= sessions.length && sessions.length > 0) {
      setSelectedIndex(sessions.length - 1);
    }
  }, [sessions.length, selectedIndex]);

  useInput((input, key) => {
    if (input === 'q') {
      exit();
      return;
    }
    if (input === 'n') {
      onNewSession();
      return;
    }
    if (input === 'r') {
      onAddRepo();
      return;
    }
    if (key.upArrow) {
      setSelectedIndex((i) => Math.max(0, i - 1));
      return;
    }
    if (key.downArrow) {
      setSelectedIndex((i) => Math.min(sessions.length - 1, i + 1));
      return;
    }
    if (key.return && sessions.length > 0) {
      onAttach(sessions[selectedIndex].id);
    }
  });

  if (error) {
    return (
      <Box flexDirection="column">
        <Text color="red">{error}</Text>
      </Box>
    );
  }

  if (loading) {
    return <Text dimColor>Loading sessions…</Text>;
  }

  return (
    <Box flexDirection="column">
      <Text bold>
        boss{repoId ? ` (repo: ${repoId})` : ''}
      </Text>
      <Box marginTop={1} flexDirection="column">
        <Header />
        {sessions.length === 0 ? (
          <Text dimColor>  No sessions. Press n to create one.</Text>
        ) : (
          sessions.map((s, i) => (
            <SessionRow key={s.id} session={s} selected={i === selectedIndex} />
          ))
        )}
      </Box>
      <ActionBar />
    </Box>
  );
}

// --- SessionList (non-interactive, for `boss ls`) ---

export interface SessionListProps {
  client: IpcClient;
  repoId?: string;
}

export function SessionList({ client, repoId }: SessionListProps) {
  const { exit } = useApp();
  const [sessions, setSessions] = useState<Session[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    (async () => {
      try {
        const params = repoId ? { repoId } : {};
        const result = await client.call('session.list', params as never);
        setSessions(result);
      } catch (err) {
        if ((err as { name?: string }).name === 'DaemonNotRunningError') {
          setError('bossd is not running. Start it with: boss daemon start');
        } else {
          setError(`Failed to connect: ${(err as Error).message}`);
        }
      }
    })();
  }, [client, repoId, exit]);

  // Exit after render when data is ready
  useEffect(() => {
    if (sessions !== null || error !== null) {
      // Allow one render cycle for output, then exit
      const timer = setTimeout(() => exit(), 0);
      return () => clearTimeout(timer);
    }
  }, [sessions, error, exit]);

  if (error) {
    return <Text color="red">{error}</Text>;
  }

  if (sessions === null) {
    return <Text dimColor>Loading…</Text>;
  }

  if (sessions.length === 0) {
    return <Text dimColor>No sessions.</Text>;
  }

  return (
    <Box flexDirection="column">
      <Header />
      {sessions.map((s) => (
        <SessionRow key={s.id} session={s} selected={false} />
      ))}
    </Box>
  );
}
