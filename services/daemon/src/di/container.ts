import 'reflect-metadata';
import { container } from 'tsyringe';
import { ClaudeSupervisor } from '~/claude/supervisor';
import { AttemptStore } from '~/db/attempts';
import { DatabaseService } from '~/db/database';
import { RepoStore } from '~/db/repos';
import { SessionStore } from '~/db/sessions';
import { Dispatcher } from '~/ipc/dispatcher';
import { IpcServer } from '~/ipc/server';
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

export function setupContainer(config: Partial<DaemonConfig> = {}): typeof container {
  const resolved: DaemonConfig = { ...defaultConfig, ...config };

  container.register(Service.Config, { useValue: resolved });
  container.register(Service.Logger, { useValue: consoleLogger });

  // Register as singletons so all consumers share the same instance
  container.registerSingleton(Service.Database, DatabaseService);
  container.registerSingleton(Service.RepoStore, RepoStore);
  container.registerSingleton(Service.SessionStore, SessionStore);
  container.registerSingleton(Service.AttemptStore, AttemptStore);
  container.register(Service.ClaudeSupervisor, { useValue: new ClaudeSupervisor() });
  container.registerSingleton(Service.Dispatcher, Dispatcher);
  container.registerSingleton(Service.IpcServer, IpcServer);

  return container;
}

export { container };
