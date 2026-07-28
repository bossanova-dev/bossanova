package session

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/rotation"
)

// --- BOS-584: confirm before cooling on the proxy 429 path ---
//
// A proxied upstream 429 used to be treated as proof of plan exhaustion and
// benched the bound account for a flat DefaultCooldown (60m) with no probe and
// no way to clear it early. Anthropic also returns 429 for short-window rate
// limiting and overload, so a network blip cost a completely healthy account an
// hour of availability. These tests pin the confirm-before-cool discipline the
// headless usage-limit intercept has always had (rotation.go).

// incidentHealthySnapshot is the EXACT account snapshot captured for
// agent.yuki@kamik.ai during the BOS-584 incident: the provider reported the
// plan ACTIVE with 11% of the 5-hour window and 2% of the weekly window used,
// while bossd benched it for a full hour off a single transient 429.
func incidentHealthySnapshot(fetchedAt time.Time) models.UsageSnapshot {
	return models.UsageSnapshot{
		Status:    "RATE_LIMIT_PLAN_STATUS_ACTIVE",
		Util5h:    0.11,
		Util7d:    0.02,
		FetchedAt: &fetchedAt,
	}
}

// TestPrepareFailover_ConfirmsBeforeCooling drives every confirm-before-cool
// branch through BOTH proxy targets — the plain session target and the tmux
// chat target — because both build the same UsageLimited signal and a fix on
// only one of them still benches healthy accounts for half the proxy.
func TestPrepareFailover_ConfirmsBeforeCooling(t *testing.T) {
	fetched := time.Now().UTC().Truncate(time.Millisecond)
	reset5h := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Millisecond)
	reset7d := time.Now().Add(30 * time.Hour).UTC().Truncate(time.Millisecond)

	cases := []struct {
		name               string
		status             int
		snap               models.UsageSnapshot
		probeErr           error
		nilProbe           bool
		notRotationCapable bool
		wantProbeCalls     int
		wantSuppress       bool
		wantResetAt        *time.Time
	}{
		{
			// The regression case for this ticket.
			name:           "429 whose probe says the account is not limited benches nothing",
			status:         429,
			snap:           incidentHealthySnapshot(fetched),
			wantProbeCalls: 1,
			wantSuppress:   true,
		},
		{
			name:   "429 whose probe confirms the cap cools until the probed reset",
			status: 429,
			snap: models.UsageSnapshot{
				Status:    "RATE_LIMIT_PLAN_STATUS_RATE_LIMITED",
				Util5h:    1,
				Util7d:    1,
				Reset5h:   &reset5h,
				Reset7d:   &reset7d,
				FetchedAt: &fetched,
			},
			wantProbeCalls: 1,
			wantResetAt:    &reset7d, // later of the two exhausted windows
		},
		{
			name:   "429 whose probe confirms the cap without a reset falls back to the default cooldown",
			status: 429,
			snap: models.UsageSnapshot{
				Status:    "RATE_LIMIT_PLAN_STATUS_RATE_LIMITED",
				FetchedAt: &fetched,
			},
			wantProbeCalls: 1,
			wantResetAt:    nil, // engine applies its own DefaultCooldown
		},
		{
			// The correlated-failure case: the incident log shows the usage probe
			// timing out four minutes BEFORE the 429. Benching on a signal we could
			// not verify is exactly what caused the outage, so this must fail OPEN.
			name:           "429 whose probe errors fails open and benches nothing",
			status:         429,
			probeErr:       context.DeadlineExceeded,
			wantProbeCalls: 1,
			wantSuppress:   true,
		},
		{
			// The shape production ACTUALLY produces on a probe failure: the daemon's
			// rateLimitProbe closure (cmd/main.go) swallows the error and hands back a
			// zero snapshot with err == nil. This must fail open like any other
			// unverifiable answer — and must NOT be mistaken for an authoritative
			// "healthy" verdict.
			name:           "429 whose probe answers with no snapshot fails open and benches nothing",
			status:         429,
			snap:           models.UsageSnapshot{},
			wantProbeCalls: 1,
			wantSuppress:   true,
		},
		{
			name:           "429 whose probe is unsupported fails open and benches nothing",
			status:         429,
			snap:           models.UsageSnapshot{Status: "RATE_LIMIT_PLAN_STATUS_UNSUPPORTED", FetchedAt: &fetched},
			wantProbeCalls: 1,
			wantSuppress:   true,
		},
		{
			name:           "429 whose probe is unspecified fails open and benches nothing",
			status:         429,
			snap:           models.UsageSnapshot{Status: "RATE_LIMIT_PLAN_STATUS_UNSPECIFIED", FetchedAt: &fetched},
			wantProbeCalls: 1,
			wantSuppress:   true,
		},
		{
			// 401 is an auth question, not a quota question: it fails health rather
			// than cooling, so it must not pay the probe's latency at all.
			name:           "401 invokes no usage probe and is unchanged",
			status:         401,
			snap:           incidentHealthySnapshot(fetched),
			wantProbeCalls: 0,
		},
		{
			// Fail-SAFE seam, matching every other optional adapter in this package:
			// with no probe wired the proxy degrades to the pre-BOS-584 behaviour.
			name:           "429 with no probe wired cools exactly as before",
			status:         429,
			nilProbe:       true,
			wantProbeCalls: 0,
		},
		{
			// A binding that cannot rotate short-circuits in Engine.Decide BEFORE any
			// store access, so no cooldown can be written whatever we answer. Paying
			// the inline probe's latency on a live proxied request for an answer
			// nobody reads is pure cost.
			name:               "429 on a binding that cannot rotate invokes no usage probe",
			status:             429,
			snap:               incidentHealthySnapshot(fetched),
			notRotationCapable: true,
			wantProbeCalls:     0,
		},
	}

	for _, tc := range cases {
		for _, target := range []string{"session target", "chat target"} {
			t.Run(tc.name+"/"+target, func(t *testing.T) {
				f := newRotationFixture(t)
				enableFailoverProxy(f.lc)
				f.decider.outcome = rotation.Outcome{
					Kind:        rotation.OutcomeRotate,
					NextAccount: &models.Account{ID: "acct-next", Provider: models.AccountProviderClaude},
				}
				f.probe.snap = tc.snap
				f.probe.err = tc.probeErr
				if tc.nilProbe {
					f.lc.rateLimitProbe = nil
				}
				if tc.notRotationCapable {
					f.binding.binding.RotationCapable = false
				}

				proxyTarget := f.sessionID
				wantCapped := "acct-capped"
				if target == "chat target" {
					f.lc.agentChats = &mockAgentChatStore{chatsBySession: map[string][]*models.AgentChat{
						f.sessionID: {{
							SessionID:      f.sessionID,
							AgentSessionID: "chat-1",
							AgentName:      "claude",
							AccountID:      stringPtr("acct-chat"),
						}},
					}}
					f.binding.binding.CappedAccountID = "acct-chat"
					proxyTarget = ProxyTargetForChat("chat-1", "acct-default")
					wantCapped = "acct-chat"
				}

				res, err := f.lc.PrepareFailover(context.Background(), proxyTarget, tc.status)
				if err != nil {
					t.Fatalf("PrepareFailover: %v", err)
				}

				// No confirm-before-cool branch may swallow the failover itself: the
				// client's request deserves a retry regardless of whether the account
				// is benched. The stub decider always answers OutcomeRotate, so this
				// pins "the cooldown decision did not suppress the rotation", NOT what
				// the real engine returns — the not-rotation-capable row would
				// short-circuit to OutcomeStatusOnly against a real engine, and only
				// its wantProbeCalls assertion is a production claim.
				if !res.Rotate {
					t.Fatalf("Rotate = false, want true (the request must still fail over)")
				}
				if f.probe.calls != tc.wantProbeCalls {
					t.Fatalf("probe calls = %d, want %d", f.probe.calls, tc.wantProbeCalls)
				}
				sig := f.decider.lastSig
				if sig.CappedAccountID != wantCapped {
					t.Fatalf("capped account = %q, want %q", sig.CappedAccountID, wantCapped)
				}
				if sig.SuppressCooldown != tc.wantSuppress {
					t.Fatalf("SuppressCooldown = %v, want %v", sig.SuppressCooldown, tc.wantSuppress)
				}
				switch {
				case tc.wantResetAt == nil && sig.ResetAt != nil:
					t.Fatalf("ResetAt = %v, want nil", sig.ResetAt)
				case tc.wantResetAt != nil && (sig.ResetAt == nil || !sig.ResetAt.Equal(*tc.wantResetAt)):
					t.Fatalf("ResetAt = %v, want probed reset %v", sig.ResetAt, *tc.wantResetAt)
				}
			})
		}
	}
}

