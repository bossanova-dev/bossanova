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

// broadcastSubscriptionSecretToken is planted in a subscription message body so
// tests can grep every other column, and every error string, for it. The body is
// a secret at rest: it must never reach a diagnostic surface.
const broadcastSubscriptionSecretToken = "SUPER-SECRET-SUB-BODY-Kw4T"

func newTestSubscription() *models.BroadcastSubscription {
	origin := "chat-origin"
	return &models.BroadcastSubscription{
		OwnerSessionID: "session-1",
		OriginChatID:   &origin,
		TriggerEvent:   models.BroadcastTriggerCompleted,
		Selector:       "chat:*",
		Message:        "child finished, " + broadcastSubscriptionSecretToken,
		ExpiresAt:      time.Now().UTC().Add(time.Hour),
	}
}

func TestBroadcastSubscriptionStore_CreateGetRoundTrip(t *testing.T) {
	store := NewBroadcastSubscriptionStore(setupTestDB(t))
	ctx := context.Background()

	sub := newTestSubscription()
	if err := store.Create(ctx, sub); err != nil {
		t.Fatalf("create: %v", err)
	}
	if sub.ID == "" {
		t.Fatal("Create must stamp a generated id onto the subscription")
	}
	if sub.State != models.BroadcastSubscriptionStateActive {
		t.Errorf("state = %q, want active", sub.State)
	}

	got, err := store.Get(ctx, sub.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != sub.ID {
		t.Errorf("id = %q, want %q", got.ID, sub.ID)
	}
	if got.OwnerSessionID != "session-1" {
		t.Errorf("owner_session_id = %q, want session-1", got.OwnerSessionID)
	}
	if got.OriginChatID == nil || *got.OriginChatID != "chat-origin" {
		t.Errorf("origin chat = %v, want chat-origin", got.OriginChatID)
	}
	if got.TriggerEvent != models.BroadcastTriggerCompleted {
		t.Errorf("trigger_event = %q, want completed", got.TriggerEvent)
	}
	if got.Selector != sub.Selector || got.Message != sub.Message {
		t.Errorf("round trip = %+v, want selector/message preserved", got)
	}
	if got.State != models.BroadcastSubscriptionStateActive {
		t.Errorf("state = %q, want active", got.State)
	}
	if got.FiredAt != nil || got.FiredBroadcastID != nil {
		t.Errorf("fired columns = %v/%v, want both nil before firing", got.FiredAt, got.FiredBroadcastID)
	}
	if d := got.ExpiresAt.Sub(sub.ExpiresAt); d > time.Millisecond || d < -time.Millisecond {
		t.Errorf("expires_at = %v, want %v", got.ExpiresAt, sub.ExpiresAt)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Errorf("timestamps = %v/%v, want both set", got.CreatedAt, got.UpdatedAt)
	}
}

func TestBroadcastSubscriptionStore_CreateRejectsInvalidInput(t *testing.T) {
	store := NewBroadcastSubscriptionStore(setupTestDB(t))
	ctx := context.Background()

	cases := []struct {
		name string
		mut  func(s *models.BroadcastSubscription)
	}{
		{"empty owner session", func(s *models.BroadcastSubscription) { s.OwnerSessionID = "" }},
		{"empty selector", func(s *models.BroadcastSubscription) { s.Selector = "" }},
		{"empty message", func(s *models.BroadcastSubscription) { s.Message = "" }},
		{"zero expiry", func(s *models.BroadcastSubscription) { s.ExpiresAt = time.Time{} }},
		{"unknown trigger", func(s *models.BroadcastSubscription) { s.TriggerEvent = models.BroadcastTrigger("exploded") }},
		{"unknown state", func(s *models.BroadcastSubscription) {
			s.State = models.BroadcastSubscriptionState("exploded")
		}},
		// Valid states, but not creatable: a subscription created already fired,
		// canceled or expired is a dead row on arrival — nothing would ever fire it.
		{"created fired", func(s *models.BroadcastSubscription) { s.State = models.BroadcastSubscriptionStateFired }},
		{"created canceled", func(s *models.BroadcastSubscription) { s.State = models.BroadcastSubscriptionStateCanceled }},
		{"created expired", func(s *models.BroadcastSubscription) { s.State = models.BroadcastSubscriptionStateExpired }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sub := newTestSubscription()
			tc.mut(sub)
			if err := store.Create(ctx, sub); err == nil {
				t.Fatal("err = nil, want a validation failure")
			}
		})
	}
	if err := store.Create(ctx, nil); err == nil {
		t.Error("create(nil) err = nil, want a failure")
	}
}

