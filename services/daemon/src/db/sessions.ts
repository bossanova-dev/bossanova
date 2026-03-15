import type { Session, SessionRow } from '@bossanova/shared';
import { inject, injectable } from 'tsyringe';
import type { DatabaseService } from '~/db/database';
import { Service } from '~/di/tokens';

function rowToSession(row: SessionRow): Session {
  return {
    id: row.id,
    repoId: row.repo_id,
    title: row.title,
    plan: row.plan,
    worktreePath: row.worktree_path,
    branchName: row.branch_name,
    baseBranch: row.base_branch,
    state: row.state as Session['state'],
    claudeSessionId: row.claude_session_id,
    prNumber: row.pr_number,
    prUrl: row.pr_url,
    lastCheckState: row.last_check_state as Session['lastCheckState'],
    automationEnabled: row.automation_enabled === 1,
    attemptCount: row.attempt_count,
    blockedReason: row.blocked_reason,
    createdAt: row.created_at,
    updatedAt: row.updated_at,
  };
}

export interface CreateSessionParams {
  repoId: string;
  title: string;
  plan: string;
  baseBranch: string;
}

export interface UpdateSessionFields {
  worktreePath?: string | null;
  branchName?: string | null;
  state?: string;
  claudeSessionId?: string | null;
  prNumber?: number | null;
  prUrl?: string | null;
  lastCheckState?: string | null;
  automationEnabled?: boolean;
  attemptCount?: number;
  blockedReason?: string | null;
}

@injectable()
export class SessionStore {
  constructor(
    @inject(Service.Database) private database: DatabaseService,
  ) {}

  create(params: CreateSessionParams): Session {
    const db = this.database.getDb();
    const id = crypto.randomUUID();
    const now = new Date().toISOString();

    db.prepare(
      `INSERT INTO sessions (id, repo_id, title, plan, base_branch, created_at, updated_at)
       VALUES (?, ?, ?, ?, ?, ?, ?)`,
    ).run(id, params.repoId, params.title, params.plan, params.baseBranch, now, now);

    // biome-ignore lint/style/noNonNullAssertion: row was just inserted
    return this.get(id)!;
  }

  list(repoId?: string): Session[] {
    const db = this.database.getDb();
    if (repoId) {
      const rows = db
        .prepare(
          'SELECT * FROM sessions WHERE repo_id = ? ORDER BY created_at DESC',
        )
        .all(repoId) as SessionRow[];
      return rows.map(rowToSession);
    }
    const rows = db
      .prepare('SELECT * FROM sessions ORDER BY created_at DESC')
      .all() as SessionRow[];
    return rows.map(rowToSession);
  }

  get(id: string): Session | null {
    const db = this.database.getDb();
    const row = db
      .prepare('SELECT * FROM sessions WHERE id = ?')
      .get(id) as SessionRow | undefined;
    return row ? rowToSession(row) : null;
  }

  update(id: string, fields: UpdateSessionFields): void {
    const db = this.database.getDb();
    const sets: string[] = [];
    const values: unknown[] = [];

    for (const [key, value] of Object.entries(fields)) {
      if (value === undefined) continue;
      const column = camelToSnake(key);
      if (key === 'automationEnabled') {
        sets.push(`${column} = ?`);
        values.push(value ? 1 : 0);
      } else {
        sets.push(`${column} = ?`);
        values.push(value);
      }
    }

    if (sets.length === 0) return;

    sets.push('updated_at = ?');
    values.push(new Date().toISOString());
    values.push(id);

    db.prepare(`UPDATE sessions SET ${sets.join(', ')} WHERE id = ?`).run(
      ...values,
    );
  }

  delete(id: string): void {
    const db = this.database.getDb();
    db.prepare('DELETE FROM sessions WHERE id = ?').run(id);
  }
}

function camelToSnake(str: string): string {
  return str.replace(/[A-Z]/g, (letter) => `_${letter.toLowerCase()}`);
}
