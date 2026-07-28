package broadcast

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
)

// clock is a deterministic, advanceable, concurrency-safe time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock(t time.Time) *clock { return &clock{t: t.UTC()} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeStore is an in-memory workerStore reproducing the CAS semantics of
// db.SQLiteBroadcastStore that the worker depends on: lease claimability
// (backoff elapsed, lease free or dead, parent live and unexpired), the
// owner-fenced MarkDelivered/ScheduleRetry, the lazy expiry sweep, and
// CompleteIfSettled's "nothing pending or leased" predicate.
type fakeStore struct {
	mu         sync.Mutex
	broadcasts map[string]*models.Broadcast
	deliveries map[string]*models.BroadcastDelivery
	order      []string // delivery ids, in creation order

	// Scripted failures.
	acquireErr       error // returned by AcquireLease when non-nil
	markDeliveredErr error // returned by MarkDelivered when non-nil

	// Call records.
	completeIfSettledCalls int
	acquiredOwners         []string
}

// owners returns every lease owner string AcquireLease was called with.
func (s *fakeStore) owners() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.acquiredOwners...)
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		broadcasts: map[string]*models.Broadcast{},
		deliveries: map[string]*models.BroadcastDelivery{},
	}
}

// testBroadcastID is the id every fixture broadcast is seeded under, and
// testOwnerPrefix the lease-owner prefix every fixture worker runs with.
const (
	testBroadcastID = "b-1"
	testOwnerPrefix = "worker-1"
)

// addBroadcast seeds a resolved broadcast plus one pending delivery per chat.
func (s *fakeStore) addBroadcast(message string, expiresAt time.Time, chatIDs ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := testBroadcastID
	b := &models.Broadcast{
		ID:          id,
		Selector:    "repo:repo-1",
		Message:     message,
		State:       models.BroadcastStateResolved,
		TargetCount: len(chatIDs),
		ExpiresAt:   expiresAt,
	}
	s.broadcasts[id] = b
	for i, chatID := range chatIDs {
		did := id + "-d" + string(rune('1'+i))
		s.deliveries[did] = &models.BroadcastDelivery{
			ID:           did,
			BroadcastID:  id,
			TargetChatID: chatID,
			State:        models.BroadcastDeliveryStatePending,
		}
		s.order = append(s.order, did)
	}
}

func (s *fakeStore) delivery(t *testing.T, id string) models.BroadcastDelivery {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.deliveries[id]
	if !ok {
		t.Fatalf("delivery %s not found", id)
	}
	return *d
}

func (s *fakeStore) broadcastState(t *testing.T) models.BroadcastState {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.broadcasts[testBroadcastID]
	if !ok {
		t.Fatalf("broadcast %s not found", testBroadcastID)
	}
	return b.State
}

func (s *fakeStore) List(_ context.Context, filter db.ListBroadcastsFilter) ([]*models.Broadcast, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*models.Broadcast
	for _, b := range s.broadcasts {
		if filter.State != nil && b.State != *filter.State {
			continue
		}
		clone := *b
		out = append(out, &clone)
	}
	return out, nil
}

func (s *fakeStore) ListDeliveries(_ context.Context, broadcastID string) ([]*models.BroadcastDelivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*models.BroadcastDelivery
	for _, id := range s.order {
		d := s.deliveries[id]
		if d.BroadcastID != broadcastID {
			continue
		}
		clone := *d
		out = append(out, &clone)
	}
	return out, nil
}

func (s *fakeStore) ExpireOverdue(_ context.Context, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	expired := 0
	for _, b := range s.broadcasts {
		if b.State != models.BroadcastStatePending && b.State != models.BroadcastStateResolved {
			continue
		}
		if b.ExpiresAt.After(now) {
			continue
		}
		b.State = models.BroadcastStateExpired
		expired++
		for _, d := range s.deliveries {
			if d.BroadcastID != b.ID {
				continue
			}
			if d.State == models.BroadcastDeliveryStatePending || d.State == models.BroadcastDeliveryStateLeased {
				d.State = models.BroadcastDeliveryStateSkipped
				d.LeaseOwner = nil
			}
		}
	}
	return expired, nil
}

