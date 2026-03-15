import net from 'node:net';
import type { JsonRpcRequest, JsonRpcResponse, RpcMethod, RpcMethods } from './rpc.js';

const DEFAULT_SOCKET_PATH = `${process.env.HOME}/Library/Application Support/bossanova/bossd.sock`;

export class DaemonNotRunningError extends Error {
  constructor(socketPath: string) {
    super(`Daemon not running (socket not found: ${socketPath})`);
    this.name = 'DaemonNotRunningError';
  }
}

export interface IpcClient {
  call<M extends RpcMethod>(
    method: M,
    params: RpcMethods[M]['params'],
  ): Promise<RpcMethods[M]['result']>;
  close(): void;
}

export function createIpcClient(socketPath = DEFAULT_SOCKET_PATH): IpcClient {
  let nextId = 1;

  function call<M extends RpcMethod>(
    method: M,
    params: RpcMethods[M]['params'],
  ): Promise<RpcMethods[M]['result']> {
    return new Promise((resolve, reject) => {
      const id = nextId++;
      const request: JsonRpcRequest = {
        jsonrpc: '2.0',
        method,
        params,
        id,
      };

      const socket = net.createConnection(socketPath, () => {
        socket.write(`${JSON.stringify(request)}\n`);
      });

      let buffer = '';

      socket.on('data', (data) => {
        buffer += data.toString();
        const idx = buffer.indexOf('\n');
        if (idx !== -1) {
          const line = buffer.slice(0, idx);
          socket.end();

          try {
            const response: JsonRpcResponse = JSON.parse(line);
            if (response.error) {
              reject(new Error(`RPC error (${response.error.code}): ${response.error.message}`));
            } else {
              resolve(response.result as RpcMethods[M]['result']);
            }
          } catch (err) {
            reject(new Error(`Failed to parse RPC response: ${line}`));
          }
        }
      });

      socket.on('error', (err: NodeJS.ErrnoException) => {
        if (err.code === 'ECONNREFUSED' || err.code === 'ENOENT') {
          reject(new DaemonNotRunningError(socketPath));
        } else {
          reject(err);
        }
      });
    });
  }

  return {
    call,
    close() {
      // Each call creates a new connection, so nothing to clean up
    },
  };
}
