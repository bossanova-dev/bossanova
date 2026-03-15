import { container } from 'tsyringe';
import { afterEach, describe, expect, it } from 'vitest';
import { DatabaseService } from '~/db/database';
import type { DaemonConfig, Logger } from '~/di/container';
import { setupContainer } from '~/di/container';
import { Service } from '~/di/tokens';

describe('DI Container', () => {
  afterEach(() => {
    container.clearInstances();
  });

  it('registers config with defaults', () => {
    setupContainer();
    const config = container.resolve<DaemonConfig>(Service.Config);
    expect(config.dbPath).toContain('bossanova/bossd.db');
    expect(config.socketPath).toContain('bossanova/bossd.sock');
    expect(config.logLevel).toBe('info');
  });

  it('accepts config overrides', () => {
    setupContainer({ dbPath: ':memory:', logLevel: 'debug' });
    const config = container.resolve<DaemonConfig>(Service.Config);
    expect(config.dbPath).toBe(':memory:');
    expect(config.logLevel).toBe('debug');
  });

  it('registers a logger', () => {
    setupContainer();
    const logger = container.resolve<Logger>(Service.Logger);
    expect(logger.debug).toBeTypeOf('function');
    expect(logger.info).toBeTypeOf('function');
    expect(logger.warn).toBeTypeOf('function');
    expect(logger.error).toBeTypeOf('function');
  });

  it('resolves DatabaseService via token', () => {
    setupContainer({ dbPath: ':memory:' });
    container.register(Service.Database, { useClass: DatabaseService });
    const db = container.resolve<DatabaseService>(Service.Database);
    expect(db).toBeInstanceOf(DatabaseService);
  });
});
