package main

import (
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/auth"
)

// TestRenderAuthStatusRetainedReloginRequired covers the BOS-659 state that
// `boss auth-status` previously could not express: credentials are still
// stored (so the identity is known) but cannot be used, because the last
// WorkOS refresh outcome could not be confirmed. It must read as "sign in
// again", not as "you are logged in" and not as "you never logged in".
func TestRenderAuthStatusRetainedReloginRequired(t *testing.T) {
	cases := []struct {
		name       string
		reason     string
		wantReason string
	}{
		{
			name:       "outcome unknown",
			reason:     auth.ReloginReasonRefreshOutcomeUnknown,
			wantReason: "refresh outcome could not be confirmed",
		},
		{
			name:       "token rejected",
			reason:     auth.ReloginReasonRefreshTokenRejected,
			wantReason: "refresh token was rejected",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := renderAuthStatus(&auth.Status{
				LoggedIn:      false,
				Email:         "dev@example.com",
				ExpiresAt:     time.Now().Add(-time.Hour),
				NeedsRelogin:  true,
				ReloginReason: tc.reason,
			})

			if !strings.Contains(out, "Sign in required") {
				t.Errorf("output does not announce the re-login state:\n%s", out)
			}
			if !strings.Contains(out, tc.wantReason) {
				t.Errorf("output does not explain %q:\n%s", tc.wantReason, out)
			}
			if !strings.Contains(out, "dev@example.com") {
				t.Errorf("output drops the retained identity:\n%s", out)
			}
			if !strings.Contains(out, "boss login") {
				t.Errorf("output does not direct the user to 'boss login':\n%s", out)
			}
			// Distinct from never-logged-in, and never claims a live session.
			if strings.Contains(out, "Not logged in.") {
				t.Errorf("retained credentials rendered as never logged in:\n%s", out)
			}
			if strings.Contains(out, "Logged in.") {
				t.Errorf("unusable credentials rendered as logged in:\n%s", out)
			}
			// Enumerated reasons only — no raw upstream payload, no token.
			if strings.Contains(out, tc.reason) {
				t.Errorf("output leaks the raw enumerated reason token %q instead of prose:\n%s", tc.reason, out)
			}
			for _, leak := range []string{"invalid_grant", "access_token", "refresh_token", "Bearer"} {
				if strings.Contains(out, leak) {
					t.Errorf("output leaks %q:\n%s", leak, out)
				}
			}
		})
	}
}

// TestRenderAuthStatusNeverLoggedIn pins the pre-existing signed-out output so
// the two states stay distinguishable.
func TestRenderAuthStatusNeverLoggedIn(t *testing.T) {
	out := renderAuthStatus(&auth.Status{LoggedIn: false})

	if !strings.Contains(out, "Not logged in.") {
		t.Errorf("signed-out output changed:\n%s", out)
	}
	if strings.Contains(out, "Sign in required") {
		t.Errorf("never-logged-in state borrowed the retained-credential wording:\n%s", out)
	}
	if !strings.Contains(out, "boss login") {
		t.Errorf("signed-out output lost its remediation:\n%s", out)
	}
}

// TestRenderAuthStatusLoggedIn pins the healthy output.
func TestRenderAuthStatusLoggedIn(t *testing.T) {
	out := renderAuthStatus(&auth.Status{
		LoggedIn:  true,
		Email:     "dev@example.com",
		ExpiresAt: time.Now().Add(time.Hour),
	})

	if !strings.Contains(out, "Logged in.") {
		t.Errorf("logged-in output changed:\n%s", out)
	}
	if !strings.Contains(out, "dev@example.com") {
		t.Errorf("logged-in output dropped the email:\n%s", out)
	}
	if strings.Contains(out, "Sign in required") {
		t.Errorf("healthy credentials rendered as needing re-login:\n%s", out)
	}
}