func (s *fakeStore) AcquireLease(_ context.Context, id, owner string, now time.Time, leaseFor time.Duration) (*models.BroadcastDelivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acquiredOwners = append(s.acquiredOwners, owner)
	if s.acquireErr != nil {
		return nil, s.acquireErr
	}
	d, ok := s.deliveries[id]
	if !ok {
		return nil, sql.ErrNoRows
	}
	if d.State != models.BroadcastDeliveryStatePending && d.State != models.BroadcastDeliveryStateLeased {
		return nil, db.ErrBroadcastDeliveryLeaseConflict
	}
	if d.NextAttemptAt != nil && d.NextAttemptAt.After(now) {
		return nil, db.ErrBroadcastDeliveryLeaseConflict
	}
	if d.LeaseOwner != nil && *d.LeaseOwner != owner &&
		d.LeaseDeadlineAt != nil && d.LeaseDeadlineAt.After(now) {
		return nil, db.ErrBroadcastDeliveryLeaseConflict
	}
	b, ok := s.broadcasts[d.BroadcastID]
	if !ok {
		return nil, sql.ErrNoRows
	}
	if b.State != models.BroadcastStatePending && b.State != models.BroadcastStateResolved {
		return nil, db.ErrBroadcastDeliveryLeaseConflict
	}
	if !b.ExpiresAt.After(now) {
		return nil, db.ErrBroadcastDeliveryLeaseConflict
	}
	deadline := now.Add(leaseFor)
	d.State = models.BroadcastDeliveryStateLeased
	d.LeaseOwner = &owner
	d.LeaseDeadlineAt = &deadline
	clone := *d
	return &clone, nil
}

func (s *fakeStore) MarkDelivered(_ context.Context, id, owner string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.markDeliveredErr != nil {
		return s.markDeliveredErr
	}
	d, ok := s.deliveries[id]
	if !ok {
		return sql.ErrNoRows
	}
	if d.State != models.BroadcastDeliveryStateLeased || d.LeaseOwner == nil || *d.LeaseOwner != owner {
		return db.ErrBroadcastDeliveryLeaseConflict
	}
	d.State = models.BroadcastDeliveryStateDelivered
	d.DeliveredAt = &now
	d.LeaseOwner = nil
	d.LeaseDeadlineAt = nil
	d.LastError = nil
	return nil
}

func (s *fakeStore) ScheduleRetry(_ context.Context, id, owner string, params db.ScheduleBroadcastRetryParams) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.deliveries[id]
	if !ok {
		return sql.ErrNoRows
	}
	if d.LeaseOwner == nil || *d.LeaseOwner != owner {
		return db.ErrBroadcastDeliveryLeaseConflict
	}
	d.AttemptCount++
	if !params.NextAttemptAt.IsZero() {
		next := params.NextAttemptAt
		d.NextAttemptAt = &next
	}
	if params.LastError != "" {
		e := params.LastError
		d.LastError = &e
	}
	if d.State == models.BroadcastDeliveryStateLeased {
		d.State = models.BroadcastDeliveryStatePending
	}
	d.LeaseOwner = nil
	d.LeaseDeadlineAt = nil
	return nil
}

func (s *fakeStore) CompleteIfSettled(_ context.Context, broadcastID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completeIfSettledCalls++
	b, ok := s.broadcasts[broadcastID]
	if !ok {
		return sql.ErrNoRows
	}
	if b.State != models.BroadcastStateResolved {
		return nil
	}
	for _, d := range s.deliveries {
		if d.BroadcastID != broadcastID {
			continue
		}
		if d.State == models.BroadcastDeliveryStatePending || d.State == models.BroadcastDeliveryStateLeased {
			return nil
		}
	}
	b.State = models.BroadcastStateCompleted
	return nil
}

// captureDeliverer records every delivery and can be scripted to fail, either
// globally or per target chat.
type captureDeliverer struct {
	mu      sync.Mutex
	err     error
	perChat map[string]error
	ids     []string
	msgs    []string
	// attemptCount records every Deliver call; ids/msgs record only successes.
	attemptCount int
}

func newCaptureDeliverer(err error) *captureDeliverer {
	return &captureDeliverer{err: err, perChat: map[string]error{}}
}

func (c *captureDeliverer) Deliver(_ context.Context, id, message string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.attemptCount++
	if err, ok := c.perChat[id]; ok && err != nil {
		return err
	}
	if c.err != nil {
		return c.err
	}
	c.ids = append(c.ids, id)
	c.msgs = append(c.msgs, message)
	return nil
}

func (c *captureDeliverer) failChat(chatID string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.perChat[chatID] = err
}

// attempts counts every Deliver call, successful or not — count() only counts
// successes, so it cannot distinguish "nothing was sent" from "the send failed".
func (c *captureDeliverer) attempts() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.attemptCount
}

func (c *captureDeliverer) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.ids)
}

func (c *captureDeliverer) lastMessage() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.msgs) == 0 {
		return ""
	}
	return c.msgs[len(c.msgs)-1]
}

