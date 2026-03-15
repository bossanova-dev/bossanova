import { Box, Text, useApp, useInput } from 'ink';
import TextInput from 'ink-text-input';
import React, { useEffect, useState } from 'react';
import type { IpcClient, Repo, ContextResolveResult } from '@bossanova/shared';

type Step = 'path' | 'confirm' | 'setup-script' | 'registering' | 'done' | 'error';

export interface AddRepoProps {
  client: IpcClient;
  onDone: (repo: Repo) => void;
  onCancel: () => void;
}

export function AddRepo({ client, onDone, onCancel }: AddRepoProps) {
  const { exit } = useApp();
  const [step, setStep] = useState<Step>('path');
  const [error, setError] = useState<string | null>(null);

  const [path, setPath] = useState(process.cwd());
  const [setupScript, setSetupScript] = useState('');
  const [detectedInfo, setDetectedInfo] = useState<{
    name: string;
    originUrl: string;
    defaultBranch: string;
  } | null>(null);
  const [createdRepo, setCreatedRepo] = useState<Repo | null>(null);

  // Try to auto-detect context on mount
  useEffect(() => {
    (async () => {
      try {
        const ctx = await client.call('context.resolve', { cwd: process.cwd() });
        if (ctx.type === 'unregistered_repo') {
          setPath(ctx.localPath);
        }
      } catch {
        // Ignore — user can still enter path manually
      }
    })();
  }, [client]);

  useInput((input, key) => {
    if (key.escape) {
      onCancel();
      return;
    }

    if (step === 'confirm' && key.return) {
      setStep('setup-script');
    }
  });

  if (step === 'error') {
    return <Text color="red">{error}</Text>;
  }

  if (step === 'registering') {
    return <Text dimColor>Registering repository…</Text>;
  }

  if (step === 'done' && createdRepo) {
    return (
      <Text color="green">
        Repository registered: {createdRepo.displayName} ({createdRepo.localPath})
      </Text>
    );
  }

  if (step === 'path') {
    return (
      <Box flexDirection="column">
        <Text bold>Add a repository</Text>
        <Box>
          <Text>Path: </Text>
          <TextInput
            value={path}
            onChange={setPath}
            onSubmit={async (value) => {
              // Validate by trying to resolve context for this path
              try {
                const ctx = await client.call('context.resolve', { cwd: value });
                if (ctx.type === 'repo') {
                  setError(`This repository is already registered (${ctx.repoId}).`);
                  setStep('error');
                  return;
                }
                if (ctx.type === 'unregistered_repo') {
                  setDetectedInfo({
                    name: ctx.localPath.split('/').pop() ?? ctx.localPath,
                    originUrl: ctx.originUrl,
                    defaultBranch: 'main', // Will be detected by daemon during register
                  });
                  setStep('confirm');
                  return;
                }
                setError('Not a Git repository. Please enter a valid path.');
                setStep('error');
              } catch (err) {
                setError(`Failed to validate path: ${(err as Error).message}`);
                setStep('error');
              }
            }}
          />
        </Box>
        <Text dimColor>{'\n'}Enter the path to a Git repository, then press Enter. Esc to cancel.</Text>
      </Box>
    );
  }

  if (step === 'confirm') {
    return (
      <Box flexDirection="column">
        <Text bold>Detected repository:</Text>
        <Text>  Name:    {detectedInfo?.name}</Text>
        <Text>  Origin:  {detectedInfo?.originUrl}</Text>
        <Text>  Path:    {path}</Text>
        <Box marginTop={1}>
          <Text dimColor>Press Enter to continue, Esc to cancel.</Text>
        </Box>
      </Box>
    );
  }

  if (step === 'setup-script') {
    return (
      <Box flexDirection="column">
        <Text bold>Setup script (optional)</Text>
        <Text dimColor>This runs in new worktrees (e.g. `pnpm install`). Leave blank to skip.</Text>
        <Box>
          <Text>Script: </Text>
          <TextInput
            value={setupScript}
            onChange={setSetupScript}
            onSubmit={async () => {
              setStep('registering');
              try {
                const repo = await client.call('repo.register', {
                  localPath: path,
                  ...(setupScript.trim() ? { setupScript: setupScript.trim() } : {}),
                });
                setCreatedRepo(repo);
                setStep('done');
                onDone(repo);
              } catch (err) {
                setError(`Failed to register: ${(err as Error).message}`);
                setStep('error');
              }
            }}
          />
        </Box>
      </Box>
    );
  }

  return null;
}
