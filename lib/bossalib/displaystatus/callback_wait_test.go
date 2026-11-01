package displaystatus

import "testing"

func TestOwnPRWaitingTarget(t *testing.T) {
	for _, tc := range []struct {
		reason, nwo string
		n           int
		want        bool
	}{
		{"awaiting checks_passed_ready on Acme/App#12", "acme/app", 12, true},
		{"awaiting checks_passed_ready on Acme/App#12", "", 12, true},
		{"awaiting merged on acme/app#12", "other/app", 12, false},
		{"awaiting merged on acme/app#12", "acme/app", 13, false},
		{"awaiting merged on acme/app#012", "acme/app", 12, false},
		{"awaiting merged on acme/app#12", "acme/app", 0, false},
	} {
		if got := IsOwnPRWaitingReason(tc.reason, tc.nwo, tc.n); got != tc.want {
			t.Errorf("%+v got %v", tc, got)
		}
	}
}
