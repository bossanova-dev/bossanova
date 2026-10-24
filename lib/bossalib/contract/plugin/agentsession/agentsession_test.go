package agentsession

import "testing"

func TestIsCanonicalUUID(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{"0199a3c1-7b2e-7d40-9c55-1a2b3c4d5e6f", true},
		{"00000000-0000-0000-0000-000000000000", true},
		{"ffffffff-ffff-ffff-ffff-ffffffffffff", true},
		{"0199A3C1-7B2E-7D40-9C55-1A2B3C4D5E6F", false},
		{"0199a3c1_7b2e-7d40-9c55-1a2b3c4d5e6f", false},
		{"0199a3c1-7b2e-7d40-9c55-1a2b3c4d5e6g", false},
		{"0199a3c1-7b2e-7d40-9c55-1a2b3c4d5e6", false},
		{"0199a3c1-7b2e-7d40-9c55-1a2b3c4d5e6ff", false},
		{"{0199a3c1-7b2e-7d40-9c55-1a2b3c4d5e6f}", false},
		{"0199a3c1-7b2e-7d40-9c55-1a2b3c4d5e6f\n", false},
		{"*", false}, {"?", false}, {"[0-9a-f]*", false}, {"../escape", false}, {"a/b", false}, {`a\b`, false}, {"", false},
	} {
		t.Run(tc.id, func(t *testing.T) {
			if got := IsCanonicalUUID(tc.id); got != tc.want {
				t.Errorf("IsCanonicalUUID(%q) = %v, want %v", tc.id, got, tc.want)
			}
		})
	}
}
