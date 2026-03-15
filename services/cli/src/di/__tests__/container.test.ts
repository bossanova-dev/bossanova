import type { IpcClient } from '@bossanova/shared';
import { container } from 'tsyringe';
import { afterEach, describe, expect, it } from 'vitest';
import { type CliConfig, type Logger, setupContainer } from '../container.js';
import { Service } from '../tokens.js';

describe('CLI DI Container', () => {
  afterEach(() => {
    container.clearInstances();
    // Reset the container to avoid leaking state between tests
    container.reset();
  });

  it('registers Config with defaults', () => {
    setupContainer();
    const config = container.resolve<CliConfig>(Service.Config);
    expect(config.socketPath).toContain('bossd.sock');
    expect(config.logLevel).toBe('info');
  });

  it('allows config overrides', () => {
    setupContainer({ socketPath: '/tmp/test.sock', logLevel: 'debug' });
    const config = container.resolve<CliConfig>(Service.Config);
    expect(config.socketPath).toBe('/tmp/test.sock');
    expect(config.logLevel).toBe('debug');
  });

  it('registers Logger', () => {
    setupContainer();
    const logger = container.resolve<Logger>(Service.Logger);
    expect(logger.debug).toBeTypeOf('function');
    expect(logger.info).toBeTypeOf('function');
    expect(logger.warn).toBeTypeOf('function');
    expect(logger.error).toBeTypeOf('function');
  });

  it('registers IpcClient with call and close methods', () => {
    setupContainer({ socketPath: '/tmp/test.sock' });
    const client = container.resolve<IpcClient>(Service.IpcClient);
    expect(client.call).toBeTypeOf('function');
    expect(client.close).toBeTypeOf('function');
  });

  it('returns the same IpcClient instance on multiple resolves', () => {
    setupContainer({ socketPath: '/tmp/test.sock' });
    const client1 = container.resolve<IpcClient>(Service.IpcClient);
    const client2 = container.resolve<IpcClient>(Service.IpcClient);
    expect(client1).toBe(client2);
  });
});
