import { describe, expect, it, vi } from 'vitest';
import { startClaudeSession, stopClaudeSession } from '~/claude/session';

vi.mock('@anthropic-ai/claude-agent-sdk', () => ({
  query: vi.fn(({ prompt, options }) => {
    // Return a mock async generator
    async function* mockGenerator() {
      yield {
        type: 'system' as const,
        subtype: 'init' as const,
        session_id: 'mock-session-123',
        uuid: 'uuid-1',
        tools: [],
        mcp_servers: [],
        model: 'claude-sonnet-4-5-20250929',
        permissionMode: options?.permissionMode ?? 'default',
        slash_commands: [],
        output_style: 'text',
        claude_code_version: '1.0.0',
        cwd: options?.cwd ?? '.',
        apiKeySource: 'user' as const,
        skills: [],
        plugins: [],
      };
      yield {
        type: 'result' as const,
        subtype: 'success' as const,
        uuid: 'uuid-2',
        session_id: 'mock-session-123',
        duration_ms: 1000,
        duration_api_ms: 800,
        is_error: false,
        num_turns: 1,
        result: `Implemented: ${prompt}`,
        stop_reason: 'end_turn',
        total_cost_usd: 0.01,
        usage: {
          input_tokens: 100,
          output_tokens: 50,
          cache_creation_input_tokens: 0,
          cache_read_input_tokens: 0,
        },
        modelUsage: {},
        permission_denials: [],
      };
    }
    return mockGenerator();
  }),
}));

describe('startClaudeSession', () => {
  it('returns a handle with messages generator and abort controller', () => {
    const handle = startClaudeSession('/tmp/worktree', 'Fix the bug', 'session-123');

    expect(handle.messages).toBeDefined();
    expect(handle.abortController).toBeInstanceOf(AbortController);
    expect(handle.sessionId).toBeNull();
  });

  it('yields messages from the SDK', async () => {
    const handle = startClaudeSession('/tmp/worktree', 'Add a feature', 'session-456');

    const messages = [];
    for await (const msg of handle.messages) {
      messages.push(msg);
    }

    expect(messages).toHaveLength(2);
    expect(messages[0].type).toBe('system');
    expect(messages[1].type).toBe('result');
  });
});

describe('stopClaudeSession', () => {
  it('aborts the session via the abort controller', () => {
    const handle = startClaudeSession('/tmp/worktree', 'Test plan', 'session-789');

    expect(handle.abortController.signal.aborted).toBe(false);
    stopClaudeSession(handle);
    expect(handle.abortController.signal.aborted).toBe(true);
  });
});
