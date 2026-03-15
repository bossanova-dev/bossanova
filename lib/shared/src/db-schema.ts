// --- Database Row Types ---

export interface RepoRow {
  id: string;
  display_name: string;
  local_path: string;
  origin_url: string;
  default_base_branch: string;
  worktree_base_dir: string;
  setup_script: string | null;
  created_at: string;
  updated_at: string;
}

export interface SessionRow {
  id: string;
  repo_id: string;
  title: string;
  plan: string;
  worktree_path: string | null;
  branch_name: string | null;
  base_branch: string;
  state: string;
  claude_session_id: string | null;
  pr_number: number | null;
  pr_url: string | null;
  last_check_state: string | null;
  automation_enabled: number; // SQLite boolean (0/1)
  attempt_count: number;
  blocked_reason: string | null;
  created_at: string;
  updated_at: string;
}

export interface AttemptRow {
  id: string;
  session_id: string;
  trigger: string;
  started_at: string;
  completed_at: string | null;
  result: string | null;
  error: string | null;
}

export interface SchemaVersionRow {
  version: number;
  applied_at: string;
}

// --- Versioned Migrations ---

export const MIGRATIONS: string[] = [
  // Version 1: Initial schema
  `
CREATE TABLE IF NOT EXISTS schema_version (
  version INTEGER PRIMARY KEY,
  applied_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS repos (
  id TEXT PRIMARY KEY,
  display_name TEXT NOT NULL,
  local_path TEXT NOT NULL UNIQUE,
  origin_url TEXT NOT NULL,
  default_base_branch TEXT NOT NULL DEFAULT 'main',
  worktree_base_dir TEXT NOT NULL,
  setup_script TEXT,
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS sessions (
  id TEXT PRIMARY KEY,
  repo_id TEXT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  title TEXT NOT NULL,
  plan TEXT NOT NULL,
  worktree_path TEXT,
  branch_name TEXT,
  base_branch TEXT NOT NULL,
  state TEXT NOT NULL DEFAULT 'creating_worktree',
  claude_session_id TEXT,
  pr_number INTEGER,
  pr_url TEXT,
  last_check_state TEXT,
  automation_enabled INTEGER NOT NULL DEFAULT 1,
  attempt_count INTEGER NOT NULL DEFAULT 0,
  blocked_reason TEXT,
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS attempts (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  trigger TEXT NOT NULL,
  started_at TEXT NOT NULL DEFAULT (datetime('now')),
  completed_at TEXT,
  result TEXT,
  error TEXT
);

CREATE INDEX IF NOT EXISTS idx_sessions_repo_id ON sessions(repo_id);
CREATE INDEX IF NOT EXISTS idx_sessions_state ON sessions(state);
CREATE INDEX IF NOT EXISTS idx_attempts_session_id ON attempts(session_id);
`,
];
