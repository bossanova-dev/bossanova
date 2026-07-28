package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/recurser/bossalib/models"
)

// broadcastWorkerStore is the shape a broadcast delivery worker will depend on,
// mirroring callback.workerStore (services/bossd/internal/callback/worker.go)
// with the broadcast types substituted. The plan requires the lease method set
// to satisfy that same shape; asserting it here makes the claim mechanical
// rather than prose — a signature drift fails the build instead of surfacing
// when the next child writes its worker.
type broadcastWorkerStore interface {
	List(ctx context.Context, filter ListBroadcastsFilter) ([]*models.Broadcast, error)
	ExpireOverdue(ctx context.Context, now time.Time) (int, error)
	AcquireLease(ctx context.Context, id, owner string, now time.Time, leaseFor time.Duration) (*models.BroadcastDelivery, error)
	MarkDelivered(ctx context.Context, id, owner string, now time.Time) error
	ScheduleRetry(ctx context.Context, id, owner string, params ScheduleBroadcastRetryParams) error
}

var _ broadcastWorkerStore = (*SQLiteBroadcastStore)(nil)

// broadcastSecretToken is planted in a broadcast message body so tests can grep
// every diagnostic column for it. The body is a secret at rest: it must never
// reach last_error or any other delivery column.
const broadcastSecretToken = "SUPER-SECRET-BODY-Zx9Q"

// deref renders a nullable column for a failure message. Formatting a *string
// with %v prints an address, which tells the reader nothing about the value
// that actually failed the assertion.
func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func newTestBroadcast() *models.Broadcast {
	origin := "chat-origin"
	return &models.Broadcast{
		OriginChatID: &origin,
		Selector:     "chat:*",
		Message:      "please rebase, " + broadcastSecretToken,
		ExpiresAt:    time.Now().UTC().Add(time.Hour),
	}
}

// seedBroadcastWithDeliveries creates a broadcast plus n pending deliveries and
// returns the broadcast and its delivery rows in listing order.
func seedBroadcastWithDeliveries(t *testing.T, store *SQLiteBroadcastStore, n int) (*models.Broadcast, []*models.BroadcastDelivery) {
	t.Helper()
	ctx := context.Background()
	b := newTestBroadcast()
	if err := store.Create(ctx, b); err != nil {
		t.Fatalf("create broadcast: %v", err)
	}
	targets := make([]models.BroadcastDelivery, 0, n)
	for i := range n {
		targets = append(targets, models.BroadcastDelivery{
			TargetChatID: "chat-" + string(rune('a'+i)),
		})
	}
	if err := store.CreateDeliveries(ctx, b.ID, targets); err != nil {
		t.Fatalf("create deliveries: %v", err)
	}
	got, err := store.ListDeliveries(ctx, b.ID)
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	if len(got) != n {
		t.Fatalf("seeded deliveries = %d, want %d", len(got), n)
	}
	return b, got
}

func TestBroadcastStore_CreateGetRoundTrip(t *testing.T) {
	store := NewBroadcastStore(setupTestDB(t))
	ctx := context.Background()

	b := newTestBroadcast()
	if err := store.Create(ctx, b); err != nil {
		t.Fatalf("create: %v", err)
	}
	if b.ID == "" {
		t.Fatal("Create must stamp a generated id onto the broadcast")
	}
	if b.State != models.BroadcastStatePending {
		t.Errorf("state = %q, want pending", b.State)
	}

	got, err := store.Get(ctx, b.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Selector != b.Selector || got.Message != b.Message {
		t.Errorf("round trip = %+v, want selector/message preserved", got)
	}
	if got.OriginChatID == nil || *got.OriginChatID != "chat-origin" {
		t.Errorf("origin chat = %v, want chat-origin", got.OriginChatID)
	}
	if got.TargetCount != 0 {
		t.Errorf("target_count = %d, want 0 before resolution", got.TargetCount)
	}
	if d := got.ExpiresAt.Sub(b.ExpiresAt); d > time.Millisecond || d < -time.Millisecond {
		t.Errorf("expires_at = %v, want %v", got.ExpiresAt, b.ExpiresAt)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Errorf("timestamps = %v/%v, want both set", got.CreatedAt, got.UpdatedAt)
	}
}

func TestBroadcastStore_CreateRejectsInvalidInput(t *testing.T) {
	store := NewBroadcastStore(setupTestDB(t))
	ctx := context.Background()

	cases := []struct {
		name string
		mut  func(b *models.Broadcast)
	}{
		{"empty selector", func(b *models.Broadcast) { b.Selector = "" }},
		{"empty message", func(b *models.Broadcast) { b.Message = "" }},
		{"zero expiry", func(b *models.Broadcast) { b.ExpiresAt = time.Time{} }},
		{"unknown state", func(b *models.Broadcast) { b.State = models.BroadcastState("exploded") }},
		// Valid but not creatable. CreateDeliveries resolves only a pending
		// broadcast, so one created in any other state could never have an
		// audience materialised — a dead row on arrival.
		{"created resolved", func(b *models.Broadcast) { b.State = models.BroadcastStateResolved }},
		{"created completed", func(b *models.Broadcast) { b.State = models.BroadcastStateCompleted }},
		{"created expired", func(b *models.Broadcast) { b.State = models.BroadcastStateExpired }},
		{"created canceled", func(b *models.Broadcast) { b.State = models.BroadcastStateCanceled }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newTestBroadcast()
			tc.mut(b)
			if err := store.Create(ctx, b); err == nil {
				t.Fatal("err = nil, want a validation failure")
			}
		})
	}
	if err := store.Create(ctx, nil); err == nil {
		t.Error("create(nil) err = nil, want a failure")
	}
}

func TestBroadcastStore_GetNotFound(t *testing.T) {
	store := NewBroadcastStore(setupTestDB(t))
	if _, err := store.Get(context.Background(), "nope"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err = %v, want sql.ErrNoRows", err)
	}
}

