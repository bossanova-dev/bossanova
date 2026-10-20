package callback

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
	"github.com/rs/zerolog"
)

// fakeInterestStore serves canned callbacks, honouring the State filter so
// DeriveInterests's per-state queries behave like the real store.
type fakeInterestStore struct {
	callbacks []*models.GitHubCallback
	err       error
}

func (f *fakeInterestStore) List(_ context.Context, filter db.ListGitHubCallbacksFilter) ([]*models.GitHubCallback, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []*models.GitHubCallback
	for _, cb := range f.callbacks {
		if filter.State != nil && cb.State != *filter.State {
			continue
		}
		if filter.TargetChatID != nil && cb.TargetChatID != *filter.TargetChatID {
			continue
		}
		out = append(out, cb)
	}
	// The real store orders by created_at then id; mirror that so the fake
	// cannot accidentally hand ActiveInterestForChat a pre-sorted convenience
	// the production store would not guarantee.
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func cb(owner, name string, pr int, state models.GitHubCallbackState) *models.GitHubCallback {
	return &models.GitHubCallback{RepoOwner: owner, RepoName: name, PRNumber: pr, State: state}
}

func TestDeriveInterests_Empty(t *testing.T) {
	store := &fakeInterestStore{}
	got, err := DeriveInterests(context.Background(), store)
	if err != nil {
		t.Fatalf("DeriveInterests: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty, got %v", got)
	}
}

func TestDeriveInterests_DedupesAndGroups(t *testing.T) {
	store := &fakeInterestStore{callbacks: []*models.GitHubCallback{
		cb("acme", "widget", 7, models.GitHubCallbackStateActive),
		cb("acme", "widget", 7, models.GitHubCallbackStateLeased),    // dup of above
		cb("acme", "widget", 7, models.GitHubCallbackStateTriggered), // dup of above
		cb("acme", "widget", 9, models.GitHubCallbackStateActive),    // same repo, diff PR
		cb("acme", "gadget", 7, models.GitHubCallbackStateActive),    // diff repo, same PR
	}}
	got, err := DeriveInterests(context.Background(), store)
	if err != nil {
		t.Fatalf("DeriveInterests: %v", err)
	}
	want := []*pb.CallbackInterest{
		{RepoOriginUrl: "https://github.com/acme/gadget", PrNumber: 7},
		{RepoOriginUrl: "https://github.com/acme/widget", PrNumber: 7},
		{RepoOriginUrl: "https://github.com/acme/widget", PrNumber: 9},
	}
	assertInterests(t, got, want)
}

func TestDeriveInterests_ExcludesTerminalStates(t *testing.T) {
	store := &fakeInterestStore{callbacks: []*models.GitHubCallback{
		cb("acme", "widget", 1, models.GitHubCallbackStateDelivered),
		cb("acme", "widget", 2, models.GitHubCallbackStateCanceled),
		cb("acme", "widget", 3, models.GitHubCallbackStateExpired),
		cb("acme", "widget", 4, models.GitHubCallbackStateActive),
		cb("acme", "widget", 5, models.GitHubCallbackStateLeased),
		cb("acme", "widget", 6, models.GitHubCallbackStateTriggered),
	}}
	got, err := DeriveInterests(context.Background(), store)
	if err != nil {
		t.Fatalf("DeriveInterests: %v", err)
	}
	want := []*pb.CallbackInterest{
		{RepoOriginUrl: "https://github.com/acme/widget", PrNumber: 4},
		{RepoOriginUrl: "https://github.com/acme/widget", PrNumber: 5},
		{RepoOriginUrl: "https://github.com/acme/widget", PrNumber: 6},
	}
	assertInterests(t, got, want)
}

func TestDeriveInterests_SortedDeterministically(t *testing.T) {
	store := &fakeInterestStore{callbacks: []*models.GitHubCallback{
		cb("zeta", "repo", 3, models.GitHubCallbackStateActive),
		cb("alpha", "repo", 5, models.GitHubCallbackStateActive),
		cb("alpha", "repo", 2, models.GitHubCallbackStateActive),
		cb("mid", "repo", 1, models.GitHubCallbackStateActive),
	}}
	got, err := DeriveInterests(context.Background(), store)
	if err != nil {
		t.Fatalf("DeriveInterests: %v", err)
	}
	want := []*pb.CallbackInterest{
		{RepoOriginUrl: "https://github.com/alpha/repo", PrNumber: 2},
		{RepoOriginUrl: "https://github.com/alpha/repo", PrNumber: 5},
		{RepoOriginUrl: "https://github.com/mid/repo", PrNumber: 1},
		{RepoOriginUrl: "https://github.com/zeta/repo", PrNumber: 3},
	}
	assertInterests(t, got, want)
}

func TestDeriveInterests_URLRoundTripsWithGitHubNWO(t *testing.T) {
	store := &fakeInterestStore{callbacks: []*models.GitHubCallback{
		cb("owner", "repo", 42, models.GitHubCallbackStateActive),
	}}
	got, err := DeriveInterests(context.Background(), store)
	if err != nil {
		t.Fatalf("DeriveInterests: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 interest, got %d", len(got))
	}
	// The URL bossd advertises MUST map back to the same owner/name the
	// webhook dispatcher derives via vcs.GitHubNWO — that is the routing key.
	if nwo := vcs.GitHubNWO(got[0].GetRepoOriginUrl()); nwo != "owner/repo" {
		t.Errorf("GitHubNWO(%q) = %q, want owner/repo", got[0].GetRepoOriginUrl(), nwo)
	}
}

func TestDeriveInterests_StoreError(t *testing.T) {
	store := &fakeInterestStore{err: errors.New("boom")}
	if _, err := DeriveInterests(context.Background(), store); err == nil {
		t.Fatal("want error, got nil")
	}
}

// --- advertiser ---

type capturePublisher struct {
	sets [][]*pb.CallbackInterest
}

func (c *capturePublisher) PublishInterests(interests []*pb.CallbackInterest) {
	c.sets = append(c.sets, interests)
}

func TestInterestAdvertiser_PublishesOnChangeAndSuppressesIdentical(t *testing.T) {
	store := &fakeInterestStore{callbacks: []*models.GitHubCallback{
		cb("acme", "widget", 7, models.GitHubCallbackStateActive),
	}}
	pub := &capturePublisher{}
	adv := NewInterestAdvertiser(AdvertiserConfig{Store: store, Publisher: pub, Logger: zerolog.Nop()})

	// First derive publishes.
	published, err := adv.Publish(context.Background())
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !published || len(pub.sets) != 1 {
		t.Fatalf("first Publish should publish once, published=%v sets=%d", published, len(pub.sets))
	}

	// Identical set is suppressed.
	published, err = adv.Publish(context.Background())
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if published || len(pub.sets) != 1 {
		t.Fatalf("identical Publish should suppress, published=%v sets=%d", published, len(pub.sets))
	}
}

func TestInterestAdvertiser_PublishesEmptyOnDrain(t *testing.T) {
	store := &fakeInterestStore{callbacks: []*models.GitHubCallback{
		cb("acme", "widget", 7, models.GitHubCallbackStateActive),
	}}
	pub := &capturePublisher{}
	adv := NewInterestAdvertiser(AdvertiserConfig{Store: store, Publisher: pub, Logger: zerolog.Nop()})

	if _, err := adv.Publish(context.Background()); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	// Drain all callbacks (e.g. delivered) → interest withdrawn.
	store.callbacks = nil
	published, err := adv.Publish(context.Background())
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !published {
		t.Fatal("drain should publish the empty withdrawal set")
	}
	last := pub.sets[len(pub.sets)-1]
	if len(last) != 0 {
		t.Fatalf("withdrawal set should be empty, got %v", last)
	}
}

// assertInterests compares two interest slices element-wise (order-sensitive).
func assertInterests(t *testing.T, got, want []*pb.CallbackInterest) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (got %+v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i].GetRepoOriginUrl() != want[i].GetRepoOriginUrl() || got[i].GetPrNumber() != want[i].GetPrNumber() {
			t.Errorf("interest[%d] = {%q, %d}, want {%q, %d}", i,
				got[i].GetRepoOriginUrl(), got[i].GetPrNumber(),
				want[i].GetRepoOriginUrl(), want[i].GetPrNumber())
		}
	}
}

