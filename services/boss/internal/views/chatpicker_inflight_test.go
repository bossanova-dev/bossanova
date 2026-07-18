package views

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// TestChatPicker_EscDuringArchive_ReturnsToList verifies that pressing esc
// while an archive is in flight sets Cancelled() = true so the app routes
// back to the session list, without aborting the archive RPC.
func TestChatPicker_EscDuringArchive_ReturnsToList(t *testing.T) {
	m := NewChatPickerModel(&chatPickerStub{}, context.Background(), "session-1", "")
	m.archiving = true

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	got := updated.(ChatPickerModel)

	if !got.Cancelled() {
		t.Fatal("esc during archive must set Cancelled() = true so the app routes back to the session list")
	}
}

// TestChatPicker_EscDuringMerge_ReturnsToList verifies that pressing esc
// while a merge is in flight sets Cancelled() = true.
func TestChatPicker_EscDuringMerge_ReturnsToList(t *testing.T) {
	m := NewChatPickerModel(&chatPickerStub{}, context.Background(), "session-1", "")
	m.merging = true

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	got := updated.(ChatPickerModel)

	if !got.Cancelled() {
		t.Fatal("esc during merge must set Cancelled() = true so the app routes back to the session list")
	}
}

// TestChatPicker_ConflictingKeysBlockedDuringArchive verifies that action keys
// (a, m, d, n) are still swallowed while an archive is in flight — no second
// conflicting operation can start.
func TestChatPicker_ConflictingKeysBlockedDuringArchive(t *testing.T) {
	for _, key := range []rune{'a', 'm', 'd', 'n'} {
		t.Run(string(key), func(t *testing.T) {
			m := seedChatPicker(&chatPickerStub{}, "")
			m.archiving = true

			updated, _ := m.Update(keyPress(key))
			got := updated.(ChatPickerModel)

			if got.confirm != confirmNone {
				t.Errorf("pressing %q during archive must not change confirm state; got %v", key, got.confirm)
			}
			if got.Cancelled() {
				t.Errorf("pressing %q during archive must not cancel the picker", key)
			}
		})
	}
}

// TestChatPicker_CanMergeFalseWhileMerging guards that a merge already in
// flight hides the [m]erge affordance: canMerge() must return false while
// m.merging is true even when the session otherwise qualifies (open passing
// PR), so re-entering a still-merging session shows the optimistic status
// instead of offering a second merge.
func TestChatPicker_CanMergeFalseWhileMerging(t *testing.T) {
	m := seedChatPickerWithPassingPR()

	if !m.canMerge() {
		t.Fatal("canMerge() = false before merge; test pre-condition broken (session must have an open passing PR)")
	}

	m.merging = true
	if m.canMerge() {
		t.Fatal("canMerge() = true while merging; expected false so [m]erge drops from the action bar during an in-flight merge")
	}
}

// TestChatPicker_NotInFlight_AStillEntersConfirmArchive is a regression guard:
// when neither archive nor merge is in flight, pressing 'a' still enters
// confirmArchive as before.
func TestChatPicker_NotInFlight_AStillEntersConfirmArchive(t *testing.T) {
	m := seedChatPicker(&chatPickerStub{}, "")
	// Ensure neither in-flight flag is set.
	m.archiving = false
	m.merging = false

	updated, _ := m.Update(keyPress('a'))
	got := updated.(ChatPickerModel)

	if got.confirm != confirmArchive {
		t.Fatalf("pressing 'a' when not in flight must arm confirmArchive; got %v", got.confirm)
	}
}

// TestChatPicker_NotInFlight_EscStillCancels is a regression guard: when
// neither archive nor merge is in flight, pressing esc still cancels the
// chat picker.
func TestChatPicker_NotInFlight_EscStillCancels(t *testing.T) {
	m := seedChatPicker(&chatPickerStub{}, "")

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	got := updated.(ChatPickerModel)

	if !got.Cancelled() {
		t.Fatal("esc when not in flight must still cancel the chat picker")
	}
}

// TestChatPicker_EscDuringArchiveInErrorState verifies that pressing esc while
// archiving in the m.err != nil sub-state also sets Cancelled() = true.
func TestChatPicker_EscDuringArchiveInErrorState(t *testing.T) {
	m := NewChatPickerModel(&chatPickerStub{}, context.Background(), "session-1", "")
	m.err = errors.New("listing failed")
	m.archiving = true

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	got := updated.(ChatPickerModel)

	if !got.Cancelled() {
		t.Fatal("esc during archive in the error sub-state must set Cancelled() = true")
	}
}

