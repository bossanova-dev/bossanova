package db

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/recurser/bossalib/githubcallback"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/dbtest"
)

// setupFileDB opens a temp file-backed SQLite DB (multiple connections) with
// migrations applied. File-backed DBs allow real concurrency, unlike the
// single-connection in-memory helper, so lease/trigger races are exercised.
func setupFileDB(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "bossd.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	dbtest.Apply(t, db)
	return db
}

func newTestCallbackParams() CreateGithubCallbackParams {
	return CreateGithubCallbackParams{
		TargetChatID: "chat-1",
		RepoOwner:    "Owner",
		RepoName:     "Repo",
		PRNumber:     42,
		Trigger:      models.GithubCallbackTriggerMerged,
		Message:      "secret prompt body",
	}
}

func TestGithubCallbackStore_CreateDefaultsAndNormalizes(t *testing.T) {
	store := NewGithubCallbackStore(setupTestDB(t))
	ctx := context.Background()

	before := time.Now().UTC()
	cb, err := store.Create(ctx, newTestCallbackParams())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if cb.State != models.GithubCallbackStateActive {
		t.Errorf("state = %q, want active", cb.State)
	}
	if cb.RepoOwner != "owner" || cb.RepoName != "repo" {
		t.Errorf("repo = %s/%s, want lowercased owner/repo", cb.RepoOwner, cb.RepoName)
	}
	if cb.AttemptCount != 0 {
		t.Errorf("attempt_count = %d, want 0", cb.AttemptCount)
	}
	if cb.ShouldRequireTransition {
		t.Error("should_require_transition = true, want false by default")
	}
	if cb.HasObservedBaseline {
		t.Error("has_observed_baseline = true, want false by default")
	}
	if cb.GroupID != nil {
		t.Errorf("group id = %v, want nil", cb.GroupID)
	}
	// Default expiry is 24h from creation.
	wantMin := before.Add(GithubCallbackDefaultExpiry - time.Minute)
	wantMax := time.Now().UTC().Add(GithubCallbackDefaultExpiry + time.Minute)
	if cb.ExpiresAt.Before(wantMin) || cb.ExpiresAt.After(wantMax) {
		t.Errorf("expires_at = %v, want ~24h from now", cb.ExpiresAt)
	}
}

func TestGithubCallbackStore_CreateExplicitExpiryAndGroup(t *testing.T) {
	store := NewGithubCallbackStore(setupTestDB(t))
	ctx := context.Background()

	group := "group-A"
	exp := time.Now().UTC().Add(48 * time.Hour)
	p := newTestCallbackParams()
	p.GroupID = &group
	p.ExpiresAt = &exp
	cb, err := store.Create(ctx, p)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if cb.GroupID == nil || *cb.GroupID != group {
		t.Errorf("group id = %v, want %q", cb.GroupID, group)
	}
	if got := cb.ExpiresAt.Sub(exp); got > time.Millisecond || got < -time.Millisecond {
		t.Errorf("expires_at = %v, want %v", cb.ExpiresAt, exp)
	}
}

func TestGithubCallbackStore_CreateValidation(t *testing.T) {
	store := NewGithubCallbackStore(setupTestDB(t))
	ctx := context.Background()

	cases := []struct {
		name  string
		mutMe func(p *CreateGithubCallbackParams)
	}{
		{"empty chat", func(p *CreateGithubCallbackParams) { p.TargetChatID = "" }},
		{"empty owner", func(p *CreateGithubCallbackParams) { p.RepoOwner = "" }},
		{"empty name", func(p *CreateGithubCallbackParams) { p.RepoName = "" }},
		{"zero pr", func(p *CreateGithubCallbackParams) { p.PRNumber = 0 }},
		{"negative pr", func(p *CreateGithubCallbackParams) { p.PRNumber = -3 }},
		{"unknown trigger", func(p *CreateGithubCallbackParams) { p.Trigger = "exploded" }},
		{"empty message", func(p *CreateGithubCallbackParams) { p.Message = "" }},
		{"past expiry", func(p *CreateGithubCallbackParams) {
			past := time.Now().UTC().Add(-time.Hour)
			p.ExpiresAt = &past
		}},
		{"expiry beyond cap", func(p *CreateGithubCallbackParams) {
			far := time.Now().UTC().Add(GithubCallbackMaxExpiry + time.Hour)
			p.ExpiresAt = &far
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestCallbackParams()
			tc.mutMe(&p)
			_, err := store.Create(ctx, p)
			if !errors.Is(err, ErrGithubCallbackInvalid) {
				t.Fatalf("err = %v, want ErrGithubCallbackInvalid", err)
			}
		})
	}
}