// chatCB builds a callback targeting a chat, with an explicit id/created-at so
// the deterministic-winner rule can be exercised. The repository is fixed at
// acme/widget: none of these cases turn on the owner or name, only on the
// trigger, the PR number and the state. expires_at is the registration default
// (24h out) so these cases exercise the state rule alone; the expiry rule has
// its own test that sets it explicitly.
func chatCB(id, chatID string, created time.Time, trigger models.GitHubCallbackTrigger, pr int, state models.GitHubCallbackState) *models.GitHubCallback {
	return &models.GitHubCallback{
		ID:           id,
		TargetChatID: chatID,
		RepoOwner:    "acme",
		RepoName:     "widget",
		PRNumber:     pr,
		Trigger:      trigger,
		State:        state,
		CreatedAt:    created,
		ExpiresAt:    created.Add(24 * time.Hour),
	}
}

func TestActiveInterestForChat_ArmedCallbackYieldsCanonicalReason(t *testing.T) {
	base := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	store := &fakeInterestStore{callbacks: []*models.GitHubCallback{
		chatCB("cb1", "chat-a", base, models.GitHubCallbackTriggerChecksPassedReady, 123, models.GitHubCallbackStateActive),
	}}
	got, err := ActiveInterestForChat(context.Background(), store, "chat-a", base)
	if err != nil {
		t.Fatalf("ActiveInterestForChat: %v", err)
	}
	if got == nil {
		t.Fatal("want an interest, got nil")
	}
	if got.ID != "cb1" {
		t.Fatalf("id = %q, want cb1", got.ID)
	}
	const want = "awaiting checks_passed_ready on acme/widget#123"
	if got.WaitingReason() != want {
		t.Fatalf("WaitingReason() = %q, want %q", got.WaitingReason(), want)
	}
}

