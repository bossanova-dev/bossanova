import type { IpcClient, Session } from '@bossanova/shared';
import { Box, Text, useApp, useInput } from 'ink';
import React, { useCallback, useEffect, useState } from 'react';

// --- State color mapping (shared with HomeScreen) ---

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

// --- AttachView ---

export interface AttachViewProps {
  client: IpcClient;
  sessionId: string;
}

export function AttachView({ client, sessionId }: AttachViewProps) {
  const { exit } = useApp();
  const [session, setSession] = useState<Session | null>(null);
  const [lines, setLines] = useState<string[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  const fetchData = useCallback(async () => {
    try {
      const [sess, logsResult] = await Promise.all([
        client.call('session.get', { sessionId }),
        client.call('session.logs', { sessionId, tail: 50 }),
      ]);
      setSession(sess);
      setLines(logsResult.lines);
      setError(null);
    } catch (err) {
      if ((err as { name?: string }).name === 'DaemonNotRunningError') {
        setError('bossd is not running. Start it with: boss daemon start');
      } else {
        setError(`Error: ${(err as Error).message}`);
      }
    } finally {
      setLoading(false);
    }
  }, [client, sessionId]);

  // Initial fetch + poll every 1s
  useEffect(() => {
    fetchData();
    const interval = setInterval(fetchData, 1000);
    return () => clearInterval(interval);
  }, [fetchData]);

  useInput((input) => {
    if (input === 'q') {
      exit();
    }
  });

  if (error) {
    return <Text color="red">{error}</Text>;
  }

  if (loading) {
    return <Text dimColor>Attaching to session {sessionId}…</Text>;
  }

  if (!session) {
    return <Text color="red">Session not found: {sessionId}</Text>;
  }

  const shortId = session.id.length > 8 ? session.id.slice(0, 8) : session.id;

  return (
    <Box flexDirection="column">
      <Box>
        <Text bold>
          {shortId} · {session.title}
        </Text>
        <Text> </Text>
        <Text color={stateColor(session.state)}>[{formatState(session.state)}]</Text>
      </Box>
      {session.branchName && (
        <Text dimColor>
          branch: {session.branchName}
          {session.prNumber ? ` · PR #${session.prNumber}` : ''}
        </Text>
      )}
      <Box marginTop={1} flexDirection="column">
        {lines.length === 0 ? (
          <Text dimColor>No output yet…</Text>
        ) : (
          lines.map((line, i) => <Text key={i}>{line}</Text>)
        )}
      </Box>
      <Box marginTop={1}>
        <Text dimColor>
          <Text bold color="cyan">
            q
          </Text>{' '}
          Quit
        </Text>
      </Box>
    </Box>
  );
}