// TestPrepareFailover_TransientCapDoesNotBenchHealthyAccount is the decisive
// regression test for BOS-584: it replays the incident snapshot through the
// proxy 429 path against a REAL rotation.Engine, and asserts the healthy
// account's cooldown_until is never written — it stays selectable immediately
// afterwards. The sibling case proves a genuine cap still cools, so the test
// cannot be satisfied by simply never cooling anything.
func TestPrepareFailover_TransientCapDoesNotBenchHealthyAccount(t *testing.T) {
	fetched := time.Now().UTC().Truncate(time.Millisecond)
	genuineReset := time.Now().Add(4 * time.Hour).UTC().Truncate(time.Millisecond)

	tests := []struct {
		name        string
		snap        models.UsageSnapshot
		wantCooling bool
	}{
		{
			name:        "transient 429 against an ACTIVE plan at 11%/2% benches nothing",
			snap:        incidentHealthySnapshot(fetched),
			wantCooling: false,
		},
		{
			name: "genuine cap still benches until its probed reset",
			snap: models.UsageSnapshot{
				Status:    "RATE_LIMIT_PLAN_STATUS_RATE_LIMITED",
				Util5h:    1,
				Reset5h:   &genuineReset,
				FetchedAt: &fetched,
			},
			wantCooling: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRotationFixture(t)
			enableFailoverProxy(f.lc)
			f.probe.snap = tt.snap

			capped := &models.Account{
				ID: "acct-capped", Provider: models.AccountProviderClaude,
				Status: models.AccountStatusActive, Health: models.AccountHealthOK,
			}
			next := &models.Account{
				ID: "acct-next", Provider: models.AccountProviderClaude, Priority: 1,
				Status: models.AccountStatusActive, Health: models.AccountHealthOK,
			}
			store := &cooldownAccountRepo{accounts: []*models.Account{capped, next}}
			engine := rotation.NewEngine(store)
			f.lc.SetRotationDecider(engine.Decide)

			res, err := f.lc.PrepareFailover(context.Background(), f.sessionID, 429)
			if err != nil {
				t.Fatalf("PrepareFailover: %v", err)
			}
			if !res.Rotate || res.NextAccountID != "acct-next" {
				t.Fatalf("result = %+v, want a rotation onto acct-next", res)
			}
			if got := capped.CooldownUntil != nil; got != tt.wantCooling {
				t.Fatalf("acct-capped cooling = %v (until %v), want %v",
					got, capped.CooldownUntil, tt.wantCooling)
			}
			if !tt.wantCooling {
				// "Remains selectable immediately afterwards": a fresh cap signal for
				// the OTHER account can still route back onto acct-capped.
				out, err := engine.Decide(context.Background(), rotation.Signal{
					Provider: "claude", CappedAccountID: "acct-next",
					Kind: rotation.UsageLimited, RotationCapable: true,
				})
				if err != nil {
					t.Fatalf("Decide: %v", err)
				}
				if out.NextAccount == nil || out.NextAccount.ID != "acct-capped" {
					t.Fatalf("next account = %v, want acct-capped (must still be selectable)", out.NextAccount)
				}
				return
			}
			if !capped.CooldownUntil.Equal(genuineReset) {
				t.Fatalf("cooldown_until = %v, want the probed reset %v (never an arbitrary hour)",
					capped.CooldownUntil, genuineReset)
			}
		})
	}
}