func TestActiveInterestForChat_TerminalStatesAreNotWaiting(t *testing.T) {
	base := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	for _, state := range []models.GitHubCallbackState{
		models.GitHubCallbackStateDelivered,
		models.GitHubCallbackStateExpired,
		models.GitHubCallbackStateCanceled,
	} {
		t.Run(string(state), func(t *testing.T) {
			store := &fakeInterestStore{callbacks: []*models.GitHubCallback{
				chatCB("cb1", "chat-a", base, models.GitHubCallbackTriggerMerged, 1, state),
			}}
			got, err := ActiveInterestForChat(context.Background(), store, "chat-a", base)
			if err != nil {
				t.Fatalf("ActiveInterestForChat: %v", err)
			}
			if got != nil {
				t.Fatalf("want nil for %s callback, got %+v", state, got)
			}
		})
	}
}

func TestActiveInterestForChat_NoCallbackYieldsNoReason(t *testing.T) {
	base := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	store := &fakeInterestStore{callbacks: []*models.GitHubCallback{
		chatCB("cb1", "other-chat", base, models.GitHubCallbackTriggerMerged, 1, models.GitHubCallbackStateActive),
	}}
	got, err := ActiveInterestForChat(context.Background(), store, "chat-a", base)
	if err != nil {
		t.Fatalf("ActiveInterestForChat: %v", err)
	}
	if got != nil {
		t.Fatalf("want nil, got %+v", got)
	}
	if got.WaitingReason() != "" {
		t.Fatalf("nil interest WaitingReason() = %q, want empty", got.WaitingReason())
	}
}

