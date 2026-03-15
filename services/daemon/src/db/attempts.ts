import type { Attempt, AttemptRow } from '@bossanova/shared';
import { inject, injectable } from 'tsyringe';
import type { DatabaseService } from '~/db/database';
import { Service } from '~/di/tokens';

function rowToAttempt(row: AttemptRow): Attempt {
  return {
    id: row.id,
    sessionId: row.session_id,
    trigger: row.trigger as Attempt['trigger'],
    startedAt: row.started_at,
    completedAt: row.completed_at,
    result: row.result as Attempt['result'],
    error: row.error,
  };
}

@injectable()
export class AttemptStore {
  constructor(@inject(Service.Database) private database: DatabaseService) {}

  record(sessionId: string, trigger: Attempt['trigger']): Attempt {
    const db = this.database.getDb();
    const id = crypto.randomUUID();
    const now = new Date().toISOString();

    db.prepare(
      `INSERT INTO attempts (id, session_id, trigger, started_at)
       VALUES (?, ?, ?, ?)`,
    ).run(id, sessionId, trigger, now);

    // biome-ignore lint/style/noNonNullAssertion: row was just inserted
    return this.get(id)!;
  }

  complete(attemptId: string, result: 'success' | 'failure', error?: string | null): void {
    const db = this.database.getDb();
    db.prepare('UPDATE attempts SET completed_at = ?, result = ?, error = ? WHERE id = ?').run(
      new Date().toISOString(),
      result,
      error ?? null,
      attemptId,
    );
  }

  get(id: string): Attempt | null {
    const db = this.database.getDb();
    const row = db.prepare('SELECT * FROM attempts WHERE id = ?').get(id) as AttemptRow | undefined;
    return row ? rowToAttempt(row) : null;
  }

  listBySession(sessionId: string): Attempt[] {
    const db = this.database.getDb();
    const rows = db
      .prepare('SELECT * FROM attempts WHERE session_id = ? ORDER BY started_at DESC')
      .all(sessionId) as AttemptRow[];
    return rows.map(rowToAttempt);
  }
}
