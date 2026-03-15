import type { Repo, RepoRow } from '@bossanova/shared';
import { inject, injectable } from 'tsyringe';
import { Service } from '../di/tokens.js';
import type { DatabaseService } from './database.js';

function rowToRepo(row: RepoRow): Repo {
  return {
    id: row.id,
    displayName: row.display_name,
    localPath: row.local_path,
    originUrl: row.origin_url,
    defaultBaseBranch: row.default_base_branch,
    worktreeBaseDir: row.worktree_base_dir,
    setupScript: row.setup_script,
    createdAt: row.created_at,
    updatedAt: row.updated_at,
  };
}

export interface RegisterRepoParams {
  displayName: string;
  localPath: string;
  originUrl: string;
  defaultBaseBranch?: string;
  worktreeBaseDir: string;
  setupScript?: string | null;
}

@injectable()
export class RepoStore {
  constructor(
    @inject(Service.Database) private database: DatabaseService,
  ) {}

  register(params: RegisterRepoParams): Repo {
    const db = this.database.getDb();
    const id = crypto.randomUUID();
    const now = new Date().toISOString();

    db.prepare(
      `INSERT INTO repos (id, display_name, local_path, origin_url, default_base_branch, worktree_base_dir, setup_script, created_at, updated_at)
       VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
    ).run(
      id,
      params.displayName,
      params.localPath,
      params.originUrl,
      params.defaultBaseBranch ?? 'main',
      params.worktreeBaseDir,
      params.setupScript ?? null,
      now,
      now,
    );

    // biome-ignore lint/style/noNonNullAssertion: row was just inserted
    return this.get(id)!;
  }

  list(): Repo[] {
    const db = this.database.getDb();
    const rows = db
      .prepare('SELECT * FROM repos ORDER BY created_at DESC')
      .all() as RepoRow[];
    return rows.map(rowToRepo);
  }

  get(id: string): Repo | null {
    const db = this.database.getDb();
    const row = db
      .prepare('SELECT * FROM repos WHERE id = ?')
      .get(id) as RepoRow | undefined;
    return row ? rowToRepo(row) : null;
  }

  findByPath(localPath: string): Repo | null {
    const db = this.database.getDb();
    const row = db
      .prepare('SELECT * FROM repos WHERE local_path = ?')
      .get(localPath) as RepoRow | undefined;
    return row ? rowToRepo(row) : null;
  }

  remove(id: string): void {
    const db = this.database.getDb();
    db.prepare('DELETE FROM repos WHERE id = ?').run(id);
  }
}