func TestActiveInterestForChat_MultipleArmedPicksDeterministicWinner(t *testing.T) {
	base := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	// Three armed interests across all three non-terminal states, deliberately
	// listed newest-first so a naive "first row wins" would pick the wrong one.
	callbacks := []*models.GitHubCallback{
		chatCB("cb-z", "chat-a", base.Add(2*time.Hour), models.GitHubCallbackTriggerMerged, 3, models.GitHubCallbackStateTriggered),
		chatCB("cb-m", "chat-a", base.Add(time.Hour), models.GitHubCallbackTriggerChecksFailed, 2, models.GitHubCallbackStateLeased),
		chatCB("cb-b", "chat-a", base, models.GitHubCallbackTriggerChecksPassed, 1, models.GitHubCallbackStateActive),
		// Same created_at as the earliest, larger id: loses the tiebreak.
		chatCB("cb-c", "chat-a", base, models.GitHubCallbackTriggerClosed, 9, models.GitHubCallbackStateActive),
	}
	const want = "awaiting checks_passed on acme/widget#1"
	// Run the permutations: the winner must not depend on listing order.
	for i := range callbacks {
		rotated := append(append([]*models.GitHubCallback{}, callbacks[i:]...), callbacks[:i]...)
		store := &fakeInterestStore{callbacks: rotated}
		got, err := ActiveInterestForChat(context.Background(), store, "chat-a", base)
		if err != nil {
			t.Fatalf("ActiveInterestForChat: %v", err)
		}
		if got == nil {
			t.Fatal("want an interest, got nil")
		}
		if got.ID != "cb-b" {
			t.Fatalf("rotation %d: id = %q, want cb-b (earliest created_at, smallest id)", i, got.ID)
		}
		if got.WaitingReason() != want {
			t.Fatalf("rotation %d: WaitingReason() = %q, want %q", i, got.WaitingReason(), want)
		}
	}
}

// TestActiveInterestForChat_ProgressBeatsOlderStandby pins BOS-1531: a chat
// holding an old standby watch (checks_failed) and a newer progress watch
// (merged / checks_passed_ready) is parked on the progress one. The standby
// watch only matters if the PR goes wrong; the progress watch is what the chat
// is actually waiting for.
func TestActiveInterestForChat_ProgressBeatsOlderStandby(t *testing.T) {
	base := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	for _, progress := range []models.GitHubCallbackTrigger{
		models.GitHubCallbackTriggerMerged,
		models.GitHubCallbackTriggerChecksPassedReady,
	} {
		t.Run(string(progress), func(t *testing.T) {
			callbacks := []*models.GitHubCallback{
				chatCB("cb-a", "chat-a", base, models.GitHubCallbackTriggerChecksFailed, 7, models.GitHubCallbackStateActive),
				chatCB("cb-b", "chat-a", base.Add(time.Minute), models.GitHubCallbackTriggerClosed, 7, models.GitHubCallbackStateLeased),
				chatCB("cb-z", "chat-a", base.Add(time.Hour), progress, 7, models.GitHubCallbackStateActive),
			}
			want := "awaiting " + string(progress) + " on acme/widget#7"
			for i := range callbacks {
				rotated := append(append([]*models.GitHubCallback{}, callbacks[i:]...), callbacks[:i]...)
				got, err := ActiveInterestForChat(context.Background(), &fakeInterestStore{callbacks: rotated}, "chat-a", base)
				if err != nil {
					t.Fatalf("ActiveInterestForChat: %v", err)
				}
				if got == nil || got.ID != "cb-z" {
					t.Fatalf("rotation %d: got %+v, want cb-z (progress beats older standby)", i, got)
				}
				if got.WaitingReason() != want {
					t.Fatalf("rotation %d: WaitingReason() = %q, want %q", i, got.WaitingReason(), want)
				}
			}
		})
	}
}

// TestActiveInterestForChat_StandbyOnlyChatKeepsItsReason: with no progress
// watch the standby one is still the chat's interest (oldest first) — the
// ranking reorders reasons, it never hides one.
func TestActiveInterestForChat_StandbyOnlyChatKeepsItsReason(t *testing.T) {
	base := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	store := &fakeInterestStore{callbacks: []*models.GitHubCallback{
		chatCB("cb-b", "chat-a", base.Add(time.Minute), models.GitHubCallbackTriggerClosed, 7, models.GitHubCallbackStateActive),
		chatCB("cb-a", "chat-a", base, models.GitHubCallbackTriggerChecksFailed, 7, models.GitHubCallbackStateActive),
	}}
	got, err := ActiveInterestForChat(context.Background(), store, "chat-a", base)
	if err != nil {
		t.Fatalf("ActiveInterestForChat: %v", err)
	}
	const want = "awaiting checks_failed on acme/widget#7"
	if got == nil || got.WaitingReason() != want {
		t.Fatalf("got %+v, want %q", got, want)
	}
}

