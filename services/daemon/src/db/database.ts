import { MIGRATIONS } from '@bossanova/shared';
import Database from 'better-sqlite3';
import { inject, injectable } from 'tsyringe';
import type { DaemonConfig, Logger } from '~/di/container';
import { Service } from '~/di/tokens';

@injectable()
export class DatabaseService {
  private db: Database.Database | null = null;

  constructor(
    @inject(Service.Config) private config: DaemonConfig,
    @inject(Service.Logger) private logger: Logger,
  ) {}

  initialize(dbPath?: string): void {
    const path = dbPath ?? this.config.dbPath;
    this.db = new Database(path);

    // Enable WAL mode and foreign keys
    this.db.pragma('journal_mode = WAL');
    this.db.pragma('foreign_keys = ON');

    this.runMigrations();
    this.logger.info(`Database initialized at ${path}`);
  }

  getDb(): Database.Database {
    if (!this.db) {
      throw new Error('Database not initialized — call initialize() first');
    }
    return this.db;
  }

  close(): void {
    if (this.db) {
      this.db.close();
      this.db = null;
    }
  }

  private runMigrations(): void {
    const db = this.getDb();

    // Ensure schema_version table exists (it's in migration 1, but we need
    // to check current version before running any migration)
    db.exec(`
      CREATE TABLE IF NOT EXISTS schema_version (
        version INTEGER PRIMARY KEY,
        applied_at TEXT NOT NULL DEFAULT (datetime('now'))
      )
    `);

    const currentVersion =
      db.prepare('SELECT MAX(version) as version FROM schema_version').get() as
        | { version: number | null }
        | undefined;
    const current = currentVersion?.version ?? 0;

    const applyMigration = db.transaction(
      (version: number, sql: string) => {
        db.exec(sql);
        db.prepare('INSERT INTO schema_version (version) VALUES (?)').run(
          version,
        );
      },
    );

    for (let i = current; i < MIGRATIONS.length; i++) {
      const version = i + 1;
      this.logger.info(`Applying migration ${version}...`);
      applyMigration(version, MIGRATIONS[i]);
    }

    if (current < MIGRATIONS.length) {
      this.logger.info(
        `Migrations complete (${current} → ${MIGRATIONS.length})`,
      );
    }
  }
}
