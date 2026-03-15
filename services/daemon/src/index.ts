import fs from 'node:fs';
import path from 'node:path';
import type { DatabaseService } from '~/db/database';
import { setupContainer } from '~/di/container';
import { Service } from '~/di/tokens';
import type { IpcServer } from '~/ipc/server';

async function main(): Promise<void> {
  const container = setupContainer();

  // Ensure data directory exists
  const config = container.resolve<{ dbPath: string; socketPath: string }>(Service.Config);
  const dataDir = path.dirname(config.dbPath);
  fs.mkdirSync(dataDir, { recursive: true });

  // Initialize database
  const db = container.resolve<DatabaseService>(Service.Database);
  db.initialize();

  // Remove stale socket file if it exists
  try {
    fs.unlinkSync(config.socketPath);
  } catch {
    // Socket file doesn't exist — that's fine
  }

  // Start IPC server
  const ipcServer = container.resolve<IpcServer>(Service.IpcServer);
  await ipcServer.start();

  console.info(`bossd started (pid: ${process.pid})`);

  // Graceful shutdown
  const shutdown = async () => {
    console.info('Shutting down...');
    await ipcServer.stop();
    db.close();

    // Clean up socket file
    try {
      fs.unlinkSync(config.socketPath);
    } catch {
      // Already gone
    }

    process.exit(0);
  };

  process.on('SIGTERM', shutdown);
  process.on('SIGINT', shutdown);
}

main().catch((err) => {
  console.error('Fatal error starting daemon:', err);
  process.exit(1);
});
