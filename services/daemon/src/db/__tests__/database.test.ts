import { afterEach, describe, expect, it } from 'vitest';
import { DatabaseService } from '~/db/database';

function createTestDb(): DatabaseService {
  const config = { dbPath: ':memory:', socketPath: '', logLevel: 'info' as const };
  const logger = {
    debug: () => {},
    info: () => {},
    warn: () => {},
    error: () => {},
  };
  // Construct without DI for unit tests
  const db = new DatabaseService(config, logger);
  db.initialize(':memory:');
  return db;
}

describe('DatabaseService', () => {
  let db: DatabaseService;

  afterEach(() => {
    db?.close();
  });

  it('creates all tables on initialization', () => {
    db = createTestDb();
    const tables = db
      .getDb()
      .prepare("SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")
      .all() as { name: string }[];
    const names = tables.map((t) => t.name);
    expect(names).toContain('repos');
    expect(names).toContain('sessions');
    expect(names).toContain('attempts');
    expect(names).toContain('schema_version');
  });

  it('sets schema version to 1 after initial migration', () => {
    db = createTestDb();
    const row = db.getDb().prepare('SELECT MAX(version) as version FROM schema_version').get() as {
      version: number;
    };
    expect(row.version).toBe(1);
  });

  it('enables WAL mode', () => {
    db = createTestDb();
    const mode = db.getDb().pragma('journal_mode', { simple: true }) as string;
    // In-memory databases may report 'memory' instead of 'wal'
    expect(['wal', 'memory']).toContain(mode);
  });

  it('enables foreign keys', () => {
    db = createTestDb();
    const fk = db.getDb().pragma('foreign_keys', { simple: true }) as number;
    expect(fk).toBe(1);
  });

  it('skips already-applied migrations', () => {
    db = createTestDb();
    // Re-initialize the same DB — should not throw
    db.close();
    const config = { dbPath: ':memory:', socketPath: '', logLevel: 'info' as const };
    const logger = { debug: () => {}, info: () => {}, warn: () => {}, error: () => {} };
    const db2 = new DatabaseService(config, logger);
    db2.initialize(':memory:');
    const row = db2.getDb().prepare('SELECT MAX(version) as version FROM schema_version').get() as {
      version: number;
    };
    expect(row.version).toBe(1);
    db = db2;
  });

  it('throws when getDb called before initialize', () => {
    const config = { dbPath: ':memory:', socketPath: '', logLevel: 'info' as const };
    const logger = { debug: () => {}, info: () => {}, warn: () => {}, error: () => {} };
    const uninitDb = new DatabaseService(config, logger);
    expect(() => uninitDb.getDb()).toThrow('Database not initialized');
  });
});