func TestBroadcastStore_ListFiltersAndLimit(t *testing.T) {
	store := NewBroadcastStore(setupTestDB(t))
	ctx := context.Background()

	base := time.Now().UTC().Add(-time.Hour)
	mk := func(origin string, offset time.Duration) *models.Broadcast {
		t.Helper()
		o := origin
		b := newTestBroadcast()
		b.OriginChatID = &o
		b.CreatedAt = base.Add(offset)
		if err := store.Create(ctx, b); err != nil {
			t.Fatalf("create: %v", err)
		}
		return b
	}
	first := mk("chat-A", 0)
	second := mk("chat-A", time.Second)
	third := mk("chat-B", 2*time.Second)

	// Unfiltered listing is created_at ascending.
	all, err := store.List(ctx, ListBroadcastsFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 3 || all[0].ID != first.ID || all[2].ID != third.ID {
		t.Fatalf("unfiltered listing = %d rows, want 3 in created_at order", len(all))
	}

	// Origin chat filter.
	originA := "chat-A"
	byOrigin, err := store.List(ctx, ListBroadcastsFilter{OriginChatID: &originA})
	if err != nil {
		t.Fatalf("list by origin: %v", err)
	}
	if len(byOrigin) != 2 || byOrigin[0].ID != first.ID || byOrigin[1].ID != second.ID {
		t.Fatalf("origin filter = %d rows, want first+second", len(byOrigin))
	}

	// Limit caps the result set without changing the ordering.
	limited, err := store.List(ctx, ListBroadcastsFilter{Limit: 2})
	if err != nil {
		t.Fatalf("list limited: %v", err)
	}
	if len(limited) != 2 || limited[0].ID != first.ID {
		t.Fatalf("limit = %d rows, want the 2 oldest", len(limited))
	}

	// State filter: expire one and re-query.
	if err := store.CreateDeliveries(ctx, third.ID, nil); err != nil {
		t.Fatalf("resolve third: %v", err)
	}
	resolved := models.BroadcastStateResolved
	byState, err := store.List(ctx, ListBroadcastsFilter{State: &resolved})
	if err != nil {
		t.Fatalf("list by state: %v", err)
	}
	if len(byState) != 1 || byState[0].ID != third.ID {
		t.Fatalf("state filter = %v, want only third", byState)
	}

	// Target chat filter matches through the deliveries table.
	if err := store.CreateDeliveries(ctx, first.ID, []models.BroadcastDelivery{
		{TargetChatID: "chat-target"},
	}); err != nil {
		t.Fatalf("resolve first: %v", err)
	}
	target := "chat-target"
	byTarget, err := store.List(ctx, ListBroadcastsFilter{TargetChatID: &target})
	if err != nil {
		t.Fatalf("list by target: %v", err)
	}
	if len(byTarget) != 1 || byTarget[0].ID != first.ID {
		t.Fatalf("target filter = %v, want only first", byTarget)
	}

	// Combined filters intersect rather than widen.
	combined, err := store.List(ctx, ListBroadcastsFilter{OriginChatID: &originA, TargetChatID: &target})
	if err != nil {
		t.Fatalf("list combined: %v", err)
	}
	if len(combined) != 1 || combined[0].ID != first.ID {
		t.Fatalf("combined filter = %v, want only first", combined)
	}
	originB := "chat-B"
	none, err := store.List(ctx, ListBroadcastsFilter{OriginChatID: &originB, TargetChatID: &target})
	if err != nil {
		t.Fatalf("list disjoint: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("disjoint filter = %v, want none", none)
	}
}

func TestBroadcastStore_CreateDeliveriesStampsCountAndResolves(t *testing.T) {
	store := NewBroadcastStore(setupTestDB(t))
	ctx := context.Background()

	b, deliveries := seedBroadcastWithDeliveries(t, store, 3)

	got, err := store.Get(ctx, b.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.TargetCount != 3 {
		t.Errorf("target_count = %d, want 3", got.TargetCount)
	}
	if got.State != models.BroadcastStateResolved {
		t.Errorf("state = %q, want resolved once the audience is materialised", got.State)
	}
	for _, d := range deliveries {
		if d.BroadcastID != b.ID {
			t.Errorf("delivery %s broadcast_id = %q, want %q", d.ID, d.BroadcastID, b.ID)
		}
		if d.State != models.BroadcastDeliveryStatePending {
			t.Errorf("delivery %s state = %q, want pending", d.ID, d.State)
		}
		if d.TargetDaemonID != "" {
			t.Errorf("delivery %s target_daemon_id = %q, want the local-daemon default", d.ID, d.TargetDaemonID)
		}
		if d.AttemptCount != 0 || d.LeaseOwner != nil || d.LastError != nil {
			t.Errorf("delivery %s = %+v, want a fresh unclaimed row", d.ID, d)
		}
	}
}

// TestBroadcastStore_CreateDeliveriesAtomicity proves the write is one
// transaction: a batch that fails part-way (a duplicate id collides on the
// second insert) must leave zero delivery rows and an unchanged target_count,
// not a half-materialised audience the worker would under-deliver.
func TestBroadcastStore_CreateDeliveriesAtomicity(t *testing.T) {
	store := NewBroadcastStore(setupTestDB(t))
	ctx := context.Background()

	b := newTestBroadcast()
	if err := store.Create(ctx, b); err != nil {
		t.Fatalf("create: %v", err)
	}

	err := store.CreateDeliveries(ctx, b.ID, []models.BroadcastDelivery{
		{ID: "dup-id", TargetChatID: "chat-a"},
		{ID: "dup-id", TargetChatID: "chat-b"},
	})
	if err == nil {
		t.Fatal("err = nil, want the duplicate id to fail the batch")
	}

	rows, err := store.ListDeliveries(ctx, b.ID)
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("deliveries after failed batch = %d, want 0 (rolled back)", len(rows))
	}
	got, err := store.Get(ctx, b.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.TargetCount != 0 {
		t.Errorf("target_count = %d, want 0 (rolled back)", got.TargetCount)
	}
	if got.State != models.BroadcastStatePending {
		t.Errorf("state = %q, want pending (rolled back)", got.State)
	}
}

func TestBroadcastStore_CreateDeliveriesUnknownBroadcast(t *testing.T) {
	store := NewBroadcastStore(setupTestDB(t))
	err := store.CreateDeliveries(context.Background(), "nope", []models.BroadcastDelivery{
		{TargetChatID: "chat-a"},
	})
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err = %v, want sql.ErrNoRows", err)
	}
}

// TestBroadcastStore_ChatIDsRoundTripWhitespace pins write/read symmetry. Both
// write paths trim chat ids, so both filters must trim too — otherwise a caller
// that hands the same padded string to Create and to List gets zero rows, and a
// padded target_chat_id persists padded and misses every equality lookup
// (including idx_broadcast_deliveries_chat).
func TestBroadcastStore_ChatIDsRoundTripWhitespace(t *testing.T) {
	store := NewBroadcastStore(setupTestDB(t))
	ctx := context.Background()

	padded := "  chat-padded  "
	b := newTestBroadcast()
	b.OriginChatID = &padded
	if err := store.Create(ctx, b); err != nil {
		t.Fatalf("create: %v", err)
	}
	if b.OriginChatID == nil || *b.OriginChatID != "chat-padded" {
		t.Errorf("in-memory origin = %v, want the trimmed value", b.OriginChatID)
	}
	got, err := store.Get(ctx, b.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.OriginChatID == nil || *got.OriginChatID != "chat-padded" {
		t.Errorf("persisted origin = %v, want chat-padded", got.OriginChatID)
	}

	if err := store.CreateDeliveries(ctx, b.ID, []models.BroadcastDelivery{
		{TargetChatID: "  chat-target  "},
	}); err != nil {
		t.Fatalf("create deliveries: %v", err)
	}
	rows, err := store.ListDeliveries(ctx, b.ID)
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	if len(rows) != 1 || rows[0].TargetChatID != "chat-target" {
		t.Fatalf("persisted target = %+v, want the trimmed chat-target", rows[0])
	}

	// Both filters accept the caller's own padded strings.
	for _, origin := range []string{"chat-padded", padded} {
		o := origin
		byOrigin, lerr := store.List(ctx, ListBroadcastsFilter{OriginChatID: &o})
		if lerr != nil {
			t.Fatalf("list by origin %q: %v", o, lerr)
		}
		if len(byOrigin) != 1 {
			t.Errorf("origin filter %q = %d rows, want 1", o, len(byOrigin))
		}
	}
	for _, target := range []string{"chat-target", "  chat-target  "} {
		tc := target
		byTarget, lerr := store.List(ctx, ListBroadcastsFilter{TargetChatID: &tc})
		if lerr != nil {
			t.Fatalf("list by target %q: %v", tc, lerr)
		}
		if len(byTarget) != 1 {
			t.Errorf("target filter %q = %d rows, want 1", tc, len(byTarget))
		}
	}

	// A blank origin is stored as NULL, and the struct is corrected to match.
	blank := "   "
	nb := newTestBroadcast()
	nb.OriginChatID = &blank
	if err := store.Create(ctx, nb); err != nil {
		t.Fatalf("create blank origin: %v", err)
	}
	if nb.OriginChatID != nil {
		t.Errorf("in-memory origin = %v, want nil for a blank value", nb.OriginChatID)
	}
	gotBlank, err := store.Get(ctx, nb.ID)
	if err != nil {
		t.Fatalf("get blank: %v", err)
	}
	if gotBlank.OriginChatID != nil {
		t.Errorf("persisted origin = %v, want NULL", gotBlank.OriginChatID)
	}
}

// TestBroadcastStore_CreateDeliveriesRejectsBadTargets covers the pre-transaction
// validation branches: a bad batch must fail before it takes a write lock, and
// must never leave a partially stamped broadcast behind.
func TestBroadcastStore_CreateDeliveriesRejectsBadTargets(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		target models.BroadcastDelivery
	}{
		{"blank target chat id", models.BroadcastDelivery{TargetChatID: "   "}},
		{"mismatched broadcast id", models.BroadcastDelivery{TargetChatID: "chat-a", BroadcastID: "other"}},
		{"unknown delivery state", models.BroadcastDelivery{TargetChatID: "chat-a", State: models.BroadcastDeliveryState("warped")}},
		// Valid but not seedable: leased/delivered/failed would be lies at resolve
		// time, and CompleteIfSettled counts delivered and failed as settled — so
		// seeding one lets a broadcast complete over a row that never went out.
		{"seeded leased state", models.BroadcastDelivery{TargetChatID: "chat-a", State: models.BroadcastDeliveryStateLeased}},
		{"seeded delivered state", models.BroadcastDelivery{TargetChatID: "chat-a", State: models.BroadcastDeliveryStateDelivered}},
		{"seeded failed state", models.BroadcastDelivery{TargetChatID: "chat-a", State: models.BroadcastDeliveryStateFailed}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewBroadcastStore(setupTestDB(t))
			b := newTestBroadcast()
			if err := store.Create(ctx, b); err != nil {
				t.Fatalf("create: %v", err)
			}
			if err := store.CreateDeliveries(ctx, b.ID, []models.BroadcastDelivery{tc.target}); err == nil {
				t.Fatal("err = nil, want a validation failure")
			}
			got, err := store.Get(ctx, b.ID)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if got.State != models.BroadcastStatePending || got.TargetCount != 0 {
				t.Errorf("broadcast = %+v, want untouched pending/0", got)
			}
			rows, err := store.ListDeliveries(ctx, b.ID)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(rows) != 0 {
				t.Errorf("deliveries = %d, want 0", len(rows))
			}
		})
	}
}

// TestBroadcastStore_CreateDeliveriesRejectsDuplicateTargets closes the other
// half of the double-send hole. The one-shot state=pending predicate only stops
// a repeat *call* from appending a second audience; it says nothing about the
// contents of a single slice. Two rows for one chat are two independently
// claimable deliveries — the CAS lease guards a row, not a target — so the
// worker would message that chat twice. The padded variant matters because the
// store trims: a resolver that deduped on the raw strings would otherwise get a
// silent collision rather than an error.
func TestBroadcastStore_CreateDeliveriesRejectsDuplicateTargets(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		targets []models.BroadcastDelivery
	}{
		{"exact repeat", []models.BroadcastDelivery{{TargetChatID: "chat-a"}, {TargetChatID: "chat-a"}}},
		{"repeat after trimming", []models.BroadcastDelivery{{TargetChatID: "chat-a"}, {TargetChatID: "  chat-a  "}}},
		{"repeat behind a distinct target", []models.BroadcastDelivery{
			{TargetChatID: "chat-a"}, {TargetChatID: "chat-b"}, {TargetChatID: "chat-a"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewBroadcastStore(setupTestDB(t))
			b := newTestBroadcast()
			if err := store.Create(ctx, b); err != nil {
				t.Fatalf("create: %v", err)
			}
			if err := store.CreateDeliveries(ctx, b.ID, tc.targets); err == nil {
				t.Fatal("err = nil, want the duplicate target rejected")
			}
			rows, err := store.ListDeliveries(ctx, b.ID)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(rows) != 0 {
				t.Errorf("deliveries = %d, want 0 (batch rejected before the write)", len(rows))
			}
			got, err := store.Get(ctx, b.ID)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if got.State != models.BroadcastStatePending || got.TargetCount != 0 {
				t.Errorf("broadcast = %+v, want untouched pending/0", got)
			}
		})
	}

	// Distinct chats that merely share a prefix are not duplicates.
	store := NewBroadcastStore(setupTestDB(t))
	b := newTestBroadcast()
	if err := store.Create(ctx, b); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.CreateDeliveries(ctx, b.ID, []models.BroadcastDelivery{
		{TargetChatID: "chat-a"}, {TargetChatID: "chat-ab"},
	}); err != nil {
		t.Fatalf("distinct targets rejected: %v", err)
	}
}

// TestBroadcastStore_CreateDeliveriesAcceptsSeededSkipped pins the other side of
// the seedable-state rule: a target already known to be gone at resolve time is
// a legitimate up-front skipped row, and it must not block completion.
func TestBroadcastStore_CreateDeliveriesAcceptsSeededSkipped(t *testing.T) {
	store := NewBroadcastStore(setupTestDB(t))
	ctx := context.Background()
	b := newTestBroadcast()
	if err := store.Create(ctx, b); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.CreateDeliveries(ctx, b.ID, []models.BroadcastDelivery{
		{TargetChatID: "chat-gone", State: models.BroadcastDeliveryStateSkipped},
	}); err != nil {
		t.Fatalf("seeded skipped rejected: %v", err)
	}
	rows, err := store.ListDeliveries(ctx, b.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].State != models.BroadcastDeliveryStateSkipped {
		t.Fatalf("rows = %+v, want one skipped delivery", rows)
	}
	if err := store.CompleteIfSettled(ctx, b.ID); err != nil {
		t.Fatalf("complete: %v", err)
	}
	got, err := store.Get(ctx, b.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != models.BroadcastStateCompleted {
		t.Errorf("state = %q, want completed (nothing outstanding)", got.State)
	}
}

// TestBroadcastStore_CreateDeliveriesIsOneShot pins the guard that makes
// resolution idempotent-by-refusal. If the stamping UPDATE guarded only its
// state assignment (CASE WHEN state = 'pending') and not its WHERE, a repeated
// resolve — an at-least-once RPC, a re-queued job, a crash after COMMIT — would
// append a *second* full audience while overwriting target_count with just the
// new batch's length. Every duplicate row is independently claimable, so a wired
// worker would message every target twice: the exact double-send the CAS lease
// layer exists to prevent.
func TestBroadcastStore_CreateDeliveriesIsOneShot(t *testing.T) {
	store := NewBroadcastStore(setupTestDB(t))
	ctx := context.Background()

	b := newTestBroadcast()
	if err := store.Create(ctx, b); err != nil {
		t.Fatalf("create: %v", err)
	}
	targets := []models.BroadcastDelivery{{TargetChatID: "chat-a"}, {TargetChatID: "chat-b"}}
	if err := store.CreateDeliveries(ctx, b.ID, targets); err != nil {
		t.Fatalf("first resolve: %v", err)
	}

	err := store.CreateDeliveries(ctx, b.ID, targets)
	if !errors.Is(err, ErrBroadcastAlreadyResolved) {
		t.Fatalf("second resolve err = %v, want ErrBroadcastAlreadyResolved", err)
	}

	rows, err := store.ListDeliveries(ctx, b.ID)
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("deliveries after repeat resolve = %d, want 2 (no duplicate audience)", len(rows))
	}
	got, err := store.Get(ctx, b.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.TargetCount != 2 {
		t.Errorf("target_count = %d, want 2", got.TargetCount)
	}
}

// TestBroadcastStore_CreateDeliveriesRejectsTerminalParent covers the other half
// of the one-shot guard. Materialising an audience under a terminal broadcast
// leaves rows that AcquireLease will never claim (its parent guard admits only
// pending/resolved) and that ExpireOverdue will never reap (its cascade matches
// only expired parents) — permanently pending zombies that also block
// CompleteIfSettled forever.
func TestBroadcastStore_CreateDeliveriesRejectsTerminalParent(t *testing.T) {
	ctx := context.Background()
	for _, state := range []models.BroadcastState{
		models.BroadcastStateExpired,
		models.BroadcastStateCanceled,
		models.BroadcastStateCompleted,
	} {
		t.Run(state.String(), func(t *testing.T) {
			rawDB := setupTestDB(t)
			store := NewBroadcastStore(rawDB)
			b := newTestBroadcast()
			if err := store.Create(ctx, b); err != nil {
				t.Fatalf("create: %v", err)
			}
			if _, err := rawDB.ExecContext(ctx,
				"UPDATE broadcasts SET state = ? WHERE id = ?", state.String(), b.ID); err != nil {
				t.Fatalf("force state %s: %v", state, err)
			}

			err := store.CreateDeliveries(ctx, b.ID, []models.BroadcastDelivery{{TargetChatID: "chat-a"}})
			if !errors.Is(err, ErrBroadcastAlreadyResolved) {
				t.Fatalf("resolve under %s err = %v, want ErrBroadcastAlreadyResolved", state, err)
			}
			rows, err := store.ListDeliveries(ctx, b.ID)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(rows) != 0 {
				t.Errorf("deliveries under %s parent = %d, want 0", state, len(rows))
			}
		})
	}
}

// TestBroadcastStore_AcquireLeaseConcurrentSingleWinner runs real concurrent
// claims against a file-backed DB (the in-memory helper is single-connection,
// so it cannot express the race). Exactly one worker may hold the claim; every
// loser must see ErrBroadcastDeliveryLeaseConflict and never a partial write.
func TestBroadcastStore_AcquireLeaseConcurrentSingleWinner(t *testing.T) {
	store := NewBroadcastStore(setupFileDB(t))
	ctx := context.Background()
	_, deliveries := seedBroadcastWithDeliveries(t, store, 1)
	target := deliveries[0]

	const workers = 8
	now := time.Now().UTC()
	var wg sync.WaitGroup
	wins := make(chan string, workers)
	for i := range workers {
		wg.Add(1)
		owner := "worker-" + string(rune('a'+i))
		go func() {
			defer wg.Done()
			got, err := store.AcquireLease(ctx, target.ID, owner, now, time.Minute)
			switch {
			case err == nil:
				wins <- *got.LeaseOwner
			case errors.Is(err, ErrBroadcastDeliveryLeaseConflict):
				// Expected for every loser.
			default:
				t.Errorf("unexpected lease err: %v", err)
			}
		}()
	}
	wg.Wait()
	close(wins)

	if got := len(wins); got != 1 {
		t.Fatalf("lease winners = %d, want exactly 1", got)
	}
	winner := <-wins

	after, err := store.ListDeliveries(ctx, target.BroadcastID)
	if err != nil {
		t.Fatalf("list after race: %v", err)
	}
	if after[0].State != models.BroadcastDeliveryStateLeased {
		t.Errorf("state = %q, want leased", after[0].State)
	}
	if after[0].LeaseOwner == nil || *after[0].LeaseOwner != winner {
		t.Errorf("lease owner = %s, want %q", deref(after[0].LeaseOwner), winner)
	}
	if after[0].LeaseDeadlineAt == nil {
		t.Error("lease_deadline_at = nil, want the claim deadline")
	}
}

func TestBroadcastStore_LeaseRecoveryAfterDeadline(t *testing.T) {
	store := NewBroadcastStore(setupTestDB(t))
	ctx := context.Background()
	_, deliveries := seedBroadcastWithDeliveries(t, store, 1)
	id := deliveries[0].ID

	t0 := time.Now().UTC()
	if _, err := store.AcquireLease(ctx, id, "worker-1", t0, time.Minute); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// A live lease cannot be stolen.
	if _, err := store.AcquireLease(ctx, id, "worker-2", t0.Add(30*time.Second), time.Minute); !errors.Is(err, ErrBroadcastDeliveryLeaseConflict) {
		t.Fatalf("premature steal err = %v, want conflict", err)
	}
	// Past the deadline a dead worker's claim is recoverable.
	recovered, err := store.AcquireLease(ctx, id, "worker-2", t0.Add(2*time.Minute), time.Minute)
	if err != nil {
		t.Fatalf("recovery acquire: %v", err)
	}
	if recovered.LeaseOwner == nil || *recovered.LeaseOwner != "worker-2" {
		t.Errorf("lease owner = %s, want worker-2", deref(recovered.LeaseOwner))
	}
}

func TestBroadcastStore_AcquireLeaseMissingDelivery(t *testing.T) {
	store := NewBroadcastStore(setupTestDB(t))
	_, err := store.AcquireLease(context.Background(), "nope", "worker-1", time.Now().UTC(), time.Minute)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err = %v, want sql.ErrNoRows", err)
	}
}

// TestBroadcastStore_AcquireLeaseHonoursBackoff pins the retry guard: a
// delivery whose next_attempt_at is still in the future is not claimable, so a
// failed delivery is not hot-looped by the next worker tick.
func TestBroadcastStore_AcquireLeaseHonoursBackoff(t *testing.T) {
	store := NewBroadcastStore(setupTestDB(t))
	ctx := context.Background()
	_, deliveries := seedBroadcastWithDeliveries(t, store, 1)
	id := deliveries[0].ID

	t0 := time.Now().UTC()
	if _, err := store.AcquireLease(ctx, id, "worker-1", t0, time.Minute); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	next := t0.Add(5 * time.Minute)
	if err := store.ScheduleRetry(ctx, id, "worker-1", ScheduleBroadcastRetryParams{
		NextAttemptAt: next,
		LastError:     "deliver: transport closed",
	}); err != nil {
		t.Fatalf("schedule retry: %v", err)
	}

	if _, err := store.AcquireLease(ctx, id, "worker-2", t0.Add(time.Minute), time.Minute); !errors.Is(err, ErrBroadcastDeliveryLeaseConflict) {
		t.Fatalf("early re-lease err = %v, want conflict (backoff not elapsed)", err)
	}
	if _, err := store.AcquireLease(ctx, id, "worker-2", next.Add(time.Second), time.Minute); err != nil {
		t.Fatalf("re-lease after backoff: %v", err)
	}

	after, err := store.ListDeliveries(ctx, deliveries[0].BroadcastID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if after[0].AttemptCount != 1 {
		t.Errorf("attempt_count = %d, want 1", after[0].AttemptCount)
	}
	if after[0].NextAttemptAt == nil {
		t.Fatal("next_attempt_at = nil, want the persisted backoff")
	}
	if d := after[0].NextAttemptAt.Sub(next); d > time.Millisecond || d < -time.Millisecond {
		t.Errorf("next_attempt_at = %v, want %v", after[0].NextAttemptAt, next)
	}
	if after[0].LastError == nil || *after[0].LastError != "deliver: transport closed" {
		t.Errorf("last_error = %v, want the diagnostic", after[0].LastError)
	}
}

// TestBroadcastStore_AcquireLeaseRejectsExpiredParent guards the same hole the
// callback store closes: expiry is swept lazily, so an overdue-but-unswept
// broadcast must not have its deliveries claimed and sent.
func TestBroadcastStore_AcquireLeaseRejectsExpiredParent(t *testing.T) {
	store := NewBroadcastStore(setupTestDB(t))
	ctx := context.Background()
	_, deliveries := seedBroadcastWithDeliveries(t, store, 1)

	past := time.Now().UTC().Add(2 * time.Hour) // the seeded broadcast expires in 1h
	if _, err := store.AcquireLease(ctx, deliveries[0].ID, "worker-1", past, time.Minute); !errors.Is(err, ErrBroadcastDeliveryLeaseConflict) {
		t.Fatalf("expired-parent acquire err = %v, want conflict", err)
	}
	after, err := store.ListDeliveries(ctx, deliveries[0].BroadcastID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if after[0].State != models.BroadcastDeliveryStatePending || after[0].LeaseOwner != nil {
		t.Errorf("delivery = %+v, want unclaimed pending", after[0])
	}
}

// TestBroadcastStore_AcquireLeaseRejectsTerminalStates pins the anti-re-send
// guard: `state IN (pending, leased)`. A terminal row has no lease_owner and no
// next_attempt_at, and its parent is still live, so every *other* predicate in
// the claim admits it — this one clause is all that stands between MarkDelivered
// and a worker re-leasing an already-delivered row on its next tick. Forcing the
// state directly keeps the assertion on that clause alone rather than on the
// parent-expiry guard that would also reject a swept row.
func TestBroadcastStore_AcquireLeaseRejectsTerminalStates(t *testing.T) {
	ctx := context.Background()
	for _, state := range []models.BroadcastDeliveryState{
		models.BroadcastDeliveryStateDelivered,
		models.BroadcastDeliveryStateFailed,
		models.BroadcastDeliveryStateSkipped,
	} {
		t.Run(state.String(), func(t *testing.T) {
			rawDB := setupTestDB(t)
			store := NewBroadcastStore(rawDB)
			_, deliveries := seedBroadcastWithDeliveries(t, store, 1)
			id := deliveries[0].ID
			if _, err := rawDB.ExecContext(ctx,
				"UPDATE broadcast_deliveries SET state = ? WHERE id = ?", state.String(), id); err != nil {
				t.Fatalf("force state %s: %v", state, err)
			}

			_, err := store.AcquireLease(ctx, id, "worker-1", time.Now().UTC(), time.Minute)
			if !errors.Is(err, ErrBroadcastDeliveryLeaseConflict) {
				t.Fatalf("acquire on %s err = %v, want conflict", state, err)
			}
			after, lerr := store.ListDeliveries(ctx, deliveries[0].BroadcastID)
			if lerr != nil {
				t.Fatalf("list: %v", lerr)
			}
			if after[0].State != state || after[0].LeaseOwner != nil {
				t.Errorf("delivery = %+v, want %s and unclaimed", after[0], state)
			}
		})
	}
}

// TestBroadcastStore_AcquireLeaseRejectsRedelivery is the behavioural companion:
// the full acquire -> deliver -> re-acquire path a worker actually walks.
func TestBroadcastStore_AcquireLeaseRejectsRedelivery(t *testing.T) {
	store := NewBroadcastStore(setupTestDB(t))
	ctx := context.Background()
	_, deliveries := seedBroadcastWithDeliveries(t, store, 1)
	id := deliveries[0].ID
	now := time.Now().UTC()

	if _, err := store.AcquireLease(ctx, id, "worker-1", now, time.Minute); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := store.MarkDelivered(ctx, id, "worker-1", now); err != nil {
		t.Fatalf("mark delivered: %v", err)
	}
	if _, err := store.AcquireLease(ctx, id, "worker-2", now.Add(time.Second), time.Minute); !errors.Is(err, ErrBroadcastDeliveryLeaseConflict) {
		t.Fatalf("re-acquire after delivery err = %v, want conflict (double-send guard)", err)
	}
}

// TestBroadcastStore_LeaseOwnerIsTheFencingToken makes the owner contract
// executable rather than advisory. MarkDelivered and ScheduleRetry compare only
// lease_owner, and AcquireLease deliberately lets the current owner re-acquire,
// so the owner string is the fence. Both halves are asserted:
//
//   - a per-acquisition owner fences correctly — after the lease is recovered
//     under a fresh owner, the hung attempt's MarkDelivered reports a conflict;
//   - a *reused* stable owner does not fence — the hung attempt settles the
//     newer claim. That is the hazard the doc comment warns about, pinned here
//     so a future worker that adopts a stable owner id has a failing expectation
//     to read rather than a silent double-settle to debug.
func TestBroadcastStore_LeaseOwnerIsTheFencingToken(t *testing.T) {
	ctx := context.Background()
	t0 := time.Now().UTC()

	t.Run("per-acquisition owner fences the stale attempt", func(t *testing.T) {
		store := NewBroadcastStore(setupTestDB(t))
		_, deliveries := seedBroadcastWithDeliveries(t, store, 1)
		id := deliveries[0].ID

		if _, err := store.AcquireLease(ctx, id, "worker-1:attempt-a", t0, time.Minute); err != nil {
			t.Fatalf("first acquire: %v", err)
		}
		// The attempt hangs past its deadline; the same worker retries under a
		// fresh per-acquisition owner and recovers the lease.
		if _, err := store.AcquireLease(ctx, id, "worker-1:attempt-b", t0.Add(2*time.Minute), time.Minute); err != nil {
			t.Fatalf("recovery acquire: %v", err)
		}
		// The zombie finally reports in. It must not settle the newer claim.
		if err := store.MarkDelivered(ctx, id, "worker-1:attempt-a", t0.Add(2*time.Minute)); !errors.Is(err, ErrBroadcastDeliveryLeaseConflict) {
			t.Fatalf("stale attempt MarkDelivered err = %v, want conflict", err)
		}
		rows, err := store.ListDeliveries(ctx, deliveries[0].BroadcastID)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if rows[0].State != models.BroadcastDeliveryStateLeased {
			t.Errorf("state = %q, want the newer claim still leased", rows[0].State)
		}
		if rows[0].LeaseOwner == nil || *rows[0].LeaseOwner != "worker-1:attempt-b" {
			t.Errorf("owner = %s, want worker-1:attempt-b", deref(rows[0].LeaseOwner))
		}
	})

	t.Run("reused stable owner does not fence", func(t *testing.T) {
		store := NewBroadcastStore(setupTestDB(t))
		_, deliveries := seedBroadcastWithDeliveries(t, store, 1)
		id := deliveries[0].ID

		if _, err := store.AcquireLease(ctx, id, "worker-1", t0, time.Minute); err != nil {
			t.Fatalf("first acquire: %v", err)
		}
		if _, err := store.AcquireLease(ctx, id, "worker-1", t0.Add(2*time.Minute), time.Minute); err != nil {
			t.Fatalf("recovery acquire: %v", err)
		}
		// Indistinguishable from the live claim, so the zombie wins. Documented
		// hazard, not a store bug: the caller supplied a non-unique fence.
		if err := store.MarkDelivered(ctx, id, "worker-1", t0.Add(2*time.Minute)); err != nil {
			t.Fatalf("stale attempt MarkDelivered err = %v, want nil: a reused owner is "+
				"indistinguishable from the live claim, so this settles the newer one. "+
				"If you got here by ADDING a fencing token, that is the fix, not the bug — "+
				"delete this subtest and update AcquireLease's owner contract.", err)
		}
	})
}

// TestBroadcastStore_LeaseArgumentsAreValidated guards the two inputs that
// silently void the lease. An empty owner is the worse one: every row a blank
// owner claims looks claimable to the next blank owner, so the CAS stops
// separating workers entirely. A non-positive leaseFor produces a claim that is
// already expired when it is handed back — reported as success while anyone can
// recover it.
func TestBroadcastStore_LeaseArgumentsAreValidated(t *testing.T) {
	store := NewBroadcastStore(setupTestDB(t))
	ctx := context.Background()
	_, deliveries := seedBroadcastWithDeliveries(t, store, 1)
	id := deliveries[0].ID
	now := time.Now().UTC()

	for _, owner := range []string{"", "   "} {
		if _, err := store.AcquireLease(ctx, id, owner, now, time.Minute); err == nil {
			t.Errorf("AcquireLease(owner=%q) err = nil, want a validation failure", owner)
		}
		// On the two CAS writers a blank owner would miss the predicate anyway and
		// come back as a lease conflict, so `err != nil` cannot tell the guard from
		// the pre-existing behaviour. Assert the *validation* error specifically.
		if err := store.MarkDelivered(ctx, id, owner, now); err == nil || errors.Is(err, ErrBroadcastDeliveryLeaseConflict) {
			t.Errorf("MarkDelivered(owner=%q) err = %v, want a validation failure (not a lease conflict)", owner, err)
		}
		if err := store.ScheduleRetry(ctx, id, owner, ScheduleBroadcastRetryParams{NextAttemptAt: now}); err == nil || errors.Is(err, ErrBroadcastDeliveryLeaseConflict) {
			t.Errorf("ScheduleRetry(owner=%q) err = %v, want a validation failure (not a lease conflict)", owner, err)
		}
	}
	for _, leaseFor := range []time.Duration{0, -time.Second} {
		if _, err := store.AcquireLease(ctx, id, "worker-1", now, leaseFor); err == nil {
			t.Errorf("AcquireLease(leaseFor=%v) err = nil, want a validation failure", leaseFor)
		}
	}

	// None of the rejected calls may have touched the row.
	after, err := store.ListDeliveries(ctx, deliveries[0].BroadcastID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if after[0].State != models.BroadcastDeliveryStatePending || after[0].LeaseOwner != nil {
		t.Errorf("delivery = %+v, want an untouched pending row", after[0])
	}
	if after[0].AttemptCount != 0 {
		t.Errorf("attempt_count = %d, want 0", after[0].AttemptCount)
	}
}

// TestBroadcastStore_ExpireOverdueSweepsLeasedAndPending covers the two cascade
// arms the main expiry test does not reach. The leased arm is the load-bearing
// one: sweeping a live claim is what keeps a dead worker's lease from stranding
// behind a parent AcquireLease will never re-admit, and it is also the arm that
// produces the documented at-least-once under-report. The pending-parent arm
// proves expiry does not require the broadcast to have been resolved first.
func TestBroadcastStore_ExpireOverdueSweepsLeasedAndPending(t *testing.T) {
	store := NewBroadcastStore(setupTestDB(t))
	ctx := context.Background()
	base := time.Now().UTC()

	// A resolved broadcast with one leased delivery.
	leasedParent := newTestBroadcast()
	leasedParent.ExpiresAt = base.Add(time.Minute)
	if err := store.Create(ctx, leasedParent); err != nil {
		t.Fatalf("create leased parent: %v", err)
	}
	if err := store.CreateDeliveries(ctx, leasedParent.ID, []models.BroadcastDelivery{
		{TargetChatID: "chat-in-flight"},
	}); err != nil {
		t.Fatalf("create deliveries: %v", err)
	}
	rows, err := store.ListDeliveries(ctx, leasedParent.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if _, err := store.AcquireLease(ctx, rows[0].ID, "worker-1:attempt-a", base, time.Hour); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// A broadcast that expires before its audience is ever resolved.
	unresolved := newTestBroadcast()
	unresolved.ExpiresAt = base.Add(time.Minute)
	if err := store.Create(ctx, unresolved); err != nil {
		t.Fatalf("create unresolved: %v", err)
	}

	n, err := store.ExpireOverdue(ctx, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if n != 2 {
		t.Fatalf("expired count = %d, want 2 (a resolved and a pending parent)", n)
	}

	for _, id := range []string{leasedParent.ID, unresolved.ID} {
		got, gerr := store.Get(ctx, id)
		if gerr != nil {
			t.Fatalf("get %s: %v", id, gerr)
		}
		if got.State != models.BroadcastStateExpired {
			t.Errorf("broadcast %s state = %q, want expired", id, got.State)
		}
	}

	// The leased delivery is retired and its claim released, so the live lease
	// cannot outlive its parent.
	after, err := store.ListDeliveries(ctx, leasedParent.ID)
	if err != nil {
		t.Fatalf("list after sweep: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("deliveries = %d, want 1", len(after))
	}
	if after[0].State != models.BroadcastDeliveryStateSkipped {
		t.Errorf("leased delivery state = %q, want skipped", after[0].State)
	}
	if after[0].LeaseOwner != nil || after[0].LeaseDeadlineAt != nil {
		t.Errorf("lease = %v/%v, want released by the sweep", after[0].LeaseOwner, after[0].LeaseDeadlineAt)
	}

	// The worker that held it now reports a lost claim rather than recording a
	// delivery against an expired broadcast — the at-least-once under-report.
	if err := store.MarkDelivered(ctx, after[0].ID, "worker-1:attempt-a", base.Add(time.Hour)); !errors.Is(err, ErrBroadcastDeliveryLeaseConflict) {
		t.Errorf("post-sweep MarkDelivered err = %v, want conflict", err)
	}
}

// TestBroadcastStore_StaleOwnerCASMisses proves both CAS writes report a lost
// claim rather than clobbering the live owner's row.
func TestBroadcastStore_StaleOwnerCASMisses(t *testing.T) {
	store := NewBroadcastStore(setupTestDB(t))
	ctx := context.Background()
	_, deliveries := seedBroadcastWithDeliveries(t, store, 1)
	id := deliveries[0].ID
	now := time.Now().UTC()

	if _, err := store.AcquireLease(ctx, id, "worker-1", now, time.Minute); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	if err := store.MarkDelivered(ctx, id, "worker-2", now); !errors.Is(err, ErrBroadcastDeliveryLeaseConflict) {
		t.Fatalf("stale MarkDelivered err = %v, want conflict", err)
	}
	if err := store.ScheduleRetry(ctx, id, "worker-2", ScheduleBroadcastRetryParams{
		NextAttemptAt: now.Add(time.Minute),
		LastError:     "stale worker",
	}); !errors.Is(err, ErrBroadcastDeliveryLeaseConflict) {
		t.Fatalf("stale ScheduleRetry err = %v, want conflict", err)
	}

	mid, err := store.ListDeliveries(ctx, deliveries[0].BroadcastID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if mid[0].State != models.BroadcastDeliveryStateLeased {
		t.Errorf("state = %q, want leased (unclobbered)", mid[0].State)
	}
	if mid[0].LeaseOwner == nil || *mid[0].LeaseOwner != "worker-1" {
		t.Errorf("lease owner = %s, want worker-1", deref(mid[0].LeaseOwner))
	}
	if mid[0].AttemptCount != 0 {
		t.Errorf("attempt_count = %d, want 0 (stale retry must not count)", mid[0].AttemptCount)
	}
	if mid[0].LastError != nil {
		t.Errorf("last_error = %v, want nil (stale retry must not write)", mid[0].LastError)
	}

	// The live owner still succeeds.
	if err := store.MarkDelivered(ctx, id, "worker-1", now); err != nil {
		t.Fatalf("owner MarkDelivered: %v", err)
	}
	done, err := store.ListDeliveries(ctx, deliveries[0].BroadcastID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if done[0].State != models.BroadcastDeliveryStateDelivered || done[0].DeliveredAt == nil {
		t.Errorf("delivery = %+v, want delivered with a timestamp", done[0])
	}
	if done[0].LeaseOwner != nil || done[0].LeaseDeadlineAt != nil {
		t.Errorf("lease = %v/%v, want released after delivery", done[0].LeaseOwner, done[0].LeaseDeadlineAt)
	}
}

func TestBroadcastStore_MarkDeliveredMissingDelivery(t *testing.T) {
	store := NewBroadcastStore(setupTestDB(t))
	ctx := context.Background()
	if err := store.MarkDelivered(ctx, "nope", "worker-1", time.Now().UTC()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("MarkDelivered err = %v, want sql.ErrNoRows", err)
	}
	if err := store.ScheduleRetry(ctx, "nope", "worker-1", ScheduleBroadcastRetryParams{
		NextAttemptAt: time.Now().UTC(),
	}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("ScheduleRetry err = %v, want sql.ErrNoRows", err)
	}
}

// TestBroadcastStore_ScheduleRetryNeverPersistsMessageBody is the secret-leak
// guard: the body is stored verbatim on the broadcast because delivery needs
// it, but no delivery column may ever carry it. A caller that passed the body
// as a diagnostic would leak it into every list/inspect surface.
func TestBroadcastStore_ScheduleRetryNeverPersistsMessageBody(t *testing.T) {
	db := setupTestDB(t)
	store := NewBroadcastStore(db)
	ctx := context.Background()
	b, deliveries := seedBroadcastWithDeliveries(t, store, 1)
	id := deliveries[0].ID

	if !strings.Contains(b.Message, broadcastSecretToken) {
		t.Fatalf("fixture message = %q, want it to carry the secret token", b.Message)
	}

	now := time.Now().UTC()
	if _, err := store.AcquireLease(ctx, id, "worker-1", now, time.Minute); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := store.ScheduleRetry(ctx, id, "worker-1", ScheduleBroadcastRetryParams{
		NextAttemptAt: now.Add(time.Minute),
		LastError:     "deliver: chat has no live agent",
	}); err != nil {
		t.Fatalf("schedule retry: %v", err)
	}

	// Grep every delivery column, not just last_error: the invariant is that the
	// body never reaches this table at all.
	var joined sql.NullString
	if err := db.QueryRowContext(ctx, `
		SELECT COALESCE(id, '') || '|' || COALESCE(broadcast_id, '') || '|' ||
		       COALESCE(target_chat_id, '') || '|' || COALESCE(target_daemon_id, '') || '|' ||
		       COALESCE(state, '') || '|' || COALESCE(lease_owner, '') || '|' ||
		       COALESCE(last_error, '')
		FROM broadcast_deliveries WHERE id = ?`, id).Scan(&joined); err != nil {
		t.Fatalf("scan delivery columns: %v", err)
	}
	if strings.Contains(joined.String, broadcastSecretToken) {
		t.Errorf("delivery row %q contains the secret message body", joined.String)
	}

	got, err := store.ListDeliveries(ctx, b.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got[0].LastError == nil || strings.Contains(*got[0].LastError, broadcastSecretToken) {
		t.Errorf("last_error = %v, want the diagnostic only", got[0].LastError)
	}
}

// TestBroadcastStore_ExpireOverdue covers the lazy sweep: overdue broadcasts go
// terminal and cascade their still-open deliveries to skipped, while delivered
// history and live broadcasts are untouched.
func TestBroadcastStore_ExpireOverdue(t *testing.T) {
	store := NewBroadcastStore(setupTestDB(t))
	ctx := context.Background()

	base := time.Now().UTC()
	overdue := newTestBroadcast()
	overdue.ExpiresAt = base.Add(time.Second)
	if err := store.Create(ctx, overdue); err != nil {
		t.Fatalf("create overdue: %v", err)
	}
	if err := store.CreateDeliveries(ctx, overdue.ID, []models.BroadcastDelivery{
		{TargetChatID: "chat-delivered"},
		{TargetChatID: "chat-pending"},
	}); err != nil {
		t.Fatalf("create overdue deliveries: %v", err)
	}
	overdueRows, err := store.ListDeliveries(ctx, overdue.ID)
	if err != nil {
		t.Fatalf("list overdue deliveries: %v", err)
	}
	var deliveredID string
	for _, d := range overdueRows {
		if d.TargetChatID == "chat-delivered" {
			deliveredID = d.ID
		}
	}
	if _, err := store.AcquireLease(ctx, deliveredID, "worker-1", base, time.Minute); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := store.MarkDelivered(ctx, deliveredID, "worker-1", base); err != nil {
		t.Fatalf("mark delivered: %v", err)
	}

	fresh, freshDeliveries := seedBroadcastWithDeliveries(t, store, 1)

	// Sweep between the overdue broadcast's expiry (base+1s) and the fresh one's
	// (base+1h) so exactly one is in scope.
	n, err := store.ExpireOverdue(ctx, base.Add(30*time.Minute))
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if n != 1 {
		t.Fatalf("expired count = %d, want 1", n)
	}

	gotOverdue, err := store.Get(ctx, overdue.ID)
	if err != nil {
		t.Fatalf("get overdue: %v", err)
	}
	if gotOverdue.State != models.BroadcastStateExpired {
		t.Errorf("overdue state = %q, want expired", gotOverdue.State)
	}
	rows, err := store.ListDeliveries(ctx, overdue.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// Guard the switch below: with zero rows every case would pass vacuously.
	if len(rows) != 2 {
		t.Fatalf("overdue deliveries = %d, want 2", len(rows))
	}
	for _, d := range rows {
		switch d.TargetChatID {
		case "chat-delivered":
			if d.State != models.BroadcastDeliveryStateDelivered {
				t.Errorf("delivered row state = %q, want delivered (history preserved)", d.State)
			}
		case "chat-pending":
			if d.State != models.BroadcastDeliveryStateSkipped {
				t.Errorf("pending row state = %q, want skipped", d.State)
			}
		}
	}

	gotFresh, err := store.Get(ctx, fresh.ID)
	if err != nil {
		t.Fatalf("get fresh: %v", err)
	}
	if gotFresh.State != models.BroadcastStateResolved {
		t.Errorf("fresh state = %q, want resolved (untouched)", gotFresh.State)
	}
	freshRows, err := store.ListDeliveries(ctx, freshDeliveries[0].BroadcastID)
	if err != nil {
		t.Fatalf("list fresh: %v", err)
	}
	if freshRows[0].State != models.BroadcastDeliveryStatePending {
		t.Errorf("fresh delivery state = %q, want pending", freshRows[0].State)
	}

	// A second sweep is a no-op: the rows are already terminal.
	again, err := store.ExpireOverdue(ctx, base.Add(30*time.Minute))
	if err != nil {
		t.Fatalf("second expire: %v", err)
	}
	if again != 0 {
		t.Errorf("second sweep = %d, want 0", again)
	}
}

func TestBroadcastStore_CompleteIfSettled(t *testing.T) {
	store := NewBroadcastStore(setupTestDB(t))
	ctx := context.Background()
	b, deliveries := seedBroadcastWithDeliveries(t, store, 2)
	now := time.Now().UTC()

	requireState := func(step string, want models.BroadcastState) {
		t.Helper()
		got, err := store.Get(ctx, b.ID)
		if err != nil {
			t.Fatalf("get after %s: %v", step, err)
		}
		if got.State != want {
			t.Fatalf("state after %s = %q, want %q", step, got.State, want)
		}
	}

	// Nothing delivered: two pending deliveries block completion.
	if err := store.CompleteIfSettled(ctx, b.ID); err != nil {
		t.Fatalf("complete (pending): %v", err)
	}
	requireState("pending deliveries", models.BroadcastStateResolved)

	// A leased delivery is still in flight.
	if _, err := store.AcquireLease(ctx, deliveries[0].ID, "worker-1", now, time.Minute); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := store.CompleteIfSettled(ctx, b.ID); err != nil {
		t.Fatalf("complete (leased): %v", err)
	}
	requireState("leased delivery", models.BroadcastStateResolved)

	if err := store.MarkDelivered(ctx, deliveries[0].ID, "worker-1", now); err != nil {
		t.Fatalf("mark delivered: %v", err)
	}
	if err := store.CompleteIfSettled(ctx, b.ID); err != nil {
		t.Fatalf("complete (one left): %v", err)
	}
	requireState("one delivery outstanding", models.BroadcastStateResolved)

	// Settle the last one; now the parent flips.
	if _, err := store.AcquireLease(ctx, deliveries[1].ID, "worker-1", now, time.Minute); err != nil {
		t.Fatalf("acquire 2: %v", err)
	}
	if err := store.MarkDelivered(ctx, deliveries[1].ID, "worker-1", now); err != nil {
		t.Fatalf("mark delivered 2: %v", err)
	}
	if err := store.CompleteIfSettled(ctx, b.ID); err != nil {
		t.Fatalf("complete (settled): %v", err)
	}
	requireState("all settled", models.BroadcastStateCompleted)

	// Idempotent, and absent ids report not-found.
	if err := store.CompleteIfSettled(ctx, b.ID); err != nil {
		t.Fatalf("complete (repeat): %v", err)
	}
	if err := store.CompleteIfSettled(ctx, "nope"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("complete (absent) err = %v, want sql.ErrNoRows", err)
	}
}

// TestBroadcastStore_CompleteIfSettledIgnoresUnresolved guards the premature
// flip: an unresolved broadcast has no delivery rows yet, so "nothing pending"
// must not be read as "everything delivered".
func TestBroadcastStore_CompleteIfSettledIgnoresUnresolved(t *testing.T) {
	store := NewBroadcastStore(setupTestDB(t))
	ctx := context.Background()
	b := newTestBroadcast()
	if err := store.Create(ctx, b); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.CompleteIfSettled(ctx, b.ID); err != nil {
		t.Fatalf("complete: %v", err)
	}
	got, err := store.Get(ctx, b.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != models.BroadcastStatePending {
		t.Errorf("state = %q, want pending (audience never resolved)", got.State)
	}
}

// TestBroadcastStore_DeleteCascadesDeliveries pins the FK cascade — and the
// PRAGMA that makes it real. SQLite enforces foreign keys only when
// foreign_keys=ON, so the assertion covers both the schema and the connection.
func TestBroadcastStore_DeleteCascadesDeliveries(t *testing.T) {
	db := setupTestDB(t)
	store := NewBroadcastStore(db)
	ctx := context.Background()

	var fkOn int
	if err := db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fkOn); err != nil {
		t.Fatalf("pragma foreign_keys: %v", err)
	}
	if fkOn != 1 {
		t.Fatalf("PRAGMA foreign_keys = %d, want 1 (the cascade is inert without it)", fkOn)
	}

	b, _ := seedBroadcastWithDeliveries(t, store, 3)
	if err := store.Delete(ctx, b.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.Get(ctx, b.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("get after delete err = %v, want sql.ErrNoRows", err)
	}
	var remaining int
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM broadcast_deliveries WHERE broadcast_id = ?", b.ID).Scan(&remaining); err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	if remaining != 0 {
		t.Errorf("deliveries after cascade = %d, want 0", remaining)
	}

	// Idempotent: deleting an absent (or already deleted) row is a nil no-op.
	if err := store.Delete(ctx, b.ID); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	if err := store.Delete(ctx, "never-existed"); err != nil {
		t.Fatalf("delete absent: %v", err)
	}
}

// TestBroadcastStore_UnknownStateIsLoud is the acceptance criterion for parsed
// (not cast) states: a value outside the vocabulary — a downgrade, a manual
// edit, a future migration read by an old binary — must surface as an error
// naming the offending value, never as a silent zero state that reads as "not
// pending" and quietly changes worker behaviour.
func TestBroadcastStore_UnknownStateIsLoud(t *testing.T) {
	db := setupTestDB(t)
	store := NewBroadcastStore(db)
	ctx := context.Background()
	b, deliveries := seedBroadcastWithDeliveries(t, store, 1)

	if _, err := db.ExecContext(ctx,
		"UPDATE broadcasts SET state = 'quantum' WHERE id = ?", b.ID); err != nil {
		t.Fatalf("poison broadcast state: %v", err)
	}
	_, err := store.Get(ctx, b.ID)
	if !errors.Is(err, models.ErrUnknownBroadcastState) {
		t.Fatalf("Get err = %v, want ErrUnknownBroadcastState", err)
	}
	if !strings.Contains(err.Error(), "quantum") {
		t.Errorf("Get err = %q, want it to name the offending value", err)
	}
	if _, err := store.List(ctx, ListBroadcastsFilter{}); !errors.Is(err, models.ErrUnknownBroadcastState) {
		t.Errorf("List err = %v, want ErrUnknownBroadcastState", err)
	}

	if _, err := db.ExecContext(ctx,
		"UPDATE broadcast_deliveries SET state = 'tachyon' WHERE id = ?", deliveries[0].ID); err != nil {
		t.Fatalf("poison delivery state: %v", err)
	}
	_, derr := store.ListDeliveries(ctx, b.ID)
	if !errors.Is(derr, models.ErrUnknownBroadcastDeliveryState) {
		t.Fatalf("ListDeliveries err = %v, want ErrUnknownBroadcastDeliveryState", derr)
	}
	if !strings.Contains(derr.Error(), "tachyon") {
		t.Errorf("ListDeliveries err = %q, want it to name the offending value", derr)
	}
}

// TestBroadcastStore_DeleteUnmaterialisedOnlyTouchesPending is the whole reason
// the method exists: the state test has to be INSIDE the DELETE.
//
// A caller that decides "this row is an abandoned, never-materialised send" by
// reading the state and then calling the plain Delete is running a check-then-act
// race, and losing it is destructive rather than merely wasteful — Delete
// cascades, so the loser removes a fully materialised broadcast and every
// delivery row under it. Folding the state into the WHERE clause makes that
// window unreachable, and the bool return is what tells the caller it lost.
func TestBroadcastStore_DeleteUnmaterialisedOnlyTouchesPending(t *testing.T) {
	db := setupTestDB(t)
	store := NewBroadcastStore(db)
	ctx := context.Background()

	t.Run("a pending row is removed and reported", func(t *testing.T) {
		b := newTestBroadcast()
		if err := store.Create(ctx, b); err != nil {
			t.Fatalf("create broadcast: %v", err)
		}
		cleared, err := store.DeleteUnmaterialised(ctx, b.ID)
		if err != nil {
			t.Fatalf("delete unmaterialised: %v", err)
		}
		if !cleared {
			t.Fatal("cleared = false, want true for a pending row")
		}
		if _, err := store.Get(ctx, b.ID); !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("get after clear err = %v, want sql.ErrNoRows", err)
		}
	})

	t.Run("a resolved row and its deliveries survive", func(t *testing.T) {
		b, _ := seedBroadcastWithDeliveries(t, store, 3)
		cleared, err := store.DeleteUnmaterialised(ctx, b.ID)
		if err != nil {
			t.Fatalf("delete unmaterialised: %v", err)
		}
		if cleared {
			t.Fatal("cleared = true for a resolved row: the state guard did not hold, and the cascade just destroyed a live audience")
		}
		if _, err := store.Get(ctx, b.ID); err != nil {
			t.Errorf("the resolved broadcast was removed: %v", err)
		}
		got, err := store.ListDeliveries(ctx, b.ID)
		if err != nil {
			t.Fatalf("list deliveries: %v", err)
		}
		if len(got) != 3 {
			t.Errorf("deliveries = %d, want 3: the cascade must not have run", len(got))
		}
	})

	t.Run("an absent row is a nil no-op reporting false", func(t *testing.T) {
		cleared, err := store.DeleteUnmaterialised(ctx, "no-such-broadcast")
		if err != nil {
			t.Fatalf("delete unmaterialised: %v", err)
		}
		if cleared {
			t.Fatal("cleared = true for an absent id")
		}
	})
}
