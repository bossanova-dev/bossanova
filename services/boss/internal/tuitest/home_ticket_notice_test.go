package tuitest_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/recurser/boss/internal/tuitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
)

// TestTUI_HomeView_TicketNoticeShowsUntilDismissed covers the TUI half of a
// ticket bossd could not move after a merge: the note it leaves shows on Home
// until [c] clears it, and the dismissal is stored on the note itself.
func TestTUI_HomeView_TicketNoticeShowsUntilDismissed(t *testing.T) {
	repoID := testRepos()[0].GetId()
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
		tuitest.WithTerminalSize(160, 40),
		tuitest.WithNotes(
			&pb.Note{Id: "n-unseen", RepoId: repoID, Body: `PR #6660 merged, but its ticket MAD-1764 could not be moved (state "Done" not found)`, Tags: []string{models.NoteTagTicketNotMoved, "tracker"}},
			&pb.Note{Id: "n-seen", RepoId: repoID, Body: `PR #5555 merged, but its ticket MAD-1 could not be moved (state "Done" not found)`, Tags: []string{models.NoteTagSeen, models.NoteTagTicketNotMoved, "tracker"}},
			&pb.Note{Id: "n-agent", RepoId: repoID, Body: "PR #4444 agent note about the tracker", Tags: []string{"tracker"}},
		),
	)

	if err := h.Driver.WaitForText(waitTimeout, "PR #6660"); err != nil {
		t.Fatalf("the unseen ticket notice never showed; screen:\n%s", h.Driver.Screen())
	}
	screen := h.Driver.Screen()
	if !strings.Contains(screen, "[c]lear") {
		t.Fatalf("the notice offers no dismissal; screen:\n%s", screen)
	}
	for _, hidden := range []string{"PR #5555", "PR #4444"} {
		if strings.Contains(screen, hidden) {
			t.Fatalf("%s is shown, want only unseen ticket notices; screen:\n%s", hidden, screen)
		}
	}

	if err := h.Driver.SendKey('c'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForNoText(waitTimeout, "PR #6660"); err != nil {
		t.Fatalf("the notice stayed after [c]; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.WaitFor(waitTimeout, func(string) bool {
		for _, n := range h.Daemon.Notes() {
			if n.GetId() == "n-unseen" {
				return slices.Contains(n.GetTags(), models.NoteTagSeen)
			}
		}
		return false
	}); err != nil {
		t.Fatalf("the dismissed note was never tagged seen: %+v", h.Daemon.Notes())
	}
	if calls := h.Daemon.UpdateNoteCalls(); len(calls) != 1 || calls[0].GetId() != "n-unseen" {
		t.Fatalf("UpdateNote calls = %+v, want one, for the dismissed note", calls)
	}
}
