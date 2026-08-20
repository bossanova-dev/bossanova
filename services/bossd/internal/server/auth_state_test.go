package server

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// stubAuthStateReporter hands GetAuthState a fixed answer so the test pins the
// mapping from daemon state to wire fields, not the daemon that produces it.
type stubAuthStateReporter struct {
	state  DaemonAuthState
	called int
}

func (s *stubAuthStateReporter) AuthState(context.Context) DaemonAuthState {
	s.called++
	return s.state
}

func TestGetAuthStateReportsReporterValuesVerbatim(t *testing.T) {
	failingSince := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	registeredAt := time.Date(2026, 8, 19, 9, 15, 0, 0, time.UTC)
	reporter := &stubAuthStateReporter{state: DaemonAuthState{
		NeedsLogin:       true,
		ReloginReason:    "refresh_outcome_unknown",
		LastRegisteredAt: registeredAt,
		Connected:        false,
		AuthFailingSince: failingSince,
	}}
	s := New(Config{AuthStateReporter: reporter})

	resp, err := s.GetAuthState(context.Background(), connect.NewRequest(&bossanovav1.GetAuthStateRequest{}))
	if err != nil {
		t.Fatalf("GetAuthState() error = %v", err)
	}
	if reporter.called != 1 {
		t.Fatalf("reporter called %d times, want 1", reporter.called)
	}
	msg := resp.Msg
	if !msg.GetUpstreamConfigured() {
		t.Error("upstream_configured = false, want true when a reporter is wired")
	}
	if !msg.GetNeedsLogin() {
		t.Error("needs_login = false, want true")
	}
	if got := msg.GetReloginReason(); got != "refresh_outcome_unknown" {
		t.Errorf("relogin_reason = %q, want %q", got, "refresh_outcome_unknown")
	}
	if msg.GetUpstreamConnected() {
		t.Error("upstream_connected = true, want false")
	}
	if got := msg.GetLastRegisteredAt().AsTime(); !got.Equal(registeredAt) {
		t.Errorf("last_registered_at = %v, want %v", got, registeredAt)
	}
	if got := msg.GetAuthFailingSince().AsTime(); !got.Equal(failingSince) {
		t.Errorf("auth_failing_since = %v, want %v", got, failingSince)
	}
}

// A healthy daemon must leave both timestamps unset rather than encoding the
// zero instant: the CLI renders "never" for unset and a 1970 date for a zero
// timestamp, and those are different sentences.
func TestGetAuthStateLeavesZeroTimestampsUnset(t *testing.T) {
	reporter := &stubAuthStateReporter{state: DaemonAuthState{Connected: true}}
	s := New(Config{AuthStateReporter: reporter})

	resp, err := s.GetAuthState(context.Background(), connect.NewRequest(&bossanovav1.GetAuthStateRequest{}))
	if err != nil {
		t.Fatalf("GetAuthState() error = %v", err)
	}
	if resp.Msg.GetLastRegisteredAt() != nil {
		t.Errorf("last_registered_at = %v, want unset", resp.Msg.GetLastRegisteredAt())
	}
	if resp.Msg.GetAuthFailingSince() != nil {
		t.Errorf("auth_failing_since = %v, want unset", resp.Msg.GetAuthFailingSince())
	}
	if !resp.Msg.GetUpstreamConnected() {
		t.Error("upstream_connected = false, want true")
	}
}

// A local-only daemon has no upstream at all. That is a real answer, not a
// failure to produce one: nil error, upstream_configured=false, nothing else
// set. A caller that saw an error here would report a broken daemon.
func TestGetAuthStateNilReporterReportsUnconfigured(t *testing.T) {
	s := New(Config{})

	resp, err := s.GetAuthState(context.Background(), connect.NewRequest(&bossanovav1.GetAuthStateRequest{}))
	if err != nil {
		t.Fatalf("GetAuthState() error = %v, want nil", err)
	}
	msg := resp.Msg
	if msg.GetUpstreamConfigured() {
		t.Error("upstream_configured = true, want false for a local-only daemon")
	}
	if msg.GetNeedsLogin() {
		t.Error("needs_login = true, want false")
	}
	if msg.GetReloginReason() != "" {
		t.Errorf("relogin_reason = %q, want empty", msg.GetReloginReason())
	}
	if msg.GetUpstreamConnected() {
		t.Error("upstream_connected = true, want false")
	}
	if msg.GetLastRegisteredAt() != nil {
		t.Errorf("last_registered_at = %v, want unset", msg.GetLastRegisteredAt())
	}
	if msg.GetAuthFailingSince() != nil {
		t.Errorf("auth_failing_since = %v, want unset", msg.GetAuthFailingSince())
	}
}
