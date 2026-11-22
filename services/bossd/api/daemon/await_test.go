package server

import (
	"testing"
	"time"
)

// testDeadline fires just before go test's own deadline (its -timeout, which
// the gate sizes for load), so a wait that never ends fails naming what it
// waited for instead of in the runtime's goroutine dump. With no deadline set
// it never fires.
func testDeadline(t *testing.T) <-chan time.Time {
	t.Helper()
	at, ok := t.Deadline()
	if !ok {
		return nil
	}
	return time.After(time.Until(at.Add(-time.Second)))
}

// awaitOrFail waits for ch to deliver or close, failing the test with what if
// its deadline arrives first.
func awaitOrFail[T any](t *testing.T, ch <-chan T, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-testDeadline(t):
		t.Fatalf("%s did not happen before the test deadline", what)
	}
}
