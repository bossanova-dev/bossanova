import { describe, expect, it, vi } from 'vitest';
import { ClaudeSupervisor } from '~/claude/supervisor';

vi.mock('@anthropic-ai/claude-agent-sdk', () => ({
  query: vi.fn(({ prompt, options }) => {
    async function* mockGenerator() {
      yield {
        type: 'system' as const,
        subtype: 'init' as const,
        session_id: `sdk-${prompt.slice(0, 8)}`,
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
        session_id: `sdk-${prompt.slice(0, 8)}`,
        duration_ms: 1000,
        duration_api_ms: 800,
        is_error: false,
        num_turns: 1,
        result: `Done: ${prompt}`,
        stop_reason: 'end_turn',
        total_cost_usd: 0.01,
        usage: { input_tokens: 100, output_tokens: 50, cache_creation_input_tokens: 0, cache_read_input_tokens: 0 },
        modelUsage: {},
        permission_denials: [],
      };
    }
    return mockGenerator();
  }),
}));

describe('ClaudeSupervisor', () => {
  it('starts a session and transitions to completed', async () => {
    const supervisor = new ClaudeSupervisor();
    await supervisor.start('s1', '/tmp/wt', 'Fix bug');

    // Wait for the async stream to complete
    await new Promise((r) => setTimeout(r, 50));

    const status = supervisor.getStatus('s1');
    expect(status?.status).toBe('completed');
    expect(status?.result).toBe('Done: Fix bug');
    expect(status?.claudeSessionId).toBe('sdk-Fix bug');
  });

  it('throws when starting a session that already exists', async () => {
    const supervisor = new ClaudeSupervisor();
    await supervisor.start('s2', '/tmp/wt', 'Plan A');

    await expect(supervisor.start('s2', '/tmp/wt', 'Plan B')).rejects.toThrow(
      'already running',
    );
  });

  it('forwards messages to the callback', async () => {
    const supervisor = new ClaudeSupervisor();
    const messages: Array<{ sessionId: string; type: string }> = [];

    supervisor.setMessageCallback((sessionId, msg) => {
      messages.push({ sessionId, type: msg.type });
    });

    await supervisor.start('s3', '/tmp/wt', 'Do stuff');
    await new Promise((r) => setTimeout(r, 50));

    expect(messages).toHaveLength(2);
    expect(messages[0]).toEqual({ sessionId: 's3', type: 'system' });
    expect(messages[1]).toEqual({ sessionId: 's3', type: 'result' });
  });

  it('stops a session', async () => {
    const supervisor = new ClaudeSupervisor();
    await supervisor.start('s4', '/tmp/wt', 'Long task');
    supervisor.stop('s4');

    const status = supervisor.getStatus('s4');
    expect(status?.status).toBe('completed');
  });

  it('removes a session from tracking', async () => {
    const supervisor = new ClaudeSupervisor();
    await supervisor.start('s5', '/tmp/wt', 'Quick task');
    await new Promise((r) => setTimeout(r, 50));

    supervisor.remove('s5');
    expect(supervisor.getStatus('s5')).toBeNull();
  });

  it('hasActiveSessions returns false when no sessions running', async () => {
    const supervisor = new ClaudeSupervisor();
    expect(supervisor.hasActiveSessions()).toBe(false);

    await supervisor.start('s6', '/tmp/wt', 'Task');
    await new Promise((r) => setTimeout(r, 50));

    // After completion, no active sessions
    expect(supervisor.hasActiveSessions()).toBe(false);
  });

  it('throws when stopping a nonexistent session', () => {
    const supervisor = new ClaudeSupervisor();
    expect(() => supervisor.stop('nonexistent')).toThrow('not found');
  });
});