func newWorker(store workerStore, deliverer ChatDeliverer, now func() time.Time) *DeliveryWorker {
	return NewDeliveryWorker(WorkerConfig{
		Store:     store,
		Deliverer: deliverer,
		Now:       now,
		Owner:     testOwnerPrefix,
		Logger:    zerolog.Nop(),
	})
}

// TestWorker_HappyPath verifies lease -> deliver -> MarkDelivered, that the
// deliverer received the rendered prompt (verbatim body plus broadcast id), and
// that the settled broadcast completes.
func TestWorker_HappyPath(t *testing.T) {
	store := newFakeStore()
	clk := newClock(time.Now().UTC())
	store.addBroadcast("ship it now", clk.now().Add(time.Hour), "chat-1")

	deliverer := newCaptureDeliverer(nil)
	w := newWorker(store, deliverer, clk.now)
	w.scan(context.Background())

	if got := store.delivery(t, "b-1-d1").State; got != models.BroadcastDeliveryStateDelivered {
		t.Fatalf("delivery state = %q, want delivered", got)
	}
	if deliverer.count() != 1 {
		t.Fatalf("deliverer called %d times, want 1", deliverer.count())
	}
	msg := deliverer.lastMessage()
	if !strings.Contains(msg, "ship it now") {
		t.Errorf("delivered message missing verbatim body:\n%s", msg)
	}
	if !strings.Contains(msg, "b-1") {
		t.Errorf("delivered message missing broadcast id:\n%s", msg)
	}
	if got := store.broadcastState(t); got != models.BroadcastStateCompleted {
		t.Errorf("broadcast state = %q, want completed", got)
	}
}

// TestWorker_RetryBackoffSchedule verifies a failing deliverer keeps the
// delivery retryable, releases the lease, records the transport error (never the
// body), and schedules each attempt at the doubling backoff.
func TestWorker_RetryBackoffSchedule(t *testing.T) {
	store := newFakeStore()
	base := time.Now().UTC()
	clk := newClock(base)
	store.addBroadcast("SECRET-BODY-TOKEN", base.Add(24*time.Hour), "chat-1")

	// The scripted error ECHOES the body, as the real tmux submit verifier does,
	// so the last_error leak assertion below is not vacuous: with the redaction
	// removed it fails.
	failing := newCaptureDeliverer(errors.New("chat offline: \"SECRET-BODY-TOKEN\" is still present at the tmux prompt"))
	w := newWorker(store, failing, clk.now)

	wantDelays := []time.Duration{
		30 * time.Second,
		time.Minute,
		2 * time.Minute,
		4 * time.Minute,
		8 * time.Minute,
		maxRetryBackoff, // 16m would exceed the cap
		maxRetryBackoff,
	}
	for i, want := range wantDelays {
		at := clk.now()
		w.scan(context.Background())

		d := store.delivery(t, "b-1-d1")
		if d.State != models.BroadcastDeliveryStatePending {
			t.Fatalf("attempt %d: state = %q, want pending (retryable)", i+1, d.State)
		}
		if d.AttemptCount != i+1 {
			t.Fatalf("attempt %d: attempt_count = %d, want %d", i+1, d.AttemptCount, i+1)
		}
		if d.LeaseOwner != nil {
			t.Errorf("attempt %d: lease still held by %q, want released", i+1, *d.LeaseOwner)
		}
		if d.NextAttemptAt == nil {
			t.Fatalf("attempt %d: next_attempt_at not set", i+1)
		}
		if got := d.NextAttemptAt.Sub(at); got != want {
			t.Errorf("attempt %d: backoff = %v, want %v", i+1, got, want)
		}
		if d.LastError == nil || !strings.Contains(*d.LastError, "chat offline") {
			t.Errorf("attempt %d: last_error = %v, want the transport error", i+1, d.LastError)
		}
		if d.LastError != nil && strings.Contains(*d.LastError, "SECRET-BODY-TOKEN") {
			t.Fatalf("attempt %d: last_error leaked the message body: %q", i+1, *d.LastError)
		}
		// A retry that is not yet due must not be re-attempted.
		w.scan(context.Background())
		if again := store.delivery(t, "b-1-d1"); again.AttemptCount != i+1 {
			t.Fatalf("attempt %d: backoff not honoured, attempt_count = %d", i+1, again.AttemptCount)
		}
		clk.advance(want)
	}
	if failing.count() != 0 {
		t.Errorf("deliverer recorded %d successful deliveries, want 0", failing.count())
	}
}

