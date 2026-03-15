import fs from 'node:fs';
import net from 'node:net';
import os from 'node:os';
import path from 'node:path';
import type { JsonRpcRequest, JsonRpcResponse } from '@bossanova/shared';
import { RpcErrorCode } from '@bossanova/shared';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { ClaudeSupervisor } from '~/claude/supervisor';
import { AttemptStore } from '~/db/attempts';
import { DatabaseService } from '~/db/database';
import { RepoStore } from '~/db/repos';
import { SessionStore } from '~/db/sessions';
import { Dispatcher } from '~/ipc/dispatcher';
import { IpcServer } from '~/ipc/server';

const noopLogger = { debug: () => {}, info: () => {}, warn: () => {}, error: () => {} };

function createTestDb(): DatabaseService {
  const config = { dbPath: ':memory:', socketPath: '', logLevel: 'info' as const };
  const db = new DatabaseService(config, noopLogger);
  db.initialize(':memory:');
  return db;
}

function sendRpc(socketPath: string, request: JsonRpcRequest): Promise<JsonRpcResponse> {
  return new Promise((resolve, reject) => {
    const client = net.createConnection(socketPath, () => {
      client.write(`${JSON.stringify(request)}\n`);
    });

    let buffer = '';
    client.on('data', (data) => {
      buffer += data.toString();
      const idx = buffer.indexOf('\n');
      if (idx !== -1) {
        const line = buffer.slice(0, idx);
        client.end();
        resolve(JSON.parse(line));
      }
    });

    client.on('error', reject);
  });
}

describe('IpcServer', () => {
  let db: DatabaseService;
  let server: IpcServer;
  let socketPath: string;

  beforeEach(() => {
    db = createTestDb();
    const repos = new RepoStore(db);
    const sessions = new SessionStore(db);
    const attempts = new AttemptStore(db);
    const dispatcher = new Dispatcher(
      repos,
      sessions,
      attempts,
      noopLogger,
      new ClaudeSupervisor(),
    );

    socketPath = path.join(os.tmpdir(), `bossd-test-${process.pid}-${Date.now()}.sock`);
    const config = { dbPath: ':memory:', socketPath, logLevel: 'info' as const };

    server = new IpcServer(config, noopLogger, dispatcher);
  });

  afterEach(async () => {
    await server.stop();
    db.close();
    try {
      fs.unlinkSync(socketPath);
    } catch {
      // Already cleaned up
    }
  });

  it('starts and accepts connections', async () => {
    await server.start();

    const response = await sendRpc(socketPath, {
      jsonrpc: '2.0',
      method: 'repo.list',
      params: {},
      id: 1,
    });

    expect(response.jsonrpc).toBe('2.0');
    expect(response.id).toBe(1);
    expect(response.result).toEqual([]);
  });

  it('returns parse error for invalid JSON', async () => {
    await server.start();

    const response = await new Promise<JsonRpcResponse>((resolve, reject) => {
      const client = net.createConnection(socketPath, () => {
        client.write('not json\n');
      });
      let buffer = '';
      client.on('data', (data) => {
        buffer += data.toString();
        const idx = buffer.indexOf('\n');
        if (idx !== -1) {
          client.end();
          resolve(JSON.parse(buffer.slice(0, idx)));
        }
      });
      client.on('error', reject);
    });

    expect(response.error?.code).toBe(RpcErrorCode.ParseError);
  });

  it('returns invalid request for missing fields', async () => {
    await server.start();

    const response = await sendRpc(socketPath, {
      jsonrpc: '1.0' as '2.0',
      method: 'repo.list',
      id: 2,
    });

    expect(response.error?.code).toBe(RpcErrorCode.InvalidRequest);
  });

  it('returns method not found for unknown method', async () => {
    await server.start();

    const response = await sendRpc(socketPath, {
      jsonrpc: '2.0',
      method: 'nonexistent.method',
      params: {},
      id: 3,
    });

    expect(response.error?.code).toBe(RpcErrorCode.MethodNotFound);
  });

  it('handles multiple messages on a single connection', async () => {
    await server.start();

    const responses = await new Promise<JsonRpcResponse[]>((resolve, reject) => {
      const client = net.createConnection(socketPath, () => {
        client.write(
          `${JSON.stringify({ jsonrpc: '2.0', method: 'repo.list', params: {}, id: 1 })}\n`,
        );
        client.write(
          `${JSON.stringify({ jsonrpc: '2.0', method: 'repo.list', params: {}, id: 2 })}\n`,
        );
      });

      const results: JsonRpcResponse[] = [];
      let buffer = '';
      client.on('data', (data) => {
        buffer += data.toString();
        let idx = buffer.indexOf('\n');
        while (idx !== -1) {
          const line = buffer.slice(0, idx).trim();
          buffer = buffer.slice(idx + 1);
          if (line) results.push(JSON.parse(line));
          if (results.length === 2) {
            client.end();
            resolve(results);
          }
          idx = buffer.indexOf('\n');
        }
      });

      client.on('error', reject);
    });

    expect(responses).toHaveLength(2);
    expect(responses[0].id).toBe(1);
    expect(responses[1].id).toBe(2);
  });

  it('stops cleanly', async () => {
    await server.start();
    await server.stop();

    // Connection should be refused after stop
    await expect(
      new Promise((_, reject) => {
        const client = net.createConnection(socketPath);
        client.on('error', reject);
      }),
    ).rejects.toThrow();
  });
});
