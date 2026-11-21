package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// usageWithGeminiRemaining rewrites the captured report's first group so its
// 5-hour bucket has remaining fraction r.
func usageWithGeminiRemaining(t *testing.T, r float64) string {
	t.Helper()
	var report map[string]any
	if err := json.Unmarshal(readTestdata(t, "usage.json"), &report); err != nil {
		t.Fatal(err)
	}
	groups := report["command"].(map[string]any)["data"].(map[string]any)["groups"].([]any)
	for _, b := range groups[0].(map[string]any)["buckets"].([]any) {
		bucket := b.(map[string]any)
		if bucket["window"] == "5h" {
			bucket["remaining_fraction"] = r
		}
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestProbeRateLimit(t *testing.T) {
	tests := []struct {
		name      string
		remaining float64
		status    bossanovav1.RateLimitPlanStatus
		limited   bool
	}{
		{"quota left", 0.435, bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_ACTIVE, false},
		{"nearly spent", 0.1, bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_WARNING, false},
		{"spent", 0, bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_RATE_LIMITED, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "usage.json"), usageWithGeminiRemaining(t, tt.remaining))
			script := `printf '%s\n' "$@" > "$BOSS_TEST_OUT/argv"; printf '%s' "$AGY_PROBE_ACCOUNT" > "$BOSS_TEST_OUT/env"; cat "$BOSS_TEST_OUT/usage.json"`
			t.Setenv("BOSS_TEST_OUT", dir)
			s, _ := newTestServer(t, WithCommandFactory(fakeAgy(script)))

			resp, err := s.ProbeRateLimit(context.Background(), &bossanovav1.ProbeRateLimitRequest{CredentialEnv: map[string]string{"AGY_PROBE_ACCOUNT": "second"}})
			if err != nil {
				t.Fatal(err)
			}
			st := resp.GetStatus()
			if st.GetStatus() != tt.status || st.GetLimited() != tt.limited {
				t.Fatalf("status=%v limited=%v; want %v %v", st.GetStatus(), st.GetLimited(), tt.status, tt.limited)
			}
			if st.GetReset_5H() == nil || st.GetReset_7D() == nil || len(st.GetModelWindows()) != 3 {
				t.Fatalf("status %+v lacks reset times or per-group windows", st)
			}
			argv, _ := os.ReadFile(filepath.Join(dir, "argv"))
			if !strings.Contains(string(argv), "/usage") {
				t.Errorf("probe argv %q is not the local /usage command", argv)
			}
			if env, _ := os.ReadFile(filepath.Join(dir, "env")); string(env) != "second" {
				t.Errorf("credential overlay did not reach agy: %q", env)
			}
		})
	}
}

func TestProbeRateLimitFailsSafe(t *testing.T) {
	s, _ := newTestServer(t, WithCommandFactory(fakeAgy(`echo 'Error: authentication required.' >&2; exit 1`)))
	resp, err := s.ProbeRateLimit(context.Background(), &bossanovav1.ProbeRateLimitRequest{CredentialEnv: map[string]string{"HOME": t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetStatus().GetLimited() || resp.GetStatus().GetStatus() != bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_UNSUPPORTED {
		t.Fatalf("failed probe = %+v; want UNSUPPORTED and not limited", resp.GetStatus())
	}
}
