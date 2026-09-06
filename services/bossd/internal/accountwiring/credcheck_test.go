package accountwiring

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/recurser/bossalib/agenterr"
	"github.com/recurser/bossalib/models"

	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/credmaterialize"
)

// --- test doubles ---------------------------------------------------------

// fakeClock is a controllable AuthCheckClock. Every After() registers a
// waiter the test releases explicitly, so schedule, jitter, and backoff are
// asserted on the requested durations instead of slept through.
type fakeClock struct {
	mu       sync.Mutex
	now      time.Time
	waits    []time.Duration
	pending  []chan time.Time
	released int
	notify   chan struct{}
}

func newFakeClock(now time.Time) *fakeClock {
	return &fakeClock{now: now, notify: make(chan struct{}, 64)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	ch := make(chan time.Time, 1)
	c.waits = append(c.waits, d)
	c.pending = append(c.pending, ch)
	c.mu.Unlock()
	select {
	case c.notify <- struct{}{}:
	default:
	}
	return ch
}

// waitForWaits blocks until at least n After() calls have been registered.
func (c *fakeClock) waitForWaits(t *testing.T, n int) []time.Duration {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		c.mu.Lock()
		got := append([]time.Duration(nil), c.waits...)
		c.mu.Unlock()
		if len(got) >= n {
			return got
		}
		select {
		case <-c.notify:
		case <-deadline:
			t.Fatalf("timed out waiting for %d scheduled waits; got %d", n, len(got))
		}
	}
}

// fire releases the next unreleased timer.
func (c *fakeClock) fire(t *testing.T) {
	t.Helper()
	c.waitForWaits(t, c.releasedCount()+1)
	c.mu.Lock()
	ch := c.pending[c.released]
	c.released++
	now := c.now
	c.mu.Unlock()
	ch <- now
}

func (c *fakeClock) releasedCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.released
}

// fakeVerifier is a controllable CredentialVerifier.
type fakeVerifier struct {
	mu      sync.Mutex
	calls   []string
	err     error
	persist bool
	// selfWrites overrides the count derived from persist, for runs that wrote
	// the credential store more than once.
	selfWrites uint64
	// ambient is the ambient-login comparison SmokeRunner would have made
	// (BOS-1175). The zero value is AmbientAuthNotEvaluable, so every existing
	// test keeps describing a run that evaluated nothing.
	ambient credmaterialize.AmbientAuthState
	// refresh is the BOS-1174 redacted expiry verdict this run reports.
	refresh  credmaterialize.RefreshAssertion
	gate     chan struct{} // when non-nil, Verify blocks until it is closed
	entered  chan struct{}
	onVerify func()
}

func (f *fakeVerifier) Verify(_ context.Context, accountID, _ string, _ []byte) (SmokeResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, accountID)
	gate, entered, onVerify := f.gate, f.entered, f.onVerify
	err, persist, explicitWrites := f.err, f.persist, f.selfWrites
	ambient := f.ambient
	refresh := f.refresh
	f.mu.Unlock()

	if entered != nil {
		entered <- struct{}{}
	}
	if onVerify != nil {
		onVerify()
	}
	if gate != nil {
		<-gate
	}
	selfWrites := explicitWrites
	if selfWrites == 0 && persist {
		selfWrites = 1
	}
	return SmokeResult{
		SelfCredentialWrites: selfWrites,
		AmbientAuth:          ambient,
		RefreshAssertion:     refresh,
	}, err
}

func (f *fakeVerifier) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// authStore records RecordAuthCheck writes and serves a fixed account list.
type authStore struct {
	mu       sync.Mutex
	accounts []*models.Account
	writes   []models.AuthCheck
	writeIDs []string
	clears   []string
	listErr  error
	getErr   error
	writeErr error
	clearErr error
	// onWrite runs inside RecordAuthCheck, which is the window between the
	// maintainer's generation compare and its durable commit.
	onWrite func()
	notify  chan struct{}
}

func newAuthStore(accounts ...*models.Account) *authStore {
	return &authStore{accounts: accounts, notify: make(chan struct{}, 64)}
}

func (s *authStore) List(context.Context) ([]*models.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	return append([]*models.Account(nil), s.accounts...), nil
}

// Get mirrors the real store: a copy of the row, or sql.ErrNoRows when the
// account is not seeded. The maintainer reads the recorded verdict through it
// before overwriting one, so an unseeded account must fail the same way the
// SQLite store does rather than look like a row with no verdict.
func (s *authStore) Get(_ context.Context, id string) (*models.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return nil, s.getErr
	}
	for _, a := range s.accounts {
		if a != nil && a.ID == id {
			clone := *a
			return &clone, nil
		}
	}
	return nil, sql.ErrNoRows
}

func (s *authStore) RecordAuthCheck(_ context.Context, id string, check models.AuthCheck) error {
	s.mu.Lock()
	onWrite := s.onWrite
	s.mu.Unlock()
	if onWrite != nil {
		onWrite()
	}
	s.mu.Lock()
	writeErr := s.writeErr
	if writeErr == nil {
		s.writes = append(s.writes, check)
		s.writeIDs = append(s.writeIDs, id)
	}
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
	return writeErr
}

func (s *authStore) ClearAuthCheck(_ context.Context, id string) error {
	s.mu.Lock()
	clearErr := s.clearErr
	if clearErr == nil {
		s.clears = append(s.clears, id)
	}
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
	return clearErr
}

func (s *authStore) cleared() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.clears...)
}

func (s *authStore) recorded() []models.AuthCheck {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]models.AuthCheck(nil), s.writes...)
}

func (s *authStore) waitForWrites(t *testing.T, n int) []models.AuthCheck {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		got := s.recorded()
		if len(got) >= n {
			return got
		}
		select {
		case <-s.notify:
		case <-deadline:
			t.Fatalf("timed out waiting for %d auth-check writes; got %d", n, len(got))
		}
	}
}

// genCreds is a CredentialStore with the lock + generation seams, so the
// generation guard is exercised exactly as the real accountcred.Store drives it.
type genCreds struct {
	mu         sync.Mutex
	generation uint64
	locks      sync.Mutex
}

func (g *genCreds) Load(string) ([]byte, error) { return nil, nil }
func (g *genCreds) Save(string, []byte) error   { return nil }
func (g *genCreds) bump()                       { g.mu.Lock(); g.generation++; g.mu.Unlock() }
func (g *genCreds) CredentialGeneration(string) uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.generation
}

func (g *genCreds) WithCredentialLock(_ string, fn func() error) error {
	g.locks.Lock()
	defer g.locks.Unlock()
	return fn()
}

func codexAcct(id string, checkedAt *time.Time) *models.Account {
	return &models.Account{
		ID:       id,
		Provider: models.AccountProviderCodex,
		Status:   models.AccountStatusActive,
		Health:   models.AccountHealthOK,
		AuthCheck: models.AuthCheck{
			CheckedAt: checkedAt,
		},
	}
}

func newMaintainer(t *testing.T, v CredentialVerifier, s AuthCheckStore, creds CredentialStore, opts ...CredentialMaintainerOption) *CredentialMaintainer {
	t.Helper()
	m, err := NewCredentialMaintainer(v, s, creds, zerolog.Nop(), opts...)
	if err != nil {
		t.Fatalf("NewCredentialMaintainer: %v", err)
	}
	return m
}

// --- scheduling -----------------------------------------------------------

// TestMaintainerBootSweepChecksStaleCodexAccountOnce is AC-1's boot half: a
// stale active managed Codex account is verified once after startup.
func TestMaintainerBootSweepChecksStaleCodexAccountOnce(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-24 * time.Hour)
	fresh := now.Add(-time.Minute)

	store := newAuthStore(
		codexAcct("stale", &stale),
		codexAcct("never", nil),
		codexAcct("fresh", &fresh),
		&models.Account{ID: "claude", Provider: models.AccountProviderClaude, Status: models.AccountStatusActive},
		&models.Account{ID: "disabled", Provider: models.AccountProviderCodex, Status: models.AccountStatusDisabled},
	)
	verifier := &fakeVerifier{}
	clock := newFakeClock(now)
	m := newMaintainer(t, verifier, store, nil,
		WithAuthCheckClock(clock),
		WithAuthCheckJitter(0, nil),
	)

	m.Start(context.Background())
	t.Cleanup(m.Stop)

	clock.fire(t) // release the boot delay
	store.waitForWrites(t, 2)

	verifier.mu.Lock()
	got := append([]string(nil), verifier.calls...)
	verifier.mu.Unlock()

	if len(got) != 2 {
		t.Fatalf("expected exactly the two due codex accounts verified, got %v", got)
	}
	seen := map[string]bool{got[0]: true, got[1]: true}
	if !seen["stale"] || !seen["never"] {
		t.Fatalf("expected stale+never-checked codex accounts, got %v", got)
	}
}

// TestMaintainerSchedulesJitteredIntervalWithinBounds is AC-1's cadence half
// plus the plan's jitter-bounds requirement.
func TestMaintainerSchedulesJitteredIntervalWithinBounds(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	store := newAuthStore()
	verifier := &fakeVerifier{}
	clock := newFakeClock(now)

	// Deterministic extremes: 0 -> -jitter, 1 -> +jitter.
	var seq []float64
	var idx int
	var mu sync.Mutex
	seq = []float64{0, 1, 0.5}
	source := func() float64 {
		mu.Lock()
		defer mu.Unlock()
		v := seq[idx%len(seq)]
		idx++
		return v
	}

	m := newMaintainer(t, verifier, store, nil,
		WithAuthCheckClock(clock),
		WithAuthCheckInterval(time.Hour),
		WithAuthCheckBootDelay(10*time.Second),
		WithAuthCheckJitter(0.2, source),
	)
	m.Start(context.Background())
	t.Cleanup(m.Stop)

	clock.fire(t) // boot delay
	clock.fire(t) // first periodic wait
	waits := clock.waitForWaits(t, 3)

	if want := 8 * time.Second; waits[0] != want { // 10s * (1 - 0.2)
		t.Fatalf("boot delay jitter: got %v want %v", waits[0], want)
	}
	if want := 72 * time.Minute; waits[1] != want { // 1h * (1 + 0.2)
		t.Fatalf("first interval jitter: got %v want %v", waits[1], want)
	}
	if want := time.Hour; waits[2] != want { // 1h * (1 + 0)
		t.Fatalf("second interval jitter: got %v want %v", waits[2], want)
	}
	for i, w := range waits[1:] {
		if w < 48*time.Minute || w > 72*time.Minute {
			t.Fatalf("interval wait %d outside +/-20%% bounds: %v", i, w)
		}
	}
}