func TestActiveInterestForChat_EmptyChatIDIsNoLookup(t *testing.T) {
	base := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	store := &fakeInterestStore{err: errors.New("must not be queried")}
	got, err := ActiveInterestForChat(context.Background(), store, "", base)
	if err != nil {
		t.Fatalf("ActiveInterestForChat: %v", err)
	}
	if got != nil {
		t.Fatalf("want nil, got %+v", got)
	}
}

func TestActiveInterestForChat_StoreErrorPropagates(t *testing.T) {
	base := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	store := &fakeInterestStore{err: errors.New("boom")}
	if _, err := ActiveInterestForChat(context.Background(), store, "chat-a", base); err == nil {
		t.Fatal("want an error, got nil")
	}
}

// Expiry is swept lazily — the delivery worker's ExpireOverdue tick and the list
// RPC are the only writers of the expired state — so a callback whose deadline
// has passed still reads as active/leased/triggered for up to a poll interval.
// The waiting derivation must not park a chat on an event that can no longer
// fire, so it filters on expires_at as well as state.
func TestActiveInterestForChat_OverdueButUnsweptCallbackIsNotWaiting(t *testing.T) {
	base := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	for _, state := range nonTerminalCallbackStates {
		t.Run(string(state), func(t *testing.T) {
			cb := chatCB("cb1", "chat-a", base, models.GitHubCallbackTriggerMerged, 1, state)
			cb.ExpiresAt = base.Add(time.Hour)
			store := &fakeInterestStore{callbacks: []*models.GitHubCallback{cb}}

			// One nanosecond before the deadline: still parked.
			got, err := ActiveInterestForChat(context.Background(), store, "chat-a", cb.ExpiresAt.Add(-time.Nanosecond))
			if err != nil {
				t.Fatalf("ActiveInterestForChat: %v", err)
			}
			if got == nil {
				t.Fatal("want an interest just before expiry, got nil")
			}

			// At the deadline and after it: no longer parked. expires_at is an
			// exclusive bound, matching AcquireLease's expires_at > now.
			for _, now := range []time.Time{cb.ExpiresAt, cb.ExpiresAt.Add(time.Hour)} {
				got, err := ActiveInterestForChat(context.Background(), store, "chat-a", now)
				if err != nil {
					t.Fatalf("ActiveInterestForChat: %v", err)
				}
				if got != nil {
					t.Fatalf("now=%v: want nil for overdue %s callback, got %+v", now, state, got)
				}
			}
		})
	}
}

// An overdue callback must not merely lose — it must be invisible, so a younger
// still-live sibling wins instead of the chat falling silent or reporting the
// dead one. The overdue row is the older of the two, so it would win the
// created_at tiebreak if the expiry filter were missing.
func TestActiveInterestForChat_OverdueLoserYieldsToLiveSibling(t *testing.T) {
	base := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	overdue := chatCB("cb-a", "chat-a", base, models.GitHubCallbackTriggerMerged, 1, models.GitHubCallbackStateActive)
	overdue.ExpiresAt = base.Add(time.Hour)
	live := chatCB("cb-b", "chat-a", base.Add(time.Minute), models.GitHubCallbackTriggerChecksPassed, 2, models.GitHubCallbackStateActive)
	live.ExpiresAt = base.Add(48 * time.Hour)
	store := &fakeInterestStore{callbacks: []*models.GitHubCallback{overdue, live}}

	got, err := ActiveInterestForChat(context.Background(), store, "chat-a", base.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("ActiveInterestForChat: %v", err)
	}
	if got == nil {
		t.Fatal("want the live sibling, got nil")
	}
	if got.ID != "cb-b" {
		t.Fatalf("id = %q, want cb-b (the un-expired sibling)", got.ID)
	}
	const want = "awaiting checks_passed on acme/widget#2"
	if got.WaitingReason() != want {
		t.Fatalf("WaitingReason() = %q, want %q", got.WaitingReason(), want)
	}
}