func TestBroadcastSubscriptionStore_GetNotFound(t *testing.T) {
	store := NewBroadcastSubscriptionStore(setupTestDB(t))
	if _, err := store.Get(context.Background(), "nope"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err = %v, want sql.ErrNoRows", err)
	}
}

// TestBroadcastSubscriptionStore_MarkFiredIsCAS is the acceptance test for the
// single guard against a double fire. The first call must win; the second must
// be a *reported* no-op — (false, nil), not an error — and must not overwrite
// the winning broadcast id.
func TestBroadcastSubscriptionStore_MarkFiredIsCAS(t *testing.T) {
	store := NewBroadcastSubscriptionStore(setupTestDB(t))
	ctx := context.Background()

	sub := newTestSubscription()
	if err := store.Create(ctx, sub); err != nil {
		t.Fatalf("create: %v", err)
	}

	now := time.Now().UTC()
	won, err := store.MarkFired(ctx, sub.ID, "broadcast-first", now)
	if err != nil {
		t.Fatalf("first mark fired: %v", err)
	}
	if !won {
		t.Fatal("first MarkFired won = false, want true")
	}

	after, err := store.Get(ctx, sub.ID)
	if err != nil {
		t.Fatalf("get after fire: %v", err)
	}
	if after.State != models.BroadcastSubscriptionStateFired {
		t.Errorf("state = %q, want fired", after.State)
	}
	if after.FiredBroadcastID == nil || *after.FiredBroadcastID != "broadcast-first" {
		t.Errorf("fired_broadcast_id = %v, want broadcast-first", after.FiredBroadcastID)
	}
	if after.FiredAt == nil {
		t.Error("fired_at = nil, want the firing timestamp")
	}

	// A second attempt is the double-fire the CAS exists to stop.
	won, err = store.MarkFired(ctx, sub.ID, "broadcast-second", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("second mark fired err = %v, want a reported no-op with nil error", err)
	}
	if won {
		t.Error("second MarkFired won = true, want false")
	}

	stable, err := store.Get(ctx, sub.ID)
	if err != nil {
		t.Fatalf("get after second fire: %v", err)
	}
	if stable.FiredBroadcastID == nil || *stable.FiredBroadcastID != "broadcast-first" {
		t.Errorf("fired_broadcast_id = %v after losing CAS, want broadcast-first unchanged", stable.FiredBroadcastID)
	}
	if !stable.FiredAt.Equal(*after.FiredAt) {
		t.Errorf("fired_at = %v after losing CAS, want %v unchanged", stable.FiredAt, after.FiredAt)
	}
}

// TestBroadcastSubscriptionStore_MarkFiredMissing reports an absent row rather
// than silently claiming a lost CAS, so a caller can tell "someone else fired
// it" from "this id does not exist".
func TestBroadcastSubscriptionStore_MarkFiredMissing(t *testing.T) {
	store := NewBroadcastSubscriptionStore(setupTestDB(t))
	if _, err := store.MarkFired(context.Background(), "nope", "b-1", time.Now().UTC()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err = %v, want sql.ErrNoRows", err)
	}
}

