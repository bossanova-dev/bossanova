import 'reflect-metadata';
import { type IpcClient, createIpcClient } from '@bossanova/shared';
import { container } from 'tsyringe';
import { Service } from './tokens.js';

export interface CliConfig {
  socketPath: string;
  logLevel: 'debug' | 'info' | 'warn' | 'error';
}

export interface Logger {
  debug(msg: string, ...args: unknown[]): void;
  info(msg: string, ...args: unknown[]): void;
  warn(msg: string, ...args: unknown[]): void;
  error(msg: string, ...args: unknown[]): void;
}

const defaultConfig: CliConfig = {
  socketPath: `${process.env.HOME}/Library/Application Support/bossanova/bossd.sock`,
  logLevel: 'info',
};

const consoleLogger: Logger = {
  debug: (msg, ...args) => console.debug(`[debug] ${msg}`, ...args),
  info: (msg, ...args) => console.info(`[info] ${msg}`, ...args),
  warn: (msg, ...args) => console.warn(`[warn] ${msg}`, ...args),
  error: (msg, ...args) => console.error(`[error] ${msg}`, ...args),
};

export function setupContainer(config: Partial<CliConfig> = {}): typeof container {
  const resolved: CliConfig = { ...defaultConfig, ...config };

  container.register(Service.Config, { useValue: resolved });
  container.register(Service.Logger, { useValue: consoleLogger });

  // IpcClient is a singleton — reuses the same instance across the CLI session
  const client: IpcClient = createIpcClient(resolved.socketPath);
  container.register(Service.IpcClient, { useValue: client });

  return container;
}

export { container };