// TestBackoffFor_DoublesAndCaps verifies the exponential backoff schedule and
// its 15-minute ceiling.
func TestBackoffFor_DoublesAndCaps(t *testing.T) {
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 30 * time.Second},
		{1, 60 * time.Second},
		{2, 2 * time.Minute},
		{3, 4 * time.Minute},
		{4, 8 * time.Minute},
		{5, maxRetryBackoff},  // 16m -> capped
		{6, maxRetryBackoff},  // stays capped
		{20, maxRetryBackoff}, // far past cap
	}
	for _, tc := range cases {
		if got := backoffFor(tc.attempt); got != tc.want {
			t.Errorf("backoffFor(%d) = %v, want %v", tc.attempt, got, tc.want)
		}
	}
}

// TestWorker_PastExpiryPinsToExpiry verifies a retry that would land at or after
// the parent's expiry is pinned to ExpiresAt, so the delivery is left for the
// lazy sweep to reap rather than retried forever.
func TestWorker_PastExpiryPinsToExpiry(t *testing.T) {
	store := newFakeStore()
	base := time.Now().UTC()
	clk := newClock(base)
	expires := base.Add(10 * time.Second) // sooner than baseRetryBackoff (30s)
	store.addBroadcast("m", expires, "chat-1")

	failing := newCaptureDeliverer(errors.New("chat offline"))
	w := newWorker(store, failing, clk.now)
	w.scan(context.Background())

	d := store.delivery(t, "b-1-d1")
	if d.NextAttemptAt == nil {
		t.Fatal("next_attempt_at not set")
	}
	if !d.NextAttemptAt.Equal(expires) {
		t.Errorf("next_attempt_at = %v, want pinned to expiry %v", d.NextAttemptAt, expires)
	}

	// Advance past expiry: the lazy sweep reaps the broadcast and skips the row.
	clk.advance(20 * time.Second)
	w.scan(context.Background())
	if got := store.broadcastState(t); got != models.BroadcastStateExpired {
		t.Errorf("broadcast state = %q, want expired", got)
	}
	if got := store.delivery(t, "b-1-d1").State; got != models.BroadcastDeliveryStateSkipped {
		t.Errorf("delivery state = %q, want skipped", got)
	}
	if failing.count() != 0 {
		t.Errorf("deliverer recorded %d successful deliveries, want 0", failing.count())
	}
}

// TestWorker_LeaseConflictIsSkippedSilently verifies a claim lost to another
// worker is a benign skip: nothing is delivered and nothing is rescheduled.
func TestWorker_LeaseConflictIsSkippedSilently(t *testing.T) {
	for name, acquireErr := range map[string]error{
		"lease conflict": db.ErrBroadcastDeliveryLeaseConflict,
		"row gone":       sql.ErrNoRows,
	} {
		t.Run(name, func(t *testing.T) {
			store := newFakeStore()
			clk := newClock(time.Now().UTC())
			store.addBroadcast("m", clk.now().Add(time.Hour), "chat-1")
			store.acquireErr = acquireErr

			deliverer := newCaptureDeliverer(nil)
			w := newWorker(store, deliverer, clk.now)
			w.scan(context.Background())

			if deliverer.count() != 0 {
				t.Errorf("deliverer called %d times, want 0", deliverer.count())
			}
			d := store.delivery(t, "b-1-d1")
			if d.State != models.BroadcastDeliveryStatePending {
				t.Errorf("delivery state = %q, want untouched pending", d.State)
			}
			if d.AttemptCount != 0 {
				t.Errorf("attempt_count = %d, want 0 (no retry scheduled)", d.AttemptCount)
			}
		})
	}
}

// TestWorker_MarkDeliveredFailureIsTolerated verifies the at-least-once race: a
// MarkDelivered that no longer applies after a successful send is logged and
// tolerated, never retried (which would double-send).
func TestWorker_MarkDeliveredFailureIsTolerated(t *testing.T) {
	store := newFakeStore()
	clk := newClock(time.Now().UTC())
	store.addBroadcast("m", clk.now().Add(time.Hour), "chat-1")
	store.markDeliveredErr = db.ErrBroadcastDeliveryLeaseConflict

	deliverer := newCaptureDeliverer(nil)
	w := newWorker(store, deliverer, clk.now)
	w.scan(context.Background())

	if deliverer.count() != 1 {
		t.Fatalf("deliverer called %d times, want 1", deliverer.count())
	}
	d := store.delivery(t, "b-1-d1")
	if d.AttemptCount != 0 {
		t.Errorf("attempt_count = %d, want 0 (a delivered message must not be retried)", d.AttemptCount)
	}
	if d.NextAttemptAt != nil {
		t.Errorf("next_attempt_at = %v, want unset", d.NextAttemptAt)
	}
}

