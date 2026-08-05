package callback

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/models"
	libtelemetry "github.com/recurser/bossalib/telemetry"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
)

type callbackTelemetryRecorder struct {
	events     []libtelemetry.Event
	properties []map[string]any
}

func (r *callbackTelemetryRecorder) Capture(_ context.Context, event libtelemetry.Event, _ string, props map[string]any) {
	r.events = append(r.events, event)
	r.properties = append(r.properties, props)
}
func (*callbackTelemetryRecorder) Identify(context.Context, string, map[string]any) {}
func (*callbackTelemetryRecorder) Alias(context.Context, string, string)            {}
func (*callbackTelemetryRecorder) Close()                                           {}
func enableCallbackTelemetry(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("BOSS_SETTINGS_PATH", filepath.Join(t.TempDir(), "settings.json"))
	s := config.DefaultSettings()
	s.EventTracingEnabled = true
	if err := config.Save(s); err != nil {
		t.Fatal(err)
	}
}

func assertCallbackTelemetryPropertiesRegistered(t *testing.T, recorder *callbackTelemetryRecorder) {
	t.Helper()
	spec := libtelemetry.Registry[libtelemetry.EventPRCallbackDelivered]
	for _, props := range recorder.properties {
		for key := range props {
			if _, common := libtelemetry.CommonProperties[key]; common {
				continue
			}
			if _, allowed := spec.Properties[key]; !allowed {
				t.Fatalf("unregistered identifying property %q in %#v", key, props)
			}
		}
	}
}

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

// triggerActive drives a freshly created callback into the triggered state via
// the store's atomic group transition (as the evaluator would).
func triggerActive(t *testing.T, store db.GithubCallbackStore, id string, now time.Time) {
	t.Helper()
	if _, err := store.TriggerGroup(context.Background(), id, "merged", now); err != nil {
		t.Fatalf("TriggerGroup %s: %v", id, err)
	}
}

func newWorker(store workerStore, deliverer ChatDeliverer, now func() time.Time, owner string) *DeliveryWorker {
	return NewDeliveryWorker(WorkerConfig{
		Store:     store,
		Deliverer: deliverer,
		Now:       now,
		Owner:     owner,
		Logger:    zerolog.Nop(),
	})
}

// TestWorker_HappyPath verifies lease -> deliver -> MarkDelivered, and that the
// deliverer received the rendered prompt (with the verbatim message).
func TestWorker_HappyPath(t *testing.T) {
	store := newStore(t)
	clk := newClock(time.Now())
	cb := mustCreate(t, store, db.CreateGithubCallbackParams{
		TargetChatID: "chat-1", RepoOwner: "acme", RepoName: "widgets", PRNumber: 7,
		Trigger: models.GithubCallbackTriggerMerged, Message: "ship it now",
	})
	triggerActive(t, store, cb.ID, clk.now())

	deliverer := newCaptureDeliverer(nil)
	w := newWorker(store, deliverer, clk.now, "worker-1")
	w.scan(context.Background())

	if got := getState(t, store, cb.ID); got != models.GithubCallbackStateDelivered {
		t.Fatalf("state = %q, want delivered", got)
	}
	if deliverer.count() != 1 {
		t.Fatalf("deliverer called %d times, want 1", deliverer.count())
	}
	msg := deliverer.lastMessage()
	if !strings.Contains(msg, "ship it now") {
		t.Errorf("delivered message missing verbatim body:\n%s", msg)
	}
	if !strings.Contains(msg, cb.ID) {
		t.Errorf("delivered message missing callback id %q", cb.ID)
	}
}

