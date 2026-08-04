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
	callbacks []*models.GithubCallback
	err       error
}

func (f *fakeInterestStore) List(_ context.Context, filter db.ListGithubCallbacksFilter) ([]*models.GithubCallback, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []*models.GithubCallback
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

func cb(owner, name string, pr int, state models.GithubCallbackState) *models.GithubCallback {
	return &models.GithubCallback{RepoOwner: owner, RepoName: name, PRNumber: pr, State: state}
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
	store := &fakeInterestStore{callbacks: []*models.GithubCallback{
		cb("acme", "widget", 7, models.GithubCallbackStateActive),
		cb("acme", "widget", 7, models.GithubCallbackStateLeased),    // dup of above
		cb("acme", "widget", 7, models.GithubCallbackStateTriggered), // dup of above
		cb("acme", "widget", 9, models.GithubCallbackStateActive),    // same repo, diff PR
		cb("acme", "gadget", 7, models.GithubCallbackStateActive),    // diff repo, same PR
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
	store := &fakeInterestStore{callbacks: []*models.GithubCallback{
		cb("acme", "widget", 1, models.GithubCallbackStateDelivered),
		cb("acme", "widget", 2, models.GithubCallbackStateCanceled),
		cb("acme", "widget", 3, models.GithubCallbackStateExpired),
		cb("acme", "widget", 4, models.GithubCallbackStateActive),
		cb("acme", "widget", 5, models.GithubCallbackStateLeased),
		cb("acme", "widget", 6, models.GithubCallbackStateTriggered),
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
	store := &fakeInterestStore{callbacks: []*models.GithubCallback{
		cb("zeta", "repo", 3, models.GithubCallbackStateActive),
		cb("alpha", "repo", 5, models.GithubCallbackStateActive),
		cb("alpha", "repo", 2, models.GithubCallbackStateActive),
		cb("mid", "repo", 1, models.GithubCallbackStateActive),
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
	store := &fakeInterestStore{callbacks: []*models.GithubCallback{
		cb("owner", "repo", 42, models.GithubCallbackStateActive),
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
	store := &fakeInterestStore{callbacks: []*models.GithubCallback{
		cb("acme", "widget", 7, models.GithubCallbackStateActive),
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
	store := &fakeInterestStore{callbacks: []*models.GithubCallback{
		cb("acme", "widget", 7, models.GithubCallbackStateActive),
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
func chatCB(id, chatID string, created time.Time, trigger models.GithubCallbackTrigger, pr int, state models.GithubCallbackState) *models.GithubCallback {
	return &models.GithubCallback{
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
	store := &fakeInterestStore{callbacks: []*models.GithubCallback{
		chatCB("cb1", "chat-a", base, models.GithubCallbackTriggerChecksPassedReady, 123, models.GithubCallbackStateActive),
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
	for _, state := range []models.GithubCallbackState{
		models.GithubCallbackStateDelivered,
		models.GithubCallbackStateExpired,
		models.GithubCallbackStateCanceled,
	} {
		t.Run(string(state), func(t *testing.T) {
			store := &fakeInterestStore{callbacks: []*models.GithubCallback{
				chatCB("cb1", "chat-a", base, models.GithubCallbackTriggerMerged, 1, state),
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
	store := &fakeInterestStore{callbacks: []*models.GithubCallback{
		chatCB("cb1", "other-chat", base, models.GithubCallbackTriggerMerged, 1, models.GithubCallbackStateActive),
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
	callbacks := []*models.GithubCallback{
		chatCB("cb-z", "chat-a", base.Add(2*time.Hour), models.GithubCallbackTriggerMerged, 3, models.GithubCallbackStateTriggered),
		chatCB("cb-m", "chat-a", base.Add(time.Hour), models.GithubCallbackTriggerChecksFailed, 2, models.GithubCallbackStateLeased),
		chatCB("cb-b", "chat-a", base, models.GithubCallbackTriggerChecksPassed, 1, models.GithubCallbackStateActive),
		// Same created_at as the earliest, larger id: loses the tiebreak.
		chatCB("cb-c", "chat-a", base, models.GithubCallbackTriggerClosed, 9, models.GithubCallbackStateActive),
	}
	const want = "awaiting checks_passed on acme/widget#1"
	// Run the permutations: the winner must not depend on listing order.
	for i := range callbacks {
		rotated := append(append([]*models.GithubCallback{}, callbacks[i:]...), callbacks[:i]...)
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
			cb := chatCB("cb1", "chat-a", base, models.GithubCallbackTriggerMerged, 1, state)
			cb.ExpiresAt = base.Add(time.Hour)
			store := &fakeInterestStore{callbacks: []*models.GithubCallback{cb}}

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
	overdue := chatCB("cb-a", "chat-a", base, models.GithubCallbackTriggerMerged, 1, models.GithubCallbackStateActive)
	overdue.ExpiresAt = base.Add(time.Hour)
	live := chatCB("cb-b", "chat-a", base.Add(time.Minute), models.GithubCallbackTriggerChecksPassed, 2, models.GithubCallbackStateActive)
	live.ExpiresAt = base.Add(48 * time.Hour)
	store := &fakeInterestStore{callbacks: []*models.GithubCallback{overdue, live}}

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
	store := &fakeInterestStore{callbacks: []*models.GithubCallback{
		chatCB("cb1", "chat-a", base, models.GithubCallbackTriggerReadyForReview, 42, models.GithubCallbackStateActive),
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
