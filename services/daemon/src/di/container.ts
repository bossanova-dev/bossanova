import 'reflect-metadata';
import { container } from 'tsyringe';
import { Service } from './tokens.js';

export interface DaemonConfig {
  dbPath: string;
  socketPath: string;
  logLevel: 'debug' | 'info' | 'warn' | 'error';
}

export interface Logger {
  debug(msg: string, ...args: unknown[]): void;
  info(msg: string, ...args: unknown[]): void;
  warn(msg: string, ...args: unknown[]): void;
  error(msg: string, ...args: unknown[]): void;
}

const defaultConfig: DaemonConfig = {
  dbPath: `${process.env.HOME}/Library/Application Support/bossanova/bossd.db`,
  socketPath: `${process.env.HOME}/Library/Application Support/bossanova/bossd.sock`,
  logLevel: 'info',
};

const consoleLogger: Logger = {
  debug: (msg, ...args) => console.debug(`[debug] ${msg}`, ...args),
  info: (msg, ...args) => console.info(`[info] ${msg}`, ...args),
  warn: (msg, ...args) => console.warn(`[warn] ${msg}`, ...args),
  error: (msg, ...args) => console.error(`[error] ${msg}`, ...args),
};

export function setupContainer(
  config: Partial<DaemonConfig> = {},
): typeof container {
  const resolved: DaemonConfig = { ...defaultConfig, ...config };

  container.register(Service.Config, { useValue: resolved });
  container.register(Service.Logger, { useValue: consoleLogger });

  // Database, stores registered lazily via @injectable() + @inject() decorators
  // tsyringe resolves them from the container when first requested

  return container;
}

export { container };