// TestMaintainerStopCancelsSchedulingAndWaits is AC-1's shutdown half.
func TestMaintainerStopCancelsSchedulingAndWaits(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	store := newAuthStore(codexAcct("a", nil))
	verifier := &fakeVerifier{}
	clock := newFakeClock(now)

	m := newMaintainer(t, verifier, store, nil, WithAuthCheckClock(clock), WithAuthCheckJitter(0, nil))
	m.Start(context.Background())
	clock.waitForWaits(t, 1)

	done := make(chan struct{})
	go func() { m.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return; the loop was not cancelled")
	}

	if n := verifier.callCount(); n != 0 {
		t.Fatalf("cancelled scheduler still ran %d verifications", n)
	}
	m.Stop() // idempotent
}

// TestMaintainerDueSkipsAccountInsideRetryBackoff proves AC-5's "no retry
// storm": a persisted NextRetryAt in the future removes the account from the
// sweep entirely.
func TestMaintainerDueSkipsAccountInsideRetryBackoff(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	past := now.Add(-24 * time.Hour)
	future := now.Add(time.Hour)
	elapsed := now.Add(-time.Second)

	m := newMaintainer(t, &fakeVerifier{}, newAuthStore(), nil)

	backingOff := codexAcct("a", &past)
	backingOff.AuthCheck.NextRetryAt = &future
	if m.due(backingOff, now) {
		t.Fatal("account inside its retry backoff must not be swept")
	}

	ready := codexAcct("b", &past)
	ready.AuthCheck.NextRetryAt = &elapsed
	if !m.due(ready, now) {
		t.Fatal("account past its retry backoff must be swept")
	}
}

// --- single flight and generation guard -----------------------------------

// TestMaintainerCollapsesConcurrentVerifications is AC-2's single-flight half:
// a boot/periodic sweep and an explicit TestAccount request for one account
// produce at most one live verification.
// TestMaintainerJoinerRejectsResultForReplacedCredential closes the join
// window. The flight stays joinable after runVerification's final revalidation
// has released the credential lock, so a RefreshAccount landing there would
// otherwise receive the OLD credential's verdict for the one it just saved.
//
// The credential is replaced AFTER the run's own generation bookkeeping is
// settled — the run itself is clean and committable — so nothing but the
// joiner-side attribution can catch it.
func TestMaintainerJoinerRejectsResultForReplacedCredential(t *testing.T) {
	store := newAuthStore(codexAcct("a", nil))
	creds := &genCreds{}
	verifier := &fakeVerifier{}
	m := newMaintainer(t, verifier, store, creds, WithAuthCheckJitter(0, nil))

	// A clean run for generation 0, recorded and committed.
	if err := m.Smoke(context.Background(), "a", "codex", nil); err != nil {
		t.Fatalf("initial Smoke: %v", err)
	}
	flight := &authFlight{done: make(chan struct{}), generation: 0, hasGeneration: true}
	close(flight.done)

	// Nothing replaced the credential: the joiner may take the result.
	if err := m.joinedResult("a", flight); err != nil {
		t.Fatalf("joiner rejected a result for the CURRENT credential: %v", err)
	}

	// A replacement lands. The same published result is now about bytes that
	// are no longer stored, and must not be handed to the joiner as a verdict.
	creds.bump()
	err := m.joinedResult("a", flight)
	if err == nil {
		t.Fatal("joiner accepted a result describing a credential that was replaced")
	}
	var stale *staleAuthCheckError
	if !errors.As(err, &stale) {
		t.Fatalf("expected staleAuthCheckError so Smoke retries, got %T: %v", err, err)
	}
}

// TestMaintainerJoinerTakesResultWithoutGenerationSeam pins the degrade: a
// store with no generation seam cannot be attributed against, so the joiner
// must take the result rather than refusing every joined verification.
func TestMaintainerJoinerTakesResultWithoutGenerationSeam(t *testing.T) {
	store := newAuthStore(codexAcct("a", nil))
	m := newMaintainer(t, &fakeVerifier{}, store, nil, WithAuthCheckJitter(0, nil))

	flight := &authFlight{done: make(chan struct{})}
	close(flight.done)
	if err := m.joinedResult("a", flight); err != nil {
		t.Fatalf("joiner refused a result although no generation seam exists: %v", err)
	}
}

func TestMaintainerCollapsesConcurrentVerifications(t *testing.T) {
	store := newAuthStore(codexAcct("a", nil))
	gate := make(chan struct{})
	verifier := &fakeVerifier{gate: gate, entered: make(chan struct{}, 4)}
	m := newMaintainer(t, verifier, store, nil, WithAuthCheckJitter(0, nil))

	const joiners = 4
	errs := make(chan error, joiners)
	// First caller enters the flight and blocks inside Verify.
	go func() { errs <- m.Smoke(context.Background(), "a", "codex", nil) }()
	<-verifier.entered

	for i := 1; i < joiners; i++ {
		go func() { errs <- m.Smoke(context.Background(), "a", "codex", nil) }()
	}
	// Give the joiners a chance to attach before the flight completes.
	time.Sleep(50 * time.Millisecond)
	close(gate)

	for i := 0; i < joiners; i++ {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatalf("verification %d: %v", i, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("joined verification never returned")
		}
	}
	if n := verifier.callCount(); n != 1 {
		t.Fatalf("expected exactly one live verification, got %d", n)
	}
	if got := store.recorded(); len(got) != 1 {
		t.Fatalf("expected exactly one durable write, got %d", len(got))
	}
}

// TestMaintainerDiscardsResultAfterCredentialGenerationChange is AC-2's
// generation-guard half: a credential replaced mid-check must not have the
// older (auth-invalid) result written against it.
func TestMaintainerDiscardsResultAfterCredentialGenerationChange(t *testing.T) {
	store := newAuthStore(codexAcct("a", nil))
	creds := &genCreds{}
	verifier := &fakeVerifier{
		err: errors.New("authentication token has been invalidated"),
		onVerify: func() {
			// A concurrent refresh replaces the credential mid-check.
			creds.bump()
			creds.bump()
		},
	}
	m := newMaintainer(t, verifier, store, creds, WithAuthCheckJitter(0, nil))

	// The DURABLE write is skipped — we no longer know which credential the
	// result describes. Every attempt here is superseded (onVerify bumps on
	// each run), so the caller hears "inconclusive".
	//
	// This deliberately does NOT surface the discarded failure text. The
	// failure belongs to the credential that was replaced, and a joined
	// RefreshAccount(test_after_save=true) would otherwise mark the credential
	// it just saved as failed without ever testing it. A discarded failing
	// check must still never be reported as a pass, which the first assertion
	// keeps pinned.
	err := m.Smoke(context.Background(), "a", "codex", nil)
	if err == nil {
		t.Fatal("a discarded FAILING check must not be reported as success")
	}
	if !errors.Is(err, ErrVerificationInconclusive) {
		t.Fatalf("expected ErrVerificationInconclusive, got %v", err)
	}
	if strings.Contains(err.Error(), "authentication token has been invalidated") {
		t.Fatalf("an obsolete failure was attributed to the replacement: %v", err)
	}
	if got := store.recorded(); len(got) != 0 {
		t.Fatalf("stale result was written: %+v", got)
	}
}

// TestMaintainerSurfacesRealFailureFromRetryAfterStaleFailure is the recovery
// direction for a failing discard: once the credential settles, the retry's
// verdict is about what is actually stored, so a genuine failure must reach the
// caller intact rather than being flattened into "inconclusive".
func TestMaintainerSurfacesRealFailureFromRetryAfterStaleFailure(t *testing.T) {
	store := newAuthStore(codexAcct("a", nil))
	creds := &genCreds{}
	var runs int
	verifier := &fakeVerifier{err: errors.New("authentication token has been invalidated")}
	// Only the first run is superseded; the retry sees a stable credential.
	verifier.onVerify = func() {
		runs++
		if runs == 1 {
			creds.bump()
			creds.bump()
		}
	}
	m := newMaintainer(t, verifier, store, creds, WithAuthCheckJitter(0, nil))

	err := m.Smoke(context.Background(), "a", "codex", nil)
	if err == nil {
		t.Fatal("a real failure against the stored credential must reach the caller")
	}
	if !strings.Contains(err.Error(), "authentication token has been invalidated") {
		t.Fatalf("expected the retry's genuine failure, got %v", err)
	}
	if got := store.recorded(); len(got) != 1 {
		t.Fatalf("expected the retry's verdict to be recorded, got %d writes", len(got))
	}
	if got := store.recorded()[0].Outcome; got != models.AuthCheckOutcomeAuthInvalid {
		t.Fatalf("outcome: got %q want auth_invalid", got)
	}
}

// TestMaintainerDiscardedResultDoesNotConsumeBackoffEscalation pins that a
// result the generation guard threw away does not advance the retry
// escalation: nothing was recorded, so nothing was learned.
func TestMaintainerDiscardedResultDoesNotConsumeBackoffEscalation(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	store := newAuthStore(codexAcct("a", nil))
	creds := &genCreds{}
	verifier := &fakeVerifier{
		err:      errors.New("sprockets exceeded the widget quorum"),
		onVerify: func() { creds.bump(); creds.bump() },
	}
	clock := newFakeClock(now)
	m := newMaintainer(t, verifier, store, creds,
		WithAuthCheckClock(clock),
		WithAuthCheckJitter(0, nil),
		WithAuthCheckBackoff(time.Minute, 4*time.Minute),
	)

	// Two discarded failures.
	for i := 0; i < 2; i++ {
		if err := m.Smoke(context.Background(), "a", "codex", nil); err == nil {
			t.Fatalf("attempt %d: expected the verification failure to surface", i)
		}
	}
	if got := store.recorded(); len(got) != 0 {
		t.Fatalf("discarded results were written: %+v", got)
	}

	// The first COMMITTED failure must still start at the base delay.
	verifier.mu.Lock()
	verifier.onVerify = nil
	verifier.mu.Unlock()
	if err := m.Smoke(context.Background(), "a", "codex", nil); err == nil {
		t.Fatal("expected the verification failure to surface")
	}
	got := store.recorded()
	if len(got) != 1 || got[0].NextRetryAt == nil {
		t.Fatalf("expected one recorded transient result, got %+v", got)
	}
	if d := got[0].NextRetryAt.Sub(now); d != time.Minute {
		t.Fatalf("backoff after discarded results: got %v want %v", d, time.Minute)
	}
}

