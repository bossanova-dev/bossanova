package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossd/internal/server"
	"github.com/recurser/bossd/internal/upstream"
)

// fakeCredentialProvider is a streamAuthAdapter token provider whose reload
// verdict the test dictates. It stands in for KeychainTokenProvider so the gate
// can be exercised without a keyring.
type fakeCredentialProvider struct {
	usable        bool
	reloginReason string
	reloadCalls   int32
	// reload overrides what the reload itself observed. Nil means the common
	// case: the record was read cleanly, and the cache below reflects it.
	reload *upstream.ReloadOutcome
}

func (f *fakeCredentialProvider) ReloadResult() upstream.ReloadOutcome {
	atomic.AddInt32(&f.reloadCalls, 1)
	if f.reload != nil {
		return *f.reload
	}
	return upstream.ReloadOutcome{ReadOK: true}
}
func (f *fakeCredentialProvider) Token() string {
	if f.usable {
		return "tok"
	}
	return ""
}
func (f *fakeCredentialProvider) ExpiresAt() time.Time  { return time.Time{} }
func (f *fakeCredentialProvider) ReloginReason() string { return f.reloginReason }
func (f *fakeCredentialProvider) CredentialVerdict() (bool, string) {
	return f.usable, f.reloginReason
}

// fakeReconnector records Reconnect calls so tests can assert the login
// path asks the stream to re-open.
type fakeReconnector struct{ calls int32 }

func (f *fakeReconnector) Reconnect() { atomic.AddInt32(&f.calls, 1) }

