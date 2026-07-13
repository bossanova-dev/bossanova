package agenterr

import (
	"testing"
	"time"
)

func TestKindString(t *testing.T) {
	cases := []struct {
		kind Kind
		want string
	}{
		{KindNone, "none"},
		{KindUsageExhausted, "usage_exhausted"},
		{KindAuthInvalidated, "auth_invalidated"},
		{KindTransientProvider, "transient_provider"},
		{Kind(99), "unknown"},
	}
	for _, tc := range cases {
		if got := tc.kind.String(); got != tc.want {
			t.Errorf("Kind(%d).String() = %q, want %q", tc.kind, got, tc.want)
		}
	}
}

func TestClassify(t *testing.T) {
	fixedNow := time.Date(2026, 7, 5, 10, 0, 0, 0, time.UTC)

	cases := []struct {
		name        string
		text        string
		wantKind    Kind
		wantResetAt bool // whether we expect ResetAt to be non-nil
	}{
		// AUTH branches
		{"auth invalidated token", "Your authentication token has been invalidated", KindAuthInvalidated, false},
		{"auth invalidated oauth", "The stored value was invalidated because the oauth token expired", KindAuthInvalidated, false},
		{"auth please sign in again", "Please sign in again to continue", KindAuthInvalidated, false},
		{"auth please try signing in again", "Please try signing in again", KindAuthInvalidated, false},

		// SUSPENSION branches (org/billing block) fold into auth so the account
		// is failed permanently, not merely cooled.
		{"suspension oauth_org_not_allowed", `{"error":"oauth_org_not_allowed"}`, KindAuthInvalidated, false},
		{"suspension org disabled subscription", "Your organization has disabled Claude subscription access for Claude Code", KindAuthInvalidated, false},
		{"suspension disabled subscription access", "disabled Claude subscription access", KindAuthInvalidated, false},

		// USAGE branches
		{"usage insufficient_quota", "Error: insufficient_quota", KindUsageExhausted, false},
		{"usage quota_exceeded", "quota_exceeded for this account", KindUsageExhausted, false},
		{"usage usage_limit_reached", "usage_limit_reached: try later", KindUsageExhausted, false},
		{"usage credits exhausted", "Your credits exhausted for today", KindUsageExhausted, false},
		{"usage credit exhausted", "Your credit exhausted for today", KindUsageExhausted, false},
		{"usage out of credits", "You are out of credits", KindUsageExhausted, false},
		{"usage usage limit", "You have hit the usage limit", KindUsageExhausted, false},
		{"usage daily limit", "You have reached your daily limit", KindUsageExhausted, false},
		{"usage weekly limit", "You have reached your weekly limit", KindUsageExhausted, false},
		{"usage monthly limit", "You have reached your monthly limit", KindUsageExhausted, false},
		{"usage n-hour limit", "You have reached your 5-hour limit", KindUsageExhausted, true},
		{"usage hit your limit", "You hit your plan's usage limit for now", KindUsageExhausted, false},
		{"usage billing limit", "billing issue: account has reached its limit", KindUsageExhausted, false},
		{"usage billing quota", "billing quota reached", KindUsageExhausted, false},
		{"usage subscription expired", "subscription has expired", KindUsageExhausted, false},
		{"usage subscription required", "a subscription is required to continue", KindUsageExhausted, false},

		// USAGE with parseable reset
		{"usage with reset time", "usage_limit_reached, resets at 15:00", KindUsageExhausted, true},
		{"usage with ambiguous reset", "usage_limit_reached, weekly limit", KindUsageExhausted, false},

		// TRANSIENT branches
		{"transient 429", "request failed with status 429", KindTransientProvider, false},
		{"transient rate limit", "rate limit exceeded, try again", KindTransientProvider, false},
		{"transient 500", "server responded 500 Internal Server Error", KindTransientProvider, false},
		{"transient 502", "bad gateway 502", KindTransientProvider, false},
		{"transient 503", "service returned 503", KindTransientProvider, false},
		{"transient 529", "provider overloaded 529", KindTransientProvider, false},
		{"transient overloaded", "the model is currently overloaded", KindTransientProvider, false},
		{"transient timed out", "the request timed out", KindTransientProvider, false},
		{"transient timeout", "connection timeout", KindTransientProvider, false},
		{"transient service unavailable", "service unavailable, try later", KindTransientProvider, false},
		{"transient temporarily unavailable", "the API is temporarily unavailable", KindTransientProvider, false},
		{"transient at capacity", "our servers are at capacity", KindTransientProvider, false},

		// Fail-safe: benign text that merely contains "limit"
		{"benign no limit to what we can do", "there is no limit to what we can do", KindNone, false},
		{"benign rate limiting middleware", "rate limiting middleware failed to load", KindNone, false},
		{"benign sky is the limit", "the sky is the limit for this team", KindNone, false},
		{"benign empty string", "", KindNone, false},
		// Benign 403 scope-refusal (setup-token lacking user:profile) must NOT be
		// mistaken for a suspension — it is an expected, healthy path.
		{"benign oauth scope refusal", `{"type":"error","error":{"type":"authentication_error","message":"OAuth token does not have the required scopes: user:profile"}}`, KindNone, false},

		// Precedence: matches both auth and transient -> auth wins
		{"precedence auth over transient", "please sign in again, the service timed out", KindAuthInvalidated, false},
		// Precedence: matches both usage and transient -> usage wins
		{"precedence usage over transient", "usage_limit_reached, the service timed out", KindUsageExhausted, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.text, fixedNow)
			if got.Kind != tc.wantKind {
				t.Fatalf("Classify(%q).Kind = %v, want %v", tc.text, got.Kind, tc.wantKind)
			}
			if tc.wantResetAt && got.ResetAt == nil {
				t.Fatalf("Classify(%q).ResetAt = nil, want non-nil", tc.text)
			}
			if !tc.wantResetAt && got.ResetAt != nil {
				t.Fatalf("Classify(%q).ResetAt = %v, want nil", tc.text, *got.ResetAt)
			}
			if tc.wantResetAt && got.ResetAt != nil && !got.ResetAt.After(fixedNow) {
				t.Fatalf("Classify(%q).ResetAt = %v, want time after now (%v)", tc.text, *got.ResetAt, fixedNow)
			}
		})
	}
}

func TestSuspensionReason(t *testing.T) {
	cases := []struct {
		name   string
		text   string
		wantOK bool
	}{
		{"oauth_org_not_allowed code", `{"error":"oauth_org_not_allowed","request_id":"req_x"}`, true},
		{"org disabled subscription message", "Your organization has disabled Claude subscription access for Claude Code", true},
		{"disabled subscription access fragment", "disabled Claude subscription access", true},
		{"case-insensitive", "OAUTH_ORG_NOT_ALLOWED", true},

		// Negatives: benign scope refusal and unrelated auth/usage text.
		{"benign scope refusal", `{"error":{"type":"authentication_error","message":"OAuth token does not have the required scopes: user:profile"}}`, false},
		{"generic auth invalidated", "authentication token has been invalidated", false},
		{"generic usage limit", "usage_limit_reached", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, ok := SuspensionReason(tc.text)
			if ok != tc.wantOK {
				t.Fatalf("SuspensionReason(%q) ok = %v, want %v", tc.text, ok, tc.wantOK)
			}
			if ok && reason == "" {
				t.Fatalf("SuspensionReason(%q) returned ok with empty reason", tc.text)
			}
			if !ok && reason != "" {
				t.Fatalf("SuspensionReason(%q) returned reason %q with ok=false", tc.text, reason)
			}
		})
	}
}
