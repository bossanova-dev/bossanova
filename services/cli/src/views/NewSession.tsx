import { Box, Text, useApp, useInput } from 'ink';
import TextInput from 'ink-text-input';
import React, { useEffect, useState } from 'react';
import type { IpcClient, Repo, ContextResolveResult } from '@bossanova/shared';

// --- Step types ---

type Step = 'repo' | 'mode' | 'pick-pr' | 'plan' | 'confirm' | 'creating' | 'done' | 'error';

interface PrOption {
  number: number;
  title: string;
  headBranch: string;
}

// --- NewSession component ---

export interface NewSessionProps {
  client: IpcClient;
  initialPlan?: string;
  onDone: (sessionId: string) => void;
  onCancel: () => void;
}

export function NewSession({
  client,
  initialPlan,
  onDone,
  onCancel,
}: NewSessionProps) {
  const { exit } = useApp();
  const [step, setStep] = useState<Step>('repo');
  const [error, setError] = useState<string | null>(null);

  // Wizard state
  const [repos, setRepos] = useState<Repo[]>([]);
  const [context, setContext] = useState<ContextResolveResult | null>(null);
  const [selectedRepoIdx, setSelectedRepoIdx] = useState(0);
  const [selectedRepo, setSelectedRepo] = useState<Repo | null>(null);
  const [mode, setMode] = useState<'new' | 'existing'>('new');
  const [prs, setPrs] = useState<PrOption[]>([]);
  const [selectedPrIdx, setSelectedPrIdx] = useState(0);
  const [selectedPr, setSelectedPr] = useState<PrOption | null>(null);
  const [plan, setPlan] = useState(initialPlan ?? '');
  const [title, setTitle] = useState('');
  const [createdSessionId, setCreatedSessionId] = useState<string | null>(null);

  // Load repos and context on mount
  useEffect(() => {
    (async () => {
      try {
        const [repoList, ctx] = await Promise.all([
          client.call('repo.list', {}),
          client.call('context.resolve', { cwd: process.cwd() }),
        ]);
        setRepos(repoList);
        setContext(ctx);

        // Auto-select if inside a registered repo
        if (ctx.type === 'repo' || ctx.type === 'session') {
          const repoId = ctx.type === 'session' ? ctx.repoId : ctx.repoId;
          const match = repoList.find((r: Repo) => r.id === repoId);
          if (match) {
            setSelectedRepo(match);
            setStep('mode');
            return;
          }
        }

        if (repoList.length === 0) {
          setError('No repositories registered. Run: boss repo add');
          setStep('error');
          return;
        }

        // Show picker
        setStep('repo');
      } catch (err) {
        setError(`Failed to load repos: ${(err as Error).message}`);
        setStep('error');
      }
    })();
  }, [client]);

  // Keyboard handling
  useInput((input, key) => {
    if (key.escape) {
      onCancel();
      return;
    }

    switch (step) {
      case 'repo': {
        if (key.upArrow) {
          setSelectedRepoIdx((i) => Math.max(0, i - 1));
        } else if (key.downArrow) {
          setSelectedRepoIdx((i) => Math.min(repos.length - 1, i + 1));
        } else if (key.return) {
          setSelectedRepo(repos[selectedRepoIdx]);
          setStep('mode');
        }
        break;
      }
      case 'mode': {
        if (key.upArrow || key.downArrow) {
          setMode((m) => (m === 'new' ? 'existing' : 'new'));
        } else if (key.return) {
          if (mode === 'existing' && selectedRepo) {
            // Load PRs
            (async () => {
              try {
                const result = await client.call('repo.listPrs', { repoId: selectedRepo.id });
                setPrs(result.prs);
                setStep('pick-pr');
              } catch (err) {
                setError(`Failed to load PRs: ${(err as Error).message}`);
                setStep('error');
              }
            })();
          } else {
            setStep('plan');
          }
        }
        break;
      }
      case 'pick-pr': {
        if (key.upArrow) {
          setSelectedPrIdx((i) => Math.max(0, i - 1));
        } else if (key.downArrow) {
          setSelectedPrIdx((i) => Math.min(prs.length - 1, i + 1));
        } else if (key.return && prs.length > 0) {
          setSelectedPr(prs[selectedPrIdx]);
          setStep('plan');
        }
        break;
      }
      case 'confirm': {
        if (key.return) {
          // Create the session
          setStep('creating');
          (async () => {
            try {
              const session = await client.call('session.create', {
                repoId: selectedRepo!.id,
                title: title || plan.slice(0, 60),
                plan,
                ...(selectedPr ? { prNumber: selectedPr.number } : {}),
              });
              setCreatedSessionId(session.id);
              setStep('done');
              onDone(session.id);
            } catch (err) {
              setError(`Failed to create session: ${(err as Error).message}`);
              setStep('error');
            }
          })();
        }
        break;
      }
    }
  });

  // --- Render steps ---

  if (step === 'error') {
    return <Text color="red">{error}</Text>;
  }

  if (step === 'creating') {
    return <Text dimColor>Creating session…</Text>;
  }

  if (step === 'done') {
    return (
      <Text color="green">
        Session created: {createdSessionId}
      </Text>
    );
  }

  if (step === 'repo') {
    return (
      <Box flexDirection="column">
        <Text bold>Select a repository:</Text>
        {repos.map((r, i) => (
          <Text key={r.id}>
            {i === selectedRepoIdx ? '▸ ' : '  '}
            <Text bold={i === selectedRepoIdx}>{r.displayName}</Text>
            <Text dimColor> ({r.localPath})</Text>
          </Text>
        ))}
        <Text dimColor>{'\n'}↑↓ navigate, Enter to select, Esc to cancel</Text>
      </Box>
    );
  }

  if (step === 'mode') {
    return (
      <Box flexDirection="column">
        <Text bold>How would you like to start?</Text>
        <Text>
          {mode === 'new' ? '▸ ' : '  '}
          <Text bold={mode === 'new'}>New PR</Text>
          <Text dimColor> — create a fresh branch</Text>
        </Text>
        <Text>
          {mode === 'existing' ? '▸ ' : '  '}
          <Text bold={mode === 'existing'}>Existing PR</Text>
          <Text dimColor> — pick from open PRs</Text>
        </Text>
        <Text dimColor>{'\n'}↑↓ navigate, Enter to select, Esc to cancel</Text>
      </Box>
    );
  }

  if (step === 'pick-pr') {
    if (prs.length === 0) {
      return <Text dimColor>No open PRs found. Press Esc to go back.</Text>;
    }
    return (
      <Box flexDirection="column">
        <Text bold>Select a pull request:</Text>
        {prs.map((pr, i) => (
          <Text key={pr.number}>
            {i === selectedPrIdx ? '▸ ' : '  '}
            <Text bold={i === selectedPrIdx}>#{pr.number}</Text>
            <Text> {pr.title}</Text>
            <Text dimColor> ({pr.headBranch})</Text>
          </Text>
        ))}
        <Text dimColor>{'\n'}↑↓ navigate, Enter to select, Esc to cancel</Text>
      </Box>
    );
  }

  if (step === 'plan') {
    return (
      <Box flexDirection="column">
        <Text bold>What should Claude do?</Text>
        <Box>
          <Text>Plan: </Text>
          <TextInput
            value={plan}
            onChange={setPlan}
            onSubmit={() => {
              if (plan.trim()) {
                setStep('confirm');
              }
            }}
          />
        </Box>
        <Text dimColor>{'\n'}Type your plan, then press Enter. Esc to cancel.</Text>
      </Box>
    );
  }

  if (step === 'confirm') {
    return (
      <Box flexDirection="column">
        <Text bold>Confirm new session:</Text>
        <Text>  Repo:   {selectedRepo?.displayName}</Text>
        <Text>  Mode:   {selectedPr ? `Existing PR #${selectedPr.number}` : 'New PR'}</Text>
        <Text>  Plan:   {plan}</Text>
        <Box marginTop={1}>
          <Text dimColor>Press Enter to create, Esc to cancel.</Text>
        </Box>
      </Box>
    );
  }

  return null;
}