// TestNotifyLogin_ReRegistersAndReconnects covers the core BOS-330 fix:
// a login on a running daemon proactively re-registers with the
// orchestrator and wakes the stream to re-open with the fresh token,
// without waiting for the reactive auth-failure path or a restart.
func TestNotifyLogin_ReRegistersAndReconnects(t *testing.T) {
	t.Parallel()

	var reRegisterCalls int32
	reconnector := &fakeReconnector{}
	authState := upstream.NewAuthState()
	authState.MarkNeedsLogin() // daemon started before login → parked

	adapter := &streamAuthAdapter{
		streamClient: reconnector,
		authState:    authState,
		logger:       zerolog.Nop(),
		reRegister: func(ctx context.Context) (string, error) {
			if ctx == nil {
				t.Error("reRegister called with nil context")
			}
			atomic.AddInt32(&reRegisterCalls, 1)
			return "fresh-session-token", nil
		},
	}

	verdict, err := adapter.NotifyLogin(context.Background(), nil)
	if err != nil {
		t.Fatalf("NotifyLogin returned error: %v", err)
	}
	if verdict.Outcome != server.LoginOutcomeOK {
		t.Fatalf("verdict outcome = %v, want LoginOutcomeOK", verdict.Outcome)
	}

	if got := atomic.LoadInt32(&reRegisterCalls); got != 1 {
		t.Fatalf("reRegister calls = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&reconnector.calls); got != 1 {
		t.Fatalf("Reconnect calls = %d, want 1", got)
	}
	if authState.NeedsLogin() {
		t.Fatal("authState still NeedsLogin after NotifyLogin; MarkOK not applied")
	}
}

// TestNotifyLogin_RegistersBeforeClearingAuthGate locks in the ordering
// invariant behind the anti-storm guarantee: the proactive re-register must
// run BEFORE MarkOK clears the NeedsLogin gate. A daemon parked on NeedsLogin
// is woken by MarkOK() itself (not only by Reconnect), so if MarkOK ran first
// the Run loop could re-open with the still-stale token and force a redundant
// reactive re-register. Asserting the gate is still closed at the instant
// reRegister runs proves the ordering; this test fails on the old
// MarkOK-then-reRegister order.
func TestNotifyLogin_RegistersBeforeClearingAuthGate(t *testing.T) {
	t.Parallel()

	authState := upstream.NewAuthState()
	authState.MarkNeedsLogin() // daemon started before login → parked

	var gateClosedAtRegister bool
	adapter := &streamAuthAdapter{
		streamClient: &fakeReconnector{},
		authState:    authState,
		logger:       zerolog.Nop(),
		reRegister: func(context.Context) (string, error) {
			// MarkOK must not have run yet: the gate is still closed while
			// we obtain the fresh session token.
			gateClosedAtRegister = authState.NeedsLogin()
			return "tok", nil
		},
	}

	if _, err := adapter.NotifyLogin(context.Background(), nil); err != nil {
		t.Fatalf("NotifyLogin returned error: %v", err)
	}
	if !gateClosedAtRegister {
		t.Fatal("MarkOK ran before reRegister: auth gate cleared before the fresh token was registered")
	}
	if authState.NeedsLogin() {
		t.Fatal("auth gate still closed after NotifyLogin; MarkOK not applied last")
	}
}

// TestNotifyLogin_ReRegisterErrorIsNonFatal verifies the login path is
// best-effort: a failed re-register does not fail the login and does not
// wake the stream (the reactive path stays responsible for recovery).
func TestNotifyLogin_ReRegisterErrorIsNonFatal(t *testing.T) {
	t.Parallel()

	reconnector := &fakeReconnector{}
	adapter := &streamAuthAdapter{
		streamClient: reconnector,
		authState:    upstream.NewAuthState(),
		logger:       zerolog.Nop(),
		reRegister: func(context.Context) (string, error) {
			return "", errors.New("register boom")
		},
	}

	// Best-effort means the LOGIN is not failed and the auth gate is still
	// cleared — not that the failure is hidden. It now rides back as a verdict
	// (and an error) so `boss login` can say the daemon will retry, instead of
	// the operator finding out from the daemon log.
	verdict, err := adapter.NotifyLogin(context.Background(), nil)
	if err == nil {
		t.Fatal("NotifyLogin swallowed the re-register failure; want it reported")
	}
	if verdict.Outcome != server.LoginOutcomeRegisterFailed {
		t.Fatalf("verdict outcome = %v, want LoginOutcomeRegisterFailed", verdict.Outcome)
	}
	if got := atomic.LoadInt32(&reconnector.calls); got != 0 {
		t.Fatalf("Reconnect calls = %d, want 0 after failed re-register", got)
	}
	if adapter.authState.NeedsLogin() {
		t.Fatal("auth gate left closed after a clean-credential login; only the re-register failed")
	}
}

// TestNotifyLogin_NilReRegisterSkips guards the nil-reRegister path: the
// adapter must still succeed (and not reconnect) when no re-register hook
// is wired.
func TestNotifyLogin_NilReRegisterSkips(t *testing.T) {
	t.Parallel()

	reconnector := &fakeReconnector{}
	adapter := &streamAuthAdapter{
		streamClient: reconnector,
		authState:    upstream.NewAuthState(),
		logger:       zerolog.Nop(),
	}

	verdict, err := adapter.NotifyLogin(context.Background(), nil)
	if err != nil {
		t.Fatalf("NotifyLogin returned error with nil reRegister: %v", err)
	}
	if verdict.Outcome != server.LoginOutcomeOK {
		t.Fatalf("verdict outcome = %v, want LoginOutcomeOK", verdict.Outcome)
	}
	if got := atomic.LoadInt32(&reconnector.calls); got != 0 {
		t.Fatalf("Reconnect calls = %d, want 0 with nil reRegister", got)
	}
}

// TestNotifyLogin_SameUserReLoginNoStorm verifies that each login triggers
// exactly one re-register — repeated same-user logins resume cleanly
// without a register storm.
func TestNotifyLogin_SameUserReLoginNoStorm(t *testing.T) {
	t.Parallel()

	var reRegisterCalls int32
	reconnector := &fakeReconnector{}
	adapter := &streamAuthAdapter{
		streamClient: reconnector,
		authState:    upstream.NewAuthState(),
		logger:       zerolog.Nop(),
		reRegister: func(context.Context) (string, error) {
			atomic.AddInt32(&reRegisterCalls, 1)
			return "tok", nil
		},
	}

	for i := 0; i < 3; i++ {
		if _, err := adapter.NotifyLogin(context.Background(), nil); err != nil {
			t.Fatalf("NotifyLogin #%d returned error: %v", i, err)
		}
	}

	if got := atomic.LoadInt32(&reRegisterCalls); got != 3 {
		t.Fatalf("reRegister calls = %d, want 3 (one per login)", got)
	}
	if got := atomic.LoadInt32(&reconnector.calls); got != 3 {
		t.Fatalf("Reconnect calls = %d, want 3 (one per login)", got)
	}
}

// TestNotifyLogin_FlaggedCredentialsLeaveGateClosed is the BOS-945 regression:
// a login that reloads a record still carrying a persisted re-login marker must
// NOT clear the auth gate. The old code called MarkOK unconditionally, which
// woke both Run loops into a handshake bosso was always going to reject, and
// the daemon fell straight back to NeedsLogin — a recovery that looked like a
// recovery and was not one.
func TestNotifyLogin_FlaggedCredentialsLeaveGateClosed(t *testing.T) {
	t.Parallel()

	authState := upstream.NewAuthState()
	authState.MarkNeedsLogin()
	reconnector := &fakeReconnector{}
	var reRegisterCalls int32

	adapter := &streamAuthAdapter{
		streamClient:  reconnector,
		tokenProvider: &fakeCredentialProvider{usable: false, reloginReason: "refresh_token_rejected"},
		authState:     authState,
		logger:        zerolog.Nop(),
		reRegister: func(context.Context) (string, error) {
			atomic.AddInt32(&reRegisterCalls, 1)
			return "tok", nil
		},
	}

	verdict, err := adapter.NotifyLogin(context.Background(), nil)
	if err == nil {
		t.Fatal("NotifyLogin returned nil error for unusable credentials")
	}
	if verdict.Outcome != server.LoginOutcomeCredentialsFlagged {
		t.Fatalf("verdict outcome = %v, want LoginOutcomeCredentialsFlagged", verdict.Outcome)
	}
	if verdict.ReloginReason != "refresh_token_rejected" {
		t.Fatalf("verdict relogin reason = %q, want refresh_token_rejected", verdict.ReloginReason)
	}
	if !authState.NeedsLogin() {
		t.Fatal("auth gate cleared on flagged credentials; MarkOK must not have run")
	}
	// Re-registering with no usable bearer token only manufactures a
	// misleading "missing or invalid Authorization header" from bosso.
	if got := atomic.LoadInt32(&reRegisterCalls); got != 0 {
		t.Fatalf("reRegister calls = %d, want 0 for unusable credentials", got)
	}
	if got := atomic.LoadInt32(&reconnector.calls); got != 0 {
		t.Fatalf("Reconnect calls = %d, want 0 for unusable credentials", got)
	}
}

// TestNotifyLogin_MissingCredentialsLeaveGateClosed covers the other unusable
// shape: no marker was ever written, but the reload found no access token
// either — commonly a CLI and daemon disagreeing about the keyring backend.
// It must be distinguishable from the flagged case, because the fix differs.
func TestNotifyLogin_MissingCredentialsLeaveGateClosed(t *testing.T) {
	t.Parallel()

	authState := upstream.NewAuthState()
	authState.MarkNeedsLogin()

	adapter := &streamAuthAdapter{
		streamClient:  &fakeReconnector{},
		tokenProvider: &fakeCredentialProvider{usable: false},
		authState:     authState,
		logger:        zerolog.Nop(),
		reRegister:    func(context.Context) (string, error) { return "tok", nil },
	}

	verdict, err := adapter.NotifyLogin(context.Background(), nil)
	if err == nil {
		t.Fatal("NotifyLogin returned nil error for missing credentials")
	}
	if verdict.Outcome != server.LoginOutcomeCredentialsMissing {
		t.Fatalf("verdict outcome = %v, want LoginOutcomeCredentialsMissing", verdict.Outcome)
	}
	if verdict.ReloginReason != "" {
		t.Fatalf("verdict relogin reason = %q, want empty for the missing case", verdict.ReloginReason)
	}
	if !authState.NeedsLogin() {
		t.Fatal("auth gate cleared on missing credentials; MarkOK must not have run")
	}
}

// TestNotifyLogin_UsableCredentialsClearGate is the positive half of the gate:
// a clean reload still reaches the BOS-330 behaviour unchanged.
func TestNotifyLogin_UsableCredentialsClearGate(t *testing.T) {
	t.Parallel()

	authState := upstream.NewAuthState()
	authState.MarkNeedsLogin()
	provider := &fakeCredentialProvider{usable: true}
	reconnector := &fakeReconnector{}

	adapter := &streamAuthAdapter{
		streamClient:  reconnector,
		tokenProvider: provider,
		authState:     authState,
		logger:        zerolog.Nop(),
		reRegister:    func(context.Context) (string, error) { return "tok", nil },
	}

	verdict, err := adapter.NotifyLogin(context.Background(), nil)
	if err != nil {
		t.Fatalf("NotifyLogin returned error for usable credentials: %v", err)
	}
	if verdict.Outcome != server.LoginOutcomeOK {
		t.Fatalf("verdict outcome = %v, want LoginOutcomeOK", verdict.Outcome)
	}
	if authState.NeedsLogin() {
		t.Fatal("auth gate still closed after a usable-credential login")
	}
	if got := atomic.LoadInt32(&reconnector.calls); got != 1 {
		t.Fatalf("Reconnect calls = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&provider.reloadCalls); got != 1 {
		t.Fatalf("ReloadResult calls = %d, want 1 (the reload must precede the verdict)", got)
	}
}

// TestNotifyLogin_NilProviderKeepsLegacyBehaviour pins the local-only daemon:
// with no token provider there is nothing to gate on, so the pre-BOS-945
// behaviour stands rather than the gate defaulting closed and parking a daemon
// that was working fine.
func TestNotifyLogin_NilProviderKeepsLegacyBehaviour(t *testing.T) {
	t.Parallel()

	authState := upstream.NewAuthState()
	authState.MarkNeedsLogin()

	adapter := &streamAuthAdapter{
		streamClient: &fakeReconnector{},
		authState:    authState,
		logger:       zerolog.Nop(),
	}

	verdict, err := adapter.NotifyLogin(context.Background(), nil)
	if err != nil {
		t.Fatalf("NotifyLogin returned error with nil tokenProvider: %v", err)
	}
	if verdict.Outcome != server.LoginOutcomeOK {
		t.Fatalf("verdict outcome = %v, want LoginOutcomeOK", verdict.Outcome)
	}
	if authState.NeedsLogin() {
		t.Fatal("auth gate left closed with nil tokenProvider; local-only mode regressed")
	}
}

// TestNotifyLogin_ConcurrentLoginsAreRaceFree runs the gate from several
// goroutines at once. Two logins landing together must each produce a coherent
// verdict; -race is what makes this test worth its runtime.
func TestNotifyLogin_ConcurrentLoginsAreRaceFree(t *testing.T) {
	t.Parallel()

	authState := upstream.NewAuthState()
	// A fresh AuthState is already OK, and MarkOK early-returns on it without
	// touching the channels. Flagging first is what puts the edge-triggered
	// path — the mutex-held reallocation of waitCh and needsLoginCh — under the
	// goroutines below; without it this test races nothing.
	authState.MarkNeedsLogin()

	adapter := &streamAuthAdapter{
		streamClient:  &fakeReconnector{},
		tokenProvider: &fakeCredentialProvider{usable: true},
		authState:     authState,
		logger:        zerolog.Nop(),
		reRegister:    func(context.Context) (string, error) { return "tok", nil },
	}

	var wg sync.WaitGroup
	// A concurrent re-flag keeps the transition live for more than the first
	// goroutine, so MarkOK's reallocation runs repeatedly against readers
	// rather than exactly once at the start.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 8; i++ {
			authState.MarkNeedsLogin()
			_ = authState.NeedsLogin()
		}
	}()
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			verdict, err := adapter.NotifyLogin(context.Background(), nil)
			if err != nil {
				t.Errorf("concurrent NotifyLogin returned error: %v", err)
			}
			if verdict.Outcome != server.LoginOutcomeOK {
				t.Errorf("concurrent verdict outcome = %v, want LoginOutcomeOK", verdict.Outcome)
			}
		}()
	}
	wg.Wait()

	// Whatever order they interleaved in, a usable-credential login is the last
	// word: the gate must be open once the dust settles.
	authState.MarkNeedsLogin()
	if _, err := adapter.NotifyLogin(context.Background(), nil); err != nil {
		t.Fatalf("settling NotifyLogin returned error: %v", err)
	}
	if authState.NeedsLogin() {
		t.Fatal("auth gate left closed after concurrent usable-credential logins")
	}
}

