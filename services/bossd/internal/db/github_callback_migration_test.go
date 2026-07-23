package db

import (
	"os"
	"strings"
	"testing"

	"github.com/recurser/bossalib/migrate"
)

// githubCallbacksVersion is the goose timestamp of the BOS-467 migration.
const githubCallbacksVersion int64 = 20260722000000

// preGithubCallbacksVersion is the migration immediately before BOS-467; rolling
// down to it exercises the github_callbacks Down step.
const preGithubCallbacksVersion int64 = 20260719000000

// TestGithubCallbacksMigrationSchema asserts the BOS-467 migration creates the
// github_callbacks table with the expected columns and canonical timestamp types,
// and that Down cleanly drops it.
func TestGithubCallbacksMigrationSchema(t *testing.T) {
	db := setupTestDB(t)

	// The BOS-467 migration is applied at its expected goose version.
	var applied int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM goose_db_version WHERE version_id = ?", githubCallbacksVersion,
	).Scan(&applied); err != nil {
		t.Fatalf("query goose version: %v", err)
	}
	if applied == 0 {
		t.Fatalf("migration version %d not applied", githubCallbacksVersion)
	}

	cols := tableColumns(t, db, "github_callbacks")
	if len(cols) == 0 {
		t.Fatal("github_callbacks table missing after migration")
	}
	want := map[string]string{
		"id":                "TEXT",
		"group_id":          "TEXT",
		"target_chat_id":    "TEXT",
		"repo_owner":        "TEXT",
		"repo_name":         "TEXT",
		"pr_number":         "INTEGER",
		"trigger_event":     "TEXT",
		"state":             "TEXT",
		"message":           "TEXT",
		"lease_owner":       "TEXT",
		"lease_deadline_at": "TEXT",
		"attempt_count":     "INTEGER",
		"next_attempt_at":   "TEXT",
		"triggered_at":      "TEXT",
		"delivered_at":      "TEXT",
		"last_error":        "TEXT",
		"last_event":        "TEXT",
		"expires_at":        "TEXT",
		"created_at":        "TEXT",
		"updated_at":        "TEXT",
	}
	for col, typ := range want {
		got, ok := cols[col]
		if !ok {
			t.Errorf("column %s missing", col)
			continue
		}
		if got != typ {
			t.Errorf("column %s type = %q, want %q", col, got, typ)
		}
	}
}

// TestGithubCallbacksMigrationDown asserts the Down step removes the table so the
// migration is reversible.
func TestGithubCallbacksMigrationDown(t *testing.T) {
	db := setupTestDB(t)
	if err := migrate.RunDownTo(db, os.DirFS(migrationsDir()), preGithubCallbacksVersion); err != nil {
		t.Fatalf("run down: %v", err)
	}
	cols := tableColumns(t, db, "github_callbacks")
	if len(cols) != 0 {
		t.Fatalf("github_callbacks table should be gone after down, got columns: %v", cols)
	}
}

// TestGithubCallbacksIndexesUsed asserts SQLite's planner picks the callback
// indexes for the store's hot-path queries instead of a full table scan.
func TestGithubCallbacksIndexesUsed(t *testing.T) {
	db := setupTestDB(t)

	cases := []struct {
		name  string
		query string
		args  []any
		index string
	}{
		{
			name:  "repo_pr_trigger",
			query: "SELECT id FROM github_callbacks WHERE repo_owner = ? AND repo_name = ? AND pr_number = ? AND trigger_event = ?",
			args:  []any{"o", "n", 1, "merged"},
			index: "idx_github_callbacks_repo_pr_trigger",
		},
		{
			name:  "chat_created",
			query: "SELECT id FROM github_callbacks WHERE target_chat_id = ? ORDER BY created_at",
			args:  []any{"chat-1"},
			index: "idx_github_callbacks_chat_created",
		},
		{
			name:  "group",
			query: "SELECT id FROM github_callbacks WHERE group_id = ?",
			args:  []any{"g-1"},
			index: "idx_github_callbacks_group",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := db.Query("EXPLAIN QUERY PLAN "+tc.query, tc.args...)
			if err != nil {
				t.Fatalf("explain: %v", err)
			}
			defer func() { _ = rows.Close() }()
			var plan string
			for rows.Next() {
				var id, parent, notused int
				var detail string
				if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
					t.Fatalf("scan: %v", err)
				}
				plan += detail + "\n"
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("rows: %v", err)
			}
			if !strings.Contains(plan, tc.index) {
				t.Errorf("plan for %q does not use %s:\n%s", tc.query, tc.index, plan)
			}
		})
	}
}
