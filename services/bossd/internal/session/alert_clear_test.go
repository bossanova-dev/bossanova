package session

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/status"
	"github.com/rs/zerolog"
)

// alertClearFixture wires the BOS-1535 sweep against the real display tracker
// and repair lease manager, with the in-memory session store fake.
type alertClearFixture struct {
	lc       *Lifecycle
	sessions *mockSessionStore
	display  *status.DisplayTracker
	leases   *status.RepairLeaseManager
	clock    *time.Time
	notified []string
}

func newAlertClearFixture(t *testing.T) *alertClearFixture {
	t.Helper()
	f := &alertClearFixture{
		sessions: newMockSessionStore(),
		display:  status.NewDisplayTracker(),
	}
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	f.clock = &now
	f.leases = status.NewRepairLeaseManager().WithClock(2*time.Hour, func() time.Time { return *f.clock })
	f.lc = newTestLifecycle(f.sessions, nil, &mockAgentChatStore{}, nil, nil, nil, nil, nil, zerolog.Nop())
	f.lc.SetAlertClearDeps(f.display, f.leases, func(_ context.Context, sessionID string) {
		f.notified = append(f.notified, sessionID)
	})
	return f
}

// seedFailedRepair stores a session whose last repair failed on head failedHead
// and was then refused (blocked) — both halves of the stored repair alert.
func (f *alertClearFixture) seedFailedRepair(id, failedHead string) *models.Session {
	pr := 42
	blockedAt := f.clock.Add(-time.Hour)
	// The failed round started after the refusal, so its recorded head is
	// the alert's head.
	startedAt := f.clock.Add(-30 * time.Minute)
	sess := &models.Session{
		ID:                      id,
		PRNumber:                &pr,
		LastRepairHeadSHA:       failedHead,
		LastRepairAttemptCount:  3,
		LastRepairExitError:     "exit status 1",
		LastRepairBlockedReason: "displace blocked: chat working",
		LastRepairBlockedAt:     &blockedAt,
		LastRepairStartedAt:     &startedAt,
	}
	f.sessions.sessions[id] = sess
	return sess
}

func (f *alertClearFixture) poll(id string, st vcs.DisplayStatus, head string, failures bool) {
	f.display.Set(id, vcs.DisplayInfo{Status: st, HeadSHA: head, HasFailures: failures})
}