// TestWorker_CompleteOnlyWhenEveryDeliverySettles verifies a broadcast stays
// resolved while any target is still outstanding, and completes once the last
// one settles.
func TestWorker_CompleteOnlyWhenEveryDeliverySettles(t *testing.T) {
	store := newFakeStore()
	base := time.Now().UTC()
	clk := newClock(base)
	store.addBroadcast("m", base.Add(time.Hour), "chat-1", "chat-2")

	deliverer := newCaptureDeliverer(nil)
	deliverer.failChat("chat-2", errors.New("chat offline"))
	w := newWorker(store, deliverer, clk.now)

	w.scan(context.Background())
	if got := store.delivery(t, "b-1-d1").State; got != models.BroadcastDeliveryStateDelivered {
		t.Fatalf("chat-1 delivery state = %q, want delivered", got)
	}
	if got := store.broadcastState(t); got != models.BroadcastStateResolved {
		t.Fatalf("broadcast state = %q, want still resolved (chat-2 outstanding)", got)
	}
	if store.completeIfSettledCalls == 0 {
		t.Error("CompleteIfSettled was never called after the first delivery settled")
	}

	// chat-2 recovers; once its backoff elapses the broadcast completes.
	deliverer.failChat("chat-2", nil)
	clk.advance(baseRetryBackoff)
	w.scan(context.Background())

	if got := store.delivery(t, "b-1-d2").State; got != models.BroadcastDeliveryStateDelivered {
		t.Fatalf("chat-2 delivery state = %q, want delivered", got)
	}
	if got := store.broadcastState(t); got != models.BroadcastStateCompleted {
		t.Errorf("broadcast state = %q, want completed", got)
	}
	if deliverer.count() != 2 {
		t.Errorf("deliverer called %d times, want 2", deliverer.count())
	}
}