func TestWaitingReasonForChat_UsesCanonicalWording(t *testing.T) {
	base := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	store := &fakeInterestStore{callbacks: []*models.GitHubCallback{
		chatCB("cb1", "chat-a", base, models.GitHubCallbackTriggerReadyForReview, 42, models.GitHubCallbackStateActive),
	}}
	got, err := WaitingReasonForChat(context.Background(), store, "chat-a", base)
	if err != nil {
		t.Fatalf("WaitingReasonForChat: %v", err)
	}
	const want = "awaiting ready_for_review on acme/widget#42"
	if got != want {
		t.Fatalf("WaitingReasonForChat = %q, want %q", got, want)
	}
}

func TestDeliveryPendingForChat(t *testing.T) {
	now := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	for _, state := range []models.GitHubCallbackState{
		models.GitHubCallbackStateActive, models.GitHubCallbackStateLeased,
		models.GitHubCallbackStateTriggered, models.GitHubCallbackStateDelivered,
		models.GitHubCallbackStateCanceled, models.GitHubCallbackStateExpired,
	} {
		t.Run(string(state), func(t *testing.T) {
			callback := chatCB("cb", "chat", now, models.GitHubCallbackTriggerMerged, 1, state)
			for _, expiry := range []time.Time{now.Add(-time.Second), now, now.Add(time.Second)} {
				callback.ExpiresAt = expiry
				store := &fakeInterestStore{callbacks: []*models.GitHubCallback{callback}}
				got, err := DeliveryPendingForChat(context.Background(), store, "chat", now)
				if err != nil {
					t.Fatal(err)
				}
				want := (state == models.GitHubCallbackStateLeased || state == models.GitHubCallbackStateTriggered) && expiry.After(now)
				if got != want {
					t.Errorf("expiry=%v: got %v, want %v", expiry, got, want)
				}
			}
		})
	}
	for _, callbacks := range [][]*models.GitHubCallback{nil, {
		chatCB("cb", "other-chat", now, models.GitHubCallbackTriggerMerged, 1, models.GitHubCallbackStateLeased),
	}} {
		got, err := DeliveryPendingForChat(context.Background(), &fakeInterestStore{callbacks: callbacks}, "chat", now)
		if err != nil || got {
			t.Errorf("unrelated callbacks: got %v, %v", got, err)
		}
	}
	boom := errors.New("boom")
	store := &fakeInterestStore{err: boom}
	if got, err := DeliveryPendingForChat(context.Background(), store, "", now); got || err != nil {
		t.Errorf("empty chat: got %v, %v", got, err)
	}
	if got, err := DeliveryPendingForChat(context.Background(), store, "chat", now); got || !errors.Is(err, boom) {
		t.Errorf("store failure: got %v, %v", got, err)
	}
}

func TestHoldsProgressWait(t *testing.T) {
	now := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	pr := 42
	for _, tt := range []struct {
		name       string
		trigger    models.GitHubCallbackTrigger
		callbackPR int
		nwo        string
		sessionPR  *int
		want       bool
	}{
		{"own merge", models.GitHubCallbackTriggerMerged, 42, "acme/widget", &pr, false},
		{"own close", models.GitHubCallbackTriggerClosed, 42, "acme/widget", &pr, false},
		{"case insensitive own merge", models.GitHubCallbackTriggerMerged, 42, "ACME/Widget", &pr, false},
		{"standby checks failed", models.GitHubCallbackTriggerChecksFailed, 42, "acme/widget", &pr, false},
		{"checks passed", models.GitHubCallbackTriggerChecksPassed, 42, "acme/widget", &pr, true},
		{"checks passed ready", models.GitHubCallbackTriggerChecksPassedReady, 42, "acme/widget", &pr, true},
		{"ready for review", models.GitHubCallbackTriggerReadyForReview, 42, "acme/widget", &pr, true},
		{"another PR", models.GitHubCallbackTriggerMerged, 43, "acme/widget", &pr, true},
		{"another repo", models.GitHubCallbackTriggerMerged, 42, "acme/other", &pr, true},
		{"another owner", models.GitHubCallbackTriggerMerged, 42, "other/widget", &pr, true},
		{"other PR close is standby", models.GitHubCallbackTriggerClosed, 43, "acme/widget", &pr, false},
		{"unknown own repo", models.GitHubCallbackTriggerMerged, 42, "", &pr, true},
		{"no session PR", models.GitHubCallbackTriggerMerged, 42, "acme/widget", nil, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, state := range []models.GitHubCallbackState{models.GitHubCallbackStateActive, models.GitHubCallbackStateLeased, models.GitHubCallbackStateTriggered} {
				cb := chatCB("cb", "chat", now, tt.trigger, tt.callbackPR, state)
				store := &fakeInterestStore{callbacks: []*models.GitHubCallback{cb}}
				got, err := HoldsProgressWait(context.Background(), store, "chat", &models.Session{PRNumber: tt.sessionPR}, tt.nwo, now)
				if err != nil || got != tt.want {
					t.Fatalf("state=%s: got %v, %v; want %v", state, got, err, tt.want)
				}
			}
		})
	}
}