// TestMaintainerDiscardsHealthyResultWhenRunWroteNothing is the misattribution
// case: a clean run whose persist closure saved NOTHING (credmaterialize's
// unchanged gate) reports zero self-writes, so the single generation bump that
// did land belongs to a concurrent credential replacement and the healthy
// result must be discarded.
//
// Under the previous invocation-based signal this bump was credited to the run
// — "the closure was invoked, so one bump is mine" — which let a healthy
// verdict for the OLD credential overwrite the state of the replacement and
// made an unverified credential selectable.
func TestMaintainerDiscardsHealthyResultWhenRunWroteNothing(t *testing.T) {
	store := newAuthStore(codexAcct("a", nil))
	creds := &genCreds{}
	// persist:false => SelfCredentialWrites == 0: the closure ran but the
	// unchanged gate meant it saved nothing.
	verifier := &fakeVerifier{persist: false, onVerify: creds.bump}
	m := newMaintainer(t, verifier, store, creds, WithAuthCheckJitter(0, nil))

	// Every attempt is superseded (onVerify bumps on each run), so the retry is
	// discarded too and the caller must hear "inconclusive" rather than nil: a
	// clean result about replaced bytes is not evidence the stored credential
	// works.
	err := m.Smoke(context.Background(), "a", "codex", nil)
	if err == nil {
		t.Fatal("a clean result discarded as stale must not be reported as a passing live test")
	}
	if !errors.Is(err, ErrVerificationInconclusive) {
		t.Fatalf("expected ErrVerificationInconclusive, got %v", err)
	}
	if got := store.recorded(); len(got) != 0 {
		t.Fatalf("a result for a credential that was replaced mid-check was written: %+v", got)
	}
	if calls := verifier.callCount(); calls != 2 {
		t.Fatalf("verify calls = %d, want 2 (one attempt plus one retry against the current credential)", calls)
	}
}

// TestMaintainerCreditsEveryWriteARunPerformed pins the other direction of the
// count: a run may legitimately write more than once (MaterializeCodex folds a
// refreshed auth.json back, then PersistBack saves again). Both bumps are the
// run's own, so the result is still committable.
//
// A boolean "did persist save" signal would fail here — it can credit at most
// one bump and would discard this healthy result.
func TestMaintainerCreditsEveryWriteARunPerformed(t *testing.T) {
	store := newAuthStore(codexAcct("a", nil))
	creds := &genCreds{}
	verifier := &fakeVerifier{selfWrites: 2, onVerify: func() { creds.bump(); creds.bump() }}
	m := newMaintainer(t, verifier, store, creds, WithAuthCheckJitter(0, nil))

	if err := m.Smoke(context.Background(), "a", "codex", nil); err != nil {
		t.Fatalf("Smoke: %v", err)
	}
	got := store.recorded()
	if len(got) != 1 {
		t.Fatalf("expected the healthy result to be recorded, got %d writes", len(got))
	}
	if got[0].Outcome != models.AuthCheckOutcomeHealthy {
		t.Fatalf("outcome: got %q want healthy", got[0].Outcome)
	}
}

// TestMaintainerReportsUnrecordedAuthCheckToExplicitCaller pins AC-4's honesty
// requirement: when the durable write fails after a CLEAN verification, an
// explicit caller must not be told the credential passed. The row may still
// read auth_invalid, and the resolver would keep refusing the account.
func TestMaintainerReportsUnrecordedAuthCheckToExplicitCaller(t *testing.T) {
	store := newAuthStore(codexAcct("a", nil))
	store.writeErr = errors.New("database is locked")
	verifier := &fakeVerifier{}
	m := newMaintainer(t, verifier, store, nil, WithAuthCheckJitter(0, nil))

	err := m.Smoke(context.Background(), "a", "codex", nil)
	if err == nil {
		t.Fatal("a verification whose durable result was not written must not be reported as a pass")
	}
	if !errors.Is(err, ErrAuthCheckNotRecorded) {
		t.Fatalf("expected ErrAuthCheckNotRecorded, got %v", err)
	}
	// The store failure is preserved for operators; the (clean) verification
	// outcome is preserved separately rather than being reported as the cause.
	var unrecorded *authCheckNotRecordedError
	if !errors.As(err, &unrecorded) {
		t.Fatalf("expected the wrapper type, got %T", err)
	}
	if unrecorded.VerifyErr() != nil {
		t.Fatalf("VerifyErr: got %v want nil (the run itself was clean)", unrecorded.VerifyErr())
	}
	if !strings.Contains(err.Error(), "database is locked") {
		t.Fatalf("expected the store failure to surface, got %v", err)
	}
}

// TestMaintainerRetriesCleanDiscardAgainstCurrentCredential is the recovery
// half: when the first run is superseded but the retry lands cleanly against
// the credential that is actually stored, the caller gets a real pass rather
// than an inconclusive error.
func TestMaintainerRetriesCleanDiscardAgainstCurrentCredential(t *testing.T) {
	store := newAuthStore(codexAcct("a", nil))
	creds := &genCreds{}
	var runs int
	verifier := &fakeVerifier{}
	// Only the FIRST run is superseded; the retry sees a stable credential.
	verifier.onVerify = func() {
		runs++
		if runs == 1 {
			creds.bump()
		}
	}
	m := newMaintainer(t, verifier, store, creds, WithAuthCheckJitter(0, nil))

	if err := m.Smoke(context.Background(), "a", "codex", nil); err != nil {
		t.Fatalf("Smoke: %v", err)
	}
	got := store.recorded()
	if len(got) != 1 {
		t.Fatalf("expected the retry's result to be recorded, got %d writes", len(got))
	}
	if got[0].Outcome != models.AuthCheckOutcomeHealthy {
		t.Fatalf("outcome: got %q want healthy", got[0].Outcome)
	}
}

// TestMaintainerWithdrawsResultWhenCredentialChangesDuringWrite closes the
// window between the generation compare and the durable commit. The credential
// lock is deliberately not held across the write, so a replacement can land
// there; the committed verdict then describes bytes that are gone, and it
// would silently restore state RefreshAccount's clear had withdrawn.
func TestMaintainerWithdrawsResultWhenCredentialChangesDuringWrite(t *testing.T) {
	store := newAuthStore(codexAcct("a", nil))
	creds := &genCreds{}
	// Bump inside the durable write: after the compare accepted, before the
	// maintainer revalidates.
	store.onWrite = creds.bump
	verifier := &fakeVerifier{}
	m := newMaintainer(t, verifier, store, creds, WithAuthCheckJitter(0, nil))

	// Every write bumps, so the retry is superseded too and the caller must
	// hear "inconclusive". Returning the withdrawn run's own (clean) verdict
	// here would hand a joined RefreshAccount a pass for the replacement.
	err := m.Smoke(context.Background(), "a", "codex", nil)
	if err == nil {
		t.Fatal("a withdrawn verdict must not be reported as a passing live test")
	}
	if !errors.Is(err, ErrVerificationInconclusive) {
		t.Fatalf("expected ErrVerificationInconclusive, got %v", err)
	}
	if got := store.cleared(); len(got) == 0 {
		t.Fatal("expected the verdict to be withdrawn for account a, got no clears")
	}
	for _, id := range store.cleared() {
		if id != "a" {
			t.Fatalf("withdrew the wrong account: %v", store.cleared())
		}
	}
}

// TestMaintainerRetriesAfterWithdrawingSupersededVerdict is the recovery half:
// once the credential settles, the retry that follows a withdrawal records a
// real verdict for the credential that is actually stored.
func TestMaintainerRetriesAfterWithdrawingSupersededVerdict(t *testing.T) {
	store := newAuthStore(codexAcct("a", nil))
	creds := &genCreds{}
	var writes int
	// Only the FIRST durable write races a replacement; the retry's does not.
	store.onWrite = func() {
		writes++
		if writes == 1 {
			creds.bump()
		}
	}
	verifier := &fakeVerifier{}
	m := newMaintainer(t, verifier, store, creds, WithAuthCheckJitter(0, nil))

	if err := m.Smoke(context.Background(), "a", "codex", nil); err != nil {
		t.Fatalf("Smoke: %v", err)
	}
	if got := store.cleared(); len(got) != 1 {
		t.Fatalf("expected exactly one withdrawal, got %v", got)
	}
	if calls := verifier.callCount(); calls != 2 {
		t.Fatalf("verify calls = %d, want 2 (the superseded run plus its retry)", calls)
	}
}

// TestMaintainerReportsUnrecordedWhenWithdrawalFails covers the other branch:
// if the compensating clear itself fails, the row still holds a verdict for
// replaced bytes and nothing later guarantees it is corrected, so an explicit
// caller must not be told the credential passed.
func TestMaintainerReportsUnrecordedWhenWithdrawalFails(t *testing.T) {
	store := newAuthStore(codexAcct("a", nil))
	creds := &genCreds{}
	store.onWrite = creds.bump
	store.clearErr = errors.New("database is locked")
	verifier := &fakeVerifier{}
	m := newMaintainer(t, verifier, store, creds, WithAuthCheckJitter(0, nil))

	err := m.Smoke(context.Background(), "a", "codex", nil)
	if err == nil {
		t.Fatal("a stale verdict that could not be withdrawn must not be reported as a pass")
	}
	if !errors.Is(err, ErrAuthCheckNotRecorded) {
		t.Fatalf("expected ErrAuthCheckNotRecorded, got %v", err)
	}
}

// TestMaintainerKeepsResultWhenCredentialIsStableThroughWrite is the control:
// the withdrawal must not fire on the ordinary path, or every healthy result
// would be thrown away immediately after being recorded.
func TestMaintainerKeepsResultWhenCredentialIsStableThroughWrite(t *testing.T) {
	store := newAuthStore(codexAcct("a", nil))
	creds := &genCreds{}
	verifier := &fakeVerifier{}
	m := newMaintainer(t, verifier, store, creds, WithAuthCheckJitter(0, nil))

	if err := m.Smoke(context.Background(), "a", "codex", nil); err != nil {
		t.Fatalf("Smoke: %v", err)
	}
	if got := store.cleared(); len(got) != 0 {
		t.Fatalf("a stable credential's result was withdrawn: clears %v", got)
	}
	if got := store.recorded(); len(got) != 1 {
		t.Fatalf("expected exactly one durable write, got %d", len(got))
	}
}

