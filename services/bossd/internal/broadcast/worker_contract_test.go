package broadcast

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossalib/migrate"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
)

// This file pins the fidelity of worker_test.go's fakeStore. Every other worker
// test runs the DeliveryWorker against that hand-written re-implementation of
// db.SQLiteBroadcastStore's CAS semantics, which proves the worker agrees with
// our MODEL of the store, not with the store: if the real SQL predicate differs
// anywhere (the parent-expiry boundary, whether ScheduleRetry requires
// state=leased, whether ExpireOverdue cascades leased rows) the fake-backed
// suite stays green while the daemon is wrong.
//
// db.SQLiteBroadcastStore already satisfies workerStore as written, so the same
// worker is driven here against a real migrated SQLite database across the
// lifecycle that matters: deliver -> settle, fail -> backoff -> recover, and the
// expiry sweep.

func broadcastContractDB(t *testing.T) *sql.DB {
	t.Helper()
	sqlDB, err := db.OpenInMemory()
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	_, filename, _, _ := runtime.Caller(0)
	migrations := filepath.Join(filepath.Dir(filename), "..", "..", "migrations")
	if err := migrate.Run(sqlDB, os.DirFS(migrations)); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	return sqlDB
}

// seedContractBroadcast creates a resolved broadcast addressing chatIDs against
// the real store and returns it.
func seedContractBroadcast(t *testing.T, store *db.SQLiteBroadcastStore, message string, expiresAt time.Time, chatIDs ...string) *models.Broadcast {
	t.Helper()
	ctx := context.Background()
	b := &models.Broadcast{
		Selector:  "repo:repo-1",
		Message:   message,
		State:     models.BroadcastStatePending,
		ExpiresAt: expiresAt,
	}
	if err := store.Create(ctx, b); err != nil {
		t.Fatalf("create broadcast: %v", err)
	}
	targets := make([]models.BroadcastDelivery, 0, len(chatIDs))
	for _, id := range chatIDs {
		targets = append(targets, models.BroadcastDelivery{TargetChatID: id})
	}
	if err := store.CreateDeliveries(ctx, b.ID, targets); err != nil {
		t.Fatalf("create deliveries: %v", err)
	}
	return b
}

func contractDelivery(t *testing.T, store *db.SQLiteBroadcastStore, broadcastID, chatID string) *models.BroadcastDelivery {
	t.Helper()
	rows, err := store.ListDeliveries(context.Background(), broadcastID)
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	for _, d := range rows {
		if d.TargetChatID == chatID {
			return d
		}
	}
	t.Fatalf("no delivery for chat %q", chatID)
	return nil
}

func contractBroadcastState(t *testing.T, store *db.SQLiteBroadcastStore, id string) models.BroadcastState {
	t.Helper()
	b, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get broadcast: %v", err)
	}
	return b.State
}

// TestWorkerContract_DeliverAndSettleAgainstTheRealStore drives the full happy
// path through the real SQL: lease, deliver, MarkDelivered, and the parent
// flipping to completed once nothing is outstanding.
func TestWorkerContract_DeliverAndSettleAgainstTheRealStore(t *testing.T) {
	store := db.NewBroadcastStore(broadcastContractDB(t))
	now := time.Now().UTC().Truncate(time.Millisecond)
	clk := newClock(now)
	b := seedContractBroadcast(t, store, "ship it", now.Add(time.Hour), "chat-1", "chat-2")

	deliverer := newCaptureDeliverer(nil)
	w := newWorker(store, deliverer, clk.now)
	w.scan(context.Background())

	if deliverer.count() != 2 {
		t.Fatalf("deliverer called %d times, want 2", deliverer.count())
	}
	if msg := deliverer.lastMessage(); !strings.Contains(msg, "ship it") || !strings.Contains(msg, b.ID) {
		t.Errorf("delivered prompt missing the body or the broadcast id:\n%s", msg)
	}
	for _, chatID := range []string{"chat-1", "chat-2"} {
		if got := contractDelivery(t, store, b.ID, chatID).State; got != models.BroadcastDeliveryStateDelivered {
			t.Errorf("%s delivery state = %q, want delivered", chatID, got)
		}
	}
	if got := contractBroadcastState(t, store, b.ID); got != models.BroadcastStateCompleted {
		t.Errorf("broadcast state = %q, want completed", got)
	}
}

