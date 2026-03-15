import type { SDKMessage } from '@anthropic-ai/claude-agent-sdk';
import { query } from '@anthropic-ai/claude-agent-sdk';

export interface ClaudeHandle {
  /** The async generator yielding SDK messages. */
  messages: AsyncGenerator<SDKMessage, void>;
  /** The session ID assigned by the SDK (available after first `init` message). */
  sessionId: string | null;
  /** The abort controller for cancelling the session. */
  abortController: AbortController;
}

const SYSTEM_PROMPT = `You are an autonomous coding agent working inside a Bossanova session.
Your task is to implement the plan provided by the user. Work autonomously:
1. Read the codebase to understand context
2. Make the necessary changes
3. Run tests and fix any failures
4. Commit your changes with a clear commit message

When done, provide a brief summary of what you accomplished.`;

/**
 * Start a Claude session in a worktree.
 * Returns a handle with the async message generator and abort controller.
 */
export function startClaudeSession(
  worktreePath: string,
  plan: string,
  sessionId: string,
): ClaudeHandle {
  const abortController = new AbortController();

  const q = query({
    prompt: plan,
    options: {
      cwd: worktreePath,
      permissionMode: 'bypassPermissions',
      allowDangerouslySkipPermissions: true,
      systemPrompt: SYSTEM_PROMPT,
      abortController,
    },
  });

  const handle: ClaudeHandle = {
    messages: q,
    sessionId: null,
    abortController,
  };

  return handle;
}

/**
 * Stop a Claude session by aborting it.
 */
export function stopClaudeSession(handle: ClaudeHandle): void {
  handle.abortController.abort();
}