// TestMaintainerRecordsHealthyResultDespiteOwnPersistBump is the flip side:
// a clean codex run's own PersistBack bump must NOT be mistaken for a
// concurrent credential replacement (AC-3).
func TestMaintainerRecordsHealthyResultDespiteOwnPersistBump(t *testing.T) {
	store := newAuthStore(codexAcct("a", nil))
	creds := &genCreds{}
	verifier := &fakeVerifier{persist: true, onVerify: creds.bump}
	m := newMaintainer(t, verifier, store, creds, WithAuthCheckJitter(0, nil))

	if err := m.Smoke(context.Background(), "a", "codex", nil); err != nil {
		t.Fatalf("Smoke: %v", err)
	}
	got := store.recorded()
	if len(got) != 1 {
		t.Fatalf("expected the healthy result to be recorded, got %d writes", len(got))
	}
	if got[0].Outcome != models.AuthCheckOutcomeHealthy {
		t.Fatalf("outcome: got %q want healthy", got[0].Outcome)
	}
}

// TestMaintainerPersistsThroughSmokeRunnerPersistBack pins AC-3's persist
// path: the coordinator credits the runner's reported self-write count, which
// is what a clean codex smoke reports after actually saving a refreshed blob.
func TestMaintainerPersistsThroughSmokeRunnerPersistBack(t *testing.T) {
	store := newAuthStore(codexAcct("a", nil))
	verifier := &fakeVerifier{persist: true}
	m := newMaintainer(t, verifier, store, nil, WithAuthCheckJitter(0, nil))

	if err := m.Smoke(context.Background(), "a", "codex", nil); err != nil {
		t.Fatalf("Smoke: %v", err)
	}
	got := store.recorded()
	if len(got) != 1 || got[0].Outcome != models.AuthCheckOutcomeHealthy {
		t.Fatalf("expected one healthy record, got %+v", got)
	}
	if got[0].FailureClass != "" {
		t.Fatalf("healthy record must carry no failure class, got %q", got[0].FailureClass)
	}
	if got[0].NextRetryAt != nil {
		t.Fatalf("healthy record must not schedule a retry, got %v", got[0].NextRetryAt)
	}
	if got[0].CheckedAt == nil {
		t.Fatal("healthy record must carry a check time")
	}
}

// --- classification -------------------------------------------------------

// TestClassifyVerification covers AC-4 and AC-5's classification contract:
// only a confirmed auth failure is auth-invalid; every unavailable, throttled,
// and unrecognized failure stays transient.
func TestClassifyVerification(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name        string
		err         error
		res         SmokeResult
		wantOutcome models.AuthCheckOutcome
		wantClass   string
	}{
		{"clean", nil, SmokeResult{}, models.AuthCheckOutcomeHealthy, ""},
		{
			// BOS-1174 both directions, half one: a clean run on a credential
			// well inside its access-token lifetime is the COMMON case. A
			// client that has not refreshed yet is behaving normally, so this
			// must stay healthy or the new state is pure noise.
			"clean and not yet due to refresh",
			nil,
			SmokeResult{RefreshAssertion: credmaterialize.RefreshAssertionNotDue},
			models.AuthCheckOutcomeHealthy, "",
		},
		{
			// Half two: the run was clean, the access token says a refresh was
			// overdue, and nothing wrote the credential. That is the dead
			// refresh chain hiding behind a still-valid access token.
			"clean but the refresh was due and never happened",
			nil,
			SmokeResult{RefreshAssertion: credmaterialize.RefreshAssertionOverdue},
			models.AuthCheckOutcomeRefreshChainUnproven, authFailureRefreshNotObserved,
		},
		{
			// A refresh that was due AND actually happened is the healthiest
			// signal this check can produce: the chain was exercised and it
			// worked. The write count is what tells it from the case above.
			"clean, refresh was due, and the run wrote the credential",
			nil,
			SmokeResult{
				RefreshAssertion:     credmaterialize.RefreshAssertionOverdue,
				SelfCredentialWrites: 1,
			},
			models.AuthCheckOutcomeHealthy, "",
		},
		{
			// A token whose expiry could not be read yields "cannot evaluate".
			// Absence of a readable claim is not evidence of a dead chain, and
			// reporting one on a parsing failure would warn about accounts for
			// a reason that has nothing to do with their credential.
			"clean but the expiry could not be evaluated",
			nil,
			SmokeResult{RefreshAssertion: credmaterialize.RefreshAssertionUnknown},
			models.AuthCheckOutcomeHealthy, "",
		},
		{
			// Combine, never substitute: the expiry assertion refines a clean
			// run and has no say over one that failed. An overdue refresh on a
			// run the provider rejected is still auth_invalid.
			"an overdue refresh never overrides an error path",
			errors.New("credential verification failed: authentication token has been invalidated"),
			SmokeResult{RefreshAssertion: credmaterialize.RefreshAssertionOverdue},
			models.AuthCheckOutcomeAuthInvalid, authFailureAuthInvalidated,
		},
		{
			"missing runner",
			agent.AgentRunnerNotLoaded("codex", nil),
			SmokeResult{},
			models.AuthCheckOutcomeUnavailable, authFailureRunnerUnavailable,
		},
		{
			"wrapped missing runner",
			errors.New("credential verification unavailable: " + agent.ErrAgentRunnerNotLoaded.Error()),
			SmokeResult{},
			models.AuthCheckOutcomeTransient, authFailureUnclassified,
		},
		{
			"auth invalidated",
			errors.New("credential verification failed: authentication token has been invalidated"),
			SmokeResult{},
			models.AuthCheckOutcomeAuthInvalid, authFailureAuthInvalidated,
		},
		{
			"sign in again",
			errors.New("credential verification failed: please sign in again"),
			SmokeResult{},
			models.AuthCheckOutcomeAuthInvalid, authFailureAuthInvalidated,
		},
		{
			"usage cap",
			errors.New("credential verification failed: usage limit reached"),
			SmokeResult{},
			models.AuthCheckOutcomeTransient, authFailureUsageExhausted,
		},
		{
			"recognized transient",
			errors.New("credential verification failed: the service is temporarily unavailable"),
			SmokeResult{},
			models.AuthCheckOutcomeTransient, authFailureTransientProvider,
		},
		{
			"unrecognized",
			errors.New("credential verification failed: sprockets exceeded the widget quorum"),
			SmokeResult{},
			models.AuthCheckOutcomeTransient, authFailureUnclassified,
		},
		{
			// The signal a real expired codex credential actually produces.
			// agenterr's patterns are written for claude's wording and match
			// none of it, so without the runner-sentinel check this records
			// transient/unclassified and the account is never benched.
			"codex auth sentinel",
			errors.New("credential verification failed: " + codexAuthRequiredSentinel),
			SmokeResult{},
			models.AuthCheckOutcomeAuthInvalid, authFailureAuthInvalidated,
		},
		{
			// The login shell exited without ever running the binary, so
			// nothing reached the provider. agenterr has no pattern for 127
			// or "command not found", so without the non-execution sentinel
			// check this falls through to transient/unclassified and the
			// maintainer retries a PATH fault on an escalating backoff
			// forever (BOS-1172).
			"codex non-execution sentinel (127)",
			errors.New(codexRunnerUnavailableSentinel),
			SmokeResult{},
			models.AuthCheckOutcomeUnavailable, authFailureRunnerUnavailable,
		},
		{
			// 126 is the same operator instruction with a different cause:
			// the target resolved but could not be executed.
			"codex non-execution sentinel (126)",
			errors.New(codexRunnerNotExecutableSentinel),
			SmokeResult{},
			models.AuthCheckOutcomeUnavailable, authFailureRunnerUnavailable,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outcome, class := classifyVerification(tc.err, tc.res, now)
			if outcome != tc.wantOutcome {
				t.Fatalf("outcome: got %q want %q", outcome, tc.wantOutcome)
			}
			if class != tc.wantClass {
				t.Fatalf("class: got %q want %q", class, tc.wantClass)
			}
		})
	}
}

// TestClassifyVerificationThrottleIsTransient pins the real gRPC throttle
// contract (ResourceExhausted + RetryInfo) rather than a stand-in.
func TestClassifyVerificationThrottleIsTransient(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	outcome, class := classifyVerification(throttleErr(t, 2*time.Minute), SmokeResult{}, now)
	if outcome != models.AuthCheckOutcomeTransient {
		t.Fatalf("throttle outcome: got %q want transient", outcome)
	}
	if class != authFailureRateLimited {
		t.Fatalf("throttle class: got %q want %q", class, authFailureRateLimited)
	}
}

// TestMaintainerTransientFailuresBackOffExponentially is AC-5's backoff half.
func TestMaintainerTransientFailuresBackOffExponentially(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	store := newAuthStore(codexAcct("a", nil))
	verifier := &fakeVerifier{err: errors.New("sprockets exceeded the widget quorum")}
	clock := newFakeClock(now)
	m := newMaintainer(t, verifier, store, nil,
		WithAuthCheckClock(clock),
		WithAuthCheckJitter(0, nil),
		WithAuthCheckBackoff(time.Minute, 4*time.Minute),
	)

	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 4 * time.Minute}
	for i, wantWait := range want {
		if err := m.Smoke(context.Background(), "a", "codex", nil); err == nil {
			t.Fatalf("attempt %d: expected the verification error to surface", i)
		}
		got := store.recorded()
		rec := got[len(got)-1]
		if rec.Outcome != models.AuthCheckOutcomeTransient {
			t.Fatalf("attempt %d outcome: got %q want transient", i, rec.Outcome)
		}
		if rec.FailureClass != authFailureUnclassified {
			t.Fatalf("attempt %d class: got %q", i, rec.FailureClass)
		}
		if rec.NextRetryAt == nil {
			t.Fatalf("attempt %d: transient result must schedule a retry", i)
		}
		if got := rec.NextRetryAt.Sub(now); got != wantWait {
			t.Fatalf("attempt %d backoff: got %v want %v", i, got, wantWait)
		}
	}

	// A healthy verification clears the escalation and stops scheduling retries.
	verifier.mu.Lock()
	verifier.err = nil
	verifier.mu.Unlock()
	if err := m.Smoke(context.Background(), "a", "codex", nil); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	got := store.recorded()
	rec := got[len(got)-1]
	if rec.Outcome != models.AuthCheckOutcomeHealthy || rec.NextRetryAt != nil {
		t.Fatalf("recovery record: %+v", rec)
	}
	// The next failure restarts at the base delay, not the ceiling.
	verifier.mu.Lock()
	verifier.err = errors.New("sprockets failed again")
	verifier.mu.Unlock()
	_ = m.Smoke(context.Background(), "a", "codex", nil)
	got = store.recorded()
	rec = got[len(got)-1]
	if d := rec.NextRetryAt.Sub(now); d != time.Minute {
		t.Fatalf("post-recovery backoff: got %v want %v", d, time.Minute)
	}
}

