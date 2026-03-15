import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import type { SDKMessage } from '@anthropic-ai/claude-agent-sdk';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { appendToLog, getLogPath, readLog } from '~/claude/logger';

// Override the log directory for tests
const tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'boss-log-test-'));
vi.stubEnv('HOME', tmpDir);

describe('session logger', () => {
  afterEach(() => {
    // Clean up log files
    const logDir = path.join(tmpDir, 'Library/Application Support/bossanova/logs');
    try {
      fs.rmSync(logDir, { recursive: true, force: true });
    } catch {
      // Already gone
    }
  });

  describe('getLogPath', () => {
    it('returns path under the logs directory', () => {
      const logPath = getLogPath('session-123');
      expect(logPath).toContain('bossanova/logs/session-123.log');
    });
  });

  describe('appendToLog', () => {
    it('creates the log file and writes system init messages', () => {
      const initMsg = {
        type: 'system',
        subtype: 'init',
        session_id: 'test-1',
        uuid: 'u1',
        tools: [],
        mcp_servers: [],
        model: 'claude-sonnet-4-5-20250929',
        permissionMode: 'default',
        slash_commands: [],
        output_style: 'text',
        claude_code_version: '1.0.0',
        cwd: '/tmp',
        apiKeySource: 'user',
        skills: [],
        plugins: [],
      } as unknown as SDKMessage;

      appendToLog('test-1', initMsg);

      const content = readLog('test-1');
      expect(content).toContain('[system] Session initialized');
      expect(content).toContain('claude-sonnet');
    });

    it('writes assistant text messages', () => {
      const assistantMsg = {
        type: 'assistant',
        uuid: 'u2',
        session_id: 'test-2',
        message: {
          content: [{ type: 'text', text: 'I will fix the bug.' }],
        },
        parent_tool_use_id: null,
      } as unknown as SDKMessage;

      appendToLog('test-2', assistantMsg);

      const content = readLog('test-2');
      expect(content).toContain('[assistant] I will fix the bug.');
    });

    it('writes tool use references', () => {
      const toolMsg = {
        type: 'assistant',
        uuid: 'u3',
        session_id: 'test-3',
        message: {
          content: [
            { type: 'text', text: 'Let me read the file.' },
            { type: 'tool_use', name: 'Read', id: 'tu1', input: {} },
          ],
        },
        parent_tool_use_id: null,
      } as unknown as SDKMessage;

      appendToLog('test-3', toolMsg);

      const content = readLog('test-3');
      expect(content).toContain('[assistant] Let me read the file. [tool: Read]');
    });

    it('writes result messages', () => {
      const resultMsg = {
        type: 'result',
        subtype: 'success',
        uuid: 'u4',
        session_id: 'test-4',
        result: 'Successfully fixed the bug.',
        duration_ms: 1000,
        duration_api_ms: 800,
        is_error: false,
        num_turns: 3,
        stop_reason: 'end_turn',
        total_cost_usd: 0.05,
        usage: { input_tokens: 200, output_tokens: 100, cache_creation_input_tokens: 0, cache_read_input_tokens: 0 },
        modelUsage: {},
        permission_denials: [],
      } as unknown as SDKMessage;

      appendToLog('test-4', resultMsg);

      const content = readLog('test-4');
      expect(content).toContain('[result] Successfully fixed the bug.');
    });
  });

  describe('readLog', () => {
    it('returns null for nonexistent session', () => {
      expect(readLog('nonexistent')).toBeNull();
    });

    it('returns accumulated log content', () => {
      const msg1 = {
        type: 'assistant',
        uuid: 'u5',
        session_id: 'test-5',
        message: { content: [{ type: 'text', text: 'Line 1' }] },
        parent_tool_use_id: null,
      } as unknown as SDKMessage;

      const msg2 = {
        type: 'assistant',
        uuid: 'u6',
        session_id: 'test-5',
        message: { content: [{ type: 'text', text: 'Line 2' }] },
        parent_tool_use_id: null,
      } as unknown as SDKMessage;

      appendToLog('test-5', msg1);
      appendToLog('test-5', msg2);

      const content = readLog('test-5');
      expect(content).toContain('Line 1');
      expect(content).toContain('Line 2');
    });
  });
});
