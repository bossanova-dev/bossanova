package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/recurser/bossd/internal/rotation"
	"github.com/recurser/bossd/internal/session"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

// TestAuthProbeConfirmsInvalidation pins the auth-path classification used by the
// AuthProbe seam wiring (BOS-316): a typed 401 arrives from the claude plugin as
// a gRPC codes.Unauthenticated status wrapped once by fmt.Errorf(... %w). It must
// be recognised through the %w chain (never via the error string), while a nil
// error (healthy/limited snapshot) and any other gRPC code are NOT auth
// invalidations.
func TestAuthProbeConfirmsInvalidation(t *testing.T) {
	authErr := grpcstatus.Error(codes.Unauthenticated, "ROTATION_KIND_AUTH_INVALIDATED")
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil error (healthy snapshot) is not an auth invalidation", err: nil, want: false},
		{name: "bare Unauthenticated status confirms", err: authErr, want: true},
		{
			name: "wrapped Unauthenticated status confirms through %w chain",
			err:  fmt.Errorf("plugin ProbeRateLimit: %w", authErr),
			want: true,
		},
		{
			name: "double-wrapped Unauthenticated status confirms",
			err:  fmt.Errorf("outer: %w", fmt.Errorf("plugin ProbeRateLimit: %w", authErr)),
			want: true,
		},
		{
			name: "non-auth gRPC code does not confirm",
			err:  fmt.Errorf("probe: %w", grpcstatus.Error(codes.Unavailable, "plugin down")),
			want: false,
		},
		{
			name: "plain error (no gRPC status) does not confirm",
			err:  errors.New("unauthenticated"), // string mentions auth but carries no status
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := authProbeConfirmsInvalidation(tt.err); got != tt.want {
				t.Fatalf("authProbeConfirmsInvalidation(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestMapRotationSwitchError pins the adapter that translates a session.SwitchAccount
// error into the sentinels the chat rotator classifies on (BOS-981). This is the joint
// that makes the cap-burn fix work end to end: without the ErrSwitchNotAttempted tag
// the rotator cannot tell "refused, nothing consumed" from "attempted and failed", and
// without ErrSwitchAccountIneligible it cannot tell that the remedy is a rotation.
func TestMapRotationSwitchError(t *testing.T) {
	// The exact production shape: SwitchAccount refuses a same-account respawn because
	// the bound account is disabled, before it touches the pane.
	disabledRefusal := fmt.Errorf("%w: %w: account %q is disabled",
		session.ErrSwitchNotAttempted, session.ErrAccountDisabled, "agent.yuki@kamik.ai")

	tests := []struct {
		name           string
		err            error
		wantAborted    bool
		wantNotAttempt bool
		wantIneligible bool
	}{
		{name: "nil stays nil"},
		{
			name:        "mid-turn refusal maps to the fail-safe abort sentinel",
			err:         fmt.Errorf("%w: %w; confirm/--force to interrupt", session.ErrSwitchNotAttempted, session.ErrChatMidTurn),
			wantAborted: true,
		},
		{
			name:           "disabled bound account is not-attempted AND ineligible",
			err:            disabledRefusal,
			wantNotAttempt: true,
			wantIneligible: true,
		},
		{
			name:           "health-failed target is not-attempted AND ineligible",
			err:            fmt.Errorf("%w: %w: account %q", session.ErrSwitchNotAttempted, session.ErrAccountFailed, "sick"),
			wantNotAttempt: true,
			wantIneligible: true,
		},
		{
			name:           "cooling target is not-attempted AND ineligible",
			err:            fmt.Errorf("%w: %w: account %q is cooling", session.ErrSwitchNotAttempted, session.ErrAccountCooling, "cool"),
			wantNotAttempt: true,
			wantIneligible: true,
		},
		{
			name:           "other pre-pane-touch refusal is not-attempted only",
			err:            fmt.Errorf("%w: get agent chat agent-1: boom", session.ErrSwitchNotAttempted),
			wantNotAttempt: true,
		},
		{
			name: "an error raised after the pane was touched passes through untagged",
			err:  errors.New("respawn chat: boom"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mapRotationSwitchError(tc.err)
			if tc.err == nil {
				if got != nil {
					t.Fatalf("mapRotationSwitchError(nil) = %v, want nil", got)
				}
				return
			}
			if aborted := errors.Is(got, rotation.ErrSwitchAborted); aborted != tc.wantAborted {
				t.Errorf("ErrSwitchAborted = %v, want %v (got %v)", aborted, tc.wantAborted, got)
			}
			if na := errors.Is(got, rotation.ErrSwitchNotAttempted); na != tc.wantNotAttempt {
				t.Errorf("ErrSwitchNotAttempted = %v, want %v (got %v)", na, tc.wantNotAttempt, got)
			}
			if ie := errors.Is(got, rotation.ErrSwitchAccountIneligible); ie != tc.wantIneligible {
				t.Errorf("ErrSwitchAccountIneligible = %v, want %v (got %v)", ie, tc.wantIneligible, got)
			}
			if tc.wantNotAttempt {
				// The original cause must survive so the audit detail can name it.
				if !errors.Is(got, tc.err) {
					t.Errorf("mapped error dropped the original cause: %v", got)
				}
			}
		})
	}
}