func TestHoldsProgressWaitIgnoresInactiveAndUnrelated(t *testing.T) {
	now := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	for _, state := range []models.GitHubCallbackState{
		models.GitHubCallbackStateActive, models.GitHubCallbackStateLeased, models.GitHubCallbackStateTriggered,
		models.GitHubCallbackStateDelivered, models.GitHubCallbackStateCanceled, models.GitHubCallbackStateExpired,
	} {
		for _, expiry := range []time.Time{now.Add(-time.Second), now, now.Add(time.Second)} {
			cb := chatCB("cb", "chat", now, models.GitHubCallbackTriggerChecksPassed, 42, state)
			cb.ExpiresAt = expiry
			got, err := HoldsProgressWait(context.Background(), &fakeInterestStore{callbacks: []*models.GitHubCallback{cb}}, "chat", &models.Session{}, "acme/widget", now)
			want := (state == models.GitHubCallbackStateActive || state == models.GitHubCallbackStateLeased || state == models.GitHubCallbackStateTriggered) && expiry.After(now)
			if err != nil || got != want {
				t.Fatalf("state=%s expiry=%v: got %v, %v; want %v", state, expiry, got, err, want)
			}
		}
	}
	other := chatCB("cb", "other-chat", now, models.GitHubCallbackTriggerChecksPassed, 42, models.GitHubCallbackStateActive)
	if got, err := HoldsProgressWait(context.Background(), &fakeInterestStore{callbacks: []*models.GitHubCallback{other}}, "chat", &models.Session{}, "", now); got || err != nil {
		t.Fatalf("unrelated callback: got %v, %v", got, err)
	}
	boom := errors.New("boom")
	store := &fakeInterestStore{err: boom}
	if got, err := HoldsProgressWait(context.Background(), store, "", &models.Session{}, "", now); got || err != nil {
		t.Fatalf("empty chat: got %v, %v", got, err)
	}
	if got, err := HoldsProgressWait(context.Background(), store, "chat", &models.Session{}, "", now); got || !errors.Is(err, boom) {
		t.Fatalf("store failure: got %v, %v", got, err)
	}
}

func TestHoldsProgressWaitFindsProgressAlongsideFinishedWatches(t *testing.T) {
	now := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	pr := 42
	store := &fakeInterestStore{callbacks: []*models.GitHubCallback{
		chatCB("own", "chat", now, models.GitHubCallbackTriggerMerged, pr, models.GitHubCallbackStateActive),
		chatCB("standby", "chat", now, models.GitHubCallbackTriggerChecksFailed, pr, models.GitHubCallbackStateActive),
	}}
	session := &models.Session{PRNumber: &pr}
	if got, err := HoldsProgressWait(context.Background(), store, "chat", session, "acme/widget", now); err != nil || got {
		t.Fatalf("finished watches: got %v, %v", got, err)
	}
	store.callbacks = append(store.callbacks, chatCB("progress", "chat", now.Add(time.Minute), models.GitHubCallbackTriggerChecksPassedReady, pr, models.GitHubCallbackStateTriggered))
	if got, err := HoldsProgressWait(context.Background(), store, "chat", session, "acme/widget", now); err != nil || !got {
		t.Fatalf("additional progress watch: got %v, %v", got, err)
	}
}
