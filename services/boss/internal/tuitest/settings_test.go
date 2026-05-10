package tuitest_test

import (
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/tuitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// settingsAgent returns an AgentInfo equivalent to the legacy hardcoded
// "claude" plugin row — bool toggle for dangerously-skip-permissions —
// so the existing settings view tests still have a plugin row to assert
// against under the schema-driven render path.
func settingsAgent() *pb.AgentInfo {
	return &pb.AgentInfo{
		Name:    "claude",
		Version: "v1",
		UserSettings: []*pb.UserSetting{
			{
				Key:   "dangerously_skip_permissions",
				Label: "Enable Claude --dangerously-skip-permissions",
				Type:  pb.UserSettingType_USER_SETTING_TYPE_BOOL,
			},
		},
	}
}

func TestTUI_SettingsView_Content(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithAgents(settingsAgent()),
	)

	if err := h.Driver.WaitForText(waitTimeout, "no active sessions"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('s'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Settings"); err != nil {
		t.Fatal(err)
	}

	screen := h.Driver.Screen()
	if !strings.Contains(screen, "dangerously-skip-permissions") {
		t.Fatalf("expected permissions setting on screen:\n%s", screen)
	}
	if !strings.Contains(screen, "Worktree base directory") {
		t.Fatalf("expected worktree setting on screen:\n%s", screen)
	}
	if !strings.Contains(screen, "Poll interval") {
		t.Fatalf("expected poll interval setting on screen:\n%s", screen)
	}
}

func TestTUI_SettingsView_TogglePermissions(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithAgents(settingsAgent()),
	)

	if err := h.Driver.WaitForText(waitTimeout, "no active sessions"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('s'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Settings"); err != nil {
		t.Fatal(err)
	}

	// The schema-driven settings view places built-in rows (worktree,
	// poll interval) before per-agent rows. Walk the cursor down past
	// them to the bool checkbox we seeded, then toggle.
	for i := 0; i < 2; i++ {
		if err := h.Driver.SendKey('j'); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := h.Driver.SendKey(' '); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)

	// Check that the checkbox state changed (either [x] appeared or [ ] appeared).
	screen := h.Driver.Screen()
	hasChecked := strings.Contains(screen, "[x]")
	hasUnchecked := strings.Contains(screen, "[ ]")
	if !hasChecked && !hasUnchecked {
		t.Fatalf("expected checkbox state on screen:\n%s", screen)
	}

	// Toggle again.
	if err := h.Driver.SendKey(' '); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)

	screen2 := h.Driver.Screen()
	hasChecked2 := strings.Contains(screen2, "[x]")
	hasUnchecked2 := strings.Contains(screen2, "[ ]")
	if !hasChecked2 && !hasUnchecked2 {
		t.Fatalf("expected checkbox state after second toggle on screen:\n%s", screen2)
	}
}

func TestTUI_SettingsView_JKNavigation(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithAgents(settingsAgent()),
	)

	if err := h.Driver.WaitForText(waitTimeout, "no active sessions"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('s'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Settings"); err != nil {
		t.Fatal(err)
	}

	// Navigate down through all 3 rows and back up.
	if err := h.Driver.SendKey('j'); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

	if err := h.Driver.SendKey('j'); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

	if err := h.Driver.SendKey('k'); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

	if err := h.Driver.SendKey('k'); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

	// All settings should still be visible.
	screen := h.Driver.Screen()
	if !strings.Contains(screen, "dangerously-skip-permissions") {
		t.Fatalf("expected permissions setting on screen:\n%s", screen)
	}
	if !strings.Contains(screen, "Worktree base directory") {
		t.Fatalf("expected worktree setting on screen:\n%s", screen)
	}
}

func TestTUI_SettingsView_EditCancel(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithAgents(settingsAgent()),
	)

	if err := h.Driver.WaitForText(waitTimeout, "no active sessions"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('s'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Settings"); err != nil {
		t.Fatal(err)
	}

	// Move to "Worktree base directory" (row 1).
	if err := h.Driver.SendKey('j'); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

	// Press enter to edit.
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)

	// Press escape to cancel the edit.
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)

	// Should still be in settings (not navigated away).
	screen := h.Driver.Screen()
	if !strings.Contains(screen, "Worktree base directory") {
		t.Fatalf("expected to still be in settings after edit cancel; screen:\n%s", screen)
	}
}

func TestTUI_SettingsView_BackWithQ(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.SendKey('s'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Settings"); err != nil {
		t.Fatal(err)
	}

	// Press esc to go back (q no longer works on sub-screens).
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatalf("expected to return to home after esc from settings; screen:\n%s", h.Driver.Screen())
	}
}
