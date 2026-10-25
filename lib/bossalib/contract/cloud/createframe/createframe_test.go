package createframe

import "testing"

func TestIsAccepted(t *testing.T) {
	for _, tt := range []struct {
		name             string
		attachedExisting bool
		isQuickChat      bool
		worktreePath     string
		want             bool
	}{
		{"bootstrap accepted", false, false, "", true},
		{"bootstrap settled or older daemon", false, false, "/worktree", false},
		{"quick chat without worktree", false, true, "", false},
		{"quick chat with worktree", false, true, "/worktree", false},
		{"attached without worktree", true, false, "", false},
		{"attached with worktree", true, false, "/worktree", false},
		{"attached quick chat without worktree", true, true, "", false},
		{"attached quick chat with worktree", true, true, "/worktree", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsAccepted(tt.attachedExisting, tt.isQuickChat, tt.worktreePath); got != tt.want {
				t.Errorf("IsAccepted(%t, %t, %q) = %t, want %t", tt.attachedExisting, tt.isQuickChat, tt.worktreePath, got, tt.want)
			}
		})
	}
}
