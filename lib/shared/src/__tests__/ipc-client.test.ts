import fs from 'node:fs';
import net from 'node:net';
import os from 'node:os';
import path from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { DaemonNotRunningError, type IpcClient, createIpcClient } from '../ipc-client.js';
import type { JsonRpcRequest, JsonRpcResponse } from '../rpc.js';
import { RpcErrorCode } from '../rpc.js';

describe('IpcClient', () => {
  let socketPath: string;
  let server: net.Server;
  let client: IpcClient;

  beforeEach(() => {
    socketPath = path.join(
      fs.realpathSync(os.tmpdir()),
      `bossd-ipc-test-${process.pid}-${Date.now()}.sock`,
    );
  });

  afterEach(async () => {
    client?.close();
    await new Promise<void>((resolve) => {
      if (server?.listening) {
        server.close(() => resolve());
      } else {
        resolve();
      }
    });
    try {
      fs.unlinkSync(socketPath);
    } catch {
      // Already gone
    }
  });

  function startMockServer(handler: (req: JsonRpcRequest) => JsonRpcResponse): Promise<void> {
    return new Promise((resolve) => {
      server = net.createServer((socket) => {
        let buffer = '';
        socket.on('data', (data) => {
          buffer += data.toString();
          const idx = buffer.indexOf('\n');
          if (idx !== -1) {
            const line = buffer.slice(0, idx);
            buffer = buffer.slice(idx + 1);
            const request = JSON.parse(line) as JsonRpcRequest;
            const response = handler(request);
            socket.write(`${JSON.stringify(response)}\n`);
          }
        });
      });
      server.listen(socketPath, () => resolve());
    });
  }

  it('sends an RPC call and receives the result', async () => {
    await startMockServer((req) => ({
      jsonrpc: '2.0',
      id: req.id,
      result: [],
    }));

    client = createIpcClient(socketPath);
    const result = await client.call('repo.list', {});
    expect(result).toEqual([]);
  });

  it('rejects with RPC error message', async () => {
    await startMockServer((req) => ({
      jsonrpc: '2.0',
      id: req.id,
      error: { code: RpcErrorCode.MethodNotFound, message: 'Method not found: bad.method' },
    }));

    client = createIpcClient(socketPath);
    await expect(client.call('repo.list', {})).rejects.toThrow('RPC error');
  });

  it('throws DaemonNotRunningError when socket does not exist', async () => {
    client = createIpcClient('/tmp/nonexistent-bossd.sock');
    await expect(client.call('repo.list', {})).rejects.toThrow(DaemonNotRunningError);
  });

  it('passes params to the server', async () => {
    let receivedParams: unknown;
    await startMockServer((req) => {
      receivedParams = req.params;
      return { jsonrpc: '2.0', id: req.id, result: { type: 'none' } };
    });

    client = createIpcClient(socketPath);
    await client.call('context.resolve', { cwd: '/some/path' });
    expect(receivedParams).toEqual({ cwd: '/some/path' });
  });

  it('increments request IDs', async () => {
    const receivedIds: (string | number)[] = [];
    await startMockServer((req) => {
      receivedIds.push(req.id);
      return { jsonrpc: '2.0', id: req.id, result: [] };
    });

    client = createIpcClient(socketPath);
    await client.call('repo.list', {});
    await client.call('repo.list', {});
    expect(receivedIds).toEqual([1, 2]);
  });
});