// TestMaintainerHonoursProviderRetryAfter proves an explicit RetryInfo wins
// over the computed backoff when it asks for longer.
func TestMaintainerHonoursProviderRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	store := newAuthStore(codexAcct("a", nil))
	verifier := &fakeVerifier{err: throttleErr(t, 90*time.Minute)}
	clock := newFakeClock(now)
	m := newMaintainer(t, verifier, store, nil,
		WithAuthCheckClock(clock),
		WithAuthCheckJitter(0, nil),
		WithAuthCheckBackoff(time.Minute, 5*time.Minute),
	)
	_ = m.Smoke(context.Background(), "a", "codex", nil)

	rec := store.recorded()[0]
	if rec.NextRetryAt == nil || rec.NextRetryAt.Sub(now) != 90*time.Minute {
		t.Fatalf("expected the provider RetryInfo to win, got %+v", rec)
	}
}

// TestMaintainerAuthInvalidClearsOnRecovery is AC-4's "a later successful
// verification clears that state".
func TestMaintainerAuthInvalidClearsOnRecovery(t *testing.T) {
	store := newAuthStore(codexAcct("a", nil))
	verifier := &fakeVerifier{err: errors.New("credential verification failed: invalidated oauth token")}
	m := newMaintainer(t, verifier, store, nil, WithAuthCheckJitter(0, nil))

	_ = m.Smoke(context.Background(), "a", "codex", nil)
	rec := store.recorded()[0]
	if rec.Outcome != models.AuthCheckOutcomeAuthInvalid {
		t.Fatalf("outcome: got %q want auth_invalid", rec.Outcome)
	}
	if rec.FailureClass != authFailureAuthInvalidated {
		t.Fatalf("class: got %q", rec.FailureClass)
	}

	verifier.mu.Lock()
	verifier.err = nil
	verifier.mu.Unlock()
	if err := m.Smoke(context.Background(), "a", "codex", nil); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	recovered := store.recorded()[1]
	if recovered.Outcome != models.AuthCheckOutcomeHealthy || recovered.FailureClass != "" {
		t.Fatalf("recovery record: %+v", recovered)
	}
}

// TestMaintainerInconclusiveCheckPreservesAuthInvalid is the other half of
// AC-4: only a later SUCCESS clears the bench. Durable eligibility is
// latest-outcome-only — RecordAuthCheck overwrites auth_check_outcome and no
// sticky column survives it — so writing "we could not tell" over a confirmed
// auth_invalid verdict would hand a credential the provider already rejected
// straight back to both selection tiers, with no re-authentication and no
// passing verification in between.
func TestMaintainerInconclusiveCheckPreservesAuthInvalid(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		err  error
	}{
		// The reviewer's case: the Codex runner is temporarily not loaded, so
		// verification could not happen at all.
		{"runner unavailable", agent.ErrAgentRunnerNotLoaded},
		// And the ordinary transient one: an unrecognized provider failure.
		{"transient", errors.New("credential verification failed: 503 upstream unavailable")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			benched := codexAcct("a", &now)
			benched.AuthCheck.Outcome = models.AuthCheckOutcomeAuthInvalid
			benched.AuthCheck.FailureClass = authFailureAuthInvalidated

			store := newAuthStore(benched)
			clock := newFakeClock(now)
			m := newMaintainer(t, &fakeVerifier{err: tc.err}, store, nil,
				WithAuthCheckClock(clock),
				WithAuthCheckJitter(0, nil),
				WithAuthCheckBackoff(time.Minute, 5*time.Minute),
			)

			_ = m.Smoke(context.Background(), "a", "codex", nil)

			got := store.recorded()
			if len(got) != 1 {
				t.Fatalf("writes: got %d want 1", len(got))
			}
			rec := got[0]
			if rec.Outcome != models.AuthCheckOutcomeAuthInvalid {
				t.Fatalf("outcome: got %q want auth_invalid", rec.Outcome)
			}
			if rec.FailureClass != authFailureAuthInvalidated {
				t.Fatalf("class: got %q want %q", rec.FailureClass, authFailureAuthInvalidated)
			}
			// The check still HAPPENED: the time advances and the backoff is
			// armed, so the account keeps being re-checked and a later healthy
			// run can clear it on its own.
			if rec.CheckedAt == nil || !rec.CheckedAt.Equal(now) {
				t.Fatalf("checked at: got %v want %v", rec.CheckedAt, now)
			}
			if rec.NextRetryAt == nil || rec.NextRetryAt.Sub(now) != time.Minute {
				t.Fatalf("next retry: got %v want now+1m", rec.NextRetryAt)
			}
			// What the eligibility predicates would read back off this row.
			written := codexAcct("a", rec.CheckedAt)
			written.AuthCheck = rec
			if !written.IsAuthInvalid() {
				t.Fatalf("account became selectable again after an inconclusive check: %+v", rec)
			}
		})
	}
}

// TestMaintainerPreserveReadFailureRecordsNothing pins the fail-closed half:
// when the recorded verdict cannot be read, an inconclusive result is not
// written at all. Writing it blind could clear a bench we could not confirm,
// and an explicit caller must not read an unrecorded run as a pass.
func TestMaintainerPreserveReadFailureRecordsNothing(t *testing.T) {
	store := newAuthStore(codexAcct("a", nil))
	store.getErr = errors.New("database is locked")
	m := newMaintainer(t, &fakeVerifier{err: agent.ErrAgentRunnerNotLoaded}, store, nil,
		WithAuthCheckJitter(0, nil))

	err := m.Smoke(context.Background(), "a", "codex", nil)
	if !errors.Is(err, ErrAuthCheckNotRecorded) {
		t.Fatalf("err: got %v want ErrAuthCheckNotRecorded", err)
	}
	if got := store.recorded(); len(got) != 0 {
		t.Fatalf("writes: got %d want 0", len(got))
	}
}

// TestMaintainerRecordsNoCredentialMaterial is the redaction guard for AC-3/6:
// whatever the provider says, only closed-set tokens reach durable state.
func TestMaintainerRecordsNoCredentialMaterial(t *testing.T) {
	const secret = "sk-live-SUPERSECRET-refresh-token"
	store := newAuthStore(codexAcct("a", nil))
	verifier := &fakeVerifier{err: errors.New("credential verification failed: token " + secret + " rejected")}
	m := newMaintainer(t, verifier, store, nil, WithAuthCheckJitter(0, nil))

	_ = m.Smoke(context.Background(), "a", "codex", nil)
	rec := store.recorded()[0]
	if rec.FailureClass == "" {
		t.Fatal("expected a redacted failure class")
	}
	if rec.FailureClass == secret || len(rec.FailureClass) > 32 {
		t.Fatalf("failure class looks like provider text: %q", rec.FailureClass)
	}
	for _, allowed := range []string{
		authFailureRunnerUnavailable, authFailureAuthInvalidated, authFailureSuspended,
		authFailureRateLimited, authFailureUsageExhausted, authFailureTransientProvider,
		authFailureUnclassified,
	} {
		if rec.FailureClass == allowed {
			return
		}
	}
	t.Fatalf("failure class %q is not one of the closed set", rec.FailureClass)
}

// TestMaintainerRequiresVerifierAndStore pins the constructor contract.
func TestMaintainerRequiresVerifierAndStore(t *testing.T) {
	if _, err := NewCredentialMaintainer(nil, newAuthStore(), nil, zerolog.Nop()); err == nil {
		t.Fatal("expected an error for a nil verifier")
	}
	if _, err := NewCredentialMaintainer(&fakeVerifier{}, nil, nil, zerolog.Nop()); err == nil {
		t.Fatal("expected an error for a nil store")
	}
}

// TestNilMaintainerIsSafe covers the degrade path main.go relies on.
func TestNilMaintainerIsSafe(t *testing.T) {
	var m *CredentialMaintainer
	m.Start(context.Background())
	m.Stop()
	if err := m.Smoke(context.Background(), "a", "codex", nil); err == nil {
		t.Fatal("expected a nil maintainer to report unavailability")
	}
}

// TestSmokeRunnerSatisfiesCredentialVerifier keeps the production wiring
// honest: the coordinator must be able to wrap the real smoke runner.
func TestSmokeRunnerSatisfiesCredentialVerifier(t *testing.T) {
	var _ CredentialVerifier = (*SmokeRunner)(nil)
}

// TestToMetaProjectsAuthInvalid is the seam that carries BOS-1141's durable
// state into the resolver's eligibility decision.
func TestToMetaProjectsAuthInvalid(t *testing.T) {
	for _, tc := range []struct {
		outcome models.AuthCheckOutcome
		want    bool
	}{
		{models.AuthCheckOutcomeUnknown, false},
		{models.AuthCheckOutcomeHealthy, false},
		{models.AuthCheckOutcomeTransient, false},
		{models.AuthCheckOutcomeUnavailable, false},
		{models.AuthCheckOutcomeAuthInvalid, true},
	} {
		t.Run(string(tc.outcome)+"_", func(t *testing.T) {
			acct := newCodexAccount()
			acct.AuthCheck = models.AuthCheck{Outcome: tc.outcome}
			if got := toMeta(acct).AuthInvalid; got != tc.want {
				t.Fatalf("AuthInvalid for %q = %v, want %v", tc.outcome, got, tc.want)
			}
		})
	}
}

// TestClassifyVerificationIgnoresSmokeLogTail is M1/AC-5's core guard: the
// agent-authored log tail that appendSmokeDiagnostic attaches for operators is
// NOT a classification input. A transient failure whose tail happens to
// contain sign-in phrasing (an agent that was, say, working on a login flow)
// must stay transient — auth-invalid removes the account from both selection
// tiers, so it may only come from what the runner itself reported.
func TestClassifyVerificationIgnoresSmokeLogTail(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	logPath := filepath.Join(t.TempDir(), "smoke.log")
	tail := "starting run\n" +
		"please sign in again to continue\n" +
		"authentication token has been invalidated\n"
	if err := os.WriteFile(logPath, []byte(tail), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	for _, tc := range []struct {
		name        string
		base        error
		wantOutcome models.AuthCheckOutcome
		wantClass   string
	}{
		{
			"transient underneath an auth-shaped tail",
			errors.New("credential verification failed: the service is temporarily unavailable"),
			models.AuthCheckOutcomeTransient, authFailureTransientProvider,
		},
		{
			"unrecognized underneath an auth-shaped tail",
			errors.New("credential verification failed: sprockets exceeded the widget quorum"),
			models.AuthCheckOutcomeTransient, authFailureUnclassified,
		},
		{
			"missing runner underneath an auth-shaped tail",
			agent.AgentRunnerNotLoaded("codex", nil),
			models.AuthCheckOutcomeUnavailable, authFailureRunnerUnavailable,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := appendSmokeDiagnostic(tc.base, logPath)
			// Non-vacuity: the auth phrasing really is present in the error text
			// that the old classifier read.
			if !strings.Contains(wrapped.Error(), "sign in again") {
				t.Fatalf("expected the log tail in the error message, got %q", wrapped.Error())
			}
			outcome, class := classifyVerification(wrapped, SmokeResult{}, now)
			if outcome != tc.wantOutcome {
				t.Fatalf("outcome: got %q want %q", outcome, tc.wantOutcome)
			}
			if class != tc.wantClass {
				t.Fatalf("class: got %q want %q", class, tc.wantClass)
			}
		})
	}

	// The flip side: a genuine auth failure REPORTED BY THE RUNNER is still
	// auth-invalid, diagnostic or not.
	real := appendSmokeDiagnostic(
		errors.New("credential verification failed: authentication token has been invalidated"),
		logPath,
	)
	if outcome, class := classifyVerification(real, SmokeResult{}, now); outcome != models.AuthCheckOutcomeAuthInvalid ||
		class != authFailureAuthInvalidated {
		t.Fatalf("runner-reported auth failure: got %q/%q want auth_invalid/%s",
			outcome, class, authFailureAuthInvalidated)
	}
}

