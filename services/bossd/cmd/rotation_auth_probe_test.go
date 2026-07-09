package main

import (
	"errors"
	"fmt"
	"testing"

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