// TestChatPicker_ResurrectedMerged_NoArchivingSpinner is the BOS-425 regression
// guard: a merged session in an archive-after-merge repo that has already been
// archived-and-resurrected (archived_at=nil, archive_pending=false) must NOT
// render "Archiving session..." — the old heuristic inferred it from
// MERGED + repo flag and showed a permanent false spinner.
func TestChatPicker_ResurrectedMerged_NoArchivingSpinner(t *testing.T) {
	m := seedChatPicker(&chatPickerStub{}, "")
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = updated.(ChatPickerModel)
	m.session = &pb.Session{
		Id:                            "session-1",
		DisplayStatus:                 pb.DisplayStatus_DISPLAY_STATUS_MERGED,
		RepoArchiveSessionsAfterMerge: true,
		ArchivePending:                false, // daemon is NOT archiving (resurrected)
		// ArchivedAt nil — the session is live again after resurrection.
	}

	if m.isArchiving() {
		t.Fatal("resurrected merged session must not be considered archiving")
	}
	rendered := m.View().Content
	if strings.Contains(rendered, "Archiving session") {
		t.Errorf("resurrected merged session rendered a false 'Archiving session' spinner; got:\n%s", rendered)
	}
}

// TestChatPicker_ArchivePending_ShowsArchivingSpinner verifies the daemon-driven
// signal drives the spinner: archive_pending=true renders "Archiving session...".
func TestChatPicker_ArchivePending_ShowsArchivingSpinner(t *testing.T) {
	m := seedChatPicker(&chatPickerStub{}, "")
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = updated.(ChatPickerModel)
	m.session = &pb.Session{
		Id:                            "session-1",
		DisplayStatus:                 pb.DisplayStatus_DISPLAY_STATUS_MERGED,
		RepoArchiveSessionsAfterMerge: true,
		ArchivePending:                true, // daemon has an archive in flight
	}

	if !m.isArchiving() {
		t.Fatal("archive_pending=true must be considered archiving")
	}
	rendered := m.View().Content
	if !strings.Contains(rendered, "Archiving session") {
		t.Errorf("archive_pending=true did not render 'Archiving session' spinner; got:\n%s", rendered)
	}
}

// TestChatPicker_OptimisticLatchAfterMerge_ShowsArchivingSpinner verifies the
// short-lived optimistic latch: right after a TUI-initiated merge on an
// archive-after-merge repo, the detail view shows "Archiving session..."
// immediately, before the daemon's archive_pending round-trips.
func TestChatPicker_OptimisticLatchAfterMerge_ShowsArchivingSpinner(t *testing.T) {
	m := seedChatPicker(&chatPickerStub{}, "")
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = updated.(ChatPickerModel)
	// Session with archive-after-merge on but archive_pending still false (webhook
	// not yet processed by the daemon).
	m.session = &pb.Session{
		Id:                            "session-1",
		RepoArchiveSessionsAfterMerge: true,
		ArchivePending:                false,
	}

	updated, _ = m.Update(mergeResultMsg{sessionID: "session-1"})
	m = updated.(ChatPickerModel)

	if !m.optimisticArchiveLatch {
		t.Fatal("mergeResultMsg on archive-after-merge repo must set the optimistic latch")
	}
	if !m.isArchiving() {
		t.Fatal("optimistic latch must make the picker considered archiving")
	}
	rendered := m.View().Content
	if !strings.Contains(rendered, "Archiving session") {
		t.Errorf("optimistic latch did not render 'Archiving session' spinner; got:\n%s", rendered)
	}
}

// TestChatPicker_OptimisticLatch_ClearedWhenRepoDisablesArchive verifies the
// latch cannot get stuck: if a poll shows the repo disabled archive-after-merge
// after the merge, the optimistic latch is dropped.
func TestChatPicker_OptimisticLatch_ClearedWhenRepoDisablesArchive(t *testing.T) {
	m := seedChatPicker(&chatPickerStub{}, "")
	m.optimisticArchiveLatch = true

	// A refresh poll lands a session with archive-after-merge now OFF.
	updated, _ := m.Update(chatPickerRefreshMsg{
		session: &pb.Session{Id: "session-1", RepoArchiveSessionsAfterMerge: false},
	})
	m = updated.(ChatPickerModel)

	if m.optimisticArchiveLatch {
		t.Fatal("optimistic latch must clear when the repo disables archive-after-merge")
	}
	if m.isArchiving() {
		t.Fatal("picker must not be considered archiving after the latch clears and archive_pending is false")
	}
}

