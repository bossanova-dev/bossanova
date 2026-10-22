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
	"github.com/recurser/bossalib/sqlutil"
	"github.com/recurser/bossd/internal/dbtest"
	"github.com/recurser/bossd/internal/domain"
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

func newTestCallbackParams() CreateGitHubCallbackParams {
	return CreateGitHubCallbackParams{
		TargetChatID: "chat-1",
		RepoOwner:    "Owner",
		RepoName:     "Repo",
		PRNumber:     42,
		Trigger:      models.GitHubCallbackTriggerMerged,
		Message:      "secret prompt body",
	}
}

func TestGitHubCallbackStore_CreateDefaultsAndNormalizes(t *testing.T) {
	store := NewGitHubCallbackStore(setupTestDB(t))
	ctx := context.Background()

	before := time.Now().UTC()
	cb, _, err := store.Create(ctx, newTestCallbackParams())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if cb.State != models.GitHubCallbackStateActive {
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
	wantMin := before.Add(GitHubCallbackDefaultExpiry - time.Minute)
	wantMax := time.Now().UTC().Add(GitHubCallbackDefaultExpiry + time.Minute)
	if cb.ExpiresAt.Before(wantMin) || cb.ExpiresAt.After(wantMax) {
		t.Errorf("expires_at = %v, want ~24h from now", cb.ExpiresAt)
	}
}

func TestGitHubCallbackStore_CreateExplicitExpiryAndGroup(t *testing.T) {
	store := NewGitHubCallbackStore(setupTestDB(t))
	ctx := context.Background()

	group := "group-A"
	exp := time.Now().UTC().Add(48 * time.Hour)
	p := newTestCallbackParams()
	p.GroupID = &group
	p.ExpiresAt = &exp
	cb, _, err := store.Create(ctx, p)
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

func TestGitHubCallbackStore_CreateValidation(t *testing.T) {
	store := NewGitHubCallbackStore(setupTestDB(t))
	ctx := context.Background()

	cases := []struct {
		name  string
		mutMe func(p *CreateGitHubCallbackParams)
	}{
		{"empty chat", func(p *CreateGitHubCallbackParams) { p.TargetChatID = "" }},
		{"empty owner", func(p *CreateGitHubCallbackParams) { p.RepoOwner = "" }},
		{"empty name", func(p *CreateGitHubCallbackParams) { p.RepoName = "" }},
		{"zero pr", func(p *CreateGitHubCallbackParams) { p.PRNumber = 0 }},
		{"negative pr", func(p *CreateGitHubCallbackParams) { p.PRNumber = -3 }},
		{"unknown trigger", func(p *CreateGitHubCallbackParams) { p.Trigger = "exploded" }},
		{"empty message", func(p *CreateGitHubCallbackParams) { p.Message = "" }},
		{"past expiry", func(p *CreateGitHubCallbackParams) {
			past := time.Now().UTC().Add(-time.Hour)
			p.ExpiresAt = &past
		}},
		{"expiry beyond cap", func(p *CreateGitHubCallbackParams) {
			far := time.Now().UTC().Add(GitHubCallbackMaxExpiry + time.Hour)
			p.ExpiresAt = &far
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestCallbackParams()
			tc.mutMe(&p)
			_, _, err := store.Create(ctx, p)
			if !errors.Is(err, ErrGitHubCallbackInvalid) {
				t.Fatalf("err = %v, want ErrGitHubCallbackInvalid", err)
			}
		})
	}
}

// TestGitHubCallbackStore_AcceptsEveryCanonicalTrigger pins the store's
// validation to the canonical vocabulary. The store is the authority that
// rejects a registration, so if its notion of "valid trigger" ever drifts from
// githubcallback.ValidTriggers() the CLI, MCP schema, and evaluator would all
// accept a trigger the write path silently refuses. Asserting acceptance of the
// whole canonical list (and rejection of a bogus value) makes that drift
// impossible to land green.
func TestGitHubCallbackStore_AcceptsEveryCanonicalTrigger(t *testing.T) {
	store := NewGitHubCallbackStore(setupTestDB(t))
	ctx := context.Background()

	triggers := githubcallback.ValidTriggers()
	if len(triggers) == 0 {
		t.Fatal("ValidTriggers() is empty; the canonical vocabulary cannot be empty")
	}
	for _, tr := range triggers {
		t.Run(string(tr), func(t *testing.T) {
			p := newTestCallbackParams()
			p.Trigger = tr
			cb, _, err := store.Create(ctx, p)
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
	for _, bogus := range []models.GitHubCallbackTrigger{"", "MERGED", " merged ", "merged\n", "ready-for-review", "checks_passed_readyy"} {
		p := newTestCallbackParams()
		p.Trigger = bogus
		if _, _, err := store.Create(ctx, p); !errors.Is(err, ErrGitHubCallbackInvalid) {
			t.Errorf("trigger %q: err = %v, want ErrGitHubCallbackInvalid", bogus, err)
		}
	}
}

func TestGitHubCallbackStore_CreateRejectsCoSatisfiableGroup(t *testing.T) {
	store := NewGitHubCallbackStore(setupTestDB(t))
	ctx := context.Background()
	group := "release-gate"

	first := newTestCallbackParams()
	first.GroupID = &group
	first.Trigger = models.GitHubCallbackTriggerChecksPassed
	if _, _, err := store.Create(ctx, first); err != nil {
		t.Fatalf("create first: %v", err)
	}

	second := newTestCallbackParams()
	second.GroupID = &group
	second.Trigger = models.GitHubCallbackTriggerChecksPassedReady
	_, _, err := store.Create(ctx, second)
	if !errors.Is(err, ErrGitHubCallbackInvalid) {
		t.Fatalf("co-satisfiable create err = %v, want ErrGitHubCallbackInvalid", err)
	}
	if !strings.Contains(err.Error(), "checks_passed") || !strings.Contains(err.Error(), "checks_passed_ready") {
		t.Fatalf("error %q should name both triggers", err.Error())
	}
}

func TestGitHubCallbackStore_CreateAcceptsMutuallyExclusiveGroup(t *testing.T) {
	store := NewGitHubCallbackStore(setupTestDB(t))
	ctx := context.Background()
	group := "closed-or-merged"

	merged := newTestCallbackParams()
	merged.GroupID = &group
	merged.Trigger = models.GitHubCallbackTriggerMerged
	if _, _, err := store.Create(ctx, merged); err != nil {
		t.Fatalf("create merged: %v", err)
	}

	closed := newTestCallbackParams()
	closed.GroupID = &group
	closed.Trigger = models.GitHubCallbackTriggerClosed
	if _, _, err := store.Create(ctx, closed); err != nil {
		t.Fatalf("mutually-exclusive create should succeed: %v", err)
	}
}

func TestGitHubCallbackStore_GetNotFound(t *testing.T) {
	store := NewGitHubCallbackStore(setupTestDB(t))
	ctx, now := context.Background(), time.Now()
	cases := map[string]func() error{
		"get":         func() error { _, err := store.Get(ctx, "nope"); return err },
		"trigger":     func() error { _, err := store.MarkGroupTriggered(ctx, "nope", "merged", now); return err },
		"lease":       func() error { _, err := store.SetLease(ctx, "nope", "worker", now, time.Minute); return err },
		"release":     func() error { return store.MarkLeaseReleased(ctx, "nope", "worker", now) },
		"baseline":    func() error { return store.MarkBaselineObserved(ctx, "nope", now) },
		"unreachable": func() error { return store.MarkUnreachable(ctx, "nope", "closed", now) },
		"cancel":      func() error { return store.MarkCanceled(ctx, "nope", "closed", now) },
		"deliver":     func() error { return store.MarkDelivered(ctx, "nope", "worker", now) },
		"retry": func() error {
			return store.SetRetry(ctx, "nope", "worker", ScheduleGitHubCallbackRetryParams{NextAttemptAt: now})
		},
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("err = %v, want domain.ErrNotFound", err)
			}
		})
	}
}

func TestGitHubCallbackStore_ListOrderingAndFilter(t *testing.T) {
	store := NewGitHubCallbackStore(setupTestDB(t))
	ctx := context.Background()

	// Three callbacks; two share a chat, one differs.
	p1 := newTestCallbackParams()
	p1.TargetChatID = "chat-A"
	first, _, err := store.Create(ctx, p1)
	if err != nil {
		t.Fatalf("create1: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	p2 := newTestCallbackParams()
	p2.TargetChatID = "chat-A"
	p2.Trigger = models.GitHubCallbackTriggerClosed
	second, _, err := store.Create(ctx, p2)
	if err != nil {
		t.Fatalf("create2: %v", err)
	}
	p3 := newTestCallbackParams()
	p3.TargetChatID = "chat-B"
	if _, _, err := store.Create(ctx, p3); err != nil {
		t.Fatalf("create3: %v", err)
	}

	chatA := "chat-A"
	got, err := store.List(ctx, ListGitHubCallbacksFilter{TargetChatID: &chatA})
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
	trig := models.GitHubCallbackTriggerClosed
	got2, err := store.List(ctx, ListGitHubCallbacksFilter{TargetChatID: &chatA, Trigger: &trig})
	if err != nil {
		t.Fatalf("list2: %v", err)
	}
	if len(got2) != 1 || got2[0].ID != second.ID {
		t.Fatalf("filtered = %v, want only %s", got2, second.ID)
	}
}

func TestGitHubCallbackStore_DeleteOutcomes(t *testing.T) {
	store := NewGitHubCallbackStore(setupTestDB(t))
	ctx := context.Background()
	cb, _, err := store.Create(ctx, newTestCallbackParams())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	outcome, err := store.Delete(ctx, cb.ID, "other-chat")
	if !errors.Is(err, ErrGitHubCallbackNotOwned) {
		t.Fatalf("wrong-owner delete err = %v, want ErrGitHubCallbackNotOwned", err)
	}
	if outcome != DeleteGitHubCallbackOutcomeNotOwned {
		t.Fatalf("wrong-owner outcome = %q, want not_owned", outcome)
	}
	stillThere, err := store.Get(ctx, cb.ID)
	if err != nil {
		t.Fatalf("row should survive not_owned delete: %v", err)
	}
	if stillThere.State != models.GitHubCallbackStateActive {
		t.Fatalf("state after not_owned delete = %q, want active", stillThere.State)
	}

	outcome, err = store.Delete(ctx, cb.ID, cb.TargetChatID)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if outcome != DeleteGitHubCallbackOutcomeDeleted {
		t.Fatalf("delete outcome = %q, want deleted", outcome)
	}
	outcome, err = store.Delete(ctx, cb.ID, cb.TargetChatID)
	if err != nil {
		t.Fatalf("second delete should return not_found without store error: %v", err)
	}
	if outcome != DeleteGitHubCallbackOutcomeNotFound {
		t.Fatalf("second delete outcome = %q, want not_found", outcome)
	}
	outcome, err = store.Delete(ctx, "never-existed", "")
	if err != nil {
		t.Fatalf("delete absent should return not_found without store error: %v", err)
	}
	if outcome != DeleteGitHubCallbackOutcomeNotFound {
		t.Fatalf("delete absent outcome = %q, want not_found", outcome)
	}
}

func TestGitHubCallbackStore_ExpireOverdue(t *testing.T) {
	store := NewGitHubCallbackStore(setupTestDB(t))
	ctx := context.Background()

	soon := time.Now().UTC().Add(time.Second)
	p := newTestCallbackParams()
	p.ExpiresAt = &soon
	overdue, _, err := store.Create(ctx, p)
	if err != nil {
		t.Fatalf("create overdue: %v", err)
	}
	fresh, _, err := store.Create(ctx, newTestCallbackParams()) // 24h default
	if err != nil {
		t.Fatalf("create fresh: %v", err)
	}

	n, err := store.MarkExpired(ctx, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("expire: %v", err)
	}
	if n != 1 {
		t.Fatalf("expired count = %d, want 1", n)
	}
	got, _ := store.Get(ctx, overdue.ID)
	if got.State != models.GitHubCallbackStateExpired {
		t.Errorf("overdue state = %q, want expired", got.State)
	}
	got2, _ := store.Get(ctx, fresh.ID)
	if got2.State != models.GitHubCallbackStateActive {
		t.Errorf("fresh state = %q, want active", got2.State)
	}
}

func TestGitHubCallbackStore_LeaseConcurrentSingleWinner(t *testing.T) {
	store := NewGitHubCallbackStore(setupFileDB(t))
	ctx := context.Background()
	cb, _, err := store.Create(ctx, newTestCallbackParams())
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
			got, lerr := store.SetLease(ctx, cb.ID, owner, now, time.Minute)
			if lerr == nil {
				wins <- *got.LeaseOwner
			} else if !errors.Is(lerr, ErrGitHubCallbackLeaseConflict) {
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
	if after.State != models.GitHubCallbackStateLeased {
		t.Errorf("state = %q, want leased", after.State)
	}
}

func TestGitHubCallbackStore_LeaseRecoveryAfterExpiry(t *testing.T) {
	store := NewGitHubCallbackStore(setupTestDB(t))
	ctx := context.Background()
	cb, _, err := store.Create(ctx, newTestCallbackParams())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	t0 := time.Now().UTC()
	if _, err := store.SetLease(ctx, cb.ID, "worker-1", t0, time.Minute); err != nil {
		t.Fatalf("acquire1: %v", err)
	}
	// A different worker cannot steal an unexpired lease.
	if _, err := store.SetLease(ctx, cb.ID, "worker-2", t0.Add(30*time.Second), time.Minute); !errors.Is(err, ErrGitHubCallbackLeaseConflict) {
		t.Fatalf("premature steal err = %v, want conflict", err)
	}
	// After the lease deadline passes, recovery succeeds.
	recovered, err := store.SetLease(ctx, cb.ID, "worker-2", t0.Add(2*time.Minute), time.Minute)
	if err != nil {
		t.Fatalf("recovery acquire: %v", err)
	}
	if recovered.LeaseOwner == nil || *recovered.LeaseOwner != "worker-2" {
		t.Errorf("lease owner = %v, want worker-2", recovered.LeaseOwner)
	}
}

func TestGitHubCallbackStore_AcquireLeaseRejectsExpired(t *testing.T) {
	store := NewGitHubCallbackStore(setupTestDB(t))
	ctx := context.Background()

	base := time.Now().UTC()
	soon := base.Add(time.Second)
	p := newTestCallbackParams()
	p.ExpiresAt = &soon
	cb, _, err := store.Create(ctx, p)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// A worker that reaches acquisition after expires_at has passed (but before the
	// row was swept) must not be able to lease the overdue callback — otherwise it
	// could deliver an expired callback before MarkDelivered rejects the state change.
	if _, err := store.SetLease(ctx, cb.ID, "worker-1", base.Add(time.Hour), time.Minute); !errors.Is(err, ErrGitHubCallbackLeaseConflict) {
		t.Fatalf("expired acquire err = %v, want conflict", err)
	}
	got, _ := store.Get(ctx, cb.ID)
	if got.State != models.GitHubCallbackStateActive {
		t.Errorf("state = %q, want active (unchanged)", got.State)
	}
	if got.LeaseOwner != nil {
		t.Errorf("lease owner = %v, want nil (not claimed)", got.LeaseOwner)
	}
}

func TestGitHubCallbackStore_TriggerGroupSingleWinnerCancelsSiblings(t *testing.T) {
	store := NewGitHubCallbackStore(setupFileDB(t))
	ctx := context.Background()

	group := "race-group"
	var ids []string
	for i := range 2 {
		p := newTestCallbackParams()
		p.GroupID = &group
		// Mutually exclusive triggers are allowed to share a one-shot group.
		switch i {
		case 0:
			p.Trigger = models.GitHubCallbackTriggerMerged
		case 1:
			p.Trigger = models.GitHubCallbackTriggerClosed
		}
		cb, _, err := store.Create(ctx, p)
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
			got, err := store.MarkGroupTriggered(ctx, id, "webhook", now)
			if err == nil {
				winners <- got.ID
			} else if !errors.Is(err, ErrGitHubCallbackTriggerConflict) {
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
		case models.GitHubCallbackStateTriggered:
			triggered++
			if cb.ID != winner {
				t.Errorf("triggered id %s != winner %s", cb.ID, winner)
			}
			if cb.TriggeredAt == nil {
				t.Errorf("winner %s has nil triggered_at", cb.ID)
			}
		case models.GitHubCallbackStateCanceled:
			canceled++
		default:
			t.Errorf("callback %s in unexpected state %q", cb.ID, cb.State)
		}
	}
	if triggered != 1 || canceled != 1 {
		t.Fatalf("triggered=%d canceled=%d, want 1 and 1", triggered, canceled)
	}
}

func TestGitHubCallbackStore_TriggerUngroupedNoSiblings(t *testing.T) {
	store := NewGitHubCallbackStore(setupTestDB(t))
	ctx := context.Background()
	cb, _, err := store.Create(ctx, newTestCallbackParams()) // no group
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := store.MarkGroupTriggered(ctx, cb.ID, "merged", time.Now().UTC())
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}
	if got.State != models.GitHubCallbackStateTriggered {
		t.Errorf("state = %q, want triggered", got.State)
	}
	// Triggering an already-triggered callback conflicts.
	if _, err := store.MarkGroupTriggered(ctx, cb.ID, "merged", time.Now().UTC()); !errors.Is(err, ErrGitHubCallbackTriggerConflict) {
		t.Fatalf("re-trigger err = %v, want conflict", err)
	}
}

func TestGitHubCallbackStore_TriggerRejectsOverdue(t *testing.T) {
	store := NewGitHubCallbackStore(setupTestDB(t))
	ctx := context.Background()

	group := "overdue-group"
	base := time.Now().UTC()
	soon := base.Add(time.Second)

	target := newTestCallbackParams()
	target.GroupID = &group
	target.ExpiresAt = &soon
	overdue, _, err := store.Create(ctx, target)
	if err != nil {
		t.Fatalf("create overdue: %v", err)
	}
	siblingParams := newTestCallbackParams()
	siblingParams.GroupID = &group
	siblingParams.Trigger = models.GitHubCallbackTriggerClosed
	sibling, _, err := store.Create(ctx, siblingParams)
	if err != nil {
		t.Fatalf("create sibling: %v", err)
	}

	// now is past the callback's expires_at but expiry has not been swept yet, so
	// the row is still active. Triggering it must be rejected (state guard alone
	// would let it through) and must not cancel its siblings.
	if _, err := store.MarkGroupTriggered(ctx, overdue.ID, "merged", base.Add(time.Hour)); !errors.Is(err, ErrGitHubCallbackTriggerConflict) {
		t.Fatalf("trigger overdue err = %v, want conflict", err)
	}

	got, _ := store.Get(ctx, overdue.ID)
	if got.State != models.GitHubCallbackStateActive {
		t.Errorf("overdue state = %q, want active (unchanged)", got.State)
	}
	if got.TriggeredAt != nil {
		t.Errorf("overdue triggered_at = %v, want nil", got.TriggeredAt)
	}
	gotSibling, _ := store.Get(ctx, sibling.ID)
	if gotSibling.State != models.GitHubCallbackStateActive {
		t.Errorf("sibling state = %q, want active (not canceled)", gotSibling.State)
	}
}

func TestGitHubCallbackStore_MarkDeliveredRequiresOwner(t *testing.T) {
	store := NewGitHubCallbackStore(setupTestDB(t))
	ctx := context.Background()
	cb, _, err := store.Create(ctx, newTestCallbackParams())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	now := time.Now().UTC()
	if _, err := store.MarkGroupTriggered(ctx, cb.ID, "merged", now); err != nil {
		t.Fatalf("trigger: %v", err)
	}
	if _, err := store.SetLease(ctx, cb.ID, "worker-1", now, time.Minute); err != nil {
		t.Fatalf("lease: %v", err)
	}
	// Wrong owner cannot mark delivered.
	if err := store.MarkDelivered(ctx, cb.ID, "worker-2", now); !errors.Is(err, ErrGitHubCallbackLeaseConflict) {
		t.Fatalf("wrong-owner deliver err = %v, want conflict", err)
	}
	if err := store.MarkDelivered(ctx, cb.ID, "worker-1", now); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	got, _ := store.Get(ctx, cb.ID)
	if got.State != models.GitHubCallbackStateDelivered || got.DeliveredAt == nil {
		t.Errorf("state=%q delivered_at=%v, want delivered", got.State, got.DeliveredAt)
	}
	if got.LeaseOwner != nil {
		t.Errorf("lease owner = %v, want nil after delivery", got.LeaseOwner)
	}
}

func TestGitHubCallbackStore_MarkDeliveredRejectsExpired(t *testing.T) {
	store := NewGitHubCallbackStore(setupTestDB(t))
	ctx := context.Background()

	base := time.Now().UTC()
	soon := base.Add(time.Second)
	p := newTestCallbackParams()
	p.ExpiresAt = &soon
	cb, _, err := store.Create(ctx, p)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Trigger and lease while still within expiry.
	if _, err := store.MarkGroupTriggered(ctx, cb.ID, "merged", base); err != nil {
		t.Fatalf("trigger: %v", err)
	}
	if _, err := store.SetLease(ctx, cb.ID, "worker-1", base, time.Minute); err != nil {
		t.Fatalf("lease: %v", err)
	}

	// Delivery runs long enough that expires_at passes while the worker still owns
	// the lease. Marking delivered must be rejected — the row belongs to the
	// expired sweep, not a delivered terminal state.
	if err := store.MarkDelivered(ctx, cb.ID, "worker-1", base.Add(time.Hour)); !errors.Is(err, ErrGitHubCallbackLeaseConflict) {
		t.Fatalf("expired deliver err = %v, want conflict", err)
	}
	got, _ := store.Get(ctx, cb.ID)
	if got.State != models.GitHubCallbackStateTriggered {
		t.Errorf("state = %q, want triggered (not delivered)", got.State)
	}
	if got.DeliveredAt != nil {
		t.Errorf("delivered_at = %v, want nil", got.DeliveredAt)
	}
}

// TestGitHubCallbackStore_RetryPersistsAcrossReopen proves retry diagnostics and
// the claimed lifecycle survive a store/DB reopen, and that the message body is
// never copied into diagnostic fields.
func TestGitHubCallbackStore_RetryPersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "bossd.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	dbtest.Apply(t, db)
	store := NewGitHubCallbackStore(db)

	cb, _, err := store.Create(ctx, newTestCallbackParams())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	now := time.Now().UTC()
	if _, err := store.MarkGroupTriggered(ctx, cb.ID, "merged", now); err != nil {
		t.Fatalf("trigger: %v", err)
	}
	if _, err := store.SetLease(ctx, cb.ID, "worker-1", now, time.Minute); err != nil {
		t.Fatalf("lease: %v", err)
	}
	nextAt := now.Add(5 * time.Minute)
	if err := store.SetRetry(ctx, cb.ID, "worker-1", ScheduleGitHubCallbackRetryParams{
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
	store2 := NewGitHubCallbackStore(db2)

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
	if got.State != models.GitHubCallbackStateTriggered {
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
	if _, err := store2.SetLease(ctx, cb.ID, "worker-2", now.Add(time.Minute), time.Minute); !errors.Is(err, ErrGitHubCallbackLeaseConflict) {
		t.Fatalf("early re-lease err = %v, want conflict (backoff)", err)
	}
	if _, err := store2.SetLease(ctx, cb.ID, "worker-2", nextAt.Add(time.Second), time.Minute); err != nil {
		t.Fatalf("re-lease after backoff: %v", err)
	}
}

// TestGitHubCallbackTriggerTableIsSymmetric pins the invariant that the
// exclusion relation is undirected. An asymmetric row would make grouping
// validation (and, downstream, cross-group conflict detection) depend on which
// leg of a pair happened to be armed first.
func TestGitHubCallbackTriggerTableIsSymmetric(t *testing.T) {
	for a, partners := range mutuallyExclusiveGitHubCallbackTriggers {
		for b, excluded := range partners {
			if !excluded {
				t.Fatalf("%s->%s is present but false; drop the row instead", a, b)
			}
			if !mutuallyExclusiveGitHubCallbackTriggers[b][a] {
				t.Fatalf("%s excludes %s but not the reverse", a, b)
			}
		}
	}
}

// TestGitHubCallbackTriggersCoSatisfiableRelation pins exactly which pairs the
// completed table calls mutually exclusive. Each "want exclusive" pair is
// justified against callback.satisfiedTriggers: merged/closed/ready_for_review
// and checks_passed_ready are gated on PR state, checks_passed and
// checks_failed on the check verdict.
func TestGitHubCallbackTriggersCoSatisfiableRelation(t *testing.T) {
	const (
		merged    = models.GitHubCallbackTriggerMerged
		closed    = models.GitHubCallbackTriggerClosed
		passed    = models.GitHubCallbackTriggerChecksPassed
		failed    = models.GitHubCallbackTriggerChecksFailed
		ready     = models.GitHubCallbackTriggerReadyForReview
		passedRdy = models.GitHubCallbackTriggerChecksPassedReady
	)
	exclusive := [][2]models.GitHubCallbackTrigger{
		{merged, closed},
		{merged, ready},
		{merged, passedRdy},
		{closed, ready},
		{closed, passedRdy},
		{passed, failed},
		{failed, passedRdy},
	}
	for _, pair := range exclusive {
		if githubCallbackTriggersCoSatisfiable(pair[0], pair[1]) {
			t.Errorf("%s + %s should be mutually exclusive", pair[0], pair[1])
		}
		if githubCallbackTriggersCoSatisfiable(pair[1], pair[0]) {
			t.Errorf("%s + %s should be mutually exclusive (reversed)", pair[1], pair[0])
		}
	}

	// checks_passed is set from the check verdict alone, with no PR-state
	// gate, so a closed or merged PR with green checks satisfies both in one
	// evaluation. These pairs must stay co-satisfiable.
	coSatisfiable := [][2]models.GitHubCallbackTrigger{
		{passed, closed},
		{passed, merged},
		{passed, ready},
		{passed, passedRdy},
		{ready, passedRdy},
		{merged, merged},
	}
	for _, pair := range coSatisfiable {
		if !githubCallbackTriggersCoSatisfiable(pair[0], pair[1]) {
			t.Errorf("%s + %s should be co-satisfiable", pair[0], pair[1])
		}
		if !githubCallbackTriggersCoSatisfiable(pair[1], pair[0]) {
			t.Errorf("%s + %s should be co-satisfiable (reversed)", pair[1], pair[0])
		}
	}
}

// TestGitHubCallbackTriggersCoSatisfiableUnknownTriggerDefaultsOpen pins the
// fail-closed grouping default: a trigger with no row is treated as
// co-satisfiable with everything, so its grouping is refused rather than
// silently allowed.
func TestGitHubCallbackTriggersCoSatisfiableUnknownTriggerDefaultsOpen(t *testing.T) {
	unknown := models.GitHubCallbackTrigger("not_a_trigger")
	if !githubCallbackTriggersCoSatisfiable(unknown, models.GitHubCallbackTriggerMerged) {
		t.Fatal("an unrowed trigger must default to co-satisfiable")
	}
}

// TestGitHubCallbackStore_CreateAcceptsChecksPassedReadyWithChecksFailed is the
// ski11 shape: this grouping is REFUSED before the table was completed and is
// accepted after. It is the observable behaviour change U1 ships.
func TestGitHubCallbackStore_CreateAcceptsChecksPassedReadyWithChecksFailed(t *testing.T) {
	store := NewGitHubCallbackStore(setupTestDB(t))
	ctx := context.Background()
	group := "ski11-settle"

	pass := newTestCallbackParams()
	pass.GroupID = &group
	pass.Trigger = models.GitHubCallbackTriggerChecksPassedReady
	if _, _, err := store.Create(ctx, pass); err != nil {
		t.Fatalf("create checks_passed_ready: %v", err)
	}

	fail := newTestCallbackParams()
	fail.GroupID = &group
	fail.Trigger = models.GitHubCallbackTriggerChecksFailed
	if _, _, err := store.Create(ctx, fail); err != nil {
		t.Fatalf("checks_passed_ready + checks_failed should now group: %v", err)
	}
}

// TestGitHubCallbackStore_CreateStillRejectsReadyWithChecksPassedReady guards
// the other direction: completing the table must not make a genuinely
// co-satisfiable pair groupable.
func TestGitHubCallbackStore_CreateStillRejectsReadyWithChecksPassedReady(t *testing.T) {
	store := NewGitHubCallbackStore(setupTestDB(t))
	ctx := context.Background()
	group := "still-refused"

	ready := newTestCallbackParams()
	ready.GroupID = &group
	ready.Trigger = models.GitHubCallbackTriggerReadyForReview
	if _, _, err := store.Create(ctx, ready); err != nil {
		t.Fatalf("create ready_for_review: %v", err)
	}

	passedReady := newTestCallbackParams()
	passedReady.GroupID = &group
	passedReady.Trigger = models.GitHubCallbackTriggerChecksPassedReady
	if _, _, err := store.Create(ctx, passedReady); !errors.Is(err, ErrGitHubCallbackInvalid) {
		t.Fatalf("err = %v, want ErrGitHubCallbackInvalid", err)
	}
}

// ---------------------------------------------------------------------------
// Cross-group conflict scan (BOS-1268)
//
// Sibling cancellation is group-scoped, so a pass/fail pair armed under two
// different group ids is two groups of one: the winner cancels nothing and the
// loser stays armed until it expires. Create reports that shape; it never
// refuses it.
// ---------------------------------------------------------------------------

// newConflictParams returns params for the same chat/repo/PR as
// newTestCallbackParams, in the named group, with the given trigger. A nil
// group means ungrouped.
func newConflictParams(group *string, trigger models.GitHubCallbackTrigger) CreateGitHubCallbackParams {
	p := newTestCallbackParams()
	p.GroupID = group
	p.Trigger = trigger
	return p
}

func strptr(s string) *string { return &s }

func TestGitHubCallbackStore_CreateReportsCrossGroupConflict(t *testing.T) {
	store := NewGitHubCallbackStore(setupTestDB(t))
	ctx := context.Background()

	first, _, err := store.Create(ctx, newConflictParams(strptr("ski109-final-pass"), models.GitHubCallbackTriggerChecksPassed))
	if err != nil {
		t.Fatalf("create checks_passed: %v", err)
	}

	second, conflict, err := store.Create(ctx, newConflictParams(strptr("ski109-final-fail"), models.GitHubCallbackTriggerChecksFailed))
	if err != nil {
		t.Fatalf("create checks_failed must still succeed: %v", err)
	}
	if second == nil {
		t.Fatal("create returned no row")
	}
	// The create is advisory-only: the row must really exist.
	if got, err := store.Get(ctx, second.ID); err != nil || got.State != models.GitHubCallbackStateActive {
		t.Fatalf("second row get = %+v, err = %v; want an active row", got, err)
	}

	if conflict == nil {
		t.Fatal("want a conflict for the ski109 shape, got none")
	}
	if conflict.ID != first.ID {
		t.Errorf("conflict.ID = %q, want %q", conflict.ID, first.ID)
	}
	if conflict.GroupID == nil || *conflict.GroupID != "ski109-final-pass" {
		t.Errorf("conflict.GroupID = %v, want ski109-final-pass", conflict.GroupID)
	}
	if conflict.Trigger != models.GitHubCallbackTriggerChecksPassed {
		t.Errorf("conflict.Trigger = %q, want checks_passed", conflict.Trigger)
	}
	if !conflict.ExpiresAt.Equal(first.ExpiresAt) {
		t.Errorf("conflict.ExpiresAt = %v, want %v", conflict.ExpiresAt, first.ExpiresAt)
	}
}

// The ski11 shape depends on the pair U1 added to the table; without that row
// this create reports nothing.
func TestGitHubCallbackStore_CreateReportsCrossGroupConflictForChecksPassedReady(t *testing.T) {
	store := NewGitHubCallbackStore(setupTestDB(t))
	ctx := context.Background()

	if _, _, err := store.Create(ctx, newConflictParams(strptr("ski11-settle-pass"), models.GitHubCallbackTriggerChecksPassedReady)); err != nil {
		t.Fatalf("create checks_passed_ready: %v", err)
	}
	_, conflict, err := store.Create(ctx, newConflictParams(strptr("ski11-settle-fail"), models.GitHubCallbackTriggerChecksFailed))
	if err != nil {
		t.Fatalf("create checks_failed: %v", err)
	}
	if conflict == nil {
		t.Fatal("want a conflict for the ski11 shape, got none")
	}
	if conflict.Trigger != models.GitHubCallbackTriggerChecksPassedReady {
		t.Errorf("conflict.Trigger = %q, want checks_passed_ready", conflict.Trigger)
	}
}

// An ungrouped row is itself a group of one, so it conflicts too. SQLite would
// evaluate `group_id != ?` to NULL for these rows and skip them silently — this
// is the NULL-arm regression guard.
func TestGitHubCallbackStore_CreateReportsConflictWithUngroupedCallback(t *testing.T) {
	store := NewGitHubCallbackStore(setupTestDB(t))
	ctx := context.Background()

	if _, _, err := store.Create(ctx, newConflictParams(nil, models.GitHubCallbackTriggerChecksPassed)); err != nil {
		t.Fatalf("create ungrouped checks_passed: %v", err)
	}
	_, conflict, err := store.Create(ctx, newConflictParams(strptr("grouped-fail"), models.GitHubCallbackTriggerChecksFailed))
	if err != nil {
		t.Fatalf("create checks_failed: %v", err)
	}
	if conflict == nil {
		t.Fatal("an ungrouped live callback must still be reported")
	}
	if conflict.GroupID != nil {
		t.Errorf("conflict.GroupID = %v, want nil for an ungrouped row", *conflict.GroupID)
	}
}

// Two ungrouped callbacks are two groups of one as well; neither can cancel the
// other, so the shape is reported.
func TestGitHubCallbackStore_CreateReportsConflictBetweenTwoUngroupedCallbacks(t *testing.T) {
	store := NewGitHubCallbackStore(setupTestDB(t))
	ctx := context.Background()

	if _, _, err := store.Create(ctx, newConflictParams(nil, models.GitHubCallbackTriggerChecksPassed)); err != nil {
		t.Fatalf("create first: %v", err)
	}
	_, conflict, err := store.Create(ctx, newConflictParams(nil, models.GitHubCallbackTriggerChecksFailed))
	if err != nil {
		t.Fatalf("create second: %v", err)
	}
	if conflict == nil {
		t.Fatal("two ungrouped exclusive callbacks must be reported")
	}
}

// A row past its expires_at is dead, not a live conflict. Expiry is swept
// lazily (list RPC / MarkExpired) and the create path sweeps nothing, so
// without the expires_at > now guard the scan reports an overdue row — printing
// a *past* expiry while asserting both legs are still armed.
func TestGitHubCallbackStore_CreateIgnoresOverdueCrossGroupCallback(t *testing.T) {
	sqlDB := setupTestDB(t)
	store := NewGitHubCallbackStore(sqlDB)
	ctx := context.Background()

	first, _, err := store.Create(ctx, newConflictParams(strptr("stale-pass"), models.GitHubCallbackTriggerChecksPassed))
	if err != nil {
		t.Fatalf("create checks_passed: %v", err)
	}
	// Create refuses a past expiry, so backdate the stored row directly. State is
	// left untouched on purpose: that is exactly the overdue-but-unswept shape.
	if _, err := sqlDB.ExecContext(ctx, "UPDATE github_callbacks SET expires_at = ? WHERE id = ?",
		sqlutil.FormatTime(time.Now().UTC().Add(-time.Hour)), first.ID); err != nil {
		t.Fatalf("backdate expiry: %v", err)
	}
	if got, err := store.Get(ctx, first.ID); err != nil || got.State != models.GitHubCallbackStateActive {
		t.Fatalf("backdated row = %+v, err = %v; want it still active", got, err)
	}

	_, conflict, err := store.Create(ctx, newConflictParams(strptr("stale-fail"), models.GitHubCallbackTriggerChecksFailed))
	if err != nil {
		t.Fatalf("create checks_failed: %v", err)
	}
	if conflict != nil {
		t.Fatalf("want no conflict for an overdue row, got %+v expiring %s", conflict, conflict.ExpiresAt)
	}
}

func TestGitHubCallbackStore_CreateNoConflictCases(t *testing.T) {
	group := "shared-group"
	cases := []struct {
		name    string
		arrange func(p *CreateGitHubCallbackParams) // mutates the SECOND create
		first   CreateGitHubCallbackParams
	}{
		{
			// The existing in-group rejection owns this case; a warning here
			// would be unreachable anyway.
			name:  "same group",
			first: newConflictParams(&group, models.GitHubCallbackTriggerChecksPassed),
			arrange: func(p *CreateGitHubCallbackParams) {
				p.GroupID = &group
				p.Trigger = models.GitHubCallbackTriggerChecksFailed
			},
		},
		{
			name:  "different chat",
			first: newConflictParams(strptr("g1"), models.GitHubCallbackTriggerChecksPassed),
			arrange: func(p *CreateGitHubCallbackParams) {
				p.GroupID = strptr("g2")
				p.Trigger = models.GitHubCallbackTriggerChecksFailed
				p.TargetChatID = "chat-other"
			},
		},
		{
			name:  "different pr",
			first: newConflictParams(strptr("g1"), models.GitHubCallbackTriggerChecksPassed),
			arrange: func(p *CreateGitHubCallbackParams) {
				p.GroupID = strptr("g2")
				p.Trigger = models.GitHubCallbackTriggerChecksFailed
				p.PRNumber = 99
			},
		},
		{
			name:  "different repo",
			first: newConflictParams(strptr("g1"), models.GitHubCallbackTriggerChecksPassed),
			arrange: func(p *CreateGitHubCallbackParams) {
				p.GroupID = strptr("g2")
				p.Trigger = models.GitHubCallbackTriggerChecksFailed
				p.RepoName = "other-repo"
			},
		},
		{
			// A trigger is co-satisfiable with itself, so a duplicate arm is
			// not the split-pair shape.
			name:  "identical trigger in another group",
			first: newConflictParams(strptr("g1"), models.GitHubCallbackTriggerChecksFailed),
			arrange: func(p *CreateGitHubCallbackParams) {
				p.GroupID = strptr("g2")
				p.Trigger = models.GitHubCallbackTriggerChecksFailed
			},
		},
		{
			// checks_passed carries no PR-state gate, so a closed PR with green
			// checks satisfies both; not exclusive, not reported.
			name:  "co-satisfiable pair in another group",
			first: newConflictParams(strptr("g1"), models.GitHubCallbackTriggerChecksPassed),
			arrange: func(p *CreateGitHubCallbackParams) {
				p.GroupID = strptr("g2")
				p.Trigger = models.GitHubCallbackTriggerClosed
			},
		},
		{
			name:  "independent watch opt-out",
			first: newConflictParams(strptr("g1"), models.GitHubCallbackTriggerChecksPassed),
			arrange: func(p *CreateGitHubCallbackParams) {
				p.GroupID = strptr("g2")
				p.Trigger = models.GitHubCallbackTriggerChecksFailed
				p.IndependentWatch = true
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewGitHubCallbackStore(setupTestDB(t))
			ctx := context.Background()
			if _, _, err := store.Create(ctx, tc.first); err != nil {
				t.Fatalf("create first: %v", err)
			}
			second := newTestCallbackParams()
			tc.arrange(&second)
			_, conflict, err := store.Create(ctx, second)
			if err != nil {
				t.Fatalf("create second: %v", err)
			}
			if conflict != nil {
				t.Fatalf("want no conflict, got %+v", conflict)
			}
		})
	}
}

// Only live callbacks can strand a watch; a terminal row cancels nothing
// because it is already done.
func TestGitHubCallbackStore_CreateIgnoresTerminalConflicts(t *testing.T) {
	for _, state := range []models.GitHubCallbackState{
		models.GitHubCallbackStateDelivered,
		models.GitHubCallbackStateCanceled,
		models.GitHubCallbackStateExpired,
	} {
		t.Run(string(state), func(t *testing.T) {
			store := NewGitHubCallbackStore(setupTestDB(t))
			ctx := context.Background()

			first, _, err := store.Create(ctx, newConflictParams(strptr("g1"), models.GitHubCallbackTriggerChecksPassed))
			if err != nil {
				t.Fatalf("create first: %v", err)
			}
			if _, err := store.db.ExecContext(ctx,
				"UPDATE github_callbacks SET state = ? WHERE id = ?", string(state), first.ID); err != nil {
				t.Fatalf("force state %s: %v", state, err)
			}

			_, conflict, err := store.Create(ctx, newConflictParams(strptr("g2"), models.GitHubCallbackTriggerChecksFailed))
			if err != nil {
				t.Fatalf("create second: %v", err)
			}
			if conflict != nil {
				t.Fatalf("a %s callback must not be reported, got %+v", state, conflict)
			}
		})
	}
}

// A leased callback is still armed and will still fire, so it counts.
func TestGitHubCallbackStore_CreateReportsLeasedConflict(t *testing.T) {
	store := NewGitHubCallbackStore(setupTestDB(t))
	ctx := context.Background()

	first, _, err := store.Create(ctx, newConflictParams(strptr("g1"), models.GitHubCallbackTriggerChecksPassed))
	if err != nil {
		t.Fatalf("create first: %v", err)
	}
	if _, err := store.db.ExecContext(ctx,
		"UPDATE github_callbacks SET state = ? WHERE id = ?",
		string(models.GitHubCallbackStateLeased), first.ID); err != nil {
		t.Fatalf("force leased: %v", err)
	}

	_, conflict, err := store.Create(ctx, newConflictParams(strptr("g2"), models.GitHubCallbackTriggerChecksFailed))
	if err != nil {
		t.Fatalf("create second: %v", err)
	}
	if conflict == nil {
		t.Fatal("a leased callback is still armed and must be reported")
	}
}

func TestGitHubCallbackStore_CancelTriggered(t *testing.T) {
	ctx := context.Background()
	for _, state := range []models.GitHubCallbackState{models.GitHubCallbackStateActive, models.GitHubCallbackStateTriggered, models.GitHubCallbackStateLeased, models.GitHubCallbackStateDelivered} {
		t.Run(string(state), func(t *testing.T) {
			database := setupTestDB(t)
			store := NewGitHubCallbackStore(database)
			cb, _, err := store.Create(ctx, newTestCallbackParams())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := database.ExecContext(ctx, "UPDATE github_callbacks SET state = ? WHERE id = ?", state, cb.ID); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			err = store.MarkCanceled(ctx, cb.ID, "dropped: session closed", now)
			if state != models.GitHubCallbackStateTriggered {
				if !errors.Is(err, ErrGitHubCallbackTriggerConflict) {
					t.Fatalf("error = %v, want conflict", err)
				}
				got, getErr := store.Get(ctx, cb.ID)
				if getErr != nil {
					t.Fatal(getErr)
				}
				if got.State != state {
					t.Fatalf("state = %s, want %s", got.State, state)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := store.Get(ctx, cb.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != models.GitHubCallbackStateCanceled || got.LastEvent == nil || *got.LastEvent != "dropped: session closed" {
				t.Fatalf("canceled row = %+v", got)
			}
			if err := store.MarkCanceled(ctx, cb.ID, "again", now); !errors.Is(err, ErrGitHubCallbackTriggerConflict) {
				t.Fatalf("second cancellation = %v", err)
			}
		})
	}
	store := NewGitHubCallbackStore(setupTestDB(t))
	if err := store.MarkCanceled(ctx, "missing", "gone", time.Now()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
}