// TestWorkerContract_RetryThenRecover proves the real store honours the backoff
// the worker writes (a not-yet-due row is genuinely unclaimable) and that the
// broadcast completes only once the recovered target settles.
func TestWorkerContract_RetryThenRecover(t *testing.T) {
	const token = "SECRET-BODY-TOKEN"
	store := db.NewBroadcastStore(broadcastContractDB(t))
	now := time.Now().UTC().Truncate(time.Millisecond)
	clk := newClock(now)
	b := seedContractBroadcast(t, store, token, now.Add(24*time.Hour), "chat-1")

	deliverer := newCaptureDeliverer(nil)
	// Echo the body back, as the real tmux submit verifier does, so the
	// last_error assertion below exercises the redaction against the real column.
	deliverer.failChat("chat-1", &echoError{payload: "Broadcast received.\n\n" + token + "\n"})
	w := newWorker(store, deliverer, clk.now)
	w.scan(context.Background())

	d := contractDelivery(t, store, b.ID, "chat-1")
	if d.State != models.BroadcastDeliveryStatePending {
		t.Fatalf("state = %q, want pending (retryable)", d.State)
	}
	if d.AttemptCount != 1 {
		t.Errorf("attempt_count = %d, want 1", d.AttemptCount)
	}
	if d.NextAttemptAt == nil || !d.NextAttemptAt.Equal(now.Add(baseRetryBackoff)) {
		t.Errorf("next_attempt_at = %v, want %v", d.NextAttemptAt, now.Add(baseRetryBackoff))
	}
	if d.LastError == nil || strings.Contains(*d.LastError, token) {
		t.Fatalf("last_error = %v, want a body-free diagnostic", d.LastError)
	}
	if got := contractBroadcastState(t, store, b.ID); got != models.BroadcastStateResolved {
		t.Errorf("broadcast state = %q, want still resolved", got)
	}

	// Not yet due. Probe the STORE directly: the worker's own cheap pre-filter
	// would short-circuit before AcquireLease, so a scan alone would stay green
	// even if the SQL claimability predicate lost its next_attempt_at guard —
	// exactly the drift this file exists to catch.
	if _, aerr := store.AcquireLease(context.Background(), d.ID, "probe:1", clk.now(), time.Minute); !errors.Is(aerr, db.ErrBroadcastDeliveryLeaseConflict) {
		t.Fatalf("AcquireLease before the backoff elapsed = %v, want a lease conflict", aerr)
	}
	w.scan(context.Background())
	if again := contractDelivery(t, store, b.ID, "chat-1"); again.AttemptCount != 1 {
		t.Errorf("attempt_count = %d after an early scan, want the backoff honoured", again.AttemptCount)
	}

	deliverer.failChat("chat-1", nil)
	clk.advance(baseRetryBackoff)
	w.scan(context.Background())

	if got := contractDelivery(t, store, b.ID, "chat-1").State; got != models.BroadcastDeliveryStateDelivered {
		t.Errorf("delivery state = %q, want delivered", got)
	}
	if got := contractBroadcastState(t, store, b.ID); got != models.BroadcastStateCompleted {
		t.Errorf("broadcast state = %q, want completed", got)
	}
}

// TestWorkerContract_ExpirySweepSkipsOutstanding proves the real ExpireOverdue
// cascade matches the fake: an overdue broadcast is expired and its outstanding
// delivery becomes skipped, with nothing sent.
func TestWorkerContract_ExpirySweepSkipsOutstanding(t *testing.T) {
	store := db.NewBroadcastStore(broadcastContractDB(t))
	now := time.Now().UTC().Truncate(time.Millisecond)
	clk := newClock(now)
	// Expiry sooner than the first backoff, so the retry is pinned to it.
	b := seedContractBroadcast(t, store, "m", now.Add(10*time.Second), "chat-1")

	deliverer := newCaptureDeliverer(nil)
	deliverer.failChat("chat-1", &echoError{payload: "m"})
	w := newWorker(store, deliverer, clk.now)
	w.scan(context.Background())

	d := contractDelivery(t, store, b.ID, "chat-1")
	if d.NextAttemptAt == nil || !d.NextAttemptAt.Equal(b.ExpiresAt) {
		t.Errorf("next_attempt_at = %v, want pinned to expiry %v", d.NextAttemptAt, b.ExpiresAt)
	}

	clk.advance(20 * time.Second)
	w.scan(context.Background())

	if got := contractBroadcastState(t, store, b.ID); got != models.BroadcastStateExpired {
		t.Errorf("broadcast state = %q, want expired", got)
	}
	if got := contractDelivery(t, store, b.ID, "chat-1").State; got != models.BroadcastDeliveryStateSkipped {
		t.Errorf("delivery state = %q, want skipped", got)
	}
	if deliverer.attempts() != 1 {
		t.Errorf("deliverer attempted %d sends, want 1 (nothing may be sent after expiry)", deliverer.attempts())
	}
}

// TestWorkerContract_SelfHealsALostCompletion proves the scan-level settle check
// works against the real store: a broadcast whose deliveries all settled in a
// previous process still reaches completed.
func TestWorkerContract_SelfHealsALostCompletion(t *testing.T) {
	sqlDB := broadcastContractDB(t)
	store := db.NewBroadcastStore(sqlDB)
	now := time.Now().UTC().Truncate(time.Millisecond)
	clk := newClock(now)
	b := seedContractBroadcast(t, store, "m", now.Add(time.Hour), "chat-1")

	// Simulate the previous process: the delivery committed, the completion did not.
	if _, err := sqlDB.Exec("UPDATE broadcast_deliveries SET state = 'delivered' WHERE broadcast_id = ?", b.ID); err != nil {
		t.Fatalf("seed delivered row: %v", err)
	}

	deliverer := newCaptureDeliverer(nil)
	w := newWorker(store, deliverer, clk.now)
	w.scan(context.Background())

	if deliverer.count() != 0 {
		t.Errorf("deliverer called %d times, want 0", deliverer.count())
	}
	if got := contractBroadcastState(t, store, b.ID); got != models.BroadcastStateCompleted {
		t.Errorf("broadcast state = %q, want completed", got)
	}
}

// echoError mimics the real transport failure: tmux's submit verifier formats
// the WHOLE payload back into its error with %q when a paste never left the
// composer, which is how the message body could reach last_error. It must really
// carry the payload, or the "last_error is body-free" assertion is tautological.
type echoError struct{ payload string }

func (e *echoError) Error() string {
	return fmt.Sprintf("send message: command was not submitted; %q is still present at the tmux prompt", e.payload)
}