// TestPrepareFailover_ProbeDeadlineFailsOpen proves the inline probe is bounded:
// a probe that outlives the proxy's own budget must not stall the live request
// forever, and its deadline must fail OPEN (rotate, bench nothing).
func TestPrepareFailover_ProbeDeadlineFailsOpen(t *testing.T) {
	f := newRotationFixture(t)
	enableFailoverProxy(f.lc)
	// Shorten the real budget rather than waiting it out: the assertion is that a
	// bound EXISTS and fails open, not what its production value is.
	f.lc.SetProxyProbeTimeoutForTest(20 * time.Millisecond)
	f.decider.outcome = rotation.Outcome{
		Kind:        rotation.OutcomeRotate,
		NextAccount: &models.Account{ID: "acct-next", Provider: models.AccountProviderClaude},
	}
	f.lc.SetRateLimitProbe(func(ctx context.Context, _ string) (models.UsageSnapshot, error) {
		<-ctx.Done() // never answers; only the proxy's own timeout can end this
		return models.UsageSnapshot{}, ctx.Err()
	})

	type probeResult struct {
		res FailoverResult
		err error
	}
	// Buffered + reported on the TEST goroutine: on the timeout branch below the
	// test finishes while this goroutine is still parked in the probe, and a
	// t.Errorf after that panics the whole run.
	done := make(chan probeResult, 1)
	go func() {
		res, err := f.lc.PrepareFailover(context.Background(), f.sessionID, 429)
		done <- probeResult{res: res, err: err}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("PrepareFailover: %v", got.err)
		}
		if !got.res.Rotate {
			t.Fatalf("Rotate = false, want true (a hung probe must not block failover)")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PrepareFailover did not return: the inline probe is unbounded")
	}
	if !f.decider.lastSig.SuppressCooldown {
		t.Fatal("SuppressCooldown = false, want true (a timed-out probe must fail open)")
	}
}