// TestBroadcastSubscriptionFiresOnceUnderConcurrency runs real concurrent
// MarkFired calls against a file-backed DB (the in-memory helper is
// single-connection, so it cannot express the race). Exactly one caller may win.
// Run with -race -count=2.
func TestBroadcastSubscriptionFiresOnceUnderConcurrency(t *testing.T) {
	store := NewBroadcastSubscriptionStore(setupFileDB(t))
	ctx := context.Background()

	sub := newTestSubscription()
	if err := store.Create(ctx, sub); err != nil {
		t.Fatalf("create: %v", err)
	}

	const callers = 8
	now := time.Now().UTC()
	var wg sync.WaitGroup
	wins := make(chan string, callers)
	for i := range callers {
		wg.Add(1)
		broadcastID := "broadcast-" + string(rune('a'+i))
		go func() {
			defer wg.Done()
			won, err := store.MarkFired(ctx, sub.ID, broadcastID, now)
			if err != nil {
				t.Errorf("unexpected MarkFired err: %v", err)
				return
			}
			if won {
				wins <- broadcastID
			}
		}()
	}
	wg.Wait()
	close(wins)

	if got := len(wins); got != 1 {
		t.Fatalf("MarkFired winners = %d, want exactly 1", got)
	}
	winner := <-wins

	after, err := store.Get(ctx, sub.ID)
	if err != nil {
		t.Fatalf("get after race: %v", err)
	}
	if after.State != models.BroadcastSubscriptionStateFired {
		t.Errorf("state = %q, want fired", after.State)
	}
	if after.FiredBroadcastID == nil || *after.FiredBroadcastID != winner {
		t.Errorf("fired_broadcast_id = %v, want the winner %q", after.FiredBroadcastID, winner)
	}
}