// TestClassifyVerificationPermissionDeniedStaysTransient pins that a
// transport-level PermissionDenied on the agent-runner smoke path is NOT read
// as a confirmed suspension. That gRPC contract belongs to the claude usage
// probe (IsSuspension / MarkSuspendedIfConfirmed); no agent-runner plugin
// reserves the code, so it must not bench an account.
func TestClassifyVerificationPermissionDeniedStaysTransient(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	err := grpcstatus.Error(codes.PermissionDenied, "rpc permission denied")
	if !IsSuspension(err) {
		t.Fatal("non-vacuity: expected IsSuspension to hold for this error")
	}
	outcome, class := classifyVerification(err, SmokeResult{}, now)
	if outcome != models.AuthCheckOutcomeTransient {
		t.Fatalf("outcome: got %q want transient", outcome)
	}
	if class != authFailureUnclassified {
		t.Fatalf("class: got %q want %q", class, authFailureUnclassified)
	}
}

// TestMaintainerNeverLeaksProviderTextToStateOrLogs is the redaction property
// asserted over the paths provider text actually flows through — the
// smoke-diagnostic wrap and the two log sites (the sweep's per-account line
// and the record-failure warning) — rather than over the closed constant set.
func TestMaintainerNeverLeaksProviderTextToStateOrLogs(t *testing.T) {
	const secret = "sk-live-SUPERSECRET-refresh-token-9f3a"

	logPath := filepath.Join(t.TempDir(), "smoke.log")
	if err := os.WriteFile(logPath, []byte("refresh token "+secret+" was rejected\n"), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}
	providerErr := appendSmokeDiagnostic(
		errors.New("credential verification failed: token "+secret+" rejected"), logPath)

	assertNoSecret := func(t *testing.T, buf *bytes.Buffer, store *authStore) {
		t.Helper()
		if bytes.Contains(buf.Bytes(), []byte(secret)) {
			t.Fatalf("credential-shaped token reached a log line: %s", buf.String())
		}
		for _, rec := range store.recorded() {
			if strings.Contains(string(rec.Outcome), secret) || strings.Contains(rec.FailureClass, secret) {
				t.Fatalf("credential-shaped token reached durable auth_check state: %+v", rec)
			}
		}
	}

	// 1. The ordinary recorded path, driven through sweep (which owns the
	//    per-account log line).
	var buf bytes.Buffer
	store := newAuthStore(codexAcct("a", nil))
	m, err := NewCredentialMaintainer(&fakeVerifier{err: providerErr}, store, nil,
		zerolog.New(&buf).Level(zerolog.DebugLevel), WithAuthCheckJitter(0, nil))
	if err != nil {
		t.Fatalf("NewCredentialMaintainer: %v", err)
	}
	m.sweep(context.Background())
	if got := store.recorded(); len(got) != 1 {
		t.Fatalf("non-vacuity: expected one recorded result, got %d", len(got))
	}
	assertNoSecret(t, &buf, store)

	// 2. The durable write fails, exercising the record-failure warning.
	buf.Reset()
	failing := newAuthStore(codexAcct("a", nil))
	failing.writeErr = errors.New("database is locked")
	m2, err := NewCredentialMaintainer(&fakeVerifier{err: providerErr}, failing, nil,
		zerolog.New(&buf).Level(zerolog.DebugLevel), WithAuthCheckJitter(0, nil))
	if err != nil {
		t.Fatalf("NewCredentialMaintainer: %v", err)
	}
	m2.sweep(context.Background())
	if !bytes.Contains(buf.Bytes(), []byte("could not record")) {
		t.Fatalf("non-vacuity: expected the record-failure warning, got %s", buf.String())
	}
	assertNoSecret(t, &buf, failing)

	// 3. The generation guard discards the result, exercising the stale line.
	buf.Reset()
	discarded := newAuthStore(codexAcct("a", nil))
	creds := &genCreds{}
	m3, err := NewCredentialMaintainer(
		&fakeVerifier{err: providerErr, onVerify: func() { creds.bump(); creds.bump() }},
		discarded, creds,
		zerolog.New(&buf).Level(zerolog.DebugLevel), WithAuthCheckJitter(0, nil))
	if err != nil {
		t.Fatalf("NewCredentialMaintainer: %v", err)
	}
	m3.sweep(context.Background())
	if !bytes.Contains(buf.Bytes(), []byte("credential changed mid-check")) {
		t.Fatalf("non-vacuity: expected the discard line, got %s", buf.String())
	}
	assertNoSecret(t, &buf, discarded)
}

// codexAuthRequiredSentinel duplicates plugins/bossd-plugin-codex/auth.go's
// ErrAuthRequired text. The plugin and the host must not share an internal
// package, so this test is the pin that keeps the copied literal in
// codexAuthRequiredMarkers honest: if the plugin's wording changes, update
// both, and this test is what fails first.
const codexAuthRequiredSentinel = "codex auth required: run `codex login`"

// codexRunnerUnavailableSentinel / codexRunnerNotExecutableSentinel duplicate
// plugins/bossd-plugin-codex/unavailable.go's ErrRunnerUnavailable and
// ErrRunnerNotExecutable text, for the same reason and with the same contract
// as codexAuthRequiredSentinel above.
//
// The pin is two-sided because neither module can import the other:
// TestRunnerUnavailableSentinelMessages (plugin side) freezes the bytes the
// plugin emits, and TestClassifyVerificationRunnerUnavailableSentinel (here)
// freezes the bytes the host matches. A reword on either side reds a test
// instead of silently degrading the classification to transient/unclassified.
const (
	codexRunnerUnavailableSentinel   = "could not run the codex binary: the login shell exited 127 without executing it"
	codexRunnerNotExecutableSentinel = "could not run the codex binary: the login shell exited 126 without executing it"
)

// TestClassifyVerificationCodexAuthSentinel builds the error exactly the way
// the production path does -- ExitStatus reports ExitError, waitClean wraps it
// with the "credential verification failed: " prefix, and appendSmokeDiagnostic
// then appends an agent-authored log tail -- and asserts a genuine expired
// codex credential is classified auth-invalid through that whole chain.
//
// The second case is the regression this pins from the other side: the same
// benign-looking tail must NOT be able to manufacture an auth-invalid verdict
// on a transient failure.
func TestClassifyVerificationCodexAuthSentinel(t *testing.T) {
	now := time.Now()

	exitErr := errors.New("credential verification failed: " + codexAuthRequiredSentinel)
	wrapped := &smokeDiagnosticError{err: exitErr, diag: "diagnostic: codex exited after printing a banner"}

	outcome, class := classifyVerification(wrapped, SmokeResult{}, now)
	if outcome != models.AuthCheckOutcomeAuthInvalid {
		t.Fatalf("expired codex credential: outcome got %q want %q", outcome, models.AuthCheckOutcomeAuthInvalid)
	}
	if class != authFailureAuthInvalidated {
		t.Fatalf("expired codex credential: class got %q want %q", class, authFailureAuthInvalidated)
	}

	// Mirror image: the sentinel appearing only in the stripped log tail must
	// not bench a working account.
	transient := &smokeDiagnosticError{
		err:  errors.New("credential verification failed: the service is temporarily unavailable"),
		diag: "diagnostic: the agent printed " + codexAuthRequiredSentinel + " while reading docs",
	}
	outcome, class = classifyVerification(transient, SmokeResult{}, now)
	if outcome != models.AuthCheckOutcomeTransient {
		t.Fatalf("sentinel in log tail only: outcome got %q want %q", outcome, models.AuthCheckOutcomeTransient)
	}
	if class == authFailureAuthInvalidated {
		t.Fatalf("sentinel in log tail only: class must not be %q", authFailureAuthInvalidated)
	}
}

// TestClassifyVerificationRunnerUnavailableSentinel is the host half of the
// two-sided drift pin for the non-execution sentinel (BOS-1172).
//
// It asserts three things the table test cannot:
//
//  1. The host's copied marker literals actually appear in the messages the
//     plugin emits. A reworded plugin error that no longer contains a marker
//     fails here rather than silently degrading to transient/unclassified.
//  2. The classification is reached by the sentinel branch and NOT by the
//     agenterr fallthrough -- proved by asserting agenterr itself recognises
//     nothing in these messages, so the branch is load-bearing.
//  3. The new branch does not shadow the auth sentinel sitting beside it: a
//     genuine provider rejection is still auth-invalid, and the two marker
//     sets are disjoint in both directions.
func TestClassifyVerificationRunnerUnavailableSentinel(t *testing.T) {
	now := time.Now()

	for _, msg := range []string{codexRunnerUnavailableSentinel, codexRunnerNotExecutableSentinel} {
		if !isRunnerUnavailableSentinel(msg) {
			t.Fatalf("host markers %q do not match the plugin sentinel %q; the copied literal has drifted",
				runnerUnavailableMarkers, msg)
		}
		// Non-vacuity: if agenterr already classified this text, the new
		// branch would be untested decoration and the table entry would pass
		// through the fallthrough instead.
		if kind := agenterr.Classify(msg, now).Kind; kind != agenterr.KindNone {
			t.Fatalf("agenterr already classifies %q as %v; the sentinel branch is not what the table test exercises", msg, kind)
		}
		// The whole point of the outcome: eligibility is untouched, because
		// an unrunnable binary is not a credential verdict.
		outcome, class := classifyVerification(errors.New(msg), SmokeResult{}, now)
		if outcome != models.AuthCheckOutcomeUnavailable || class != authFailureRunnerUnavailable {
			t.Fatalf("%q: got %q/%q want %q/%q", msg, outcome, class,
				models.AuthCheckOutcomeUnavailable, authFailureRunnerUnavailable)
		}
		if (models.AuthCheck{Outcome: outcome}).IsAuthInvalid() {
			t.Fatalf("%q: an unrunnable binary must not bench the account", msg)
		}
	}

	// The auth sentinel it sits beside is unshadowed in both directions.
	if isRunnerUnavailableSentinel(codexAuthRequiredSentinel) {
		t.Fatalf("the non-execution markers must not match the auth sentinel %q", codexAuthRequiredSentinel)
	}
	for _, msg := range []string{codexRunnerUnavailableSentinel, codexRunnerNotExecutableSentinel} {
		if isRunnerAuthSentinel(msg) {
			t.Fatalf("the auth markers must not match the non-execution sentinel %q", msg)
		}
	}
	outcome, class := classifyVerification(
		errors.New("credential verification failed: "+codexAuthRequiredSentinel), SmokeResult{}, now)
	if outcome != models.AuthCheckOutcomeAuthInvalid || class != authFailureAuthInvalidated {
		t.Fatalf("provider rejection after the new branch: got %q/%q want %q/%q",
			outcome, class, models.AuthCheckOutcomeAuthInvalid, authFailureAuthInvalidated)
	}
}

