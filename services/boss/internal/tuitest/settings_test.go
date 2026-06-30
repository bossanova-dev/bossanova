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

func TestTUI_Settings_TrashReturnsToSettings(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "[s]ettings"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendKey('s'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Settings") &&
			strings.Contains(screen, "[r]epos") &&
			strings.Contains(screen, "[c]ron") &&
			strings.Contains(screen, "[t]rash")
	}); err != nil {
		t.Fatalf("expected settings hub actions; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendKey('t'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Archived Sessions"); err != nil {
		t.Fatalf("expected trash view from settings; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.WaitForText(waitTimeout, "[esc] back"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Settings") &&
			strings.Contains(screen, "[r]epos") &&
			strings.Contains(screen, "[c]ron")
	}); err != nil {
		t.Fatalf("expected to return to settings after esc from trash; screen:\n%s", h.Driver.Screen())
	}
}

func TestTUI_Settings_ReposReturnsToSettings(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "[s]ettings"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendKey('s'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Settings") &&
			strings.Contains(screen, "[r]epos") &&
			strings.Contains(screen, "[c]ron") &&
			strings.Contains(screen, "[t]rash")
	}); err != nil {
		t.Fatalf("expected settings hub actions; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendKey('r'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "PATH"); err != nil {
		t.Fatalf("expected repos view from settings; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.WaitForText(waitTimeout, "[esc] back"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Settings") &&
			strings.Contains(screen, "[c]ron")
	}); err != nil {
		t.Fatalf("expected to return to settings after esc from repos; screen:\n%s", h.Driver.Screen())
	}
}

func TestTUI_Settings_CronReturnsToSettings(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "[s]ettings"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendKey('s'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Settings") &&
			strings.Contains(screen, "[c]ron")
	}); err != nil {
		t.Fatalf("expected settings cron action; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendKey('c'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "No cron jobs"); err != nil {
		t.Fatalf("expected cron view from settings; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Settings") &&
			strings.Contains(screen, "[r]epos") &&
			strings.Contains(screen, "[t]rash")
	}); err != nil {
		t.Fatalf("expected to return to settings after esc from cron; screen:\n%s", h.Driver.Screen())
	}
}

func TestTUI_Settings_RepoAddFirstRepoReturnsToSettings(t *testing.T) {
	h := tuitest.New(t)
	h.Daemon.SetValidateRepoPathResult(&pb.ValidateRepoPathResponse{
		IsValid:       true,
		IsGithub:      true,
		OriginUrl:     "https://github.com/acme/widgets.git",
		DefaultBranch: "main",
	})

	// With no repos, the app lands on the Home empty state (no auto-route). Open
	// Settings via the 's' key (the hint is hidden on the zero-repo screen).
	waitForZeroRepoHome(t, h)
	if err := h.Driver.SendKey('s'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Settings") &&
			strings.Contains(screen, "[r]epos")
	}); err != nil {
		t.Fatalf("expected settings repos action; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendKey('r'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Press 'a' to add your first repository"); err != nil {
		t.Fatalf("expected empty repo list from settings; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendKey('a'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Open project"); err != nil {
		t.Fatalf("expected source phase; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Add a local repository"); err != nil {
		t.Fatalf("expected input phase; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendString("widgets"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Merge strategy"); err != nil {
		t.Fatalf("expected details phase; screen:\n%s", h.Driver.Screen())
	}
	// Accept the details form (5 fields) with all defaults.
	for range 5 {
		if err := h.Driver.SendEnter(); err != nil {
			t.Fatal(err)
		}
	}

	// Completing the add returns to the settings hub it was launched from; the
	// repo's options were configured inline on the add wizard.
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Settings") &&
			strings.Contains(screen, "[r]epos") &&
			strings.Contains(screen, "[c]ron")
	}); err != nil {
		t.Fatalf("expected settings after adding first repo from settings; screen:\n%s", h.Driver.Screen())
	}
	if got := len(h.Daemon.Repos()); got != 1 {
		t.Fatalf("registered repos = %d, want 1", got)
	}
}