// TestGithubCallbackStore_AcceptsEveryCanonicalTrigger pins the store's
// validation to the canonical vocabulary. The store is the authority that
// rejects a registration, so if its notion of "valid trigger" ever drifts from
// githubcallback.ValidTriggers() the CLI, MCP schema, and evaluator would all
// accept a trigger the write path silently refuses. Asserting acceptance of the
// whole canonical list (and rejection of a bogus value) makes that drift
// impossible to land green.
func TestGithubCallbackStore_AcceptsEveryCanonicalTrigger(t *testing.T) {
	store := NewGithubCallbackStore(setupTestDB(t))
	ctx := context.Background()

	triggers := githubcallback.ValidTriggers()
	if len(triggers) == 0 {
		t.Fatal("ValidTriggers() is empty; the canonical vocabulary cannot be empty")
	}
	for _, tr := range triggers {
		t.Run(string(tr), func(t *testing.T) {
			p := newTestCallbackParams()
			p.Trigger = tr
			cb, err := store.Create(ctx, p)
			if err != nil {
				t.Fatalf("canonical trigger %q rejected by the store: %v", tr, err)
			}
			if cb.Trigger != tr {
				t.Fatalf("stored trigger = %q, want %q (the store must persist the value verbatim)", cb.Trigger, tr)
			}
		})
	}

	// Exact membership: a near-miss must not be normalized into acceptance, or
	// the stored value would stop being a canonical trigger string.
	for _, bogus := range []models.GithubCallbackTrigger{"", "MERGED", " merged ", "merged\n", "ready-for-review", "checks_passed_readyy"} {
		p := newTestCallbackParams()
		p.Trigger = bogus
		if _, err := store.Create(ctx, p); !errors.Is(err, ErrGithubCallbackInvalid) {
			t.Errorf("trigger %q: err = %v, want ErrGithubCallbackInvalid", bogus, err)
		}
	}
}

func TestGithubCallbackStore_CreateRejectsCoSatisfiableGroup(t *testing.T) {
	store := NewGithubCallbackStore(setupTestDB(t))
	ctx := context.Background()
	group := "release-gate"

	first := newTestCallbackParams()
	first.GroupID = &group
	first.Trigger = models.GithubCallbackTriggerChecksPassed
	if _, err := store.Create(ctx, first); err != nil {
		t.Fatalf("create first: %v", err)
	}

	second := newTestCallbackParams()
	second.GroupID = &group
	second.Trigger = models.GithubCallbackTriggerChecksPassedReady
	_, err := store.Create(ctx, second)
	if !errors.Is(err, ErrGithubCallbackInvalid) {
		t.Fatalf("co-satisfiable create err = %v, want ErrGithubCallbackInvalid", err)
	}
	if !strings.Contains(err.Error(), "checks_passed") || !strings.Contains(err.Error(), "checks_passed_ready") {
		t.Fatalf("error %q should name both triggers", err.Error())
	}
}

