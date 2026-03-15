import type { SDKMessage } from '@anthropic-ai/claude-agent-sdk';
import type { ClaudeHandle } from '~/claude/session';
import { startClaudeSession, stopClaudeSession } from '~/claude/session';

export type SupervisedSessionStatus = 'running' | 'paused' | 'completed' | 'error';

export interface SupervisedSession {
  handle: ClaudeHandle;
  status: SupervisedSessionStatus;
  claudeSessionId: string | null;
  result: string | null;
  error: string | null;
}

export type MessageCallback = (sessionId: string, message: SDKMessage) => void;

/**
 * Manages active Claude sessions, consuming their message streams
 * and tracking state.
 */
export class ClaudeSupervisor {
  private sessions = new Map<string, SupervisedSession>();
  private onMessage: MessageCallback | null = null;

  /** Register a callback to receive messages from all sessions. */
  setMessageCallback(cb: MessageCallback): void {
    this.onMessage = cb;
  }

  /** Start a new Claude session in a worktree. */
  async start(sessionId: string, worktreePath: string, plan: string): Promise<void> {
    if (this.sessions.has(sessionId)) {
      throw new Error(`Session ${sessionId} is already running`);
    }

    const handle = startClaudeSession(worktreePath, plan, sessionId);
    const supervised: SupervisedSession = {
      handle,
      status: 'running',
      claudeSessionId: null,
      result: null,
      error: null,
    };
    this.sessions.set(sessionId, supervised);

    // Consume the message stream in the background
    this.consumeMessages(sessionId, supervised);
  }

  /** Stop a running session. */
  stop(sessionId: string): void {
    const session = this.sessions.get(sessionId);
    if (!session) {
      throw new Error(`Session ${sessionId} not found`);
    }
    stopClaudeSession(session.handle);
    session.status = 'completed';
  }

  /** Pause a running session (aborts current stream). */
  pause(sessionId: string): void {
    const session = this.sessions.get(sessionId);
    if (!session || session.status !== 'running') {
      throw new Error(`Session ${sessionId} is not running`);
    }
    stopClaudeSession(session.handle);
    session.status = 'paused';
  }

  /** Resume a paused session with the same plan. */
  async resume(sessionId: string, worktreePath: string, plan: string): Promise<void> {
    const session = this.sessions.get(sessionId);
    if (!session || session.status !== 'paused') {
      throw new Error(`Session ${sessionId} is not paused`);
    }

    const handle = startClaudeSession(worktreePath, plan, sessionId);
    session.handle = handle;
    session.status = 'running';
    session.result = null;
    session.error = null;

    this.consumeMessages(sessionId, session);
  }

  /** Get the status of a session. */
  getStatus(sessionId: string): SupervisedSession | null {
    return this.sessions.get(sessionId) ?? null;
  }

  /** Check if any sessions are currently running. */
  hasActiveSessions(): boolean {
    for (const session of this.sessions.values()) {
      if (session.status === 'running') return true;
    }
    return false;
  }

  /** Remove a finished/errored session from tracking. */
  remove(sessionId: string): void {
    const session = this.sessions.get(sessionId);
    if (session?.status === 'running') {
      stopClaudeSession(session.handle);
    }
    this.sessions.delete(sessionId);
  }

  private async consumeMessages(sessionId: string, supervised: SupervisedSession): Promise<void> {
    try {
      for await (const message of supervised.handle.messages) {
        // Capture the Claude SDK session ID from the init message
        if (message.type === 'system' && 'subtype' in message && message.subtype === 'init') {
          supervised.claudeSessionId = message.session_id;
        }

        // Capture result
        if (message.type === 'result') {
          if ('result' in message) {
            supervised.result = message.result as string;
          }
          if ('errors' in message && Array.isArray(message.errors)) {
            supervised.error = message.errors.join('; ');
            supervised.status = 'error';
          } else {
            supervised.status = 'completed';
          }
        }

        // Forward to callback
        this.onMessage?.(sessionId, message);
      }

      // Stream ended normally
      if (supervised.status === 'running') {
        supervised.status = 'completed';
      }
    } catch (err) {
      // Handle abort errors gracefully (pause/stop)
      if (supervised.status !== 'paused') {
        supervised.status = 'error';
        supervised.error = err instanceof Error ? err.message : 'Unknown error';
      }
    }
  }
}