// codexPluginNonExecutionSource locates plugins/bossd-plugin-codex/
// unavailable.go by walking up from the test's working directory, so it
// resolves both under `go test` in the checkout and under a Bazel runfiles
// tree (where the file is supplied by the go_test data dependency).
func codexPluginNonExecutionSource(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		candidate := filepath.Join(dir, "plugins", "bossd-plugin-codex", "unavailable.go")
		if _, statErr := os.Stat(candidate); statErr == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// Never a skip: an unreachable plugin source silently retires the
			// only gate that compares the two sides of the boundary.
			t.Fatal("plugins/bossd-plugin-codex/unavailable.go not reachable from the test working directory; " +
				"if this is a Bazel run, the go_test data dependency on it has been dropped")
		}
		dir = parent
	}
}

// TestRunnerUnavailableMarkersMatchPluginSource is the half of the drift pin
// that actually crosses the boundary (BOS-1172).
//
// TestRunnerUnavailableSentinelMessages (plugin side) and
// TestClassifyVerificationRunnerUnavailableSentinel (host side) each freeze
// their OWN copy of the literal, so they only catch a one-sided edit. A
// plugin-side-complete reword -- the sentinel and the plugin's own test
// updated together, the host forgotten -- leaves both suites green while host
// classification silently degrades to transient/unclassified, which is the
// exact regression this branch fixes. Acceptance criterion 6 asks for a test
// that fails when the plugin's message and the host's markers drift apart, so
// this one reads the plugin's SOURCE and compares it to the host's literals.
//
// Reading a source file is not a module dependency: nothing here imports the
// plugin. The precedent is lib/bossalib/productparity, which gates copy drift
// across four modules the same way.
func TestRunnerUnavailableMarkersMatchPluginSource(t *testing.T) {
	path := codexPluginNonExecutionSource(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	source := string(raw)

	// Every marker the host matches on must exist verbatim as a Go string
	// literal in the plugin that emits it.
	for _, marker := range runnerUnavailableMarkers {
		if !strings.Contains(source, strconv.Quote(marker)) {
			t.Fatalf("host marker %q no longer appears as a literal in %s; the plugin sentinel was reworded "+
				"without updating runnerUnavailableMarkers in credcheck.go", marker, path)
		}
	}

	// And the host's frozen full sentinels must decompose into that marker
	// plus a remainder the plugin source also spells out, so neither half of
	// either message can move unnoticed.
	marker := runnerUnavailableMarkers[0]
	for _, sentinel := range []string{codexRunnerUnavailableSentinel, codexRunnerNotExecutableSentinel} {
		idx := strings.Index(sentinel, marker)
		if idx < 0 {
			t.Fatalf("frozen sentinel %q does not carry the host marker %q", sentinel, marker)
		}
		if suffix := sentinel[idx+len(marker):]; !strings.Contains(source, strconv.Quote(suffix)) {
			t.Fatalf("frozen sentinel %q: %s no longer spells out %q; update the constant here to match the plugin",
				sentinel, path, suffix)
		}
	}
}

// TestMaintainerUnavailableDoesNotEscalateBackoff is the acceptance criterion
// the outcome string alone cannot carry (BOS-1172).
//
// The harm of the old classification was not the label -- it was the recorded
// failure level: `unavailable` shared a switch arm with `transient`, so a PATH
// fault that can never clear itself walked the exponential ladder like a
// provider blip, and backoff() wrote that inflated level into NextRetryAt --
// the field the accounts table persists and the API surfaces. A test that
// asserted only `outcome == unavailable` would pass with that escalation fully
// intact.
//
// It also pins the other half of the rule: an unavailable result must not
// CLEAR an escalation a genuine transient failure earned. "We learned nothing"
// cuts both ways.
func TestMaintainerUnavailableDoesNotEscalateBackoff(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	store := newAuthStore(codexAcct("a", nil))
	verifier := &fakeVerifier{err: errors.New(codexRunnerUnavailableSentinel)}
	clock := newFakeClock(now)
	m := newMaintainer(t, verifier, store, nil,
		WithAuthCheckClock(clock),
		WithAuthCheckJitter(0, nil),
		WithAuthCheckBackoff(time.Minute, 4*time.Minute),
	)

	// Four consecutive unavailable results. A transient classification would
	// walk 1m, 2m, 4m, 4m here; a non-escalating one stays at the base delay.
	for i := 0; i < 4; i++ {
		if err := m.Smoke(context.Background(), "a", "codex", nil); err == nil {
			t.Fatalf("attempt %d: expected the verification error to surface", i)
		}
		rec := store.recorded()[i]
		if rec.Outcome != models.AuthCheckOutcomeUnavailable {
			t.Fatalf("attempt %d outcome: got %q want %q", i, rec.Outcome, models.AuthCheckOutcomeUnavailable)
		}
		if rec.FailureClass != authFailureRunnerUnavailable {
			t.Fatalf("attempt %d class: got %q want %q", i, rec.FailureClass, authFailureRunnerUnavailable)
		}
		if rec.NextRetryAt == nil {
			t.Fatalf("attempt %d: an unavailable result must still schedule a re-check", i)
		}
		if got := rec.NextRetryAt.Sub(now); got != time.Minute {
			t.Fatalf("attempt %d backoff: got %v want %v (unavailable must not escalate)", i, got, time.Minute)
		}
	}

	// A real transient failure escalates from the level unavailable left
	// alone -- the base delay, not a ladder the unavailable runs had climbed.
	verifier.mu.Lock()
	verifier.err = errors.New("sprockets exceeded the widget quorum")
	verifier.mu.Unlock()
	if err := m.Smoke(context.Background(), "a", "codex", nil); err == nil {
		t.Fatal("expected the transient verification error to surface")
	}
	got := store.recorded()
	rec := got[len(got)-1]
	if rec.Outcome != models.AuthCheckOutcomeTransient {
		t.Fatalf("transient outcome: got %q", rec.Outcome)
	}
	if d := rec.NextRetryAt.Sub(now); d != time.Minute {
		t.Fatalf("first transient after unavailable runs: got %v want %v", d, time.Minute)
	}

	// And an unavailable result arriving mid-escalation must not forgive it:
	// the transient ladder resumes where it was, at 2m, not back at the base.
	verifier.mu.Lock()
	verifier.err = errors.New(codexRunnerNotExecutableSentinel)
	verifier.mu.Unlock()
	if err := m.Smoke(context.Background(), "a", "codex", nil); err == nil {
		t.Fatal("expected the unavailable verification error to surface")
	}
	got = store.recorded()
	rec = got[len(got)-1]
	if d := rec.NextRetryAt.Sub(now); d != time.Minute {
		t.Fatalf("unavailable mid-escalation: got %v want the unchanged level %v", d, time.Minute)
	}
	verifier.mu.Lock()
	verifier.err = errors.New("sprockets exceeded the widget quorum")
	verifier.mu.Unlock()
	if err := m.Smoke(context.Background(), "a", "codex", nil); err == nil {
		t.Fatal("expected the transient verification error to surface")
	}
	got = store.recorded()
	rec = got[len(got)-1]
	if d := rec.NextRetryAt.Sub(now); d != 2*time.Minute {
		t.Fatalf("transient after an interleaved unavailable: got %v want %v (the escalation must survive)", d, 2*time.Minute)
	}
}

// TestNewCredentialMaintainerDefaultStaleness pins the 6h default that
// WithAuthCheckStaleness no longer overrides in production wiring. Without
// this the option is dead code and the default is unasserted.
func TestNewCredentialMaintainerDefaultStaleness(t *testing.T) {
	m, err := NewCredentialMaintainer(&fakeVerifier{}, newAuthStore(), nil, zerolog.Nop())
	if err != nil {
		t.Fatalf("NewCredentialMaintainer: %v", err)
	}
	if m.staleness != defaultAuthCheckStaleness {
		t.Fatalf("default staleness: got %v want %v", m.staleness, defaultAuthCheckStaleness)
	}
}

// TestMaintainerConsultsKillSwitchEachSweep pins the managed-accounts kill
// switch as a LIVE read rather than a boot-time snapshot.
//
// The daemon starts this maintainer unconditionally, so the gate inside sweep
// is the only thing standing between a disabled operator setting and a real
// Codex verification subprocess. `boss settings --no-managed-accounts` rewrites
// settings.json with no restart and no RPC, so both transitions have to be
// observed while the loop runs: a disabled sweep must verify nothing, and a
// later enable must take effect without a restart.
//
// The assertion is deliberately on the count of verifications per sweep, not on
// the number of times the loader was called: a maintainer that consulted the
// switch once at construction and cached the answer would still "call" it, and
// would still pass a call-counting test while sweeping through a kill switch
// the operator had turned off.
func TestMaintainerConsultsKillSwitchEachSweep(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	store := newAuthStore(codexAcct("never-checked", nil))
	verifier := &fakeVerifier{}
	clock := newFakeClock(now)

	var mu sync.Mutex
	enabled := false
	setEnabled := func(v bool) { mu.Lock(); enabled = v; mu.Unlock() }

	m := newMaintainer(t, verifier, store, nil,
		WithAuthCheckClock(clock),
		WithAuthCheckJitter(0, nil),
		WithAuthCheckEnabled(func() bool {
			mu.Lock()
			defer mu.Unlock()
			return enabled
		}),
	)
	m.Start(context.Background())
	t.Cleanup(m.Stop)

	// Sweep 1, kill switch off: the account is due, so anything that reaches
	// the store would verify it.
	clock.fire(t)
	clock.waitForWaits(t, 2) // the loop parked on the next interval ⇒ sweep 1 finished
	if n := verifier.callCount(); n != 0 {
		t.Fatalf("kill switch off: %d verification(s) ran; a disabled sweep must spawn no provider work", n)
	}

	// Operator flips it on mid-run. No restart happens here by construction.
	setEnabled(true)

	// Sweep 2 must pick the new value up.
	clock.fire(t)
	writes := store.waitForWrites(t, 1)
	if n := verifier.callCount(); n != 1 {
		t.Fatalf("kill switch on: %d verification(s) ran, want 1; the switch is being read once and cached, not per sweep", n)
	}
	// The re-enabled sweep must produce a real durable verdict, not merely a
	// call: a gate that let the sweep through but short-circuited before the
	// commit would satisfy the count above and still leave nothing recorded.
	if got := writes[0].Outcome; got != models.AuthCheckOutcomeHealthy {
		t.Fatalf("recorded outcome = %q, want %q", got, models.AuthCheckOutcomeHealthy)
	}
}

// TestAuthFailureCredentialSupersededLiteralContract pins the exact wire value
// the daemon writes into AuthCheck.failure_class for a superseded stored
// credential.
//
// The value is DUPLICATED in every surface that renders it, because none of
// them can import this package:
//
//   - services/boss/internal/views/account_health.go (the TUI mirror)
//   - services/web/src/lib/accountRows.ts (the web mirror)
//   - services/boss/internal/fixtures/fixtures.go (the proof fixture)
//
// Drift detection over those copies is one-directional and this is the missing
// direction. The proof scenario pins fixture→TUI and accountRows.test.ts pins
// the TS constant, but nothing fails if THIS producer's literal is renamed:
// the daemon would then write a class no surface matches, both mirrors would
// silently fall back to a bare "ok", and every other test would still pass —
// restoring exactly the green-while-broken state BOS-1175 exists to prevent.
//
// Rename the constant freely; changing its VALUE means changing all three
// readers above in the same commit.
func TestAuthFailureCredentialSupersededLiteralContract(t *testing.T) {
	const wire = "credential_superseded"
	if authFailureCredentialSuperseded != wire {
		t.Fatalf("authFailureCredentialSuperseded = %q, want %q; the TUI (views/account_health.go), the web (lib/accountRows.ts) and the proof fixture (fixtures/fixtures.go) all match this literal and must change with it",
			authFailureCredentialSuperseded, wire)
	}
}

// --- BOS-1174: the refresh-chain-unproven outcome -------------------------

// TestOutcomeSwitchesCoverEveryOutcome pins BOTH closed switches over the whole
// closed outcome set, which is the only way this class of defect is visible.
//
// An outcome missing from settledOutcome is not a compile error and produces no
// log line: it silently becomes "inconclusive", so preserveSettledOutcome keeps
// a stale auth-invalid bench across a run that disproved it, and classify sends
// a settled answer down the retry-escalation path. Both symptoms show up days
// later as scheduling behavior nobody connects back to a missing switch case.
func TestOutcomeSwitchesCoverEveryOutcome(t *testing.T) {
	for _, tc := range []struct {
		outcome     models.AuthCheckOutcome
		wantSettled bool
	}{
		{models.AuthCheckOutcomeUnknown, false},
		{models.AuthCheckOutcomeHealthy, true},
		{models.AuthCheckOutcomeAuthInvalid, true},
		{models.AuthCheckOutcomeTransient, false},
		{models.AuthCheckOutcomeUnavailable, false},
		// The BOS-1174 addition. It is an answer: only a clean run produces it.
		{models.AuthCheckOutcomeRefreshChainUnproven, true},
	} {
		t.Run(string(tc.outcome), func(t *testing.T) {
			if got := settledOutcome(tc.outcome); got != tc.wantSettled {
				t.Fatalf("settledOutcome(%q) = %v, want %v", tc.outcome, got, tc.wantSettled)
			}
			// The two predicates are exact complements by construction; pin
			// that so a future edit cannot let them drift apart.
			if got := inconclusiveOutcome(tc.outcome); got == tc.wantSettled {
				t.Fatalf("inconclusiveOutcome(%q) = %v, want %v", tc.outcome, got, !tc.wantSettled)
			}
		})
	}
}

// TestRefreshChainUnprovenIsASettledAnswerForBackoff proves the new outcome
// reaches classify's SETTLED arm rather than falling through to neither.
//
// A value in neither arm looks identical to a settled one in the record it
// writes — NextRetryAt is nil either way — so the only observable difference is
// whether the failure counter was reset. This escalates the backoff, records
// the new outcome, then fails again: a settled arm restarts at the base delay,
// a dropped value resumes the escalation.
func TestRefreshChainUnprovenIsASettledAnswerForBackoff(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	store := newAuthStore(codexAcct("a", nil))
	verifier := &fakeVerifier{err: errors.New("sprockets exceeded the widget quorum")}
	clock := newFakeClock(now)
	m := newMaintainer(t, verifier, store, nil,
		WithAuthCheckClock(clock),
		WithAuthCheckJitter(0, nil),
		WithAuthCheckBackoff(time.Minute, 4*time.Minute),
	)

	// Two transient failures escalate the backoff to 2x base.
	for i := 0; i < 2; i++ {
		if err := m.Smoke(context.Background(), "a", "codex", nil); err == nil {
			t.Fatalf("attempt %d: expected the verification error to surface", i)
		}
	}
	if got := store.recorded(); got[len(got)-1].NextRetryAt.Sub(now) != 2*time.Minute {
		t.Fatalf("precondition: backoff did not escalate")
	}

	// A clean run on a credential whose refresh was overdue.
	verifier.mu.Lock()
	verifier.err = nil
	verifier.refresh = credmaterialize.RefreshAssertionOverdue
	verifier.mu.Unlock()
	if err := m.Smoke(context.Background(), "a", "codex", nil); err != nil {
		t.Fatalf("unproven run: %v", err)
	}
	rec := lastRecord(t, store)
	if rec.Outcome != models.AuthCheckOutcomeRefreshChainUnproven {
		t.Fatalf("outcome: got %q want %q", rec.Outcome, models.AuthCheckOutcomeRefreshChainUnproven)
	}
	if rec.FailureClass != authFailureRefreshNotObserved {
		t.Fatalf("class: got %q want %q", rec.FailureClass, authFailureRefreshNotObserved)
	}
	if rec.NextRetryAt != nil {
		t.Fatalf("a settled answer must not schedule an escalated retry, got %v", rec.NextRetryAt)
	}

	// The next failure restarts at the base delay. It would resume at 4x if the
	// outcome had been dropped by classify's switch.
	verifier.mu.Lock()
	verifier.err = errors.New("sprockets failed again")
	verifier.refresh = credmaterialize.RefreshAssertionUnknown
	verifier.mu.Unlock()
	_ = m.Smoke(context.Background(), "a", "codex", nil)
	if d := lastRecord(t, store).NextRetryAt.Sub(now); d != time.Minute {
		t.Fatalf("post-unproven backoff: got %v want %v (the outcome never reached the settled arm)", d, time.Minute)
	}
}

// TestRefreshChainUnprovenClearsASettledBench proves the new outcome is NOT
// inconclusive where it matters: preserveSettledOutcome. The run completed
// cleanly, so it carries everything a healthy verdict carries plus one extra
// observation — keeping a bench the run just disproved would be reading the
// evidence backwards.
func TestRefreshChainUnprovenClearsASettledBench(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	acct := codexAcct("a", nil)
	acct.AuthCheck = models.AuthCheck{
		Outcome:      models.AuthCheckOutcomeAuthInvalid,
		FailureClass: authFailureAuthInvalidated,
	}
	store := newAuthStore(acct)
	verifier := &fakeVerifier{refresh: credmaterialize.RefreshAssertionOverdue}
	m := newMaintainer(t, verifier, store, nil,
		WithAuthCheckClock(newFakeClock(now)),
		WithAuthCheckJitter(0, nil),
	)

	if err := m.Smoke(context.Background(), "a", "codex", nil); err != nil {
		t.Fatalf("smoke: %v", err)
	}
	rec := lastRecord(t, store)
	if rec.Outcome != models.AuthCheckOutcomeRefreshChainUnproven {
		t.Fatalf("outcome: got %q want %q (the settled bench was preserved over a clean run)",
			rec.Outcome, models.AuthCheckOutcomeRefreshChainUnproven)
	}
	if rec.FailureClass != authFailureRefreshNotObserved {
		t.Fatalf("class: got %q want %q", rec.FailureClass, authFailureRefreshNotObserved)
	}
}

// TestRefreshChainUnprovenLeavesSelectionUnchanged is the acceptance criterion
// that keeps this a REPORTING state rather than a condemnation. auth_invalid is
// the only outcome that removes an account from selection, and every selection
// tier — rotation.isSelectable, the resolver's pre-materialization refusal, and
// the BOS-1142 pre-worktree bind gate — asks exactly one question to decide it.
func TestRefreshChainUnprovenLeavesSelectionUnchanged(t *testing.T) {
	check := models.AuthCheck{Outcome: models.AuthCheckOutcomeRefreshChainUnproven}
	if check.IsAuthInvalid() {
		t.Fatal("AuthCheck.IsAuthInvalid must stay false: the credential demonstrably still works")
	}
	acct := &models.Account{
		ID:        "a",
		Provider:  models.AccountProviderCodex,
		Status:    models.AccountStatusActive,
		Health:    models.AccountHealthOK,
		AuthCheck: check,
	}
	if acct.IsAuthInvalid() {
		t.Fatal("Account.IsAuthInvalid must stay false for a refresh-chain-unproven account")
	}
	// The single choke point every selection tier reads.
	if toMeta(acct).AuthInvalid {
		t.Fatal("AccountMeta.AuthInvalid must stay false: a warning is not a bench")
	}
}

// lastRecord returns the most recent durable auth-check write, failing the test
// when nothing was recorded at all — a discarded result and a recorded one are
// different outcomes and must not read the same way.
func lastRecord(t *testing.T, store *authStore) models.AuthCheck {
	t.Helper()
	got := store.recorded()
	if len(got) == 0 {
		t.Fatal("no verification result was recorded")
	}
	return got[len(got)-1]
}