func TestWorker_TelemetryTerminalOutcomesOnly(t *testing.T) {
	enableCallbackTelemetry(t)
	store := newStore(t)
	clk := newClock(time.Now().UTC())
	cb := mustCreate(t, store, db.CreateGithubCallbackParams{TargetChatID: "chat-1", RepoOwner: "acme", RepoName: "widgets", PRNumber: 7, Trigger: models.GithubCallbackTriggerMerged, Message: "m"})
	triggerActive(t, store, cb.ID, clk.now())
	recorder := &callbackTelemetryRecorder{}
	w := NewDeliveryWorker(WorkerConfig{Store: store, Deliverer: newCaptureDeliverer(errors.New("offline")), Now: clk.now, Owner: "worker-1", Logger: zerolog.Nop(), Telemetry: recorder})
	w.scan(context.Background())
	if len(recorder.events) != 0 {
		t.Fatalf("retry captured %d events", len(recorder.events))
	}
	for len(recorder.events) == 0 {
		got, err := store.Get(context.Background(), cb.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.NextAttemptAt == nil {
			t.Fatal("retry missing next attempt")
		}
		clk.advance(got.NextAttemptAt.Sub(clk.now()))
		w.scan(context.Background())
	}
	if len(recorder.events) != 1 || recorder.properties[0]["status"] != "abandoned" || recorder.properties[0]["attempt_count"] != maxDeliveryAttempts {
		t.Fatalf("captures = %#v", recorder.properties)
	}
	assertCallbackTelemetryPropertiesRegistered(t, recorder)
	w.scan(context.Background())
	if len(recorder.events) != 1 {
		t.Fatalf("terminal outcome recaptured: %d", len(recorder.events))
	}

	store2 := newStore(t)
	cb2 := mustCreate(t, store2, db.CreateGithubCallbackParams{TargetChatID: "chat-2", RepoOwner: "acme", RepoName: "widgets", PRNumber: 8, Trigger: models.GithubCallbackTriggerClosed, Message: "m"})
	triggerActive(t, store2, cb2.ID, clk.now())
	recorder2 := &callbackTelemetryRecorder{}
	NewDeliveryWorker(WorkerConfig{Store: store2, Deliverer: newCaptureDeliverer(nil), Now: clk.now, Owner: "worker-2", Logger: zerolog.Nop(), Telemetry: recorder2}).scan(context.Background())
	if len(recorder2.events) != 1 || recorder2.properties[0]["status"] != "delivered" || recorder2.properties[0]["attempt_count"] != 1 {
		t.Fatalf("captures = %#v", recorder2.properties)
	}
	assertCallbackTelemetryPropertiesRegistered(t, recorder2)
	NewDeliveryWorker(WorkerConfig{Store: store2, Deliverer: newCaptureDeliverer(nil), Now: clk.now, Owner: "worker-3", Logger: zerolog.Nop()}).scan(context.Background())
}

func TestWorker_TelemetryNaturalExpiryEmitsAbandonedOnce(t *testing.T) {
	enableCallbackTelemetry(t)
	store := newStore(t)
	base := time.Now().UTC()
	expires := base.Add(time.Minute)
	cb := mustCreate(t, store, db.CreateGithubCallbackParams{
		TargetChatID: "chat-1", RepoOwner: "acme", RepoName: "widgets", PRNumber: 7,
		Trigger: models.GithubCallbackTriggerMerged, Message: "m", ExpiresAt: &expires,
	})
	recorder := &callbackTelemetryRecorder{}
	w := NewDeliveryWorker(WorkerConfig{
		Store: store, Deliverer: newCaptureDeliverer(nil), Now: func() time.Time { return expires.Add(time.Second) },
		Owner: "worker-1", Logger: zerolog.Nop(), Telemetry: recorder,
	})

	w.scan(context.Background())
	w.scan(context.Background())

	if got := getState(t, store, cb.ID); got != models.GithubCallbackStateExpired {
		t.Fatalf("state = %q, want expired", got)
	}
	if len(recorder.events) != 1 || recorder.events[0] != libtelemetry.EventPRCallbackDelivered {
		t.Fatalf("events = %#v, want one pr_callback_delivered", recorder.events)
	}
	want := map[string]any{"trigger": "merged", "status": "abandoned", "attempt_count": 0}
	for key, value := range want {
		if recorder.properties[0][key] != value {
			t.Errorf("property %s = %#v, want %#v", key, recorder.properties[0][key], value)
		}
	}
	assertCallbackTelemetryPropertiesRegistered(t, recorder)
}

// TestWorker_RetryOnDeliveryFailure verifies a failed delivery increments the
// attempt count, keeps the callback triggered, and schedules the next attempt at
// now + baseRetryBackoff.
func TestWorker_RetryOnDeliveryFailure(t *testing.T) {
	store := newStore(t)
	base := time.Now().UTC()
	clk := newClock(base)
	cb := mustCreate(t, store, db.CreateGithubCallbackParams{
		TargetChatID: "chat-1", RepoOwner: "acme", RepoName: "widgets", PRNumber: 7,
		Trigger: models.GithubCallbackTriggerMerged, Message: "m",
	})
	triggerActive(t, store, cb.ID, clk.now())

	failing := newCaptureDeliverer(errors.New("chat offline"))
	w := newWorker(store, failing, clk.now, "worker-1")
	w.scan(context.Background())

	got, err := store.Get(context.Background(), cb.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != models.GithubCallbackStateTriggered {
		t.Errorf("state = %q, want triggered (retryable)", got.State)
	}
	if got.AttemptCount != 1 {
		t.Errorf("attempt_count = %d, want 1", got.AttemptCount)
	}
	if got.NextAttemptAt == nil {
		t.Fatal("next_attempt_at not set")
	}
	wantNext := base.Add(baseRetryBackoff)
	if got.NextAttemptAt.Sub(wantNext).Abs() > time.Second {
		t.Errorf("next_attempt_at = %v, want ~%v", got.NextAttemptAt, wantNext)
	}
	if got.LastError == nil || *got.LastError != "chat offline" {
		t.Errorf("last_error = %v, want normal delivery error", got.LastError)
	}
	// Lease was released, so the row is not held by the failed worker.
	if got.LeaseOwner != nil {
		t.Errorf("lease still held by %v, want released", *got.LeaseOwner)
	}
}

func TestWorker_RetryDeliveryUsesRepeatPrompt(t *testing.T) {
	store := newStore(t)
	base := time.Now().UTC()
	clk := newClock(base)
	cb := mustCreate(t, store, db.CreateGithubCallbackParams{
		TargetChatID: "chat-1", RepoOwner: "acme", RepoName: "widgets", PRNumber: 7,
		Trigger: models.GithubCallbackTriggerMerged, Message: "m",
	})
	triggerActive(t, store, cb.ID, clk.now())

	var messages []string
	deliverer := DelivererFunc(func(_ context.Context, _ string, message string) error {
		messages = append(messages, message)
		return errors.New("chat offline")
	})
	w := newWorker(store, deliverer, clk.now, "worker-1")
	w.scan(context.Background())
	clk.advance(baseRetryBackoff)
	w.scan(context.Background())

	if len(messages) != 2 {
		t.Fatalf("delivery attempts = %d, want 2", len(messages))
	}
	if !strings.Contains(messages[1], "REPEAT DELIVERY — attempt 2 for callback id "+cb.ID+"; an already-actioned callback needs no further action.") {
		t.Errorf("second delivery missing repeat banner:\n%s", messages[1])
	}
}

func TestWorker_ExhaustedAttemptsPinToExpiryAndExpire(t *testing.T) {
	store := newStore(t)
	base := time.Now().UTC()
	clk := newClock(base)
	expires := base.Add(24 * time.Hour)
	cb := mustCreate(t, store, db.CreateGithubCallbackParams{
		TargetChatID: "chat-1", RepoOwner: "acme", RepoName: "widgets", PRNumber: 7,
		Trigger: models.GithubCallbackTriggerMerged, Message: "m", ExpiresAt: &expires,
	})
	triggerActive(t, store, cb.ID, clk.now())

	attempts := 0
	w := newWorker(store, DelivererFunc(func(_ context.Context, _ string, _ string) error {
		attempts++
		return errors.New("chat offline")
	}), clk.now, "worker-1")
	for range maxDeliveryAttempts {
		w.scan(context.Background())
		got, err := store.Get(context.Background(), cb.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.NextAttemptAt == nil {
			t.Fatal("next_attempt_at not set")
		}
		clk.advance(got.NextAttemptAt.Sub(clk.now()))
	}

	got, err := store.Get(context.Background(), cb.ID)
	if err != nil {
		t.Fatalf("get exhausted callback: %v", err)
	}
	if attempts != maxDeliveryAttempts {
		t.Fatalf("delivery attempts = %d, want %d", attempts, maxDeliveryAttempts)
	}
	if got.NextAttemptAt == nil || got.NextAttemptAt.Sub(expires).Abs() > time.Second {
		t.Errorf("next_attempt_at = %v, want pinned to expiry %v", got.NextAttemptAt, expires)
	}
	if got.LastError == nil || !strings.Contains(*got.LastError, "attempt exhaustion") {
		t.Errorf("last_error = %v, want attempt exhaustion diagnostic", got.LastError)
	}

	// The pin keeps the exhausted row out of delivery contention until the lazy
	// expiry sweep turns it terminal.
	w.scan(context.Background())
	if attempts != maxDeliveryAttempts {
		t.Errorf("delivery attempts after exhaustion = %d, want %d", attempts, maxDeliveryAttempts)
	}
	clk.advance(time.Second)
	w.scan(context.Background())
	if got := getState(t, store, cb.ID); got != models.GithubCallbackStateExpired {
		t.Errorf("state = %q, want expired", got)
	}
	if attempts != maxDeliveryAttempts {
		t.Errorf("delivery attempts after expiry = %d, want %d", attempts, maxDeliveryAttempts)
	}
}

func TestWorker_PreviouslyExhaustedCallbackIsNotDeliveredAgain(t *testing.T) {
	store := newStore(t)
	base := time.Now().UTC()
	clk := newClock(base)
	expires := base.Add(24 * time.Hour)
	cb := mustCreate(t, store, db.CreateGithubCallbackParams{
		TargetChatID: "chat-1", RepoOwner: "acme", RepoName: "widgets", PRNumber: 7,
		Trigger: models.GithubCallbackTriggerMerged, Message: "m", ExpiresAt: &expires,
	})
	triggerActive(t, store, cb.ID, clk.now())

	for range maxDeliveryAttempts {
		if _, err := store.AcquireLease(context.Background(), cb.ID, "old-worker", clk.now(), DefaultLeaseDuration); err != nil {
			t.Fatalf("acquire old attempt: %v", err)
		}
		if err := store.ScheduleRetry(context.Background(), cb.ID, "old-worker", db.ScheduleGithubCallbackRetryParams{
			NextAttemptAt: clk.now(), LastError: "old delivery failure", LastEvent: string(cb.Trigger),
		}); err != nil {
			t.Fatalf("schedule old attempt: %v", err)
		}
	}

	attempts := 0
	w := newWorker(store, DelivererFunc(func(_ context.Context, _ string, _ string) error {
		attempts++
		return nil
	}), clk.now, "worker-1")
	w.scan(context.Background())

	got, err := store.Get(context.Background(), cb.ID)
	if err != nil {
		t.Fatalf("get callback: %v", err)
	}
	if attempts != 0 {
		t.Errorf("delivery attempts = %d, want 0", attempts)
	}
	if got.AttemptCount != maxDeliveryAttempts {
		t.Errorf("attempt_count = %d, want %d", got.AttemptCount, maxDeliveryAttempts)
	}
	if got.NextAttemptAt == nil || got.NextAttemptAt.Sub(expires).Abs() > time.Second {
		t.Errorf("next_attempt_at = %v, want expiry %v", got.NextAttemptAt, expires)
	}
	if got.LastError == nil || !strings.Contains(*got.LastError, "attempt exhaustion after 5 attempts") {
		t.Errorf("last_error = %v, want five-attempt exhaustion diagnostic", got.LastError)
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

// TestWorker_PastExpiryLetsExpire verifies that when the next retry would land at
// or after expiry, the callback is left to expire rather than retried forever.
func TestWorker_PastExpiryLetsExpire(t *testing.T) {
	store := newStore(t)
	base := time.Now().UTC()
	clk := newClock(base)
	expires := base.Add(10 * time.Second) // sooner than baseRetryBackoff (30s)
	cb := mustCreate(t, store, db.CreateGithubCallbackParams{
		TargetChatID: "chat-1", RepoOwner: "acme", RepoName: "widgets", PRNumber: 7,
		Trigger: models.GithubCallbackTriggerMerged, Message: "m", ExpiresAt: &expires,
	})
	triggerActive(t, store, cb.ID, clk.now())

	failing := newCaptureDeliverer(errors.New("chat offline"))
	w := newWorker(store, failing, clk.now, "worker-1")

	// First scan: delivery fails; retry would exceed expiry so it's pinned to
	// ExpiresAt (kept out of contention).
	w.scan(context.Background())
	got, err := store.Get(context.Background(), cb.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// Pinned to ExpiresAt (millisecond storage precision), well short of the
	// unpinned base+30s backoff.
	if got.NextAttemptAt == nil || got.NextAttemptAt.Before(expires.Add(-time.Second)) {
		t.Errorf("next_attempt_at = %v, want pinned to ~expiry %v", got.NextAttemptAt, expires)
	}
	if got.NextAttemptAt != nil && got.NextAttemptAt.After(base.Add(baseRetryBackoff)) {
		t.Errorf("next_attempt_at = %v, want <= unpinned backoff %v", got.NextAttemptAt, base.Add(baseRetryBackoff))
	}

	// Advance past expiry and scan again: the lazy sweep reaps it.
	clk.advance(20 * time.Second)
	w.scan(context.Background())
	if st := getState(t, store, cb.ID); st != models.GithubCallbackStateExpired {
		t.Errorf("state = %q, want expired", st)
	}
	if failing.count() != 0 {
		t.Errorf("deliverer recorded %d successful deliveries, want 0", failing.count())
	}
}

// TestWorker_LeaseContention runs two workers concurrently against a file-backed
// store and asserts exactly one delivery (no double-delivery).
func TestWorker_LeaseContention(t *testing.T) {
	store := newFileStore(t)
	clk := newClock(time.Now())
	cb := mustCreate(t, store, db.CreateGithubCallbackParams{
		TargetChatID: "chat-1", RepoOwner: "acme", RepoName: "widgets", PRNumber: 7,
		Trigger: models.GithubCallbackTriggerMerged, Message: "once",
	})
	triggerActive(t, store, cb.ID, clk.now())

	deliverer := newCaptureDeliverer(nil) // shared, concurrency-safe
	wA := newWorker(store, deliverer, clk.now, "worker-A")
	wB := newWorker(store, deliverer, clk.now, "worker-B")

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); wA.deliverOne(context.Background(), cb.ID) }()
	go func() { defer wg.Done(); wB.deliverOne(context.Background(), cb.ID) }()
	wg.Wait()

	if deliverer.count() != 1 {
		t.Errorf("delivered %d times, want exactly 1", deliverer.count())
	}
	if got := getState(t, store, cb.ID); got != models.GithubCallbackStateDelivered {
		t.Errorf("state = %q, want delivered", got)
	}
}

// TestWorker_RecoversExpiredLease verifies restart recovery: a lease held by a
// dead worker is re-acquired by another worker once its deadline passes, and the
// callback is delivered.
func TestWorker_RecoversExpiredLease(t *testing.T) {
	store := newStore(t)
	base := time.Now().UTC()
	clk := newClock(base)
	cb := mustCreate(t, store, db.CreateGithubCallbackParams{
		TargetChatID: "chat-1", RepoOwner: "acme", RepoName: "widgets", PRNumber: 7,
		Trigger: models.GithubCallbackTriggerMerged, Message: "recover me",
	})
	triggerActive(t, store, cb.ID, clk.now())

	// Worker A leases the callback then "dies" without delivering.
	if _, err := store.AcquireLease(context.Background(), cb.ID, "worker-A", clk.now(), DefaultLeaseDuration); err != nil {
		t.Fatalf("A acquire lease: %v", err)
	}

	// Worker B, after the lease deadline passes, recovers and delivers.
	clk.advance(DefaultLeaseDuration + time.Minute)
	deliverer := newCaptureDeliverer(nil)
	wB := newWorker(store, deliverer, clk.now, "worker-B")
	wB.scan(context.Background())

	if got := getState(t, store, cb.ID); got != models.GithubCallbackStateDelivered {
		t.Errorf("state = %q, want delivered after recovery", got)
	}
	if deliverer.count() != 1 {
		t.Errorf("deliverer called %d times, want 1", deliverer.count())
	}
}

// TestWorker_ConcurrentEvaluateAndDeliver exercises an evaluator and a delivery
// worker against one shared file-backed store to surface races under -race.
func TestWorker_ConcurrentEvaluateAndDeliver(t *testing.T) {
	store := newFileStore(t)
	clk := newClock(time.Now())

	const n = 12
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		cb := mustCreate(t, store, db.CreateGithubCallbackParams{
			TargetChatID: "chat-1", RepoOwner: "acme", RepoName: "widgets", PRNumber: i + 1,
			Trigger: models.GithubCallbackTriggerMerged, Message: "m",
		})
		ids = append(ids, cb.ID)
	}

	prov := &fakeProvider{status: prStatus(vcs.PRStateMerged)}
	ev := NewEvaluator(store, prov, clk.now, zerolog.Nop())
	deliverer := newCaptureDeliverer(nil)
	w := newWorker(store, deliverer, clk.now, "worker-1")

	var wg sync.WaitGroup
	// Evaluator fires each PR (concurrently), advancing active -> triggered.
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(pr int) {
			defer wg.Done()
			_ = ev.EvaluatePR(context.Background(), "acme", "widgets", pr)
		}(i + 1)
	}
	// Worker scans repeatedly, delivering triggered callbacks.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			w.scan(context.Background())
		}
	}()
	wg.Wait()
	// Final drain to deliver any triggered after the last evaluate.
	w.scan(context.Background())

	if deliverer.count() != n {
		t.Errorf("delivered %d, want %d", deliverer.count(), n)
	}
	for _, id := range ids {
		if got := getState(t, store, id); got != models.GithubCallbackStateDelivered {
			t.Errorf("callback %s state = %q, want delivered", id, got)
		}
	}
}
