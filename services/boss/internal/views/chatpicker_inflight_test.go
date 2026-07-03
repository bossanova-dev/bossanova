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
