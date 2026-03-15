import net from 'node:net';
import type { JsonRpcRequest, JsonRpcResponse } from '@bossanova/shared';
import { RpcErrorCode } from '@bossanova/shared';
import { inject, injectable } from 'tsyringe';
import type { DaemonConfig, Logger } from '~/di/container';
import { Service } from '~/di/tokens';
import type { Dispatcher } from '~/ipc/dispatcher';

@injectable()
export class IpcServer {
  private server: net.Server | null = null;

  constructor(
    @inject(Service.Config) private config: DaemonConfig,
    @inject(Service.Logger) private logger: Logger,
    @inject(Service.Dispatcher) private dispatcher: Dispatcher,
  ) {}

  start(): Promise<void> {
    return new Promise((resolve, reject) => {
      this.server = net.createServer((socket) => this.handleConnection(socket));

      this.server.on('error', (err) => {
        this.logger.error('IPC server error', err);
        reject(err);
      });

      this.server.listen(this.config.socketPath, () => {
        this.logger.info(`IPC server listening on ${this.config.socketPath}`);
        resolve();
      });
    });
  }

  stop(): Promise<void> {
    return new Promise((resolve) => {
      if (!this.server) {
        resolve();
        return;
      }
      this.server.close(() => {
        this.server = null;
        this.logger.info('IPC server stopped');
        resolve();
      });
    });
  }

  private handleConnection(socket: net.Socket): void {
    this.logger.debug('Client connected');
    let buffer = '';

    socket.on('data', (data) => {
      buffer += data.toString();

      // Newline-delimited JSON-RPC
      let newlineIdx = buffer.indexOf('\n');
      while (newlineIdx !== -1) {
        const line = buffer.slice(0, newlineIdx).trim();
        buffer = buffer.slice(newlineIdx + 1);

        if (line.length > 0) {
          this.handleMessage(line, socket);
        }

        newlineIdx = buffer.indexOf('\n');
      }
    });

    socket.on('error', (err) => {
      this.logger.debug('Client socket error', err);
    });

    socket.on('close', () => {
      this.logger.debug('Client disconnected');
    });
  }

  private async handleMessage(raw: string, socket: net.Socket): Promise<void> {
    let request: JsonRpcRequest;

    try {
      request = JSON.parse(raw);
    } catch {
      const response: JsonRpcResponse = {
        jsonrpc: '2.0',
        id: 0,
        error: {
          code: RpcErrorCode.ParseError,
          message: 'Parse error: invalid JSON',
        },
      };
      socket.write(`${JSON.stringify(response)}\n`);
      return;
    }

    if (request.jsonrpc !== '2.0' || typeof request.method !== 'string' || request.id == null) {
      const response: JsonRpcResponse = {
        jsonrpc: '2.0',
        id: request.id ?? 0,
        error: {
          code: RpcErrorCode.InvalidRequest,
          message: 'Invalid JSON-RPC 2.0 request',
        },
      };
      socket.write(`${JSON.stringify(response)}\n`);
      return;
    }

    const response = await this.dispatcher.dispatch(request);
    socket.write(`${JSON.stringify(response)}\n`);
  }
}
