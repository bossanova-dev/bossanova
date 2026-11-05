package plugin

import "testing"

func TestChatRunRetryable(t *testing.T) {
	for _, tc := range []struct {
		name, exitErr string
		want          bool
	}{
		{"incomplete", "interactive chat did not report completion before wait deadline: context canceled", true},
		{"idle", "agent went idle without completing (no output for 1m0s; watchdog)", true},
		{"multiline", "runner output\nagent went idle without completing\nretry", true},
		{"empty", "", false},
		{"unrelated", "exit status 1", false},
		{"non-explicit", "agent run did not signal completion within deadline 1m0s elapsed", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ChatRunRetryable(tc.exitErr); got != tc.want {
				t.Fatalf("ChatRunRetryable(%q) = %v, want %v", tc.exitErr, got, tc.want)
			}
		})
	}
}
