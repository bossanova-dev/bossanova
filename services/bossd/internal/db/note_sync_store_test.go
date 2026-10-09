package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sqlutil"
	"github.com/recurser/bossd/internal/dbtest"
)

// noteSyncStatesVersion is the goose timestamp of the BOS-1429 migration.
const noteSyncStatesVersion int64 = 20261008000100

// preNoteSyncStatesVersion is the migration immediately before BOS-1429.
const preNoteSyncStatesVersion int64 = 20261008000000

// mustSyncState reads a note's raw outbox row, failing the test when absent.
func mustSyncState(t *testing.T, db *sql.DB, noteID string) *models.NoteSyncState {
	t.Helper()
	state, err := syncFor(context.Background(), db, noteID)
	if err != nil {
		t.Fatalf("read sync state %s: %v", noteID, err)
	}
	if state == nil {
		t.Fatalf("note %s has no sync state row", noteID)
	}
	return state
}

// countRows counts a table's rows matching an optional WHERE clause.
func countRows(t *testing.T, db *sql.DB, table, where string, args ...any) int {
	t.Helper()
	query := "SELECT COUNT(*) FROM " + table
	if where != "" {
		query += " WHERE " + where
	}
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// installAbortTrigger makes every matching statement on note_sync_states fail,
// which is how the rollback tests inject a failure AFTER the note write in the
// same transaction.
func installAbortTrigger(t *testing.T, db *sql.DB, event string) {
	t.Helper()
	if _, err := db.Exec(fmt.Sprintf(
		`CREATE TRIGGER note_sync_fail_%s BEFORE %s ON note_sync_states BEGIN SELECT RAISE(ABORT, 'outbox boom'); END;`,
		event, event)); err != nil {
		t.Fatalf("create %s trigger: %v", event, err)
	}
}

func TestNoteSync_CreateWritesPendingVersionOne(t *testing.T) {
	db := setupTestDB(t)
	store := NewNoteStore(db)

	note := mustCreateNote(t, store, newTestNoteParams("repo-1"))

	state := mustSyncState(t, db, note.ID)
	if state.State != models.NoteSyncPending || state.SourceVersion != 1 || state.IsDeleted ||
		state.SyncedVersion != 0 || state.AttemptCount != 0 || state.NextAttemptAt != nil {
		t.Errorf("sync state after create = %+v, want pending v1 live, never attempted", state)
	}
	if note.Sync == nil || note.Sync.State != models.NoteSyncPending || note.Sync.SourceVersion != 1 {
		t.Errorf("Create read-back Sync = %+v, want pending v1", note.Sync)
	}
}

// TestNoteSync_CreateRollsBackWithOutbox proves the note insert and its outbox
// row share one transaction: an outbox failure after the note and tag inserts
// leaves neither the note, its tags, nor a sync row behind.
func TestNoteSync_CreateRollsBackWithOutbox(t *testing.T) {
	db := setupTestDB(t)
	store := NewNoteStore(db)
	installAbortTrigger(t, db, "INSERT")

	if _, err := store.Create(context.Background(), newTestNoteParams("repo-1")); err == nil {
		t.Fatal("create with a failing outbox insert = nil error, want the outbox failure")
	}
	if n := countRows(t, db, "notes", ""); n != 0 {
		t.Errorf("notes rows after rolled-back create = %d, want 0", n)
	}
	if n := countRows(t, db, "note_tags", ""); n != 0 {
		t.Errorf("note_tags rows after rolled-back create = %d, want 0", n)
	}
	if n := countRows(t, db, "note_sync_states", ""); n != 0 {
		t.Errorf("note_sync_states rows after rolled-back create = %d, want 0", n)
	}
}

// TestNoteSync_UpdateRollsBackWithOutbox proves an update's field writes roll
// back when its outbox bump fails.
func TestNoteSync_UpdateRollsBackWithOutbox(t *testing.T) {
	db := setupTestDB(t)
	store := NewNoteStore(db)
	ctx := context.Background()
	note := mustCreateNote(t, store, newTestNoteParams("repo-1"))
	installAbortTrigger(t, db, "UPDATE")

	if _, err := store.Update(ctx, UpdateNoteParams{
		ID: note.ID, Body: strPtr("rewritten"), Tags: []string{"other"},
	}); err == nil {
		t.Fatal("update with a failing outbox bump = nil error, want the outbox failure")
	}
	got, err := store.Get(ctx, note.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Body != note.Body || !reflect.DeepEqual(got.Tags, note.Tags) {
		t.Errorf("note after rolled-back update = %q %v, want %q %v unchanged", got.Body, got.Tags, note.Body, note.Tags)
	}
	if got.Sync == nil || got.Sync.SourceVersion != 1 {
		t.Errorf("sync after rolled-back update = %+v, want version 1 unchanged", got.Sync)
	}
}

// TestNoteSync_DeleteRollsBackWithOutbox proves a delete does not commit
// without its tombstone.
func TestNoteSync_DeleteRollsBackWithOutbox(t *testing.T) {
	db := setupTestDB(t)
	store := NewNoteStore(db)
	ctx := context.Background()
	note := mustCreateNote(t, store, newTestNoteParams("repo-1"))
	installAbortTrigger(t, db, "UPDATE")

	if err := store.Delete(ctx, note.ID); err == nil {
		t.Fatal("delete with a failing tombstone write = nil error, want the outbox failure")
	}
	if !noteExists(t, db, note.ID) {
		t.Error("note is gone after a delete whose tombstone failed, want it still present")
	}
	if state := mustSyncState(t, db, note.ID); state.IsDeleted || state.SourceVersion != 1 {
		t.Errorf("sync after rolled-back delete = %+v, want live v1", state)
	}
}

func TestNoteSync_IdempotentReplayDoesNotBump(t *testing.T) {
	db := setupTestDB(t)
	store := NewNoteStore(db)
	params := newTestNoteParams("repo-1")
	params.IdempotencyKey = strPtr("replay-key")

	first := mustCreateNote(t, store, params)
	again := mustCreateNote(t, store, params)

	if again.ID != first.ID {
		t.Fatalf("replay returned %s, want %s", again.ID, first.ID)
	}
	if state := mustSyncState(t, db, first.ID); state.SourceVersion != 1 {
		t.Errorf("source_version after idempotent replay = %d, want 1", state.SourceVersion)
	}
	if n := countRows(t, db, "note_sync_states", ""); n != 1 {
		t.Errorf("note_sync_states rows = %d, want 1", n)
	}
}

// TestNoteSync_UpdateBumpsAndResets covers a real edit after the previous
// version settled: the version bumps and the row becomes a fresh pending
// change with no carried-over retry history, while synced_version still names
// the last version the cloud accepted.
func TestNoteSync_UpdateBumpsAndResets(t *testing.T) {
	db := setupTestDB(t)
	store := NewNoteStore(db)
	syncStore := NewNoteSyncStore(db)
	ctx := context.Background()
	note := mustCreateNote(t, store, newTestNoteParams("repo-1"))

	if _, err := syncStore.ClaimDue(ctx, time.Now(), 10); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if applied, err := syncStore.RecordOutcome(ctx, note.ID, 1, models.NoteSyncSynced, NoteSyncOutcomeDetails{
		OrganizationID: strPtr("org-1"),
	}); err != nil || !applied {
		t.Fatalf("record synced = %v, %v; want applied", applied, err)
	}

	updated, err := store.Update(ctx, UpdateNoteParams{ID: note.ID, Body: strPtr("edited")})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	state := mustSyncState(t, db, note.ID)
	if state.State != models.NoteSyncPending || state.SourceVersion != 2 || state.AttemptCount != 0 ||
		state.NextAttemptAt != nil || state.LastError != nil {
		t.Errorf("sync after update = %+v, want pending v2 with reset retry state", state)
	}
	if state.SyncedVersion != 1 || state.SyncedAt == nil {
		t.Errorf("synced_version/synced_at after update = %d/%v, want 1 and set", state.SyncedVersion, state.SyncedAt)
	}
	if state.OrganizationID == nil || *state.OrganizationID != "org-1" {
		t.Errorf("organization_id after update = %v, want org-1 kept", state.OrganizationID)
	}
	if updated.Sync == nil || updated.Sync.SourceVersion != 2 {
		t.Errorf("Update read-back Sync = %+v, want v2", updated.Sync)
	}

	// A tags-only update is a write too.
	if _, err := store.Update(ctx, UpdateNoteParams{ID: note.ID, Tags: []string{}}); err != nil {
		t.Fatalf("tags update: %v", err)
	}
	if v := mustSyncState(t, db, note.ID).SourceVersion; v != 3 {
		t.Errorf("source_version after tags update = %d, want 3", v)
	}
}

func TestNoteSync_NoOpUpdateDoesNotBump(t *testing.T) {
	db := setupTestDB(t)
	store := NewNoteStore(db)
	note := mustCreateNote(t, store, newTestNoteParams("repo-1"))

	if _, err := store.Update(context.Background(), UpdateNoteParams{ID: note.ID}); err != nil {
		t.Fatalf("no-op update: %v", err)
	}
	if v := mustSyncState(t, db, note.ID).SourceVersion; v != 1 {
		t.Errorf("source_version after no-op update = %d, want 1", v)
	}
}

// TestNoteSync_DeleteLeavesTombstone proves a delete removes the note but
// leaves a pending tombstone that outlives it, and that deleting an absent id
// writes nothing.
func TestNoteSync_DeleteLeavesTombstone(t *testing.T) {
	db := setupTestDB(t)
	store := NewNoteStore(db)
	ctx := context.Background()
	note := mustCreateNote(t, store, newTestNoteParams("repo-1"))

	if err := store.Delete(ctx, note.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.Get(ctx, note.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("get after delete = %v, want sql.ErrNoRows", err)
	}
	state := mustSyncState(t, db, note.ID)
	if !state.IsDeleted || state.State != models.NoteSyncPending || state.SourceVersion != 2 {
		t.Errorf("tombstone = %+v, want deleted pending v2", state)
	}

	// Repeating the delete is still a no-op and must not bump the tombstone.
	if err := store.Delete(ctx, note.ID); err != nil {
		t.Fatalf("repeat delete: %v", err)
	}
	if v := mustSyncState(t, db, note.ID).SourceVersion; v != 2 {
		t.Errorf("tombstone version after repeat delete = %d, want 2", v)
	}
	if err := store.Delete(ctx, "never-existed"); err != nil {
		t.Fatalf("delete absent: %v", err)
	}
	if n := countRows(t, db, "note_sync_states", "note_id = ?", "never-existed"); n != 0 {
		t.Errorf("deleting an absent id wrote %d sync rows, want 0", n)
	}
}

// TestNoteSync_RecordOutcomeIgnoresStaleVersion is the version guard: an
// outcome for version 1 that arrives after a local edit made version 2 must not
// mark the newer version synced.
func TestNoteSync_RecordOutcomeIgnoresStaleVersion(t *testing.T) {
	db := setupTestDB(t)
	store := NewNoteStore(db)
	syncStore := NewNoteSyncStore(db)
	ctx := context.Background()
	note := mustCreateNote(t, store, newTestNoteParams("repo-1"))

	claimed, err := syncStore.ClaimDue(ctx, time.Now(), 10)
	if err != nil || len(claimed) != 1 || claimed[0].SourceVersion != 1 {
		t.Fatalf("claim = %+v, %v; want one v1 change", claimed, err)
	}
	if _, err := store.Update(ctx, UpdateNoteParams{ID: note.ID, Body: strPtr("newer")}); err != nil {
		t.Fatalf("update: %v", err)
	}

	applied, err := syncStore.RecordOutcome(ctx, note.ID, 1, models.NoteSyncSynced, NoteSyncOutcomeDetails{})
	if err != nil {
		t.Fatalf("record stale outcome: %v", err)
	}
	if applied {
		t.Error("stale v1 outcome applied, want dropped")
	}
	state := mustSyncState(t, db, note.ID)
	if state.State != models.NoteSyncPending || state.SourceVersion != 2 || state.SyncedVersion != 0 || state.SyncedAt != nil {
		t.Errorf("sync after stale outcome = %+v, want pending v2 never synced", state)
	}

	// The current version's outcome does apply.
	applied, err = syncStore.RecordOutcome(ctx, note.ID, 2, models.NoteSyncSynced, NoteSyncOutcomeDetails{})
	if err != nil || !applied {
		t.Fatalf("record current outcome = %v, %v; want applied", applied, err)
	}
	if state := mustSyncState(t, db, note.ID); state.State != models.NoteSyncSynced || state.SyncedVersion != 2 {
		t.Errorf("sync after current outcome = %+v, want synced v2", state)
	}
}

func TestNoteSync_RecordOutcomeRetryableAndInvalid(t *testing.T) {
	db := setupTestDB(t)
	store := NewNoteStore(db)
	syncStore := NewNoteSyncStore(db)
	ctx := context.Background()
	note := mustCreateNote(t, store, newTestNoteParams("repo-1"))

	next := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
	applied, err := syncStore.RecordOutcome(ctx, note.ID, 1, models.NoteSyncRateLimited, NoteSyncOutcomeDetails{
		NextAttemptAt: &next, Error: "slow down",
	})
	if err != nil || !applied {
		t.Fatalf("record rate_limited = %v, %v; want applied", applied, err)
	}
	state := mustSyncState(t, db, note.ID)
	if state.State != models.NoteSyncRateLimited || state.LastError == nil || *state.LastError != "slow down" ||
		state.NextAttemptAt == nil || !state.NextAttemptAt.Equal(next) {
		t.Errorf("sync after rate_limited = %+v, want rate_limited with error and next attempt %v", state, next)
	}

	for _, bad := range []models.NoteSyncStatus{models.NoteSyncPending, "bogus", ""} {
		if _, err := syncStore.RecordOutcome(ctx, note.ID, 1, bad, NoteSyncOutcomeDetails{}); !errors.Is(err, ErrNoteSyncInvalid) {
			t.Errorf("RecordOutcome(%q) error = %v, want ErrNoteSyncInvalid", bad, err)
		}
	}
	if applied, err := syncStore.RecordOutcome(ctx, "missing", 1, models.NoteSyncSynced, NoteSyncOutcomeDetails{}); err != nil || applied {
		t.Errorf("RecordOutcome for a missing row = %v, %v; want not applied, nil", applied, err)
	}
}

// TestNoteSync_ClaimDue covers due-ness (next_attempt_at), tombstones,
// terminal states, the limit bound, the attempt bump, and content capture.
func TestNoteSync_ClaimDue(t *testing.T) {
	db := setupTestDB(t)
	store := NewNoteStore(db, WithNoteRetention(NoteRetention{}))
	syncStore := NewNoteSyncStore(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	live := mustCreateNote(t, store, CreateNoteParams{RepoID: "repo-1", Body: "live body", Tags: []string{"a"}})
	deferred := mustCreateNote(t, store, newTestNoteParams("repo-1"))
	rejected := mustCreateNote(t, store, newTestNoteParams("repo-1"))
	gone := mustCreateNote(t, store, newTestNoteParams("repo-1"))
	if err := store.Delete(ctx, gone.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	later := now.Add(time.Hour)
	if _, err := syncStore.RecordOutcome(ctx, deferred.ID, 1, models.NoteSyncFailed,
		NoteSyncOutcomeDetails{NextAttemptAt: &later, Error: "transient"}); err != nil {
		t.Fatalf("defer: %v", err)
	}
	if _, err := syncStore.RecordOutcome(ctx, rejected.ID, 1, models.NoteSyncRejected,
		NoteSyncOutcomeDetails{Error: "invalid"}); err != nil {
		t.Fatalf("reject: %v", err)
	}
	// An orphan live row (note gone without a tombstone) has nothing to send.
	if _, err := db.Exec(`INSERT INTO note_sync_states (note_id, source_version, sync_state) VALUES ('orphan', 1, 'pending')`); err != nil {
		t.Fatalf("seed orphan: %v", err)
	}

	if got, err := syncStore.ClaimDue(ctx, now, 0); err != nil || got != nil {
		t.Errorf("ClaimDue(limit 0) = %v, %v; want nil", got, err)
	}

	claimed, err := syncStore.ClaimDue(ctx, now, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	byID := map[string]NoteSyncChange{}
	for _, c := range claimed {
		byID[c.NoteID] = c
	}
	if len(claimed) != 2 {
		t.Fatalf("claimed %v, want exactly the live note and the tombstone", claimed)
	}
	if c, ok := byID[live.ID]; !ok || c.IsDeleted || c.Note == nil || c.Note.Body != "live body" ||
		!reflect.DeepEqual(c.Note.Tags, []string{"a"}) || c.SourceVersion != 1 || c.AttemptCount != 1 {
		t.Errorf("live change = %+v (note %+v), want v1 attempt 1 with content and tags", c, c.Note)
	}
	if c, ok := byID[gone.ID]; !ok || !c.IsDeleted || c.Note != nil || c.SourceVersion != 2 {
		t.Errorf("tombstone change = %+v, want deleted v2 with no note", c)
	}
	state := mustSyncState(t, db, live.ID)
	if state.AttemptCount != 1 || state.LastAttemptedAt == nil || !state.LastAttemptedAt.Equal(now) {
		t.Errorf("live row after claim = %+v, want attempt_count 1 and last_attempted_at %v", state, now)
	}
	if n := mustSyncState(t, db, deferred.ID).AttemptCount; n != 0 {
		t.Errorf("not-yet-due row attempt_count = %d, want 0 (not claimed)", n)
	}

	// Once next_attempt_at passes the deferred row is due again; the terminal
	// rejected row never is.
	claimed, err = syncStore.ClaimDue(ctx, later, 10)
	if err != nil {
		t.Fatalf("claim later: %v", err)
	}
	var ids []string
	for _, c := range claimed {
		ids = append(ids, c.NoteID)
		if c.NoteID == rejected.ID || c.NoteID == "orphan" {
			t.Errorf("claimed %s, which is terminal/orphaned", c.NoteID)
		}
	}
	if !slices.Contains(ids, deferred.ID) {
		t.Errorf("claim at next_attempt_at = %v, want it to include %s", ids, deferred.ID)
	}

	// The limit bounds the batch.
	if claimed, err := syncStore.ClaimDue(ctx, later, 1); err != nil || len(claimed) != 1 {
		t.Errorf("ClaimDue(limit 1) = %d changes, %v; want 1", len(claimed), err)
	}
}

func TestNoteSync_PurgeSettledTombstones(t *testing.T) {
	db := setupTestDB(t)
	store := NewNoteStore(db, WithNoteRetention(NoteRetention{}))
	syncStore := NewNoteSyncStore(db)
	ctx := context.Background()

	tombstone := func(state models.NoteSyncStatus) string {
		t.Helper()
		note := mustCreateNote(t, store, newTestNoteParams("repo-1"))
		if err := store.Delete(ctx, note.ID); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if state != models.NoteSyncPending {
			if _, err := syncStore.RecordOutcome(ctx, note.ID, 2, state, NoteSyncOutcomeDetails{}); err != nil {
				t.Fatalf("record %s: %v", state, err)
			}
		}
		return note.ID
	}
	synced := tombstone(models.NoteSyncSynced)
	suppressed := tombstone(models.NoteSyncSuppressed)
	pending := tombstone(models.NoteSyncPending)
	stale := tombstone(models.NoteSyncFailed)
	if _, err := db.Exec(`UPDATE note_sync_states SET updated_at = ? WHERE note_id = ?`,
		sqlutil.FormatTime(time.Now().Add(-100*day)), stale); err != nil {
		t.Fatalf("age stale tombstone: %v", err)
	}
	live := mustCreateNote(t, store, newTestNoteParams("repo-1"))
	if _, err := db.Exec(`INSERT INTO note_sync_states (note_id, source_version, sync_state) VALUES ('orphan', 1, 'synced')`); err != nil {
		t.Fatalf("seed orphan: %v", err)
	}

	removed, err := syncStore.PurgeSettledTombstones(ctx, time.Now().Add(-90*day))
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if removed != 4 {
		t.Errorf("purged %d rows, want 4 (synced, suppressed, stale tombstones + orphan)", removed)
	}
	for _, id := range []string{synced, suppressed, stale, "orphan"} {
		if n := countRows(t, db, "note_sync_states", "note_id = ?", id); n != 0 {
			t.Errorf("row %s survived the purge", id)
		}
	}
	for _, id := range []string{pending, live.ID} {
		if n := countRows(t, db, "note_sync_states", "note_id = ?", id); n != 1 {
			t.Errorf("row %s was purged, want kept (unsettled tombstone / live note)", id)
		}
	}
}

// TestNoteSync_PurgeClampsCutoffToCloudTTL proves an unsettled tombstone is
// never aged out while the cloud copy it deletes may still be live, even when
// the caller passes a cutoff later than the cloud TTL horizon.
func TestNoteSync_PurgeClampsCutoffToCloudTTL(t *testing.T) {
	db := setupTestDB(t)
	store := NewNoteStore(db, WithNoteRetention(NoteRetention{}))
	syncStore := NewNoteSyncStore(db)
	ctx := context.Background()

	tombstone := func(age time.Duration) string {
		t.Helper()
		note := mustCreateNote(t, store, newTestNoteParams("repo-1"))
		if err := store.Delete(ctx, note.ID); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := syncStore.RecordOutcome(ctx, note.ID, 2, models.NoteSyncFailed, NoteSyncOutcomeDetails{}); err != nil {
			t.Fatalf("record failed: %v", err)
		}
		if _, err := db.Exec(`UPDATE note_sync_states SET updated_at = ? WHERE note_id = ?`,
			sqlutil.FormatTime(time.Now().Add(-age)), note.ID); err != nil {
			t.Fatalf("age tombstone: %v", err)
		}
		return note.ID
	}
	recent := tombstone(10 * day)
	expired := tombstone(NoteCloudTTL + day)

	// A cutoff of now would age out every unsettled tombstone without the clamp.
	removed, err := syncStore.PurgeSettledTombstones(ctx, time.Now())
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if removed != 1 {
		t.Errorf("purged %d rows, want 1 (only the tombstone past the cloud TTL)", removed)
	}
	if n := countRows(t, db, "note_sync_states", "note_id = ?", recent); n != 1 {
		t.Error("unpropagated 10-day-old tombstone was purged, want kept until the cloud copy expires")
	}
	if n := countRows(t, db, "note_sync_states", "note_id = ?", expired); n != 0 {
		t.Error("tombstone older than the cloud TTL survived the purge")
	}
}

// TestNoteSync_PruneRemovesOutboxRowsWithoutTombstones proves retention
// pruning takes the pruned notes' outbox rows with them and writes no
// tombstone, while the surviving notes keep theirs.
func TestNoteSync_PruneRemovesOutboxRowsWithoutTombstones(t *testing.T) {
	db := setupTestDB(t)
	store := NewNoteStore(db, WithNoteRetention(NoteRetention{MaxAge: 180 * day, MaxPerRepo: 2}))
	now := time.Now()
	seedSynced := func(id string, createdAt time.Time) {
		t.Helper()
		seedNoteAt(t, db, id, "repo-1", createdAt)
		if _, err := db.Exec(`INSERT INTO note_sync_states (note_id, source_version, sync_state) VALUES (?, 1, 'pending')`, id); err != nil {
			t.Fatalf("seed sync row %s: %v", id, err)
		}
	}
	seedSynced("expired", now.Add(-200*day))
	seedSynced("over-cap", now.Add(-20*day))
	seedSynced("kept", now.Add(-10*day))

	created := mustCreateNote(t, store, newTestNoteParams("repo-1"))

	for _, id := range []string{"expired", "over-cap"} {
		if noteExists(t, db, id) {
			t.Errorf("note %s survived the prune", id)
		}
		if n := countRows(t, db, "note_sync_states", "note_id = ?", id); n != 0 {
			t.Errorf("pruned note %s left %d outbox rows, want 0 (no tombstone)", id, n)
		}
	}
	for _, id := range []string{"kept", created.ID} {
		if n := countRows(t, db, "note_sync_states", "note_id = ? AND is_deleted = 0", id); n != 1 {
			t.Errorf("surviving note %s has %d live outbox rows, want 1", id, n)
		}
	}
	if n := countRows(t, db, "note_sync_states", "is_deleted = 1"); n != 0 {
		t.Errorf("prune wrote %d tombstones, want 0", n)
	}
}

// TestNoteSync_FailedPruneKeepsOutboxRows proves each prune kind's outbox and
// note deletes share a transaction: when the note delete fails, the expired
// note's outbox row survives with it rather than being orphaned.
func TestNoteSync_FailedPruneKeepsOutboxRows(t *testing.T) {
	db := setupTestDB(t)
	store := NewNoteStore(db, WithNoteRetention(NoteRetention{MaxAge: 180 * day}))
	seedNoteAt(t, db, "expired", "repo-1", time.Now().Add(-200*day))
	if _, err := db.Exec(`INSERT INTO note_sync_states (note_id, source_version, sync_state) VALUES ('expired', 1, 'pending')`); err != nil {
		t.Fatalf("seed sync row: %v", err)
	}
	if _, err := db.Exec(
		`CREATE TRIGGER notes_prune_fail BEFORE DELETE ON notes BEGIN SELECT RAISE(ABORT, 'prune boom'); END;`,
	); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	mustCreateNote(t, store, newTestNoteParams("repo-1"))

	if !noteExists(t, db, "expired") {
		t.Fatal("expired note is gone, so the prune did not actually fail")
	}
	if n := countRows(t, db, "note_sync_states", "note_id = 'expired'"); n != 1 {
		t.Errorf("expired note's outbox rows after a failed prune = %d, want 1 (rolled back)", n)
	}
}

// TestNoteSync_ReadsExposeSync proves Get and List attach the outbox state and
// leave Sync nil for a note with no row.
func TestNoteSync_ReadsExposeSync(t *testing.T) {
	db := setupTestDB(t)
	store := NewNoteStore(db)
	ctx := context.Background()
	note := mustCreateNote(t, store, newTestNoteParams("repo-1"))
	seedNoteAt(t, db, "no-row", "repo-1", time.Now())

	got, err := store.Get(ctx, note.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Sync == nil || got.Sync.State != models.NoteSyncPending {
		t.Errorf("Get Sync = %+v, want pending", got.Sync)
	}
	listed, err := store.List(ctx, ListNotesFilter{RepoID: strPtr("repo-1")})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("listed %d notes, want 2", len(listed))
	}
	for _, n := range listed {
		switch n.ID {
		case note.ID:
			if n.Sync == nil || n.Sync.SourceVersion != 1 || n.Sync.State != models.NoteSyncPending {
				t.Errorf("List Sync for %s = %+v, want pending v1", n.ID, n.Sync)
			}
		case "no-row":
			if n.Sync != nil {
				t.Errorf("List Sync for a note with no outbox row = %+v, want nil", n.Sync)
			}
		}
	}
}

func TestNoteSyncStatusValid(t *testing.T) {
	for _, s := range []models.NoteSyncStatus{
		models.NoteSyncPending, models.NoteSyncSynced, models.NoteSyncRejected, models.NoteSyncRateLimited,
		models.NoteSyncExpired, models.NoteSyncNotEntitled, models.NoteSyncRefused, models.NoteSyncSuppressed,
		models.NoteSyncFailed,
	} {
		if !s.Valid() {
			t.Errorf("%q.Valid() = false, want true", s)
		}
	}
	if models.NoteSyncStatus("bogus").Valid() {
		t.Error(`"bogus".Valid() = true, want false`)
	}
}

// TestNoteSyncStatesMigrationSchema asserts the BOS-1429 table and its claim
// index exist with the documented columns.
func TestNoteSyncStatesMigrationSchema(t *testing.T) {
	db := setupTestDB(t)

	cols := broadcastTableInfo(t, db, "note_sync_states")
	assertColumns(t, cols, "note_sync_states", map[string]broadcastColumn{
		"note_id":           {declType: "TEXT", notNull: false, pk: true},
		"source_version":    {declType: "INTEGER", notNull: true},
		"is_deleted":        {declType: "INTEGER", notNull: true},
		"sync_state":        {declType: "TEXT", notNull: true},
		"synced_version":    {declType: "INTEGER", notNull: true},
		"attempt_count":     {declType: "INTEGER", notNull: true},
		"next_attempt_at":   {declType: "TEXT", notNull: false},
		"last_attempted_at": {declType: "TEXT", notNull: false},
		"synced_at":         {declType: "TEXT", notNull: false},
		"organization_id":   {declType: "TEXT", notNull: false},
		"last_error":        {declType: "TEXT", notNull: false},
		"created_at":        {declType: "TEXT", notNull: true},
		"updated_at":        {declType: "TEXT", notNull: true},
		// BOS-1435 tombstone routing metadata.
		"repo_id":         {declType: "TEXT", notNull: false},
		"note_created_at": {declType: "TEXT", notNull: false},
	})
	if len(cols) != 15 {
		t.Errorf("note_sync_states has %d columns, want 15", len(cols))
	}
	if !indexNames(t, db, "note_sync_states")["idx_note_sync_states_sync_state_next_attempt_at"] {
		t.Error("idx_note_sync_states_sync_state_next_attempt_at missing")
	}
	if _, err := db.Exec(`INSERT INTO note_sync_states (note_id, source_version, sync_state) VALUES ('x', 1, 'bogus')`); err == nil {
		t.Error("sync_state CHECK accepted 'bogus'")
	}
}

// TestNoteSyncStatesMigrationBackfillAndDown seeds notes before the migration,
// asserts each is backfilled as a pending version-1 change, then rolls the
// migration down and asserts the table and index are gone with notes intact.
func TestNoteSyncStatesMigrationBackfillAndDown(t *testing.T) {
	fsys := os.DirFS(migrationsDir())
	db := dbtest.NewEmpty(t)
	if err := dbtest.RunUpTo(db, fsys, preNoteSyncStatesVersion); err != nil {
		t.Fatalf("run up to %d: %v", preNoteSyncStatesVersion, err)
	}
	seedNoteAt(t, db, "old-a", "repo-1", time.Now().Add(-400*day))
	seedNoteAt(t, db, "old-b", "repo-2", time.Now())

	if err := dbtest.RunUpTo(db, fsys, noteSyncStatesVersion); err != nil {
		t.Fatalf("run up to %d: %v", noteSyncStatesVersion, err)
	}
	for _, id := range []string{"old-a", "old-b"} {
		state := mustSyncState(t, db, id)
		if state.State != models.NoteSyncPending || state.SourceVersion != 1 || state.IsDeleted || state.SyncedVersion != 0 {
			t.Errorf("backfilled row %s = %+v, want pending v1 live", id, state)
		}
	}
	if n := countRows(t, db, "note_sync_states", ""); n != 2 {
		t.Errorf("backfilled %d rows, want 2", n)
	}

	if err := dbtest.RunDownTo(db, fsys, preNoteSyncStatesVersion); err != nil {
		t.Fatalf("run down: %v", err)
	}
	if cols := tableColumns(t, db, "note_sync_states"); len(cols) != 0 {
		t.Errorf("note_sync_states survived down: %v", cols)
	}
	if n := countRows(t, db, "sqlite_master", "type = 'index' AND name = 'idx_note_sync_states_sync_state_next_attempt_at'"); n != 0 {
		t.Error("sync index survived down")
	}
	if !noteExists(t, db, "old-a") || !noteExists(t, db, "old-b") {
		t.Error("down migration removed notes")
	}
}

// TestNoteSync_TombstoneCarriesSource proves a delete captures the note's repo
// and creation time onto its tombstone (BOS-1435), so the worker can route and
// date the delete after the note row is gone, and that a later write to the
// same row keeps that metadata.
func TestNoteSync_TombstoneCarriesSource(t *testing.T) {
	db := setupTestDB(t)
	store := NewNoteStore(db, WithNoteRetention(NoteRetention{}))
	syncStore := NewNoteSyncStore(db)
	ctx := context.Background()

	note := mustCreateNote(t, store, newTestNoteParams("repo-7"))
	if err := store.Delete(ctx, note.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	claimed, err := syncStore.ClaimDue(ctx, time.Now(), 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim = %+v, %v; want the tombstone", claimed, err)
	}
	c := claimed[0]
	if !c.IsDeleted || c.TombstoneRepoID != "repo-7" || c.TombstoneNoteCreatedAt == nil ||
		!c.TombstoneNoteCreatedAt.Equal(note.CreatedAt) {
		t.Errorf("tombstone = %+v (created %v), want repo-7 created %v", c, c.TombstoneNoteCreatedAt, note.CreatedAt)
	}

	// A live change never reports tombstone metadata, even on a row that has it.
	live := mustCreateNote(t, store, newTestNoteParams("repo-8"))
	claimed, err = syncStore.ClaimDue(ctx, time.Now(), 10)
	if err != nil {
		t.Fatalf("claim live: %v", err)
	}
	for _, c := range claimed {
		if c.NoteID == live.ID && (c.TombstoneRepoID != "" || c.TombstoneNoteCreatedAt != nil) {
			t.Errorf("live change carries tombstone metadata: %+v", c)
		}
	}
}

// TestNoteSync_NotEntitledAndRefusedRetryWhenDue proves the hourly re-check the
// worker schedules for not_entitled and refused actually reclaims the row.
func TestNoteSync_NotEntitledAndRefusedRetryWhenDue(t *testing.T) {
	db := setupTestDB(t)
	store := NewNoteStore(db, WithNoteRetention(NoteRetention{}))
	syncStore := NewNoteSyncStore(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	later := now.Add(time.Hour)

	for _, state := range []models.NoteSyncStatus{models.NoteSyncNotEntitled, models.NoteSyncRefused} {
		t.Run(string(state), func(t *testing.T) {
			note := mustCreateNote(t, store, newTestNoteParams("repo-1"))
			if _, err := syncStore.RecordOutcome(ctx, note.ID, 1, state,
				NoteSyncOutcomeDetails{NextAttemptAt: &later}); err != nil {
				t.Fatalf("record %s: %v", state, err)
			}
			claimed, err := syncStore.ClaimDue(ctx, now, 10)
			if err != nil {
				t.Fatalf("claim now: %v", err)
			}
			for _, c := range claimed {
				if c.NoteID == note.ID {
					t.Fatalf("%s row claimed before next_attempt_at", state)
				}
			}
			claimed, err = syncStore.ClaimDue(ctx, later, 10)
			if err != nil {
				t.Fatalf("claim later: %v", err)
			}
			found := false
			for _, c := range claimed {
				found = found || c.NoteID == note.ID
			}
			if !found {
				t.Errorf("%s row not reclaimed at next_attempt_at", state)
			}
		})
	}
}

func TestNoteSync_CountByState(t *testing.T) {
	db := setupTestDB(t)
	store := NewNoteStore(db, WithNoteRetention(NoteRetention{}))
	syncStore := NewNoteSyncStore(db)
	ctx := context.Background()

	if counts, err := syncStore.CountByState(ctx); err != nil || len(counts) != 0 {
		t.Fatalf("empty CountByState = %v, %v; want empty map", counts, err)
	}
	a := mustCreateNote(t, store, newTestNoteParams("repo-1"))
	mustCreateNote(t, store, newTestNoteParams("repo-1"))
	gone := mustCreateNote(t, store, newTestNoteParams("repo-1"))
	if err := store.Delete(ctx, gone.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := syncStore.RecordOutcome(ctx, a.ID, 1, models.NoteSyncSynced, NoteSyncOutcomeDetails{}); err != nil {
		t.Fatalf("record: %v", err)
	}

	counts, err := syncStore.CountByState(ctx)
	if err != nil {
		t.Fatalf("CountByState: %v", err)
	}
	want := map[models.NoteSyncStatus]int64{models.NoteSyncPending: 2, models.NoteSyncSynced: 1}
	if !reflect.DeepEqual(counts, want) {
		t.Errorf("CountByState = %v, want %v (the tombstone counts as pending)", counts, want)
	}
}
