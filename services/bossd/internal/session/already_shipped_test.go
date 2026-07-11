package session

import (
	"testing"

	"github.com/recurser/bossalib/vcs"
)

func TestFirstShippedTaggedPR(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		prs       []vcs.PRSummary
		trackerID string
		wantOK    bool
		wantNum   int
	}{
		{
			name:      "no prs",
			prs:       nil,
			trackerID: "BOS-289",
			wantOK:    false,
		},
		{
			name: "merged exact tag blocks",
			prs: []vcs.PRSummary{
				{Number: 1146, Title: "consolidate [BOS-289]", State: vcs.PRStateMerged},
			},
			trackerID: "BOS-289",
			wantOK:    true,
			wantNum:   1146,
		},
		{
			name: "open exact tag blocks",
			prs: []vcs.PRSummary{
				{Number: 1147, Title: "work [BOS-289] wip", State: vcs.PRStateOpen},
			},
			trackerID: "BOS-289",
			wantOK:    true,
			wantNum:   1147,
		},
		{
			name: "closed unmerged does not block",
			prs: []vcs.PRSummary{
				{Number: 99, Title: "abandoned [BOS-289]", State: vcs.PRStateClosed},
			},
			trackerID: "BOS-289",
			wantOK:    false,
		},
		{
			name: "near-miss longer identifier ignored",
			prs: []vcs.PRSummary{
				{Number: 42, Title: "unrelated [BOS-2890]", State: vcs.PRStateMerged},
			},
			trackerID: "BOS-289",
			wantOK:    false,
		},
		{
			name: "first open-or-merged match wins, closed skipped",
			prs: []vcs.PRSummary{
				{Number: 10, Title: "closed [BOS-289]", State: vcs.PRStateClosed},
				{Number: 11, Title: "merged [BOS-289]", State: vcs.PRStateMerged},
				{Number: 12, Title: "open [BOS-289]", State: vcs.PRStateOpen},
			},
			trackerID: "BOS-289",
			wantOK:    true,
			wantNum:   11,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := firstShippedTaggedPR(tt.prs, tt.trackerID)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got.Number != tt.wantNum {
				t.Fatalf("PR number = %d, want %d", got.Number, tt.wantNum)
			}
		})
	}
}

func TestAlreadyShippedTaggedPR_BuildsTypedError(t *testing.T) {
	t.Parallel()

	prs := []vcs.PRSummary{
		{Number: 1146, Title: "consolidate [BOS-289]", State: vcs.PRStateMerged},
	}
	shipped, ok := AlreadyShippedTaggedPR(prs, "BOS-289")
	if !ok {
		t.Fatal("AlreadyShippedTaggedPR ok = false, want true")
	}
	if shipped.PRNumber != 1146 || shipped.TrackerID != "BOS-289" || shipped.State != "merged" {
		t.Fatalf("error fields = %+v, want {BOS-289 1146 merged}", shipped)
	}
	if !IsAlreadyShippedError(shipped) {
		t.Fatal("IsAlreadyShippedError = false, want true")
	}
	want := "ticket BOS-289 already shipped via PR #1146 (merged); pass force to create another"
	if shipped.Error() != want {
		t.Fatalf("Error() = %q, want %q", shipped.Error(), want)
	}
}

func TestAlreadyShippedTaggedPR_NoMatchReturnsNilFalse(t *testing.T) {
	t.Parallel()

	shipped, ok := AlreadyShippedTaggedPR(nil, "BOS-289")
	if ok || shipped != nil {
		t.Fatalf("got (%+v, %v), want (nil, false)", shipped, ok)
	}
}

func TestIsAlreadyShippedError_OtherErrorFalse(t *testing.T) {
	t.Parallel()

	if IsAlreadyShippedError(nil) {
		t.Fatal("IsAlreadyShippedError(nil) = true, want false")
	}
	if IsAlreadyShippedError(&DuplicateActivePRSessionError{}) {
		t.Fatal("IsAlreadyShippedError(duplicate-active) = true, want false")
	}
}