func TestGithubCallbackStore_CreateAcceptsMutuallyExclusiveGroup(t *testing.T) {
	store := NewGithubCallbackStore(setupTestDB(t))
	ctx := context.Background()
	group := "closed-or-merged"

	merged := newTestCallbackParams()
	merged.GroupID = &group
	merged.Trigger = models.GithubCallbackTriggerMerged
	if _, err := store.Create(ctx, merged); err != nil {
		t.Fatalf("create merged: %v", err)
	}

	closed := newTestCallbackParams()
	closed.GroupID = &group
	closed.Trigger = models.GithubCallbackTriggerClosed
	if _, err := store.Create(ctx, closed); err != nil {
		t.Fatalf("mutually-exclusive create should succeed: %v", err)
	}
}

func TestGithubCallbackStore_GetNotFound(t *testing.T) {
	store := NewGithubCallbackStore(setupTestDB(t))
	_, err := store.Get(context.Background(), "nope")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err = %v, want sql.ErrNoRows", err)
	}
}

func TestGithubCallbackStore_ListOrderingAndFilter(t *testing.T) {
	store := NewGithubCallbackStore(setupTestDB(t))
	ctx := context.Background()

	// Three callbacks; two share a chat, one differs.
	p1 := newTestCallbackParams()
	p1.TargetChatID = "chat-A"
	first, err := store.Create(ctx, p1)
	if err != nil {
		t.Fatalf("create1: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	p2 := newTestCallbackParams()
	p2.TargetChatID = "chat-A"
	p2.Trigger = models.GithubCallbackTriggerClosed
	second, err := store.Create(ctx, p2)
	if err != nil {
		t.Fatalf("create2: %v", err)
	}
	p3 := newTestCallbackParams()
	p3.TargetChatID = "chat-B"
	if _, err := store.Create(ctx, p3); err != nil {
		t.Fatalf("create3: %v", err)
	}

	chatA := "chat-A"
	got, err := store.List(ctx, ListGithubCallbacksFilter{TargetChatID: &chatA})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].ID != first.ID || got[1].ID != second.ID {
		t.Errorf("ordering = [%s, %s], want [%s, %s] (created_at asc)", got[0].ID, got[1].ID, first.ID, second.ID)
	}

	// Filter by trigger narrows further.
	trig := models.GithubCallbackTriggerClosed
	got2, err := store.List(ctx, ListGithubCallbacksFilter{TargetChatID: &chatA, Trigger: &trig})
	if err != nil {
		t.Fatalf("list2: %v", err)
	}
	if len(got2) != 1 || got2[0].ID != second.ID {
		t.Fatalf("filtered = %v, want only %s", got2, second.ID)
	}
}

func TestGithubCallbackStore_DeleteOutcomes(t *testing.T) {
	store := NewGithubCallbackStore(setupTestDB(t))
	ctx := context.Background()
	cb, err := store.Create(ctx, newTestCallbackParams())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	outcome, err := store.Delete(ctx, cb.ID, "other-chat")
	if !errors.Is(err, ErrGithubCallbackNotOwned) {
		t.Fatalf("wrong-owner delete err = %v, want ErrGithubCallbackNotOwned", err)
	}
	if outcome != DeleteGithubCallbackOutcomeNotOwned {
		t.Fatalf("wrong-owner outcome = %q, want not_owned", outcome)
	}
	stillThere, err := store.Get(ctx, cb.ID)
	if err != nil {
		t.Fatalf("row should survive not_owned delete: %v", err)
	}
	if stillThere.State != models.GithubCallbackStateActive {
		t.Fatalf("state after not_owned delete = %q, want active", stillThere.State)
	}

	outcome, err = store.Delete(ctx, cb.ID, cb.TargetChatID)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if outcome != DeleteGithubCallbackOutcomeDeleted {
		t.Fatalf("delete outcome = %q, want deleted", outcome)
	}
	outcome, err = store.Delete(ctx, cb.ID, cb.TargetChatID)
	if err != nil {
		t.Fatalf("second delete should return not_found without store error: %v", err)
	}
	if outcome != DeleteGithubCallbackOutcomeNotFound {
		t.Fatalf("second delete outcome = %q, want not_found", outcome)
	}
	outcome, err = store.Delete(ctx, "never-existed", "")
	if err != nil {
		t.Fatalf("delete absent should return not_found without store error: %v", err)
	}
	if outcome != DeleteGithubCallbackOutcomeNotFound {
		t.Fatalf("delete absent outcome = %q, want not_found", outcome)
	}
}

func TestGithubCallbackStore_ExpireOverdue(t *testing.T) {
	store := NewGithubCallbackStore(setupTestDB(t))
	ctx := context.Background()

	soon := time.Now().UTC().Add(time.Second)
	p := newTestCallbackParams()
	p.ExpiresAt = &soon
	overdue, err := store.Create(ctx, p)
	if err != nil {
		t.Fatalf("create overdue: %v", err)
	}
	fresh, err := store.Create(ctx, newTestCallbackParams()) // 24h default
	if err != nil {
		t.Fatalf("create fresh: %v", err)
	}

	n, err := store.ExpireOverdue(ctx, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if n != 1 {
		t.Fatalf("expired count = %d, want 1", n)
	}
	got, _ := store.Get(ctx, overdue.ID)
	if got.State != models.GithubCallbackStateExpired {
		t.Errorf("overdue state = %q, want expired", got.State)
	}
	got2, _ := store.Get(ctx, fresh.ID)
	if got2.State != models.GithubCallbackStateActive {
		t.Errorf("fresh state = %q, want active", got2.State)
	}
}

func TestGithubCallbackStore_LeaseConcurrentSingleWinner(t *testing.T) {
	store := NewGithubCallbackStore(setupFileDB(t))
	ctx := context.Background()
	cb, err := store.Create(ctx, newTestCallbackParams())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	const workers = 8
	now := time.Now().UTC()
	var wg sync.WaitGroup
	wins := make(chan string, workers)
	for i := range workers {
		wg.Add(1)
		owner := "worker-" + string(rune('a'+i))
		go func() {
			defer wg.Done()
			got, lerr := store.AcquireLease(ctx, cb.ID, owner, now, time.Minute)
			if lerr == nil {
				wins <- *got.LeaseOwner
			} else if !errors.Is(lerr, ErrGithubCallbackLeaseConflict) {
				t.Errorf("unexpected lease err: %v", lerr)
			}
		}()
	}
	wg.Wait()
	close(wins)
	if got := len(wins); got != 1 {
		t.Fatalf("lease winners = %d, want exactly 1", got)
	}

	after, _ := store.Get(ctx, cb.ID)
	if after.State != models.GithubCallbackStateLeased {
		t.Errorf("state = %q, want leased", after.State)
	}
}

func TestGithubCallbackStore_LeaseRecoveryAfterExpiry(t *testing.T) {
	store := NewGithubCallbackStore(setupTestDB(t))
	ctx := context.Background()
	cb, err := store.Create(ctx, newTestCallbackParams())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	t0 := time.Now().UTC()
	if _, err := store.AcquireLease(ctx, cb.ID, "worker-1", t0, time.Minute); err != nil {
		t.Fatalf("acquire1: %v", err)
	}
	// A different worker cannot steal an unexpired lease.
	if _, err := store.AcquireLease(ctx, cb.ID, "worker-2", t0.Add(30*time.Second), time.Minute); !errors.Is(err, ErrGithubCallbackLeaseConflict) {
		t.Fatalf("premature steal err = %v, want conflict", err)
	}
	// After the lease deadline passes, recovery succeeds.
	recovered, err := store.AcquireLease(ctx, cb.ID, "worker-2", t0.Add(2*time.Minute), time.Minute)
	if err != nil {
		t.Fatalf("recovery acquire: %v", err)
	}
	if recovered.LeaseOwner == nil || *recovered.LeaseOwner != "worker-2" {
		t.Errorf("lease owner = %v, want worker-2", recovered.LeaseOwner)
	}
}

func TestGithubCallbackStore_AcquireLeaseRejectsExpired(t *testing.T) {
	store := NewGithubCallbackStore(setupTestDB(t))
	ctx := context.Background()

	base := time.Now().UTC()
	soon := base.Add(time.Second)
	p := newTestCallbackParams()
	p.ExpiresAt = &soon
	cb, err := store.Create(ctx, p)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// A worker that reaches acquisition after expires_at has passed (but before the
	// row was swept) must not be able to lease the overdue callback — otherwise it
	// could deliver an expired callback before MarkDelivered rejects the state change.
	if _, err := store.AcquireLease(ctx, cb.ID, "worker-1", base.Add(time.Hour), time.Minute); !errors.Is(err, ErrGithubCallbackLeaseConflict) {
		t.Fatalf("expired acquire err = %v, want conflict", err)
	}
	got, _ := store.Get(ctx, cb.ID)
	if got.State != models.GithubCallbackStateActive {
		t.Errorf("state = %q, want active (unchanged)", got.State)
	}
	if got.LeaseOwner != nil {
		t.Errorf("lease owner = %v, want nil (not claimed)", got.LeaseOwner)
	}
}

func TestGithubCallbackStore_TriggerGroupSingleWinnerCancelsSiblings(t *testing.T) {
	store := NewGithubCallbackStore(setupFileDB(t))
	ctx := context.Background()

	group := "race-group"
	var ids []string
	for i := range 2 {
		p := newTestCallbackParams()
		p.GroupID = &group
		// Mutually exclusive triggers are allowed to share a one-shot group.
		switch i {
		case 0:
			p.Trigger = models.GithubCallbackTriggerMerged
		case 1:
			p.Trigger = models.GithubCallbackTriggerClosed
		}
		cb, err := store.Create(ctx, p)
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		ids = append(ids, cb.ID)
	}

	now := time.Now().UTC()
	var wg sync.WaitGroup
	winners := make(chan string, len(ids))
	for _, id := range ids {
		wg.Add(1)
		id := id
		go func() {
			defer wg.Done()
			got, err := store.TriggerGroup(ctx, id, "webhook", now)
			if err == nil {
				winners <- got.ID
			} else if !errors.Is(err, ErrGithubCallbackTriggerConflict) {
				t.Errorf("unexpected trigger err: %v", err)
			}
		}()
	}
	wg.Wait()
	close(winners)

	if got := len(winners); got != 1 {
		t.Fatalf("trigger winners = %d, want exactly 1", got)
	}
	winner := <-winners

	var triggered, canceled int
	for _, id := range ids {
		cb, _ := store.Get(ctx, id)
		switch cb.State {
		case models.GithubCallbackStateTriggered:
			triggered++
			if cb.ID != winner {
				t.Errorf("triggered id %s != winner %s", cb.ID, winner)
			}
			if cb.TriggeredAt == nil {
				t.Errorf("winner %s has nil triggered_at", cb.ID)
			}
		case models.GithubCallbackStateCanceled:
			canceled++
		default:
			t.Errorf("callback %s in unexpected state %q", cb.ID, cb.State)
		}
	}
	if triggered != 1 || canceled != 1 {
		t.Fatalf("triggered=%d canceled=%d, want 1 and 1", triggered, canceled)
	}
}

func TestGithubCallbackStore_TriggerUngroupedNoSiblings(t *testing.T) {
	store := NewGithubCallbackStore(setupTestDB(t))
	ctx := context.Background()
	cb, err := store.Create(ctx, newTestCallbackParams()) // no group
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := store.TriggerGroup(ctx, cb.ID, "merged", time.Now().UTC())
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}
	if got.State != models.GithubCallbackStateTriggered {
		t.Errorf("state = %q, want triggered", got.State)
	}
	// Triggering an already-triggered callback conflicts.
	if _, err := store.TriggerGroup(ctx, cb.ID, "merged", time.Now().UTC()); !errors.Is(err, ErrGithubCallbackTriggerConflict) {
		t.Fatalf("re-trigger err = %v, want conflict", err)
	}
}

func TestGithubCallbackStore_TriggerRejectsOverdue(t *testing.T) {
	store := NewGithubCallbackStore(setupTestDB(t))
	ctx := context.Background()

	group := "overdue-group"
	base := time.Now().UTC()
	soon := base.Add(time.Second)

	target := newTestCallbackParams()
	target.GroupID = &group
	target.ExpiresAt = &soon
	overdue, err := store.Create(ctx, target)
	if err != nil {
		t.Fatalf("create overdue: %v", err)
	}
	siblingParams := newTestCallbackParams()
	siblingParams.GroupID = &group
	siblingParams.Trigger = models.GithubCallbackTriggerClosed
	sibling, err := store.Create(ctx, siblingParams)
	if err != nil {
		t.Fatalf("create sibling: %v", err)
	}

	// now is past the callback's expires_at but expiry has not been swept yet, so
	// the row is still active. Triggering it must be rejected (state guard alone
	// would let it through) and must not cancel its siblings.
	if _, err := store.TriggerGroup(ctx, overdue.ID, "merged", base.Add(time.Hour)); !errors.Is(err, ErrGithubCallbackTriggerConflict) {
		t.Fatalf("trigger overdue err = %v, want conflict", err)
	}

	got, _ := store.Get(ctx, overdue.ID)
	if got.State != models.GithubCallbackStateActive {
		t.Errorf("overdue state = %q, want active (unchanged)", got.State)
	}
	if got.TriggeredAt != nil {
		t.Errorf("overdue triggered_at = %v, want nil", got.TriggeredAt)
	}
	gotSibling, _ := store.Get(ctx, sibling.ID)
	if gotSibling.State != models.GithubCallbackStateActive {
		t.Errorf("sibling state = %q, want active (not canceled)", gotSibling.State)
	}
}

func TestGithubCallbackStore_MarkDeliveredRequiresOwner(t *testing.T) {
	store := NewGithubCallbackStore(setupTestDB(t))
	ctx := context.Background()
	cb, err := store.Create(ctx, newTestCallbackParams())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	now := time.Now().UTC()
	if _, err := store.TriggerGroup(ctx, cb.ID, "merged", now); err != nil {
		t.Fatalf("trigger: %v", err)
	}
	if _, err := store.AcquireLease(ctx, cb.ID, "worker-1", now, time.Minute); err != nil {
		t.Fatalf("lease: %v", err)
	}
	// Wrong owner cannot mark delivered.
	if err := store.MarkDelivered(ctx, cb.ID, "worker-2", now); !errors.Is(err, ErrGithubCallbackLeaseConflict) {
		t.Fatalf("wrong-owner deliver err = %v, want conflict", err)
	}
	if err := store.MarkDelivered(ctx, cb.ID, "worker-1", now); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	got, _ := store.Get(ctx, cb.ID)
	if got.State != models.GithubCallbackStateDelivered || got.DeliveredAt == nil {
		t.Errorf("state=%q delivered_at=%v, want delivered", got.State, got.DeliveredAt)
	}
	if got.LeaseOwner != nil {
		t.Errorf("lease owner = %v, want nil after delivery", got.LeaseOwner)
	}
}

func TestGithubCallbackStore_MarkDeliveredRejectsExpired(t *testing.T) {
	store := NewGithubCallbackStore(setupTestDB(t))
	ctx := context.Background()

	base := time.Now().UTC()
	soon := base.Add(time.Second)
	p := newTestCallbackParams()
	p.ExpiresAt = &soon
	cb, err := store.Create(ctx, p)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Trigger and lease while still within expiry.
	if _, err := store.TriggerGroup(ctx, cb.ID, "merged", base); err != nil {
		t.Fatalf("trigger: %v", err)
	}
	if _, err := store.AcquireLease(ctx, cb.ID, "worker-1", base, time.Minute); err != nil {
		t.Fatalf("lease: %v", err)
	}

	// Delivery runs long enough that expires_at passes while the worker still owns
	// the lease. Marking delivered must be rejected — the row belongs to the
	// expired sweep, not a delivered terminal state.
	if err := store.MarkDelivered(ctx, cb.ID, "worker-1", base.Add(time.Hour)); !errors.Is(err, ErrGithubCallbackLeaseConflict) {
		t.Fatalf("expired deliver err = %v, want conflict", err)
	}
	got, _ := store.Get(ctx, cb.ID)
	if got.State != models.GithubCallbackStateTriggered {
		t.Errorf("state = %q, want triggered (not delivered)", got.State)
	}
	if got.DeliveredAt != nil {
		t.Errorf("delivered_at = %v, want nil", got.DeliveredAt)
	}
}

// TestGithubCallbackStore_RetryPersistsAcrossReopen proves retry diagnostics and
// the claimed lifecycle survive a store/DB reopen, and that the message body is
// never copied into diagnostic fields.
func TestGithubCallbackStore_RetryPersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "bossd.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	dbtest.Apply(t, db)
	store := NewGithubCallbackStore(db)

	cb, err := store.Create(ctx, newTestCallbackParams())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	now := time.Now().UTC()
	if _, err := store.TriggerGroup(ctx, cb.ID, "merged", now); err != nil {
		t.Fatalf("trigger: %v", err)
	}
	if _, err := store.AcquireLease(ctx, cb.ID, "worker-1", now, time.Minute); err != nil {
		t.Fatalf("lease: %v", err)
	}
	nextAt := now.Add(5 * time.Minute)
	if err := store.ScheduleRetry(ctx, cb.ID, "worker-1", ScheduleGithubCallbackRetryParams{
		NextAttemptAt: nextAt,
		LastError:     "delivery timed out",
		LastEvent:     "attempt-1",
	}); err != nil {
		t.Fatalf("schedule retry: %v", err)
	}
	_ = db.Close()

	// Reopen the same file with a fresh store.
	db2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })
	store2 := NewGithubCallbackStore(db2)

	got, err := store2.Get(ctx, cb.ID)
	if err != nil {
		t.Fatalf("get after reopen: %v", err)
	}
	if got.AttemptCount != 1 {
		t.Errorf("attempt_count = %d, want 1", got.AttemptCount)
	}
	if got.LastError == nil || *got.LastError != "delivery timed out" {
		t.Errorf("last_error = %v, want the failure diagnostic", got.LastError)
	}
	if got.LeaseOwner != nil {
		t.Errorf("lease owner = %v, want nil (released for retry)", got.LeaseOwner)
	}
	if got.NextAttemptAt == nil {
		t.Fatalf("next_attempt_at nil, want persisted")
	}
	if d := got.NextAttemptAt.Sub(nextAt); d > time.Millisecond || d < -time.Millisecond {
		t.Errorf("next_attempt_at = %v, want %v", got.NextAttemptAt, nextAt)
	}
	if got.State != models.GithubCallbackStateTriggered {
		t.Errorf("state = %q, want triggered (claim retained across reopen)", got.State)
	}
	// Secret hygiene: the message body must never leak into diagnostics.
	if got.LastError != nil && *got.LastError == got.Message {
		t.Error("last_error must not contain the registered message body")
	}
	if got.LastEvent != nil && *got.LastEvent == got.Message {
		t.Error("last_event must not contain the registered message body")
	}

	// A retry claim cannot be re-acquired before next_attempt_at, but can after.
	if _, err := store2.AcquireLease(ctx, cb.ID, "worker-2", now.Add(time.Minute), time.Minute); !errors.Is(err, ErrGithubCallbackLeaseConflict) {
		t.Fatalf("early re-lease err = %v, want conflict (backoff)", err)
	}
	if _, err := store2.AcquireLease(ctx, cb.ID, "worker-2", nextAt.Add(time.Second), time.Minute); err != nil {
		t.Fatalf("re-lease after backoff: %v", err)
	}
}
