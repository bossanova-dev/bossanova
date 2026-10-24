package tui

import (
	"errors"
	"testing"

	"github.com/recurser/boss/api/tui/ui"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/sessionreason"
)

func TestSessionSubRowCount_AgreesWithBuildTableRows(t *testing.T) {
	sessions := []*pb.Session{
		{Id: "s0", Title: "no hints", DisplayLabel: "idle"},
		{Id: "s1", Title: "one hint", DisplayLabel: "working", SetupError: "npm install failed"},
		{Id: "s2", Title: "several hints", DisplayLabel: "working",
			SetupError:             "npm install failed",
			LastRepairAttemptCount: 2,
			LastRepairExitError:    "exit status 1",
			BlockedReason:          draftPRFailureReason("gh pr create: auth required"),
		},
	}
	wantExtra := 0
	for _, sess := range sessions {
		wantExtra += ui.SessionSubRowCount(sess, "", true)
	}
	if wantExtra == 0 {
		t.Fatal("fixture produced no auxiliary rows; the test would be vacuous")
	}

	home := HomeModel{Sessions: sessions, Width: 200, Height: 60, Spinner: ui.NewStatusSpinner()}
	home.BuildTableRows()
	gotRows := len(home.Table.Rows())
	if want := len(sessions) + wantExtra; gotRows != want {
		t.Fatalf("buildTableRows emitted %d rows, want %d (%d primary + %d auxiliary)",
			gotRows, want, len(sessions), wantExtra)
	}
	if got, want := ui.SessionSubRowCount(sessions[0], "", true), 0; got != want {
		t.Errorf("no-hint session sub-rows = %d, want %d", got, want)
	}
	if got, want := ui.SessionSubRowCount(sessions[1], "", true), 1; got != want {
		t.Errorf("one-hint session sub-rows = %d, want %d", got, want)
	}
	if got, want := ui.SessionSubRowCount(sessions[2], "", true), 3; got != want {
		t.Errorf("several-hint session sub-rows = %d, want %d", got, want)
	}
}

func draftPRFailureReason(detail string) *string {
	r := sessionreason.DraftPRCreationFailure(errors.New(detail))
	return &r
}