// TestPrepareFailover_ProbeNeverLogsSecrets keeps the new probe log lines inside
// the package's log-hygiene contract. It captures what the Lifecycle logger
// ACTUALLY emits (the fixture's zerolog.Nop would make any such assertion
// vacuous) and proves the bearer token the failover materializes never reaches
// it — on the fail-open branch, which is the noisiest and the one an operator
// reads during an incident.
func TestPrepareFailover_ProbeNeverLogsSecrets(t *testing.T) {
	cases := []struct {
		name     string
		snap     models.UsageSnapshot
		probeErr error
		wantLog  string
	}{
		{
			name:     "probe error",
			probeErr: errors.New("usage probe unreachable"),
			wantLog:  "usage probe failed",
		},
		{
			// The production failure shape: error swallowed, zero snapshot returned.
			// It must NOT be logged as an authoritative "not limited" verdict.
			name:    "probe answers with no snapshot",
			snap:    models.UsageSnapshot{},
			wantLog: "usage probe returned no snapshot",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			f := newRotationFixture(t)
			f.lc.logger = zerolog.New(&logs)
			enableFailoverProxy(f.lc)
			f.decider.outcome = rotation.Outcome{
				Kind:        rotation.OutcomeRotate,
				NextAccount: &models.Account{ID: "acct-next", Provider: models.AccountProviderClaude},
			}
			f.probe.snap = tc.snap
			f.probe.err = tc.probeErr

			res, err := f.lc.PrepareFailover(context.Background(), f.sessionID, 429)
			if err != nil {
				t.Fatalf("PrepareFailover: %v", err)
			}
			if !f.decider.lastSig.SuppressCooldown {
				t.Fatal("SuppressCooldown = false, want true on an unverifiable probe")
			}
			out := logs.String()
			if !strings.Contains(out, tc.wantLog) {
				t.Fatalf("logs %q, want a line containing %q", out, tc.wantLog)
			}
			// The materialized bearer is what a careless log line would leak.
			if res.Token == "" {
				t.Fatal("no token materialized; the leak assertion below would be vacuous")
			}
			if strings.Contains(out, res.Token) {
				t.Fatalf("logs leaked the materialized bearer token: %q", out)
			}
		})
	}
}

// The package's shared rotation fakes record their last call without locking —
// correct for every other test, which drives them from a single goroutine. The
// coalescing test below is the only one with concurrent callers, so it brings
// its own locked seams rather than making every sibling pay for a mutex.
type syncBinding struct{ binding RotationBinding }

func (s syncBinding) CurrentBinding(context.Context, *models.Session) (RotationBinding, bool, error) {
	return s.binding, true, nil
}

type syncMaterializer struct {
	mu    sync.Mutex
	env   map[string]string
	calls int
}

func (m *syncMaterializer) Materialize(context.Context, *models.Account) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	return m.env, nil
}

type syncDecider struct {
	mu      sync.Mutex
	outcome rotation.Outcome
	lastSig rotation.Signal
	calls   int
}

func (d *syncDecider) Decide(_ context.Context, sig rotation.Signal) (rotation.Outcome, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	d.lastSig = sig
	return d.outcome, nil
}

func (d *syncDecider) signal() rotation.Signal {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastSig
}

