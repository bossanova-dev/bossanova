import fs from 'node:fs';
import path from 'node:path';
import type { SDKMessage } from '@anthropic-ai/claude-agent-sdk';

const LOG_DIR = `${process.env.HOME}/Library/Application Support/bossanova/logs`;

/**
 * Get the log file path for a session.
 */
export function getLogPath(sessionId: string): string {
  return path.join(LOG_DIR, `${sessionId}.log`);
}

/**
 * Append a formatted SDK message to a session's log file.
 */
export function appendToLog(sessionId: string, message: SDKMessage): void {
  const logPath = getLogPath(sessionId);
  fs.mkdirSync(path.dirname(logPath), { recursive: true });

  const line = formatMessage(message);
  if (line) {
    fs.appendFileSync(logPath, `${line}\n`);
  }
}

/**
 * Read a session's log file.
 * Returns null if the log file doesn't exist.
 */
export function readLog(sessionId: string): string | null {
  const logPath = getLogPath(sessionId);
  try {
    return fs.readFileSync(logPath, 'utf8');
  } catch {
    return null;
  }
}

/**
 * Format an SDK message into a human-readable log line.
 */
function formatMessage(message: SDKMessage): string | null {
  const ts = new Date().toISOString();

  switch (message.type) {
    case 'system':
      if ('subtype' in message && message.subtype === 'init') {
        return `[${ts}] [system] Session initialized (model: ${message.model})`;
      }
      return null;

    case 'assistant': {
      const content = message.message?.content;
      if (Array.isArray(content)) {
        const texts: string[] = [];
        for (const block of content) {
          if ('type' in block && block.type === 'text' && 'text' in block) {
            texts.push(block.text as string);
          }
          if ('type' in block && block.type === 'tool_use' && 'name' in block) {
            texts.push(`[tool: ${block.name}]`);
          }
        }
        if (texts.length > 0) {
          return `[${ts}] [assistant] ${texts.join(' ')}`;
        }
      }
      return null;
    }

    case 'result':
      if ('result' in message) {
        return `[${ts}] [result] ${message.result}`;
      }
      if ('errors' in message && Array.isArray(message.errors)) {
        return `[${ts}] [error] ${message.errors.join('; ')}`;
      }
      return null;

    default:
      return null;
  }
}
