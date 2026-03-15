import type { SessionStateValue } from './session-machine.js';

// --- Repo ---

export interface Repo {
  id: string;
  displayName: string;
  localPath: string;
  originUrl: string;
  defaultBaseBranch: string;
  worktreeBaseDir: string;
  setupScript: string | null;
  createdAt: string;
  updatedAt: string;
}

// --- Session ---

export interface Session {
  id: string;
  repoId: string;
  title: string;
  plan: string;
  worktreePath: string | null;
  branchName: string | null;
  baseBranch: string;
  state: SessionStateValue;
  claudeSessionId: string | null;
  prNumber: number | null;
  prUrl: string | null;
  lastCheckState: 'pending' | 'passed' | 'failed' | null;
  automationEnabled: boolean;
  attemptCount: number;
  blockedReason: string | null;
  createdAt: string;
  updatedAt: string;
}

// --- Attempt ---

export interface Attempt {
  id: string;
  sessionId: string;
  trigger: 'check_failed' | 'conflict' | 'review_feedback' | 'manual';
  startedAt: string;
  completedAt: string | null;
  result: 'success' | 'failure' | null;
  error: string | null;
}