// TestPrepareFailover_ConcurrentProbesCoalesce pins the coalescing of the inline
// confirm-before-cool probe. A rate-limit episode 429s every in-flight request
// bound to the same account at once; without coalescing each one fires its own
// MaterializeAccount + ProbeRateLimit round trip plus a usage-cache write, aiming
// a burst of probes at the provider that is already rate-limiting us. They all
// ask the same question at the same instant, so one answer must serve them all.
func TestPrepareFailover_ConcurrentProbesCoalesce(t *testing.T) {
	const callers = 8

	f := newRotationFixture(t)
	enableFailoverProxy(f.lc)
	decider := &syncDecider{outcome: rotation.Outcome{
		Kind:        rotation.OutcomeRotate,
		NextAccount: &models.Account{ID: "acct-next", Provider: models.AccountProviderClaude},
	}}
	f.lc.SetRotationDecider(decider.Decide)
	f.lc.SetRotationBinding(syncBinding{binding: RotationBinding{
		CappedAccountID: "acct-capped", Provider: "claude", RotationCapable: true,
	}})
	f.lc.SetAccountMaterializer(&syncMaterializer{env: map[string]string{claudeOAuthTokenEnvKey: "next-token"}})

	fetched := time.Now().UTC()
	var probes atomic.Int64
	// The probe parks until every caller has arrived, so they provably overlap:
	// a serialized run would deadlock rather than pass by luck.
	arrived := make(chan struct{}, callers)
	release := make(chan struct{})
	f.lc.SetRateLimitProbe(func(context.Context, string) (models.UsageSnapshot, error) {
		probes.Add(1)
		<-release
		return incidentHealthySnapshot(fetched), nil
	})

	var wg sync.WaitGroup
	results := make([]FailoverResult, callers)
	errs := make([]error, callers)
	for i := range callers {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			arrived <- struct{}{}
			results[idx], errs[idx] = f.lc.PrepareFailover(context.Background(), f.sessionID, 429)
		}(i)
	}
	for range callers {
		<-arrived
	}
	// Give the callers a moment to pile into the single-flight before answering,
	// so this measures coalescing rather than a serialized race.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if !results[i].Rotate {
			t.Fatalf("caller %d did not rotate; every caller must get the shared verdict", i)
		}
	}
	if got := probes.Load(); got != 1 {
		t.Fatalf("upstream probes = %d, want 1 (%d concurrent 429s must coalesce)", got, callers)
	}
	if !decider.signal().SuppressCooldown {
		t.Fatal("SuppressCooldown = false, want true (the shared probe said healthy)")
	}
}

// TestPrepareFailover_ProbeOutlivesClientDisconnect proves the cooldown decision
// does not hinge on whether the client stayed on the line. The probe context is
// the proxied request's, so inheriting its cancellation would let an unrelated
// disconnect land on the error arm and suppress a GENUINE cap — the account
// would escape a bench it had earned because a browser tab closed. That is a
// different thing from the deliberate fail-open, which is about the provider
// being unverifiable.
func TestPrepareFailover_ProbeOutlivesClientDisconnect(t *testing.T) {
	fetched := time.Now().UTC()
	genuineReset := time.Now().Add(4 * time.Hour).UTC().Truncate(time.Millisecond)

	f := newRotationFixture(t)
	enableFailoverProxy(f.lc)
	f.decider.outcome = rotation.Outcome{
		Kind:        rotation.OutcomeRotate,
		NextAccount: &models.Account{ID: "acct-next", Provider: models.AccountProviderClaude},
	}
	f.lc.SetRateLimitProbe(func(ctx context.Context, _ string) (models.UsageSnapshot, error) {
		if err := ctx.Err(); err != nil {
			return models.UsageSnapshot{}, err
		}
		return models.UsageSnapshot{
			Status:    "RATE_LIMIT_PLAN_STATUS_RATE_LIMITED",
			Util5h:    1,
			Reset5h:   &genuineReset,
			FetchedAt: &fetched,
		}, nil
	})

	// The client is already gone by the time the 429 is handled.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := f.lc.PrepareFailover(ctx, f.sessionID, 429); err != nil {
		t.Fatalf("PrepareFailover: %v", err)
	}
	sig := f.decider.lastSig
	if sig.SuppressCooldown {
		t.Fatal("SuppressCooldown = true, want false (a disconnect must not excuse a confirmed cap)")
	}
	if sig.ResetAt == nil || !sig.ResetAt.Equal(genuineReset) {
		t.Fatalf("ResetAt = %v, want the probed reset %v", sig.ResetAt, genuineReset)
	}
}

