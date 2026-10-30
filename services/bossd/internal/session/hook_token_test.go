package session

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestNewHookToken(t *testing.T) {
	t.Parallel()
	a, err := NewHookToken()
	if err != nil {
		t.Fatalf("NewHookToken: %v", err)
	}
	if len(a) != 64 {
		t.Fatalf("token length = %d, want 64", len(a))
	}
	if _, err := hex.DecodeString(a); err != nil {
		t.Fatalf("token is not valid hex: %v", err)
	}
	if a != strings.ToLower(a) {
		t.Fatal("token is not lowercase")
	}
	b, err := NewHookToken()
	if err != nil {
		t.Fatalf("NewHookToken: %v", err)
	}
	if a == b {
		t.Fatal("two minted tokens are identical")
	}
}

func TestIsDependabotBranch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		branch string
		want   bool
	}{
		{"dependabot/npm/react-18.1.0", true},
		{"  DePenDaBot/go/x-1.2.3\n", true},
		{"dependabot/", true},
		{"", false},
		{"dependabot", false},
		{"feature/dependabot/npm/x", false},
		{"dependabot-fix/x", false},
	} {
		t.Run(tc.branch, func(t *testing.T) {
			if got := IsDependabotBranch(tc.branch); got != tc.want {
				t.Fatalf("IsDependabotBranch(%q) = %v, want %v", tc.branch, got, tc.want)
			}
		})
	}
}