// stall acquires a lease for id and lets it run past the stall window, leaving
// a live stall record and no lease.
func (f *alertClearFixture) stall(t *testing.T, id string) {
	t.Helper()
	if err := f.leases.Acquire(id, "plugin"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	*f.clock = f.clock.Add(status.RepairStallWindow + time.Second)
	if _, ok := f.leases.StalledAt(id); !ok {
		t.Fatal("expected a live stall record")
	}
}

func (f *alertClearFixture) run(t *testing.T) int {
	t.Helper()
	n, err := f.lc.ClearResolvedAlerts(context.Background())
	if err != nil {
		t.Fatalf("ClearResolvedAlerts: %v", err)
	}
	return n
}

func assertRepairAlertCleared(t *testing.T, sess *models.Session, wantHead string) {
	t.Helper()
	if sess.LastRepairBlockedReason != "" || sess.LastRepairBlockedAt != nil {
		t.Fatalf("blocked pair not cleared: reason=%q at=%v", sess.LastRepairBlockedReason, sess.LastRepairBlockedAt)
	}
	if sess.LastRepairAttemptCount != 0 || sess.LastRepairExitError != "" || sess.LastRepairRunnerError != "" {
		t.Fatalf("failure alert not cleared: count=%d exit=%q runner=%q",
			sess.LastRepairAttemptCount, sess.LastRepairExitError, sess.LastRepairRunnerError)
	}
	if sess.LastRepairHeadSHA != wantHead {
		t.Fatalf("LastRepairHeadSHA=%q, want %q kept as history", sess.LastRepairHeadSHA, wantHead)
	}
}

func assertRepairAlertKept(t *testing.T, sess *models.Session) {
	t.Helper()
	if sess.LastRepairBlockedReason == "" || sess.LastRepairAttemptCount != 3 {
		t.Fatalf("alert cleared but should be kept: reason=%q count=%d", sess.LastRepairBlockedReason, sess.LastRepairAttemptCount)
	}
}

func TestClearResolvedAlerts_HeadMovedClears(t *testing.T) {
	f := newAlertClearFixture(t)
	sess := f.seedFailedRepair("s1", "aaa111")
	f.poll("s1", vcs.DisplayStatusFailing, "bbb222", true)

	if n := f.run(t); n != 1 {
		t.Fatalf("cleared=%d, want 1", n)
	}
	assertRepairAlertCleared(t, sess, "aaa111")
	if !slices.Equal(f.notified, []string{"s1"}) {
		t.Fatalf("notified=%v, want [s1] so the stream gets an UPDATED delta", f.notified)
	}
}

func TestClearResolvedAlerts_CIGreenClears(t *testing.T) {
	f := newAlertClearFixture(t)
	sess := f.seedFailedRepair("s1", "aaa111")
	f.poll("s1", vcs.DisplayStatusPassing, "aaa111", false)

	if n := f.run(t); n != 1 {
		t.Fatalf("cleared=%d, want 1", n)
	}
	assertRepairAlertCleared(t, sess, "aaa111")
}

func TestClearResolvedAlerts_PRMergedClears(t *testing.T) {
	f := newAlertClearFixture(t)
	sess := f.seedFailedRepair("s1", "aaa111")
	f.poll("s1", vcs.DisplayStatusMerged, "aaa111", true)

	if n := f.run(t); n != 1 {
		t.Fatalf("cleared=%d, want 1", n)
	}
	assertRepairAlertCleared(t, sess, "aaa111")
}

func TestClearResolvedAlerts_NoPRClears(t *testing.T) {
	f := newAlertClearFixture(t)
	sess := f.seedFailedRepair("s1", "aaa111")
	sess.PRNumber = nil

	if n := f.run(t); n != 1 {
		t.Fatalf("cleared=%d, want 1", n)
	}
	assertRepairAlertCleared(t, sess, "aaa111")
}

func TestClearResolvedAlerts_SameHeadStillFailingKept(t *testing.T) {
	f := newAlertClearFixture(t)
	sess := f.seedFailedRepair("s1", "aaa111")
	f.poll("s1", vcs.DisplayStatusFailing, "aaa111", true)

	if n := f.run(t); n != 0 {
		t.Fatalf("cleared=%d, want 0", n)
	}
	assertRepairAlertKept(t, sess)
	if len(f.notified) != 0 {
		t.Fatalf("notified=%v, want none", f.notified)
	}
}

func TestClearResolvedAlerts_UnpolledSessionKept(t *testing.T) {
	f := newAlertClearFixture(t)
	sess := f.seedFailedRepair("s1", "aaa111")

	if n := f.run(t); n != 0 {
		t.Fatalf("cleared=%d, want 0 — no display entry means nothing is known", n)
	}
	assertRepairAlertKept(t, sess)
}

func TestClearResolvedAlerts_ActiveLeaseKept(t *testing.T) {
	f := newAlertClearFixture(t)
	sess := f.seedFailedRepair("s1", "aaa111")
	f.poll("s1", vcs.DisplayStatusPassing, "bbb222", false)
	if err := f.leases.Acquire("s1", "plugin"); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	if n := f.run(t); n != 0 {
		t.Fatalf("cleared=%d, want 0 while a repair lease is active", n)
	}
	assertRepairAlertKept(t, sess)
	if !f.leases.Active("s1") {
		t.Fatal("the sweep must not touch a held lease")
	}
}

func TestClearResolvedAlerts_StallClearedAlongsideRow(t *testing.T) {
	f := newAlertClearFixture(t)
	sess := f.seedFailedRepair("s1", "aaa111")
	f.stall(t, "s1")
	// A live stall's round has no recorded head, so only CI-green / left-states
	// legs may end it.
	f.poll("s1", vcs.DisplayStatusPassing, "bbb222", false)

	if n := f.run(t); n != 1 {
		t.Fatalf("cleared=%d, want 1", n)
	}
	assertRepairAlertCleared(t, sess, "aaa111")
	if _, ok := f.leases.StalledAt("s1"); ok {
		t.Fatal("stall record survived the sweep")
	}
}

func TestClearResolvedAlerts_StallOnlyCleared(t *testing.T) {
	f := newAlertClearFixture(t)
	pr := 7
	f.sessions.sessions["s1"] = &models.Session{ID: "s1", PRNumber: &pr, LastRepairHeadSHA: "aaa111"}
	f.stall(t, "s1")
	f.poll("s1", vcs.DisplayStatusPassing, "aaa111", false)

	if n := f.run(t); n != 1 {
		t.Fatalf("cleared=%d, want 1", n)
	}
	if _, ok := f.leases.StalledAt("s1"); ok {
		t.Fatal("stall record survived the sweep")
	}
	if got := f.sessions.clearRepairAlertCalls; len(got) != 0 {
		t.Fatalf("ClearRepairAlert calls=%v, want none for a stall-only session with no stored alert", got)
	}
}

func TestClearResolvedAlerts_StallKeptWhileFailing(t *testing.T) {
	f := newAlertClearFixture(t)
	pr := 7
	f.sessions.sessions["s1"] = &models.Session{ID: "s1", PRNumber: &pr, LastRepairHeadSHA: "aaa111"}
	f.stall(t, "s1")
	f.poll("s1", vcs.DisplayStatusFailing, "aaa111", true)

	if n := f.run(t); n != 0 {
		t.Fatalf("cleared=%d, want 0", n)
	}
	if _, ok := f.leases.StalledAt("s1"); !ok {
		t.Fatal("stall record cleared while its condition still holds")
	}
}

func TestClearResolvedAlerts_SkipsSessionsWithoutAlerts(t *testing.T) {
	f := newAlertClearFixture(t)
	f.sessions.sessions["s1"] = &models.Session{ID: "s1", LastRepairHeadSHA: "aaa111"}
	f.poll("s1", vcs.DisplayStatusMerged, "bbb222", false)

	if n := f.run(t); n != 0 {
		t.Fatalf("cleared=%d, want 0", n)
	}
	if len(f.sessions.clearRepairAlertCalls) != 0 || len(f.notified) != 0 {
		t.Fatalf("no alert to clear, but store calls=%v notified=%v", f.sessions.clearRepairAlertCalls, f.notified)
	}
}

// TestClearResolvedAlerts_MixedSessionsClearsOnlyEnded runs one sweep over a
// session whose alert still holds and one whose alert ended: only the ended
// one is cleared and published.
func TestClearResolvedAlerts_MixedSessionsClearsOnlyEnded(t *testing.T) {
	f := newAlertClearFixture(t)
	kept := f.seedFailedRepair("s1", "aaa111")
	f.poll("s1", vcs.DisplayStatusFailing, "aaa111", true)
	ended := f.seedFailedRepair("s2", "ccc333")
	f.poll("s2", vcs.DisplayStatusFailing, "ddd444", true)

	if n := f.run(t); n != 1 {
		t.Fatalf("cleared=%d, want 1", n)
	}
	assertRepairAlertKept(t, kept)
	assertRepairAlertCleared(t, ended, "ccc333")
	if !slices.Equal(f.notified, []string{"s2"}) {
		t.Fatalf("notified=%v, want [s2]", f.notified)
	}
}

func TestClearResolvedAlerts_UnwiredIsNoop(t *testing.T) {
	sessions := newMockSessionStore()
	pr := 1
	sessions.sessions["s1"] = &models.Session{ID: "s1", PRNumber: &pr, LastRepairAttemptCount: 2, LastRepairExitError: "x"}
	lc := newTestLifecycle(sessions, nil, &mockAgentChatStore{}, nil, nil, nil, nil, nil, zerolog.Nop())
	n, err := lc.ClearResolvedAlerts(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("unwired sweep = (%d, %v), want (0, nil)", n, err)
	}
}

func TestClearResolvedAlerts_BlockedOnNewHeadKept(t *testing.T) {
	f := newAlertClearFixture(t)
	sess := f.seedFailedRepair("s1", "aaa111")
	// Blocked on the current head, last real round on the older head.
	blockedAt := f.clock.Add(-time.Minute)
	sess.LastRepairBlockedAt = &blockedAt
	f.poll("s1", vcs.DisplayStatusFailing, "bbb222", true)

	if n := f.run(t); n != 0 {
		t.Fatalf("cleared=%d, want 0", n)
	}
	assertRepairAlertKept(t, sess)
}