// TestBroadcastSubscriptionStore_CancelIsCAS pins the other CAS: active ->
// canceled once, and a repeat is a reported no-op so a retried cancel RPC is
// safe.
func TestBroadcastSubscriptionStore_CancelIsCAS(t *testing.T) {
	store := NewBroadcastSubscriptionStore(setupTestDB(t))
	ctx := context.Background()

	sub := newTestSubscription()
	if err := store.Create(ctx, sub); err != nil {
		t.Fatalf("create: %v", err)
	}

	won, err := store.Cancel(ctx, sub.ID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if !won {
		t.Fatal("first Cancel won = false, want true")
	}
	got, err := store.Get(ctx, sub.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != models.BroadcastSubscriptionStateCanceled {
		t.Errorf("state = %q, want canceled", got.State)
	}

	won, err = store.Cancel(ctx, sub.ID)
	if err != nil {
		t.Fatalf("second cancel err = %v, want a reported no-op", err)
	}
	if won {
		t.Error("second Cancel won = true, want false")
	}

	// A fired subscription cannot be retro-canceled: the broadcast already went
	// out, so flipping the row would misreport history.
	fired := newTestSubscription()
	if err := store.Create(ctx, fired); err != nil {
		t.Fatalf("create fired: %v", err)
	}
	if _, err := store.MarkFired(ctx, fired.ID, "b-1", time.Now().UTC()); err != nil {
		t.Fatalf("mark fired: %v", err)
	}
	if won, err := store.Cancel(ctx, fired.ID); err != nil || won {
		t.Errorf("cancel of a fired subscription = (%v, %v), want (false, nil)", won, err)
	}
}

// TestBroadcastSubscriptionStore_ExpireOverdue retires overdue active rows,
// leaves fresh ones alone, and never touches a row that already fired — a
// subscription's firing history must survive the sweep.
func TestBroadcastSubscriptionStore_ExpireOverdue(t *testing.T) {
	store := NewBroadcastSubscriptionStore(setupTestDB(t))
	ctx := context.Background()
	base := time.Now().UTC()

	mk := func(expiry time.Duration) *models.BroadcastSubscription {
		t.Helper()
		sub := newTestSubscription()
		sub.ExpiresAt = base.Add(expiry)
		if err := store.Create(ctx, sub); err != nil {
			t.Fatalf("create: %v", err)
		}
		return sub
	}
	overdue := mk(-time.Minute)
	fresh := mk(time.Hour)
	alreadyFired := mk(-time.Minute)
	if _, err := store.MarkFired(ctx, alreadyFired.ID, "b-1", base.Add(-2*time.Minute)); err != nil {
		t.Fatalf("mark fired: %v", err)
	}

	n, err := store.ExpireOverdue(ctx, base)
	if err != nil {
		t.Fatalf("expire overdue: %v", err)
	}
	if n != 1 {
		t.Errorf("expired = %d, want 1 (only the overdue active row)", n)
	}

	assertState := func(id string, want models.BroadcastSubscriptionState) {
		t.Helper()
		got, err := store.Get(ctx, id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if got.State != want {
			t.Errorf("state of %s = %q, want %q", id, got.State, want)
		}
	}
	assertState(overdue.ID, models.BroadcastSubscriptionStateExpired)
	assertState(fresh.ID, models.BroadcastSubscriptionStateActive)
	assertState(alreadyFired.ID, models.BroadcastSubscriptionStateFired)

	// The sweep is idempotent: a second pass finds nothing left to retire.
	again, err := store.ExpireOverdue(ctx, base)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if again != 0 {
		t.Errorf("second sweep expired = %d, want 0", again)
	}
}

// TestBroadcastSubscriptionStore_ListActiveForSession is the evaluator's hot
// path: it must see only live registrations for this session. Expired-by-clock
// rows are excluded even before the sweep has retired them, because the sweep is
// lazy and a stale row must never fire.
func TestBroadcastSubscriptionStore_ListActiveForSession(t *testing.T) {
	store := NewBroadcastSubscriptionStore(setupTestDB(t))
	ctx := context.Background()
	base := time.Now().UTC()

	mk := func(session string, expiry time.Duration) *models.BroadcastSubscription {
		t.Helper()
		sub := newTestSubscription()
		sub.OwnerSessionID = session
		sub.ExpiresAt = base.Add(expiry)
		if err := store.Create(ctx, sub); err != nil {
			t.Fatalf("create: %v", err)
		}
		return sub
	}
	live := mk("session-1", time.Hour)
	pastExpiry := mk("session-1", -time.Minute)
	canceled := mk("session-1", time.Hour)
	fired := mk("session-1", time.Hour)
	otherSession := mk("session-2", time.Hour)

	if _, err := store.Cancel(ctx, canceled.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := store.MarkFired(ctx, fired.ID, "b-1", base); err != nil {
		t.Fatalf("mark fired: %v", err)
	}

	got, err := store.ListActiveForSession(ctx, "session-1")
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	if len(got) != 1 || got[0].ID != live.ID {
		ids := make([]string, 0, len(got))
		for _, s := range got {
			ids = append(ids, s.ID)
		}
		t.Fatalf("active for session-1 = %v, want only %s (excluded: expired %s, canceled %s, fired %s, other %s)",
			ids, live.ID, pastExpiry.ID, canceled.ID, fired.ID, otherSession.ID)
	}
	if got[0].Message != live.Message {
		t.Errorf("message = %q, want the stored body (the evaluator needs it)", got[0].Message)
	}

	// A session with no registrations is an empty result, not an error.
	none, err := store.ListActiveForSession(ctx, "session-unknown")
	if err != nil {
		t.Fatalf("list active for unknown session: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("active for unknown session = %d rows, want 0", len(none))
	}
}

func TestBroadcastSubscriptionStore_ListFiltersAndLimit(t *testing.T) {
	store := NewBroadcastSubscriptionStore(setupTestDB(t))
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour)

	mk := func(session, origin string, offset time.Duration) *models.BroadcastSubscription {
		t.Helper()
		o := origin
		sub := newTestSubscription()
		sub.OwnerSessionID = session
		sub.OriginChatID = &o
		sub.CreatedAt = base.Add(offset)
		if err := store.Create(ctx, sub); err != nil {
			t.Fatalf("create: %v", err)
		}
		return sub
	}
	first := mk("session-1", "chat-A", 0)
	second := mk("session-1", "chat-B", time.Second)
	third := mk("session-2", "chat-A", 2*time.Second)

	all, err := store.List(ctx, ListBroadcastSubscriptionsFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 3 || all[0].ID != first.ID || all[2].ID != third.ID {
		t.Fatalf("unfiltered listing = %d rows, want 3 in created_at order", len(all))
	}

	session := "session-1"
	bySession, err := store.List(ctx, ListBroadcastSubscriptionsFilter{OwnerSessionID: &session})
	if err != nil {
		t.Fatalf("list by session: %v", err)
	}
	if len(bySession) != 2 || bySession[0].ID != first.ID || bySession[1].ID != second.ID {
		t.Fatalf("session filter = %d rows, want first+second", len(bySession))
	}

	originA := "chat-A"
	byOrigin, err := store.List(ctx, ListBroadcastSubscriptionsFilter{OriginChatID: &originA})
	if err != nil {
		t.Fatalf("list by origin: %v", err)
	}
	if len(byOrigin) != 2 || byOrigin[0].ID != first.ID || byOrigin[1].ID != third.ID {
		t.Fatalf("origin filter = %d rows, want first+third", len(byOrigin))
	}

	limited, err := store.List(ctx, ListBroadcastSubscriptionsFilter{Limit: 2})
	if err != nil {
		t.Fatalf("list limited: %v", err)
	}
	if len(limited) != 2 || limited[0].ID != first.ID {
		t.Fatalf("limit = %d rows, want the 2 oldest", len(limited))
	}

	if _, err := store.Cancel(ctx, second.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	canceled := models.BroadcastSubscriptionStateCanceled
	byState, err := store.List(ctx, ListBroadcastSubscriptionsFilter{State: &canceled})
	if err != nil {
		t.Fatalf("list by state: %v", err)
	}
	if len(byState) != 1 || byState[0].ID != second.ID {
		t.Fatalf("state filter = %d rows, want only the canceled one", len(byState))
	}

	trigger := models.BroadcastTriggerErrored
	byTrigger, err := store.List(ctx, ListBroadcastSubscriptionsFilter{TriggerEvent: &trigger})
	if err != nil {
		t.Fatalf("list by trigger: %v", err)
	}
	if len(byTrigger) != 0 {
		t.Errorf("errored-trigger filter = %d rows, want 0 (every fixture is completed)", len(byTrigger))
	}
}

// TestBroadcastSubscriptionStore_ListActiveOwnerSessionIDs backs the reconcile
// sweep: it needs the distinct set of sessions still carrying a live
// registration, so it can re-check sessions that reached a trigger state while
// the daemon was down.
func TestBroadcastSubscriptionStore_ListActiveOwnerSessionIDs(t *testing.T) {
	store := NewBroadcastSubscriptionStore(setupTestDB(t))
	ctx := context.Background()
	base := time.Now().UTC()

	mk := func(session string, expiry time.Duration) *models.BroadcastSubscription {
		t.Helper()
		sub := newTestSubscription()
		sub.OwnerSessionID = session
		sub.ExpiresAt = base.Add(expiry)
		if err := store.Create(ctx, sub); err != nil {
			t.Fatalf("create: %v", err)
		}
		return sub
	}
	mk("session-1", time.Hour)
	mk("session-1", time.Hour) // duplicate owner: the result is distinct
	mk("session-2", time.Hour)
	mk("session-3", -time.Minute) // past expiry: never reported
	fired := mk("session-4", time.Hour)
	if _, err := store.MarkFired(ctx, fired.ID, "b-1", base); err != nil {
		t.Fatalf("mark fired: %v", err)
	}

	got, err := store.ListActiveOwnerSessionIDs(ctx)
	if err != nil {
		t.Fatalf("list active owner session ids: %v", err)
	}
	want := []string{"session-1", "session-2"}
	if len(got) != len(want) {
		t.Fatalf("active owner sessions = %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("active owner sessions = %v, want %v", got, want)
		}
	}
}

// TestBroadcastSubscriptionStore_NeverLeaksMessageBody is the secret-leak guard.
// The body is stored verbatim in `message` because the evaluator needs it to
// build the broadcast, but no other column may carry it and no error string the
// store returns may echo it.
func TestBroadcastSubscriptionStore_NeverLeaksMessageBody(t *testing.T) {
	db := setupTestDB(t)
	store := NewBroadcastSubscriptionStore(db)
	ctx := context.Background()

	sub := newTestSubscription()
	if !strings.Contains(sub.Message, broadcastSubscriptionSecretToken) {
		t.Fatalf("fixture message = %q, want it to carry the secret token", sub.Message)
	}
	if err := store.Create(ctx, sub); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := store.MarkFired(ctx, sub.ID, "broadcast-1", time.Now().UTC()); err != nil {
		t.Fatalf("mark fired: %v", err)
	}

	// Grep every column but `message`: the invariant is that the body lives in
	// exactly one place.
	var joined string
	if err := db.QueryRowContext(ctx, `
		SELECT COALESCE(id, '') || '|' || COALESCE(owner_session_id, '') || '|' ||
		       COALESCE(origin_chat_id, '') || '|' || COALESCE(trigger_event, '') || '|' ||
		       COALESCE(selector, '') || '|' || COALESCE(state, '') || '|' ||
		       COALESCE(fired_at, '') || '|' || COALESCE(fired_broadcast_id, '') || '|' ||
		       COALESCE(expires_at, '')
		FROM broadcast_subscriptions WHERE id = ?`, sub.ID).Scan(&joined); err != nil {
		t.Fatalf("scan subscription columns: %v", err)
	}
	if strings.Contains(joined, broadcastSubscriptionSecretToken) {
		t.Errorf("subscription row %q contains the secret message body", joined)
	}

	// Every rejection path must diagnose the *field*, never the value. A store
	// that formatted the struct into its error would leak the body into a log the
	// moment a validation failed.
	bad := newTestSubscription()
	bad.Selector = ""
	err := store.Create(ctx, bad)
	if err == nil {
		t.Fatal("create with an empty selector err = nil, want a validation failure")
	}
	if strings.Contains(err.Error(), broadcastSubscriptionSecretToken) {
		t.Errorf("validation error %q contains the secret message body", err)
	}
}

// TestBroadcastSubscriptionStore_UnknownStateIsLoud pins parsed reads: a value
// outside the vocabulary — a hand edit, a future migration read by an old binary
// — must surface as an error rather than a silent non-matching state.
func TestBroadcastSubscriptionStore_UnknownStateIsLoud(t *testing.T) {
	db := setupTestDB(t)
	store := NewBroadcastSubscriptionStore(db)
	ctx := context.Background()

	sub := newTestSubscription()
	if err := store.Create(ctx, sub); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"UPDATE broadcast_subscriptions SET state = 'exploded' WHERE id = ?", sub.ID); err != nil {
		t.Fatalf("corrupt state: %v", err)
	}
	if _, err := store.Get(ctx, sub.ID); !errors.Is(err, models.ErrUnknownBroadcastSubscriptionState) {
		t.Errorf("get err = %v, want ErrUnknownBroadcastSubscriptionState", err)
	}

	if _, err := db.ExecContext(ctx,
		"UPDATE broadcast_subscriptions SET state = 'active', trigger_event = 'exploded' WHERE id = ?",
		sub.ID); err != nil {
		t.Fatalf("corrupt trigger: %v", err)
	}
	if _, err := store.Get(ctx, sub.ID); !errors.Is(err, models.ErrUnknownBroadcastTrigger) {
		t.Errorf("get err = %v, want ErrUnknownBroadcastTrigger", err)
	}
}