// TestChatPicker_OptimisticLatch_HandsOffToDaemonAndCannotStick is the BOS-425
// stuck-spinner guard: after a TUI-initiated merge sets the optimistic latch, the
// first poll reporting archive_pending=true hands off to the daemon signal
// (latch dropped, spinner still shown via archive_pending). When the daemon then
// clears archive_pending WITHOUT the session archiving (a failed ArchiveSession:
// the defer clears the flag, archived_at stays nil), the spinner must stop — the
// latch cannot outlive the archive attempt it bridged.
func TestChatPicker_OptimisticLatch_HandsOffToDaemonAndCannotStick(t *testing.T) {
	m := seedChatPicker(&chatPickerStub{}, "")
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m = updated.(ChatPickerModel)
	m.session = &pb.Session{Id: "session-1", RepoArchiveSessionsAfterMerge: true, ArchivePending: false}

	// TUI-initiated merge sets the optimistic latch (pre-signal bridge).
	updated, _ = m.Update(mergeResultMsg{sessionID: "session-1"})
	m = updated.(ChatPickerModel)
	if !m.optimisticArchiveLatch || !m.isArchiving() {
		t.Fatal("merge must set the optimistic latch and show archiving")
	}

	// First poll reports the daemon archive actually in flight: hand off.
	updated, _ = m.Update(chatPickerRefreshMsg{session: &pb.Session{
		Id: "session-1", RepoArchiveSessionsAfterMerge: true, ArchivePending: true,
	}})
	m = updated.(ChatPickerModel)
	if m.optimisticArchiveLatch {
		t.Fatal("latch must be dropped once archive_pending is observed true (handoff)")
	}
	if !m.isArchiving() {
		t.Fatal("spinner must still show via the daemon archive_pending signal during the archive")
	}

	// The daemon's ArchiveSession fails: archive_pending clears, archived_at stays
	// nil. With the latch already handed off, the spinner must stop — not stick.
	updated, _ = m.Update(chatPickerRefreshMsg{session: &pb.Session{
		Id: "session-1", RepoArchiveSessionsAfterMerge: true, ArchivePending: false,
	}})
	m = updated.(ChatPickerModel)
	if m.optimisticArchiveLatch {
		t.Fatal("latch must remain cleared after handoff")
	}
	if m.isArchiving() {
		t.Fatal("spinner must stop once the daemon clears archive_pending; a stuck latch would reintroduce the BOS-425 false spinner")
	}
	if strings.Contains(m.View().Content, "Archiving session") {
		t.Errorf("stuck 'Archiving session' spinner after a failed daemon archive:\n%s", m.View().Content)
	}
}

// TestChatPicker_InFlightSpinnerCopy_OmitsEscHint verifies that the rendered
// View() output for the in-flight merging spinner renders the "Merging PR"
// copy without the esc-discoverability hint (esc is the app's standard
// back-navigation, so the hint adds no value).
func TestChatPicker_InFlightSpinnerCopy_OmitsEscHint(t *testing.T) {
	t.Run("merging", func(t *testing.T) {
		m := seedChatPicker(&chatPickerStub{}, "")
		updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
		m = updated.(ChatPickerModel)
		m.merging = true

		rendered := m.View().Content
		if !strings.Contains(rendered, "Merging PR") {
			t.Errorf("merging spinner missing 'Merging PR' copy; got:\n%s", rendered)
		}
		if strings.Contains(rendered, "esc to return to list") {
			t.Errorf("merging spinner should not include esc hint; got:\n%s", rendered)
		}
	})

	t.Run("merging with PR number", func(t *testing.T) {
		m := seedChatPicker(&chatPickerStub{}, "")
		updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
		m = updated.(ChatPickerModel)
		prNumber := int32(42)
		m.session = &pb.Session{Id: "session-1", PrNumber: &prNumber}
		m.merging = true

		rendered := m.View().Content
		if !strings.Contains(rendered, "Merging PR #42") {
			t.Errorf("merging spinner missing 'Merging PR #42' copy; got:\n%s", rendered)
		}
		if strings.Contains(rendered, "esc to return to list") {
			t.Errorf("merging spinner with PR number should not include esc hint; got:\n%s", rendered)
		}
	})
}
