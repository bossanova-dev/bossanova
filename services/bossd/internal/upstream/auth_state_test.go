package upstream

import (
	"context"
	"testing"
	"time"
)

func TestAuthStateNeedsLoginSignalClosesOnFirstMarkNeedsLogin(t *testing.T) {
	t.Parallel()
	state := NewAuthState()
	signal := state.NeedsLoginSignal()

	if !state.MarkNeedsLogin() {
		t.Fatal("first MarkNeedsLogin returned false")
	}

	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for needs-login signal")
	}

	if state.MarkNeedsLogin() {
		t.Fatal("second MarkNeedsLogin returned true")
	}

	select {
	case <-state.NeedsLoginSignal():
	case <-time.After(2 * time.Second):
		t.Fatal("needs-login signal was not already closed")
	}
}

func TestAuthStateMarkOKRearmsNeedsLoginSignal(t *testing.T) {
	t.Parallel()
	state := NewAuthState()
	first := state.NeedsLoginSignal()

	if !state.MarkNeedsLogin() {
		t.Fatal("MarkNeedsLogin returned false")
	}
	if !state.MarkOK() {
		t.Fatal("MarkOK returned false")
	}

	second := state.NeedsLoginSignal()
	if first == second {
		t.Fatal("MarkOK did not rearm needs-login signal")
	}

	select {
	case <-second:
		t.Fatal("rearmed needs-login signal was already closed")
	default:
	}

	if !state.MarkNeedsLogin() {
		t.Fatal("second MarkNeedsLogin returned false")
	}

	select {
	case <-second:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for rearmed needs-login signal")
	}
}

func TestAuthStateMarkTransitionsReturnRealChangesOnly(t *testing.T) {
	t.Parallel()
	state := NewAuthState()

	if state.NeedsLogin() {
		t.Fatal("fresh AuthState reports NeedsLogin true, want false")
	}
	if state.MarkOK() {
		t.Fatal("MarkOK on already-OK state returned true, want false")
	}

	if !state.MarkNeedsLogin() {
		t.Fatal("first MarkNeedsLogin returned false, want true")
	}
	if !state.NeedsLogin() {
		t.Fatal("after MarkNeedsLogin, NeedsLogin returned false, want true")
	}
	if state.MarkNeedsLogin() {
		t.Fatal("idempotent MarkNeedsLogin returned true, want false")
	}

	if !state.MarkOK() {
		t.Fatal("MarkOK after needs-login returned false, want true")
	}
	if state.NeedsLogin() {
		t.Fatal("after MarkOK, NeedsLogin returned true, want false")
	}
	if state.MarkOK() {
		t.Fatal("second MarkOK returned true, want false")
	}
}

func TestAuthStateWaitUnblocksOnMarkOKAndRearms(t *testing.T) {
	t.Parallel()
	state := NewAuthState()

	// Steady state: Wait must not be closed when auth is OK, so a Run loop
	// that blocks on it stays parked until the next MarkOK.
	select {
	case <-state.Wait():
		t.Fatal("Wait closed while auth was OK")
	default:
	}

	state.MarkNeedsLogin()
	waiting := state.Wait()
	select {
	case <-waiting:
		t.Fatal("Wait closed before MarkOK")
	default:
	}

	if !state.MarkOK() {
		t.Fatal("MarkOK returned false, want true")
	}
	select {
	case <-waiting:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not unblock after MarkOK")
	}

	// MarkOK rearms a fresh wait channel for the next round.
	rearmed := state.Wait()
	if rearmed == waiting {
		t.Fatal("MarkOK did not rearm the wait channel")
	}
	select {
	case <-rearmed:
		t.Fatal("rearmed wait channel was already closed")
	default:
	}
}

func TestAuthStateNilReceiverNeverBlocks(t *testing.T) {
	t.Parallel()
	var state *AuthState

	if state.MarkNeedsLogin() {
		t.Fatal("nil MarkNeedsLogin returned true, want false")
	}
	if state.MarkOK() {
		t.Fatal("nil MarkOK returned true, want false")
	}
	if state.NeedsLogin() {
		t.Fatal("nil NeedsLogin returned true, want false")
	}
	if state.NeedsLoginSignal() != nil {
		t.Fatal("nil NeedsLoginSignal returned non-nil channel")
	}

	// Wait on a nil state must return an already-closed channel so a Run
	// loop never deadlocks when auth tracking is disabled.
	select {
	case <-state.Wait():
	case <-time.After(2 * time.Second):
		t.Fatal("nil Wait did not return a closed channel")
	}
}

func TestCancelOnNeedsLoginCancelsContext(t *testing.T) {
	t.Parallel()
	state := NewAuthState()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := cancelOnNeedsLogin(ctx, state, cancel)

	select {
	case <-ctx.Done():
		t.Fatal("context canceled before needs-login signal")
	default:
	}

	if !state.MarkNeedsLogin() {
		t.Fatal("MarkNeedsLogin returned false")
	}

	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for context cancellation")
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for cancelOnNeedsLogin done")
	}
}

func TestCancelOnNeedsLoginNilStateIsNoop(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := cancelOnNeedsLogin(ctx, nil, cancel)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for nil-state done")
	}

	select {
	case <-ctx.Done():
		t.Fatal("nil state canceled context")
	default:
	}
}
