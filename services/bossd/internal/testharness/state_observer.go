package testharness

import (
	"context"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// WaitForSessionState polls until sessionID reaches want or timeout elapses.
func (h *Harness) WaitForSessionState(t *testing.T, sessionID string, want pb.SessionState, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		sess, err := h.Sessions.Get(context.Background(), sessionID)
		if err != nil {
			t.Fatalf("WaitForSessionState: get session: %v", err)
		}
		got := pb.SessionState(sess.State)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("WaitForSessionState: session %s state = %v, want %v", sessionID, got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
