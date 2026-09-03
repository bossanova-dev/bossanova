package db

import (
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/recurser/bossd/internal/dbtest"
)

const agentRunsReviewTelemetryVersion int64 = 20260827000000

const preAgentRunsReviewTelemetryVersion int64 = 20260826000000

func TestAgentRunsReviewTelemetryMigrationSchema(t *testing.T) {
	db := setupTestDB(t)

	var applied int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM goose_db_version WHERE version_id = ?", agentRunsReviewTelemetryVersion,
	).Scan(&applied); err != nil {
		t.Fatalf("query goose version: %v", err)
	}
	if applied == 0 {
		t.Fatalf("migration version %d not applied", agentRunsReviewTelemetryVersion)
	}

	cols := tableColumns(t, db, "agent_runs")
	for name, wantType := range map[string]string{
		"reviewer_dispatch_count": "INTEGER",
		"terminal_state":          "TEXT",
	} {
		got, ok := cols[name]
		if !ok {
			t.Fatalf("agent_runs.%s missing after migration", name)
		}
		if got != wantType {
			t.Errorf("agent_runs.%s type = %q, want %s", name, got, wantType)
		}
		var notnull int
		var dflt *string
		if err := db.QueryRow(`SELECT "notnull", dflt_value FROM pragma_table_info('agent_runs') WHERE name = ?`, name).Scan(&notnull, &dflt); err != nil {
			t.Fatalf("pragma_table_info(agent_runs.%s): %v", name, err)
		}
		if notnull != 1 {
			t.Errorf("agent_runs.%s notnull = %d, want 1", name, notnull)
		}
		if dflt == nil {
			t.Fatalf("agent_runs.%s has no default", name)
		}
	}
}

// TestAgentRunsReviewTelemetryCheckRejectsUnknownState proves the SQL CHECK is
// enforced, not merely written. NormalizeAgentRunTerminalState is the Go-side
// belt; this is the braces, and a silently-unenforced CHECK would make that
// normalizer the only guard without anyone noticing.
func TestAgentRunsReviewTelemetryCheckRejectsUnknownState(t *testing.T) {
	db := setupTestDB(t)
	seedAgentRunFixture(t, db, "run-check")

	if _, err := db.Exec(`UPDATE agent_runs SET terminal_state = 'REVIEW_READY' WHERE id = 'run-check'`); err != nil {
		t.Fatalf("update with a known terminal_state: %v", err)
	}

	_, err := db.Exec(`UPDATE agent_runs SET terminal_state = 'NOPE' WHERE id = 'run-check'`)
	if err == nil {
		t.Fatal("update with unknown terminal_state succeeded; the CHECK constraint is not enforced")
	}
	if !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Fatalf("update with unknown terminal_state = %v, want a CHECK constraint failure", err)
	}
}

// seedAgentRunFixture inserts the repo/session/agent_run chain the foreign keys
// require, so a test can exercise a column rather than the schema around it.
func seedAgentRunFixture(t *testing.T, db *sql.DB, runID string) {
	t.Helper()

	if _, err := db.Exec(`INSERT OR IGNORE INTO repos (id, display_name, local_path, origin_url, worktree_base_dir) VALUES ('r1', 'repo', '/tmp/repo', 'https://example.test/repo.git', '/tmp/worktrees')`); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO sessions (id, repo_id, title, worktree_path, branch_name, base_branch) VALUES ('s1', 'r1', 'session', '/tmp/worktrees/s1', 'branch', 'main')`); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO agent_runs (id, session_id, agent_session_id, agent_name, started_at) VALUES (?, 's1', ?, 'claude', '2026-08-26T00:00:00Z')`,
		runID, runID+"-agent",
	); err != nil {
		t.Fatalf("insert agent run: %v", err)
	}
}

func TestAgentRunsReviewTelemetryMigrationDown(t *testing.T) {
	migrations := os.DirFS(migrationsDir())
	db, err := OpenInMemory()
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := dbtest.RunUpTo(db, migrations, agentRunsReviewTelemetryVersion); err != nil {
		t.Fatalf("run up to %d: %v", agentRunsReviewTelemetryVersion, err)
	}
	if _, ok := tableColumns(t, db, "agent_runs")["terminal_state"]; !ok {
		t.Fatal("terminal_state missing before down")
	}

	if err := dbtest.RunDownTo(db, migrations, preAgentRunsReviewTelemetryVersion); err != nil {
		t.Fatalf("run down: %v", err)
	}
	cols := tableColumns(t, db, "agent_runs")
	if _, ok := cols["terminal_state"]; ok {
		t.Fatal("terminal_state still present after down")
	}
	if _, ok := cols["reviewer_dispatch_count"]; ok {
		t.Fatal("reviewer_dispatch_count still present after down")
	}

	if err := dbtest.RunUpTo(db, migrations, agentRunsReviewTelemetryVersion); err != nil {
		t.Fatalf("re-run up after down: %v", err)
	}
}

// TestAgentRunsReviewTelemetryLeavesExistingRowsUnrecorded pins the reason the
// columns are defaulted rather than backfilled at migration time: a run that
// predates the telemetry must read as "not recorded" (0 / ""), never as a run
// that dispatched no reviewers and ended in no state.
func TestAgentRunsReviewTelemetryLeavesExistingRowsUnrecorded(t *testing.T) {
	migrations := os.DirFS(migrationsDir())
	db, err := OpenInMemory()
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := dbtest.RunUpTo(db, migrations, preAgentRunsReviewTelemetryVersion); err != nil {
		t.Fatalf("run up to %d: %v", preAgentRunsReviewTelemetryVersion, err)
	}
	seedAgentRunFixture(t, db, "run1")

	if err := dbtest.RunUpTo(db, migrations, agentRunsReviewTelemetryVersion); err != nil {
		t.Fatalf("run up to %d: %v", agentRunsReviewTelemetryVersion, err)
	}
	var dispatches int64
	var state string
	if err := db.QueryRow(`SELECT reviewer_dispatch_count, terminal_state FROM agent_runs WHERE id = 'run1'`).Scan(&dispatches, &state); err != nil {
		t.Fatalf("read migrated agent run: %v", err)
	}
	if dispatches != 0 || state != "" {
		t.Fatalf("legacy telemetry = %d dispatches, state %q; want 0 and \"\"", dispatches, state)
	}
}