// TestPrepareFailover_SuppressedNoEligibleAuditsTruthfully covers the audit
// detail on the most common deployment shape. With the bench suppressed the
// capped account carries no cooldown, so on a single-account pool the engine
// resolves OutcomeNoEligibleAccount rather than OutcomeAllExhausted — and the
// stock remedy for that outcome ("enable or re-authenticate one") is wrong
// advice about an account that is active, healthy, and merely got a 429 nobody
// could confirm.
func TestPrepareFailover_SuppressedNoEligibleAuditsTruthfully(t *testing.T) {
	f := newRotationFixture(t)
	enableFailoverProxy(f.lc)
	store := &recentAuditStore{}
	f.lc.SetRotationRecorder(rotation.NewRecorder(store, zerolog.Nop()))
	f.probe.snap = incidentHealthySnapshot(time.Now().UTC())
	f.decider.outcome = rotation.Outcome{Kind: rotation.OutcomeNoEligibleAccount}

	if _, err := f.lc.PrepareFailover(context.Background(), f.sessionID, 429); err != nil {
		t.Fatalf("PrepareFailover: %v", err)
	}
	events := store.all()
	if len(events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(events))
	}
	if got := events[0].Detail; got != unconfirmedCapNoAlternateDetail {
		t.Fatalf("detail = %q, want the unconfirmed-cap detail %q", got, unconfirmedCapNoAlternateDetail)
	}
	if events[0].Detail == rotation.NoEligibleAccountDetail {
		t.Fatal("recorded the enable/re-authenticate remedy for a healthy account")
	}
}

// TestPrepareFailover_ConfirmedNoEligibleKeepsStockDetail is the counterweight:
// when the cap WAS confirmed, the no-eligible remedy is still the right advice,
// so the suppressed detail must not leak onto every decline.
func TestPrepareFailover_ConfirmedNoEligibleKeepsStockDetail(t *testing.T) {
	fetched := time.Now().UTC()
	f := newRotationFixture(t)
	enableFailoverProxy(f.lc)
	store := &recentAuditStore{}
	f.lc.SetRotationRecorder(rotation.NewRecorder(store, zerolog.Nop()))
	f.probe.snap = models.UsageSnapshot{
		Status:    "RATE_LIMIT_PLAN_STATUS_RATE_LIMITED",
		Util5h:    1,
		FetchedAt: &fetched,
	}
	f.decider.outcome = rotation.Outcome{Kind: rotation.OutcomeNoEligibleAccount}

	if _, err := f.lc.PrepareFailover(context.Background(), f.sessionID, 429); err != nil {
		t.Fatalf("PrepareFailover: %v", err)
	}
	events := store.all()
	if len(events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(events))
	}
	if got := events[0].Detail; got != rotation.NoEligibleAccountDetail {
		t.Fatalf("detail = %q, want the stock no-eligible remedy", got)
	}
}

// cooldownAccountRepo is a minimal in-memory rotation.AccountRepository so the
// session-package regression test can assert against real engine writes rather
// than a stubbed decision.
type cooldownAccountRepo struct {
	mu       sync.Mutex
	accounts []*models.Account
}

func (r *cooldownAccountRepo) DecideTx(ctx context.Context, _ string, fn func(tx rotation.TxAccountView) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return fn(r)
}

func (r *cooldownAccountRepo) ListByProvider(_ context.Context, _ time.Time) ([]*models.Account, error) {
	return r.accounts, nil
}

func (r *cooldownAccountRepo) SetCooldownIfNotCooling(_ context.Context, accountID string, until, now time.Time) (bool, error) {
	for _, a := range r.accounts {
		if a.ID != accountID {
			continue
		}
		if a.CooldownUntil != nil && a.CooldownUntil.After(now) {
			return false, nil
		}
		u := until
		a.CooldownUntil = &u
		return true, nil
	}
	return false, nil
}

func (r *cooldownAccountRepo) FailHealth(_ context.Context, accountID string) error {
	for _, a := range r.accounts {
		if a.ID == accountID {
			a.Health = models.AccountHealthFailed
		}
	}
	return nil
}