// TestWorker_RunScansImmediatelyAndStops verifies Run performs one scan before
// the first tick and returns when the context is cancelled.
func TestWorker_RunScansImmediatelyAndStops(t *testing.T) {
	store := newFakeStore()
	clk := newClock(time.Now().UTC())
	store.addBroadcast("m", clk.now().Add(time.Hour), "chat-1")

	deliverer := newCaptureDeliverer(nil)
	w := NewDeliveryWorker(WorkerConfig{
		Store:        store,
		Deliverer:    deliverer,
		Now:          clk.now,
		Owner:        "worker-1",
		Logger:       zerolog.Nop(),
		PollInterval: time.Hour, // never ticks during the test
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()

	deadline := time.After(2 * time.Second)
	for deliverer.count() == 0 {
		select {
		case <-deadline:
			t.Fatal("Run did not scan immediately")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

// TestWorker_LeaseOwnerIsUniquePerAcquisition guards the store's fencing-token
// contract: a stable owner would let a hung attempt whose lease expired settle
// the newer claim that replaced it.
func TestWorker_LeaseOwnerIsUniquePerAcquisition(t *testing.T) {
	store := newFakeStore()
	base := time.Now().UTC()
	clk := newClock(base)
	store.addBroadcast("m", base.Add(time.Hour), "chat-1")

	failing := newCaptureDeliverer(errors.New("chat offline"))
	w := newWorker(store, failing, clk.now)

	for range 3 {
		w.scan(context.Background())
		clk.advance(maxRetryBackoff)
	}

	owners := store.owners()
	if len(owners) != 3 {
		t.Fatalf("AcquireLease called %d times, want 3", len(owners))
	}
	seen := map[string]bool{}
	for _, owner := range owners {
		if strings.TrimSpace(owner) == "" {
			t.Fatal("lease owner is empty, which voids the fencing token")
		}
		if !strings.HasPrefix(owner, testOwnerPrefix) {
			t.Errorf("lease owner %q does not carry the configured worker prefix", owner)
		}
		if seen[owner] {
			t.Fatalf("lease owner %q reused across acquisitions", owner)
		}
		seen[owner] = true
	}
}

// TestDeliveryDiagnostic_NeverCarriesTheMessageBody is the distinctive-token
// guard on the one column the store's secret-body rule singles out. The errors
// here reproduce the shapes the REAL delivery path produces: tmux's submit
// verifier formats the whole payload back with %q when a paste never left the
// composer, so a diagnostic built from the raw transport error would write the
// broadcast body straight into last_error.
func TestDeliveryDiagnostic_NeverCarriesTheMessageBody(t *testing.T) {
	const token = "SECRET-BODY-TOKEN"
	multiLineBody := "deploy " + token + "\nsecond line"

	cases := []struct {
		name string
		body string
		err  error
	}{
		{
			name: "raw single-line body interpolated",
			body: token,
			err:  fmt.Errorf("send message: could not deliver %s to the pane", token),
		},
		{
			name: "quoted payload from the tmux submit verifier",
			body: token,
			err: fmt.Errorf("send message: payload not submitted to tmux session %q after an Enter retry: "+
				"command was not submitted; %q is still present at the tmux prompt",
				"chat-sess", "Broadcast received.\n\n"+token+"\n"),
		},
		{
			// %q escapes a multi-line body's newlines, so a raw-substring scrub
			// alone would miss it.
			name: "quoted multi-line body",
			body: multiLineBody,
			err:  fmt.Errorf("command was not submitted; %q is still present at the tmux prompt", multiLineBody),
		},
		{
			name: "body wrapped through several layers",
			body: multiLineBody,
			err: fmt.Errorf("schedule: %w",
				fmt.Errorf("send message: %q", "prompt preamble\n"+multiLineBody)),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := deliveryDiagnostic(tc.err, tc.body)
			if strings.Contains(got, token) {
				t.Fatalf("diagnostic leaked the message body: %q", got)
			}
			if !strings.Contains(got, redactedBodyMarker) {
				t.Errorf("diagnostic did not mark the redaction: %q", got)
			}
			if got == "" {
				t.Error("diagnostic is empty; the transport context was lost entirely")
			}
		})
	}
}

// TestDeliveryDiagnostic_KeepsTransportContextAndBounds verifies the scrub does
// not destroy the diagnostic value of a body-free error, and that one failure
// cannot write an unbounded string into last_error.
func TestDeliveryDiagnostic_KeepsTransportContextAndBounds(t *testing.T) {
	if got := deliveryDiagnostic(errors.New("chat offline"), "some body"); got != "chat offline" {
		t.Errorf("diagnostic = %q, want the transport error verbatim", got)
	}
	if got := deliveryDiagnostic(nil, "some body"); got != "delivery failed" {
		t.Errorf("nil-cause diagnostic = %q, want a non-empty placeholder", got)
	}
	long := errors.New(strings.Repeat("x", maxDiagnosticLen*3))
	got := deliveryDiagnostic(long, "some body")
	if len(got) > maxDiagnosticLen+len("…") {
		t.Errorf("diagnostic length = %d, want it bounded to %d", len(got), maxDiagnosticLen)
	}
}

// TestWorker_RetryDiagnosticIsScrubbedEndToEnd drives the redaction through the
// worker itself: a deliverer failing exactly the way the real tmux submit
// verifier does must not put the body into the persisted last_error.
func TestWorker_RetryDiagnosticIsScrubbedEndToEnd(t *testing.T) {
	const token = "SECRET-BODY-TOKEN"
	store := newFakeStore()
	base := time.Now().UTC()
	clk := newClock(base)
	store.addBroadcast(token, base.Add(24*time.Hour), "chat-1")

	// The deliverer echoes back the prompt it was handed, as tmux does.
	echoing := DelivererFunc(func(_ context.Context, _, message string) error {
		return fmt.Errorf("send message: command was not submitted; %q is still present at the tmux prompt", message)
	})
	w := newWorker(store, echoing, clk.now)
	w.scan(context.Background())

	d := store.delivery(t, "b-1-d1")
	if d.LastError == nil {
		t.Fatal("last_error not recorded")
	}
	if strings.Contains(*d.LastError, token) {
		t.Fatalf("last_error leaked the message body: %q", *d.LastError)
	}
}

// TestWorker_CompletesASettledBroadcastItDidNotDeliver covers the restart /
// lost-write hole: every delivery is already terminal, so no delivery path runs,
// yet the broadcast must still reach completed rather than sit in resolved until
// it expires.
func TestWorker_CompletesASettledBroadcastItDidNotDeliver(t *testing.T) {
	store := newFakeStore()
	clk := newClock(time.Now().UTC())
	store.addBroadcast("m", clk.now().Add(time.Hour), "chat-1")
	// Simulate the previous process: the delivery committed, the completion write
	// never landed.
	store.deliveries["b-1-d1"].State = models.BroadcastDeliveryStateDelivered

	deliverer := newCaptureDeliverer(nil)
	w := newWorker(store, deliverer, clk.now)
	w.scan(context.Background())

	if deliverer.count() != 0 {
		t.Errorf("deliverer called %d times, want 0 (nothing outstanding)", deliverer.count())
	}
	if got := store.broadcastState(t); got != models.BroadcastStateCompleted {
		t.Errorf("broadcast state = %q, want completed", got)
	}
}

// TestWorker_BackoffIsPreFilteredBeforeLeasing verifies a delivery whose retry
// is not yet due is skipped without touching the store's lease path at all.
func TestWorker_BackoffIsPreFilteredBeforeLeasing(t *testing.T) {
	store := newFakeStore()
	base := time.Now().UTC()
	clk := newClock(base)
	store.addBroadcast("m", base.Add(time.Hour), "chat-1")

	failing := newCaptureDeliverer(errors.New("chat offline"))
	w := newWorker(store, failing, clk.now)
	w.scan(context.Background()) // first attempt fails, schedules a 30s backoff

	before := len(store.owners())
	w.scan(context.Background()) // not yet due
	if after := len(store.owners()); after != before {
		t.Errorf("AcquireLease called %d extra times during backoff, want 0", after-before)
	}

	clk.advance(baseRetryBackoff)
	w.scan(context.Background())
	if after := len(store.owners()); after != before+1 {
		t.Errorf("AcquireLease called %d times after the backoff elapsed, want 1", after-before)
	}
}

// TestWorker_DeliveryIsBoundedByTheLease verifies one wedged target cannot block
// the worker forever: the attempt is cancelled at the lease deadline and the
// delivery is rescheduled.
func TestWorker_DeliveryIsBoundedByTheLease(t *testing.T) {
	store := newFakeStore()
	clk := newClock(time.Now().UTC())
	store.addBroadcast("m", clk.now().Add(time.Hour), "chat-1")

	wedged := DelivererFunc(func(ctx context.Context, _, _ string) error {
		<-ctx.Done() // never returns on its own
		return ctx.Err()
	})
	w := NewDeliveryWorker(WorkerConfig{
		Store:     store,
		Deliverer: wedged,
		Now:       clk.now,
		Owner:     testOwnerPrefix,
		Logger:    zerolog.Nop(),
		LeaseFor:  20 * time.Millisecond,
	})

	done := make(chan struct{})
	go func() { defer close(done); w.scan(context.Background()) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("scan blocked on a wedged delivery instead of bounding it by the lease")
	}

	if d := store.delivery(t, "b-1-d1"); d.AttemptCount != 1 {
		t.Errorf("attempt_count = %d, want 1 (the timed-out attempt is rescheduled)", d.AttemptCount)
	}
}

// TestDeliveryDiagnostic_MarkerIsNotGarbledByOverlappingNeedles guards the
// redaction's own wording: replacing straight into the human-readable marker let
// a shorter needle match INSIDE the marker a longer one had just inserted.
func TestDeliveryDiagnostic_MarkerIsNotGarbledByOverlappingNeedles(t *testing.T) {
	// Trailing space so the raw and trimmed needles differ and both match.
	for _, body := range []string{"message ", "broadcast ", "redacted "} {
		t.Run(strings.TrimSpace(body), func(t *testing.T) {
			got := deliveryDiagnostic(fmt.Errorf("send message: %s failed", body), body)
			intact := strings.Count(got, redactedBodyMarker)
			if intact == 0 {
				t.Fatalf("diagnostic = %q, want the body redacted", got)
			}
			// Every marker opening must belong to an INTACT marker. The old
			// replace-into-the-marker form produced nested wreckage such as
			// "[broadcast [broadcast message redacted] redacted]", which this
			// equality catches.
			if opened := strings.Count(got, "[broadcast"); opened != intact {
				t.Errorf("diagnostic = %q has %d marker openings but %d intact markers", got, opened, intact)
			}
			if strings.Contains(got, redactionSentinel) {
				t.Errorf("diagnostic = %q, still carries the internal sentinel", got)
			}
		})
	}
}

// TestAttemptDeadline_LeavesRoomForTheRetryWrite verifies one attempt always
// times out strictly inside its lease, so the ScheduleRetry that follows still
// lands under the claim that produced it rather than at the instant another
// owner may recover the row.
func TestAttemptDeadline_LeavesRoomForTheRetryWrite(t *testing.T) {
	for _, lease := range []time.Duration{DefaultLeaseDuration, 30 * time.Second, time.Second, time.Millisecond} {
		got := attemptDeadline(lease)
		if got <= 0 {
			t.Errorf("attemptDeadline(%v) = %v, want positive", lease, got)
		}
		if got >= lease {
			t.Errorf("attemptDeadline(%v) = %v, want strictly less than the lease", lease, got)
		}
	}
}

// TestWorker_MarkDeliveredRealErrorIsNotDowngraded guards the distinction
// between a lost CAS and a store fault. A lost claim is the benign
// at-least-once race and must not be retried; a real fault leaves the row leased
// (it will be re-delivered once the lease dies) and must not be relabelled
// benign. Either way the delivery must never be rescheduled here — the message
// has already been sent.
func TestWorker_MarkDeliveredFailureClassification(t *testing.T) {
	for name, markErr := range map[string]error{
		"lost claim":  db.ErrBroadcastDeliveryLeaseConflict,
		"row gone":    sql.ErrNoRows,
		"store fault": errors.New("database is locked"),
	} {
		t.Run(name, func(t *testing.T) {
			store := newFakeStore()
			clk := newClock(time.Now().UTC())
			store.addBroadcast("m", clk.now().Add(time.Hour), "chat-1")
			store.markDeliveredErr = markErr

			deliverer := newCaptureDeliverer(nil)
			w := newWorker(store, deliverer, clk.now)
			w.scan(context.Background())

			if deliverer.count() != 1 {
				t.Fatalf("deliverer called %d times, want 1", deliverer.count())
			}
			d := store.delivery(t, "b-1-d1")
			if d.AttemptCount != 0 {
				t.Errorf("attempt_count = %d, want 0 (a delivered message must not be retried)", d.AttemptCount)
			}
			if d.NextAttemptAt != nil {
				t.Errorf("next_attempt_at = %v, want unset", d.NextAttemptAt)
			}
		})
	}
}

// TestIsBenignClaimLoss pins which store errors the worker is allowed to treat
// as "someone else owns this now" rather than as a fault to surface.
func TestIsBenignClaimLoss(t *testing.T) {
	if !isBenignClaimLoss(db.ErrBroadcastDeliveryLeaseConflict) {
		t.Error("a lease conflict must be benign")
	}
	if !isBenignClaimLoss(fmt.Errorf("wrapped: %w", sql.ErrNoRows)) {
		t.Error("a wrapped ErrNoRows must be benign")
	}
	if isBenignClaimLoss(errors.New("database is locked")) {
		t.Error("a store fault must NOT be classified as a benign claim loss")
	}
}

// --- Reconcile safety net (BOS-557) ----------------------------------------

// fakeReconciler counts ReconcileAll invocations and can be scripted to fail.
type fakeReconciler struct {
	calls atomic.Int32
	err   error
}

func (f *fakeReconciler) ReconcileAll(_ context.Context) error {
	f.calls.Add(1)
	return f.err
}

// TestWorker_ReconcilesPeriodically pins the standing-subscription safety net
// onto the worker tick, exactly as callback.DeliveryWorker does: every
// reconcileEveryTicks ticks the reconciler runs, catching a session that
// reached a trigger state while nothing was listening (daemon down, or the
// startup bulk orphan advance, which bypasses the transition hook).
func TestWorker_ReconcilesPeriodically(t *testing.T) {
	store := newFakeStore()
	clk := newClock(time.Now().UTC())
	rec := &fakeReconciler{}
	w := NewDeliveryWorker(WorkerConfig{
		Store:        store,
		Deliverer:    newCaptureDeliverer(nil),
		Reconciler:   rec,
		Now:          clk.now,
		Logger:       zerolog.Nop(),
		PollInterval: time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()

	deadline := time.After(5 * time.Second)
	for rec.calls.Load() == 0 {
		select {
		case <-deadline:
			t.Fatalf("reconciler never ran (needs %d ticks)", reconcileEveryTicks)
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

// TestWorker_ReconcileFailureDoesNotStopTheWorker: the sweep is best-effort, so
// a failing reconciler must not take delivery down with it.
func TestWorker_ReconcileFailureDoesNotStopTheWorker(t *testing.T) {
	store := newFakeStore()
	clk := newClock(time.Now().UTC())
	rec := &fakeReconciler{err: errors.New("sweep exploded")}
	w := NewDeliveryWorker(WorkerConfig{
		Store:        store,
		Deliverer:    newCaptureDeliverer(nil),
		Reconciler:   rec,
		Now:          clk.now,
		Logger:       zerolog.Nop(),
		PollInterval: time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()

	deadline := time.After(5 * time.Second)
	for rec.calls.Load() < 2 {
		select {
		case <-deadline:
			t.Fatalf("reconciler ran %d times; a failure must not stop the tick", rec.calls.Load())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

// TestWorker_NilReconcilerIsANoOp: only the daemon wires one.
func TestWorker_NilReconcilerIsANoOp(t *testing.T) {
	store := newFakeStore()
	clk := newClock(time.Now().UTC())
	w := NewDeliveryWorker(WorkerConfig{
		Store:        store,
		Deliverer:    newCaptureDeliverer(nil),
		Now:          clk.now,
		Logger:       zerolog.Nop(),
		PollInterval: time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}