// TestNotifyLogin_UnreadableReloadClearsTheGateUnjudged covers the case the
// gate must NOT judge. A ReloadErrorReadFailed leaves the pre-login cache in
// place by design, so CredentialVerdict here is answering about the
// credentials the user just replaced. Refusing MarkOK on that evidence would
// park both Run loops on a transient keyring hiccup, and the reactive Reload()
// hatch cannot rescue them: it lives inside openStream, downstream of the
// NeedsLogin gate a parked loop never gets past.
func TestNotifyLogin_UnreadableReloadClearsTheGateUnjudged(t *testing.T) {
	t.Parallel()

	authState := upstream.NewAuthState()
	authState.MarkNeedsLogin()
	reconnector := &fakeReconnector{}
	var reRegisterCalls int32

	adapter := &streamAuthAdapter{
		streamClient: reconnector,
		// The stale cache looks as bad as it possibly can: flagged AND with no
		// token. It still must not be believed, because it was never re-read.
		tokenProvider: &fakeCredentialProvider{
			usable:        false,
			reloginReason: "refresh_token_rejected",
			reload:        &upstream.ReloadOutcome{ErrorClass: upstream.ReloadErrorReadFailed},
		},
		authState: authState,
		logger:    zerolog.Nop(),
		reRegister: func(context.Context) (string, error) {
			atomic.AddInt32(&reRegisterCalls, 1)
			return "tok", nil
		},
	}

	verdict, err := adapter.NotifyLogin(context.Background(), nil)
	if err != nil {
		t.Fatalf("NotifyLogin failed a login it could not judge: %v", err)
	}
	if verdict.Outcome != server.LoginOutcomeUnspecified {
		t.Fatalf("verdict outcome = %v, want LoginOutcomeUnspecified for an unread reload", verdict.Outcome)
	}
	if verdict.ReloginReason != "" {
		t.Fatalf("verdict relogin reason = %q; a stale marker must not be reported as this login's", verdict.ReloginReason)
	}
	if authState.NeedsLogin() {
		t.Fatal("auth gate left closed on an unreadable reload; the daemon is stranded")
	}
	if got := atomic.LoadInt32(&reRegisterCalls); got != 1 {
		t.Fatalf("reRegister calls = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&reconnector.calls); got != 1 {
		t.Fatalf("Reconnect calls = %d, want 1", got)
	}
}

// TestNotifyLogin_UnreadableReloadWithAFailingRegisterStillClearsTheGate is the
// compound case: the reload could not read the record, so the register runs
// against the preserved pre-login cache and fails for want of a usable token.
// The register failure is real and is reported, but the gate must still open —
// withholding it here would strand the daemon on exactly the evidence this
// change decided not to trust.
func TestNotifyLogin_UnreadableReloadWithAFailingRegisterStillClearsTheGate(t *testing.T) {
	t.Parallel()

	authState := upstream.NewAuthState()
	authState.MarkNeedsLogin()
	reconnector := &fakeReconnector{}

	adapter := &streamAuthAdapter{
		streamClient: reconnector,
		tokenProvider: &fakeCredentialProvider{
			usable:        false,
			reloginReason: "refresh_token_rejected",
			reload:        &upstream.ReloadOutcome{ErrorClass: upstream.ReloadErrorReadFailed},
		},
		authState: authState,
		logger:    zerolog.Nop(),
		reRegister: func(context.Context) (string, error) {
			return "", errors.New("missing or invalid Authorization header")
		},
	}

	verdict, err := adapter.NotifyLogin(context.Background(), nil)
	if err == nil {
		t.Fatal("NotifyLogin hid a real re-register failure")
	}
	if verdict.Outcome != server.LoginOutcomeRegisterFailed {
		t.Fatalf("verdict outcome = %v, want LoginOutcomeRegisterFailed", verdict.Outcome)
	}
	if authState.NeedsLogin() {
		t.Fatal("auth gate left closed; a register failure is not a credential verdict")
	}
	// A failed register never reconnects — there is no fresh session token to
	// re-open with.
	if got := atomic.LoadInt32(&reconnector.calls); got != 0 {
		t.Fatalf("Reconnect calls = %d, want 0 after a failed re-register", got)
	}
}

// TestNotifyLogin_DeletedRecordIsStillJudged is the other side of that line.
// ReloadErrorRecordDeleted is an authoritative observation — the record is
// gone, and the reload cleared the cache to say so — so the gate applies
// normally and the operator gets a real verdict.
func TestNotifyLogin_DeletedRecordIsStillJudged(t *testing.T) {
	t.Parallel()

	authState := upstream.NewAuthState()
	authState.MarkNeedsLogin()

	adapter := &streamAuthAdapter{
		streamClient: &fakeReconnector{},
		tokenProvider: &fakeCredentialProvider{
			usable: false,
			reload: &upstream.ReloadOutcome{ErrorClass: upstream.ReloadErrorRecordDeleted},
		},
		authState:  authState,
		logger:     zerolog.Nop(),
		reRegister: func(context.Context) (string, error) { return "tok", nil },
	}

	verdict, err := adapter.NotifyLogin(context.Background(), nil)
	if err == nil {
		t.Fatal("NotifyLogin returned nil error for a deleted credential record")
	}
	if verdict.Outcome != server.LoginOutcomeCredentialsMissing {
		t.Fatalf("verdict outcome = %v, want LoginOutcomeCredentialsMissing", verdict.Outcome)
	}
	if !authState.NeedsLogin() {
		t.Fatal("auth gate cleared for a deleted credential record")
	}
}
