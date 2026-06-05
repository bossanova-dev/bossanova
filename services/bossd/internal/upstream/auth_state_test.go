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
