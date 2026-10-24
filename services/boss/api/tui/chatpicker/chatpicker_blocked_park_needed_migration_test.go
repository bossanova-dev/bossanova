package chatpicker

import "testing"

// TestBlockedParkNeededListsEachOpenItemAsABullet pins the "Needed from you"
// text of an open-checklist park: a heading line, then one bullet per open
// item in the plain words the park note recorded.
func TestBlockedParkNeededListsEachOpenItemAsABullet(t *testing.T) {
	a := parkSession("ledger-open", "The Acceptance criteria section is missing.", "Open question: which region first?").GetAttention()
	want := "Open in the PR checklist:\n  • The Acceptance criteria section is missing.\n  • Open question: which region first?"
	if got := blockedParkNeeded(a); got != want {
		t.Fatalf("blockedParkNeeded = %q, want %q", got, want)
	}
}
