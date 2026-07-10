package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossd/internal/upstream"
)

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

	if err := adapter.NotifyLogin(context.Background(), nil); err != nil {
		t.Fatalf("NotifyLogin returned error: %v", err)
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

	if err := adapter.NotifyLogin(context.Background(), nil); err != nil {
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

	if err := adapter.NotifyLogin(context.Background(), nil); err != nil {
		t.Fatalf("NotifyLogin returned error on best-effort re-register: %v", err)
	}
	if got := atomic.LoadInt32(&reconnector.calls); got != 0 {
		t.Fatalf("Reconnect calls = %d, want 0 after failed re-register", got)
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

	if err := adapter.NotifyLogin(context.Background(), nil); err != nil {
		t.Fatalf("NotifyLogin returned error with nil reRegister: %v", err)
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
		if err := adapter.NotifyLogin(context.Background(), nil); err != nil {
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
