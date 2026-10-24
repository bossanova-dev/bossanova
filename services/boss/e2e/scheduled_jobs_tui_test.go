package e2e_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/tuitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// testCronJob returns a seeded cron job for tests that need a pre-existing job.
func testCronJob() *pb.CronJob {
	return &pb.CronJob{
		Id:        "cron-test-1",
		RepoId:    "repo-1",
		Name:      "Daily update",
		Prompt:    "Run the daily update script.",
		Schedule:  "0 9 * * 1-5",
		Timezone:  "",
		IsEnabled: true,
		CreatedAt: timestamppb.Now(),
		UpdatedAt: timestamppb.Now(),
	}
}

// navigateToCronList navigates from the home screen to the cron list via Settings.
func navigateToCronList(t *testing.T, h *tuitest.Harness) {
	t.Helper()

	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Bossanova") || strings.Contains(screen, "no active sessions")
	}); err != nil {
		t.Fatal(err)
	}

	openSettingsHub(t, h)

	if err := h.Driver.SendKey('c'); err != nil {
		t.Fatal(err)
	}

	// Wait for the cron list view to appear. We require something that is
	// unique to the cron list and cannot match the home screen's action bar
	// (which also contains "[n]ew session").
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "No cron jobs") ||
			strings.Contains(screen, "Scheduled Jobs") ||
			strings.Contains(screen, "CRON") ||
			(strings.Contains(screen, "[esc] back") && strings.Contains(screen, "[n]ew"))
	}); err != nil {
		t.Fatalf("expected cron list view; screen:\n%s", h.Driver.Screen())
	}
}

// waitForCronListPopulated waits for the cron list to be populated (non-empty state),
// then gives the TUI a moment to render the table rows.
func waitForCronListPopulated(t *testing.T, h *tuitest.Harness) {
	t.Helper()

	// When WithCronJobs is used, the cron list should show the table (not
	// empty-state) once data arrives. We wait until we do NOT see "No cron jobs"
	// (which appears in the empty-state branch of View) and DO see the action bar,
	// which means at least one job is loaded.
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Scheduled Jobs") &&
			strings.Contains(screen, "[d]elete")
	}); err != nil {
		t.Fatalf("expected populated cron list; screen:\n%s", h.Driver.Screen())
	}

	// Wait for the table row to appear (unique text from the seeded job) so
	// the TUI has fully settled before we send the next keystroke.
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Daily update") ||
			strings.Contains(screen, "Roundtrip Job") ||
			strings.Contains(screen, "[e/enter]dit")
	}); err != nil {
		t.Fatalf("expected cron table rows to be visible; screen:\n%s", h.Driver.Screen())
	}
}

// TestCron_SettingsC_OpensList verifies that pressing 'c' from Settings opens the cron
// list with the empty-state copy when no cron jobs exist.
func TestCron_SettingsC_OpensList(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow TUI test in -short; run make test-boss for coverage")
	}
	h := tuitest.NewMock(t,
		tuitest.WithRepos(testRepos()...),
	)

	navigateToCronList(t, h)

	if err := h.Driver.WaitForText(waitTimeout, "No cron jobs. Press [n] to create one."); err != nil {
		t.Fatalf("expected empty-state copy; screen:\n%s", h.Driver.Screen())
	}
}

// focusedCronField returns the first row of the focused form field: its title,
// or its buttons for the untitled save confirm, without huh's " *" error mark.
// huh draws the focused field behind the thick gutter (views.fieldGutter) and
// every blurred one behind a blank gutter of the same width. "" means no field
// has focus: the form is still loading, has completed, or the frame is torn.
func focusedCronField(screen string) string {
	for _, line := range strings.Split(screen, "\n") {
		if _, rest, ok := strings.Cut(line, "┃"); ok {
			return strings.TrimSuffix(strings.TrimSpace(rest), " *")
		}
	}
	return ""
}

// openCronForm presses key on the cron list and waits for the form titled
// title to finish loading with focus on Name. Until repos and agents load, the
// form shows "Loading…", draws no field, and drops every key but Esc, so keys
// typed before Name has focus can be lost.
func openCronForm(t *testing.T, h *tuitest.Harness, key byte, title string) {
	t.Helper()
	if err := h.Driver.SendKey(key); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, title) && focusedCronField(screen) == "Name"
	}); err != nil {
		t.Fatalf("expected %q loaded with focus on Name; screen:\n%s", title, h.Driver.Screen())
	}
}

// waitForAnyFocusedCronField waits until some form field has focus and
// returns it.
func waitForAnyFocusedCronField(t *testing.T, h *tuitest.Harness) string {
	t.Helper()
	var focused string
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		focused = focusedCronField(screen)
		return focused != ""
	}); err != nil {
		t.Fatalf("no cron form field has focus; screen:\n%s", h.Driver.Screen())
	}
	return focused
}

// advanceCronFormField sends Enter and waits until another field has focus.
// huh moves focus in a command that runs after the key, so a key sent before
// the screen shows the move lands in the field being left.
func advanceCronFormField(t *testing.T, h *tuitest.Harness) {
	t.Helper()
	advanceCronForm(t, h, h.Driver.SendEnter)
}

// advanceCronFormPrompt is advanceCronFormField for the Prompt text area,
// where Enter types a newline and Tab advances.
func advanceCronFormPrompt(t *testing.T, h *tuitest.Harness) {
	t.Helper()
	advanceCronForm(t, h, func() error { return h.Driver.SendKey('\t') })
}

func advanceCronForm(t *testing.T, h *tuitest.Harness, send func() error) {
	t.Helper()
	from := waitForAnyFocusedCronField(t, h)
	if err := send(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		to := focusedCronField(screen)
		return to != "" && to != from
	}); err != nil {
		t.Fatalf("focus did not move from %q to another field; screen:\n%s", from, h.Driver.Screen())
	}
}

// submitCronForm presses Enter on the focused save confirm and waits for the
// completed form to stop drawing fields.
func submitCronForm(t *testing.T, h *tuitest.Harness, confirm string) {
	t.Helper()
	if from := waitForAnyFocusedCronField(t, h); !strings.Contains(from, confirm) {
		t.Fatalf("expected focus on the %q confirm, got %q; screen:\n%s", confirm, from, h.Driver.Screen())
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return focusedCronField(screen) == ""
	}); err != nil {
		t.Fatalf("form still has a focused field after submit; screen:\n%s", h.Driver.Screen())
	}
}

// rejectCronFormField sends a key that tries to leave a field whose value fails
// validation, and waits for huh's error with focus kept on the field.
func rejectCronFormField(t *testing.T, h *tuitest.Harness, send func() error, wantErr string) {
	t.Helper()
	from := waitForAnyFocusedCronField(t, h)
	if err := send(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, wantErr) && focusedCronField(screen) == from
	}); err != nil {
		t.Fatalf("expected %q with focus kept on %q; screen:\n%s", wantErr, from, h.Driver.Screen())
	}
}

// waitForFocusedCronField waits until the field titled title has focus.
func waitForFocusedCronField(t *testing.T, h *tuitest.Harness, title string) {
	t.Helper()
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return focusedCronField(screen) == title
	}); err != nil {
		t.Fatalf("field %q never took focus; screen:\n%s", title, h.Driver.Screen())
	}
}

// TestCron_NewForm_LivePreview verifies that the form shows a live "Next fire:"
// preview when the schedule field contains a valid cron expression.
func TestCron_NewForm_LivePreview(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow TUI test in -short; run make test-boss for coverage")
	}
	// Use a taller terminal so the live-preview line (rendered below the form)
	// is visible. The cron form has ~30 rows of content at the default height.
	h := tuitest.NewMock(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithTerminalSize(120, 50),
	)

	navigateToCronList(t, h)

	openCronForm(t, h, 'n', "New Scheduled Job")

	// Field 1: Name.
	if err := h.Driver.SendString("My Test Job"); err != nil {
		t.Fatal(err)
	}
	advanceCronFormField(t, h)

	// Field 2: Repo — already has "my-app" selected; advance.
	advanceCronFormField(t, h)

	// Field 3: Model — free-text; leave blank and advance with Enter.
	advanceCronFormField(t, h)

	// Field 4: Prompt — type and advance with Tab (huh text-area convention).
	if err := h.Driver.SendString("Do the thing."); err != nil {
		t.Fatal(err)
	}
	advanceCronFormPrompt(t, h)

	// Field 4: Schedule — type a valid named expression.
	// Use "@hourly" (no special terminal characters) to avoid PTY quirks with '*'.
	if err := h.Driver.SendString("@hourly"); err != nil {
		t.Fatal(err)
	}

	// The live preview should appear below the form once recomputePreview runs.
	if err := h.Driver.WaitForText(waitTimeout, "Next fire:"); err != nil {
		t.Fatalf("expected live 'Next fire:' preview; screen:\n%s", h.Driver.Screen())
	}
}

// TestCron_FormValidation verifies that invalid inputs prevent form submission.
func TestCron_FormValidation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow TUI test in -short; run make test-boss for coverage")
	}
	t.Run("bad_schedule", func(t *testing.T) {
		h := tuitest.NewMock(t,
			tuitest.WithRepos(testRepos()...),
		)

		navigateToCronList(t, h)

		openCronForm(t, h, 'n', "New Scheduled Job")

		// Field 1: Name.
		if err := h.Driver.SendString("Bad Schedule Test"); err != nil {
			t.Fatal(err)
		}
		advanceCronFormField(t, h)

		// Field 2: Repo — advance.
		advanceCronFormField(t, h)

		// Field 3: Model — free-text; leave blank and advance with Enter.
		advanceCronFormField(t, h)

		// Field 4: Prompt — type and advance with Tab.
		if err := h.Driver.SendString("Some prompt."); err != nil {
			t.Fatal(err)
		}
		advanceCronFormPrompt(t, h)

		// Field 4: Schedule — type an invalid expression; huh rejects the advance.
		if err := h.Driver.SendString("not-a-valid-cron"); err != nil {
			t.Fatal(err)
		}
		rejectCronFormField(t, h, h.Driver.SendEnter, `parse cron schedule "not-a-valid-cron"`)

		// No CreateCronJob call should have been made.
		if n := h.Daemon.CreateCronJobCallCount(); n != 0 {
			t.Fatalf("CreateCronJob called %d times with invalid input, want 0", n)
		}
	})

	t.Run("bad_timezone", func(t *testing.T) {
		h := tuitest.NewMock(t,
			tuitest.WithRepos(testRepos()...),
		)

		navigateToCronList(t, h)

		openCronForm(t, h, 'n', "New Scheduled Job")

		// Field 1: Name.
		if err := h.Driver.SendString("TZ Test Job"); err != nil {
			t.Fatal(err)
		}
		advanceCronFormField(t, h)

		// Field 2: Repo — advance.
		advanceCronFormField(t, h)

		// Field 3: Model — free-text; leave blank and advance with Enter.
		advanceCronFormField(t, h)

		// Field 4: Prompt — type and advance with Tab.
		if err := h.Driver.SendString("Some prompt."); err != nil {
			t.Fatal(err)
		}
		advanceCronFormPrompt(t, h)

		// Field 4: Schedule — valid expression.
		if err := h.Driver.SendString("@daily"); err != nil {
			t.Fatal(err)
		}
		advanceCronFormField(t, h)

		// Field 5: Timezone — type an invalid timezone; huh rejects the advance.
		if err := h.Driver.SendString("Not/A/Zone"); err != nil {
			t.Fatal(err)
		}
		rejectCronFormField(t, h, h.Driver.SendEnter, `load timezone "Not/A/Zone"`)

		// No CreateCronJob call should have been made.
		if n := h.Daemon.CreateCronJobCallCount(); n != 0 {
			t.Fatalf("CreateCronJob called %d times with invalid timezone, want 0", n)
		}
	})

	t.Run("empty_required_field", func(t *testing.T) {
		h := tuitest.NewMock(t,
			tuitest.WithRepos(testRepos()...),
		)

		navigateToCronList(t, h)

		openCronForm(t, h, 'n', "New Scheduled Job")

		// Field 1: Name.
		if err := h.Driver.SendString("Empty Prompt Test"); err != nil {
			t.Fatal(err)
		}
		advanceCronFormField(t, h)

		// Field 2: Repo — advance.
		advanceCronFormField(t, h)

		// Field 3: Model — free-text; leave blank and advance with Enter.
		advanceCronFormField(t, h)

		// Field 4: Prompt — leave empty; huh rejects the Tab advance.
		rejectCronFormField(t, h, func() error { return h.Driver.SendKey('\t') }, "prompt is required")

		if n := h.Daemon.CreateCronJobCallCount(); n != 0 {
			t.Fatalf("CreateCronJob called %d times with empty prompt, want 0", n)
		}
	})
}

// TestCron_CreateRoundtrip verifies that a valid form submission results in a
// CreateCronJob RPC call and the new job appears in the list.
func TestCron_CreateRoundtrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow TUI test in -short; run make test-boss for coverage")
	}
	h := tuitest.NewMock(t,
		tuitest.WithRepos(testRepos()...),
	)

	navigateToCronList(t, h)

	openCronForm(t, h, 'n', "New Scheduled Job")

	// Field 1: Name.
	if err := h.Driver.SendString("Roundtrip Job"); err != nil {
		t.Fatal(err)
	}
	advanceCronFormField(t, h)

	// Field 2: Repo — advance with Enter.
	advanceCronFormField(t, h)

	// Field 3: Model — free-text; leave blank and advance with Enter.
	advanceCronFormField(t, h)

	// Field 4: Prompt — type and advance with Tab.
	if err := h.Driver.SendString("Run the roundtrip prompt."); err != nil {
		t.Fatal(err)
	}
	advanceCronFormPrompt(t, h)

	// Field 4: Schedule — use a numeric expression. The '@' aliases are valid,
	// but the leading character can race with the text-area focus transition in
	// PTY tests and land in the previous prompt field.
	if err := h.Driver.SendString("0 9 * * 1-5"); err != nil {
		t.Fatal(err)
	}
	advanceCronFormField(t, h)

	// Field 5: Timezone — leave empty, advance.
	advanceCronFormField(t, h)

	// Field 6: Gate command — leave empty, advance.
	advanceCronFormField(t, h)

	// Field 7: Concurrency — keep default (Skip), advance.
	advanceCronFormField(t, h)

	// Field 8: Run setup command — keep default (Yes), advance.
	advanceCronFormField(t, h)

	// Field 9: Zero output — keep default (No), advance.
	advanceCronFormField(t, h)

	// Field 10: Enabled toggle — already "Yes"; advance.
	advanceCronFormField(t, h)

	// Field 11: Save confirm — "Add Scheduled Job" is highlighted; submit.
	submitCronForm(t, h, "Add Scheduled Job")

	// After submit, the form completes and we return to the cron list.
	// Wait for the CreateCronJob RPC to be received.
	if err := h.Driver.WaitFor(waitTimeout, func(_ string) bool {
		return h.Daemon.CreateCronJobCallCount() >= 1
	}); err != nil {
		t.Fatalf("CreateCronJob not called after form submit; screen:\n%s", h.Driver.Screen())
	}

	if n := h.Daemon.CreateCronJobCallCount(); n != 1 {
		t.Fatalf("CreateCronJob called %d times, want 1", n)
	}

	// After submit, the form returns to the cron list and re-fetches.
	// The new job's name should appear in the list.
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Roundtrip Job")
	}); err != nil {
		t.Fatalf("new cron job 'Roundtrip Job' did not appear in list; screen:\n%s", h.Driver.Screen())
	}
}

func TestCron_CreateWithSelectedAgentSendsAgentName(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow TUI test in -short; run make test-boss for coverage")
	}
	h := tuitest.NewMock(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithAgents(
			&pb.AgentInfo{Name: "claude"},
			&pb.AgentInfo{Name: "opencode"},
		),
		tuitest.WithTerminalSize(120, 50),
	)

	navigateToCronList(t, h)

	openCronForm(t, h, 'n', "New Scheduled Job")

	if err := h.Driver.SendString("Agent Roundtrip Job"); err != nil {
		t.Fatal(err)
	}
	advanceCronFormField(t, h)

	// Repo — keep default.
	advanceCronFormField(t, h)

	// Agent — default is daemon default; move past claude to opencode and select it.
	waitForFocusedCronField(t, h, "Agent")
	if err := h.Driver.SendString("jj"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(lineWithText(screen, "opencode"), "❯")
	}); err != nil {
		t.Fatalf("opencode never took the Agent cursor; screen:\n%s", h.Driver.Screen())
	}
	advanceCronFormField(t, h)

	// Model — free-text; leave blank and advance with Enter.
	advanceCronFormField(t, h)

	if err := h.Driver.SendString("Run with opencode."); err != nil {
		t.Fatal(err)
	}
	advanceCronFormPrompt(t, h)

	if err := h.Driver.SendString("0 9 1 1 1"); err != nil {
		t.Fatal(err)
	}
	advanceCronFormField(t, h) // Schedule
	advanceCronFormField(t, h) // Timezone
	advanceCronFormField(t, h) // Gate command
	advanceCronFormField(t, h) // Concurrency
	advanceCronFormField(t, h) // Run setup command
	advanceCronFormField(t, h) // Zero output
	advanceCronFormField(t, h) // Enabled toggle
	submitCronForm(t, h, "Add Scheduled Job")

	if err := h.Driver.WaitFor(waitTimeout, func(_ string) bool {
		return h.Daemon.CreateCronJobCallCount() >= 1
	}); err != nil {
		t.Fatalf("CreateCronJob not called after form submit; screen:\n%s", h.Driver.Screen())
	}

	calls := h.Daemon.CreateCronJobCalls()
	if len(calls) != 1 {
		t.Fatalf("CreateCronJob called %d times, want 1", len(calls))
	}
	if got, want := calls[0].AgentName, "opencode"; got != want {
		t.Fatalf("CreateCronJob.AgentName = %q, want %q; screen:\n%s", got, want, h.Driver.Screen())
	}
}

// TestCron_EditRoundtrip verifies that the edit form pre-populates from the
// existing job and calls UpdateCronJob with only the changed field.
func TestCron_EditRoundtrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow TUI test in -short; run make test-boss for coverage")
	}
	h := tuitest.NewMock(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithCronJobs(testCronJob()),
	)

	navigateToCronList(t, h)

	// Wait for the list to be populated (a job is selected).
	waitForCronListPopulated(t, h)

	// Hold the agent list back so the form sits on "Loading…", with the job's
	// name already in its title, for longer than it takes to type. Keys sent
	// before the form loads are dropped, so the edit below only lands if
	// openCronForm waits for the loaded form.
	h.Daemon.SetRPCStallFor("ListAgents", 500*time.Millisecond)
	openCronForm(t, h, 'e', "Edit Scheduled Job")

	// Clear the name field and type a new name.
	// Ctrl+A moves cursor to start, then Ctrl+K kills to end-of-line, clearing
	// the field. Then type the new value.
	if err := h.Driver.SendKey(0x01); err != nil { // ctrl+a — move to start
		t.Fatal(err)
	}
	if err := h.Driver.SendKey(0x0b); err != nil { // ctrl+k — kill to end
		t.Fatal(err)
	}
	if err := h.Driver.SendString("Weekly update"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "┃ > Weekly update"); err != nil {
		t.Fatalf("expected the new name in the Name input; screen:\n%s", h.Driver.Screen())
	}
	advanceCronFormField(t, h)

	// Field 2: Repo — keep as-is; advance.
	advanceCronFormField(t, h)

	// Field 3: Model — keep as-is (blank); advance with Enter.
	advanceCronFormField(t, h)

	// Field 4: Prompt — keep as-is; advance with Tab.
	advanceCronFormPrompt(t, h)

	// Field 4: Schedule — keep as-is; advance.
	advanceCronFormField(t, h)

	// Field 5: Timezone — keep as-is; advance.
	advanceCronFormField(t, h)

	// Field 6: Gate command — keep as-is (blank); advance.
	advanceCronFormField(t, h)

	// Field 7: Concurrency — keep as-is; advance.
	advanceCronFormField(t, h)

	// Field 8: Run setup command — keep as-is; advance.
	advanceCronFormField(t, h)

	// Field 9: Zero output — keep as-is; advance.
	advanceCronFormField(t, h)

	// Field 10: Enabled toggle — keep as-is; advance.
	advanceCronFormField(t, h)

	// Field 11: Save confirm — "Update Scheduled Job" is highlighted; submit.
	submitCronForm(t, h, "Update Scheduled Job")

	// Wait for list to reappear.
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "CRON") || strings.Contains(screen, "[n]ew")
	}); err != nil {
		t.Fatalf("expected cron list after edit; screen:\n%s", h.Driver.Screen())
	}

	calls := h.Daemon.UpdateCronJobCalls()
	if len(calls) != 1 {
		t.Fatalf("UpdateCronJob called %d times, want 1", len(calls))
	}

	// Verify that only the Name field was sent (spec: "only the changed fields").
	req := calls[0]
	if req.Name == nil {
		t.Fatalf("expected Name to be set in UpdateCronJob request, got nil")
	}
	if got, want := *req.Name, "Weekly update"; got != want {
		t.Fatalf("expected req.Name = %q, got %q", want, got)
	}
	if req.Prompt != nil {
		t.Fatalf("expected Prompt to be nil (unchanged) in UpdateCronJob request, got %q", *req.Prompt)
	}
	if req.Schedule != nil {
		t.Fatalf("expected Schedule to be nil (unchanged) in UpdateCronJob request, got %q", *req.Schedule)
	}
	if req.Timezone != nil {
		t.Fatalf("expected Timezone to be nil (unchanged) in UpdateCronJob request, got %q", *req.Timezone)
	}
	if req.IsEnabled != nil {
		t.Fatalf("expected Enabled to be nil (unchanged) in UpdateCronJob request, got %v", *req.IsEnabled)
	}
}

// lineWithText returns the first screen line containing sub, or "" if none.
func lineWithText(screen, sub string) string {
	for _, line := range strings.Split(screen, "\n") {
		if strings.Contains(line, sub) {
			return line
		}
	}
	return ""
}

// TestCron_EditHighlightsEditedRow verifies BOS-312: after editing and saving a
// non-first cron job, the list returns with the caret/chevron on that same job's
// row (identified by name), not the first row. Two jobs are seeded; the second
// is edited. ListCronJobs sorts by id, so cron-1 renders first and cron-2 second.
func TestCron_EditHighlightsEditedRow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow TUI test in -short; run make test-boss for coverage")
	}
	job1 := &pb.CronJob{
		Id:        "cron-1",
		RepoId:    "repo-1",
		Name:      "Alpha job",
		Prompt:    "Run alpha.",
		Schedule:  "0 9 * * 1-5",
		IsEnabled: true,
		CreatedAt: timestamppb.Now(),
		UpdatedAt: timestamppb.Now(),
	}
	job2 := &pb.CronJob{
		Id:        "cron-2",
		RepoId:    "repo-1",
		Name:      "Bravo job",
		Prompt:    "Run bravo.",
		Schedule:  "0 9 * * 1-5",
		IsEnabled: true,
		CreatedAt: timestamppb.Now(),
		UpdatedAt: timestamppb.Now(),
	}
	h := tuitest.NewMock(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithCronJobs(job1, job2),
	)

	navigateToCronList(t, h)

	// Wait for both seeded rows to render.
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Alpha job") && strings.Contains(screen, "Bravo job")
	}); err != nil {
		t.Fatalf("expected both cron rows; screen:\n%s", h.Driver.Screen())
	}

	// Move the cursor down to the second row (Bravo job), then edit it.
	if err := h.Driver.SendKey('j'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		// The chevron must be on the Bravo row before we open the edit form.
		return strings.Contains(lineWithText(screen, "Bravo job"), "❯")
	}); err != nil {
		t.Fatalf("expected caret on Bravo row before edit; screen:\n%s", h.Driver.Screen())
	}

	openCronForm(t, h, 'e', "Edit Scheduled Job")
	// The focused Name input is pre-populated with the second job's name.
	if err := h.Driver.WaitForText(waitTimeout, "┃ > Bravo job"); err != nil {
		t.Fatalf("expected pre-populated Bravo name; screen:\n%s", h.Driver.Screen())
	}

	// Change the name so the edited row is unambiguous, then advance through
	// every field and save. Field order mirrors TestCron_EditRoundtrip.
	if err := h.Driver.SendKey(0x01); err != nil { // ctrl+a — move to start
		t.Fatal(err)
	}
	if err := h.Driver.SendKey(0x0b); err != nil { // ctrl+k — kill to end
		t.Fatal(err)
	}
	if err := h.Driver.SendString("Bravo edited"); err != nil {
		t.Fatal(err)
	}
	advanceCronFormField(t, h)  // Name
	advanceCronFormField(t, h)  // Repo
	advanceCronFormField(t, h)  // Model
	advanceCronFormPrompt(t, h) // Prompt (text area — Tab)
	advanceCronFormField(t, h)  // Schedule
	advanceCronFormField(t, h)  // Timezone
	advanceCronFormField(t, h)  // Gate command
	advanceCronFormField(t, h)  // Concurrency
	advanceCronFormField(t, h)  // Run setup command
	advanceCronFormField(t, h)  // Zero output
	advanceCronFormField(t, h)  // Enabled toggle
	submitCronForm(t, h, "Update Scheduled Job")

	// Wait for the list to reappear with the renamed row.
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Bravo edited") &&
			(strings.Contains(screen, "CRON") || strings.Contains(screen, "[n]ew"))
	}); err != nil {
		t.Fatalf("expected cron list with renamed row after edit; screen:\n%s", h.Driver.Screen())
	}

	// The caret must be on the edited (Bravo) row, not the first (Alpha) row.
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(lineWithText(screen, "Bravo edited"), "❯")
	}); err != nil {
		t.Fatalf("expected caret on edited Bravo row; screen:\n%s", h.Driver.Screen())
	}
	screen := h.Driver.Screen()
	if strings.Contains(lineWithText(screen, "Alpha job"), "❯") {
		t.Fatalf("caret should not be on the Alpha (first) row; screen:\n%s", screen)
	}
}

// TestCron_ToggleEnabled verifies that pressing space toggles the enabled
// field and calls UpdateCronJob once per press.
func TestCron_ToggleEnabled(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow TUI test in -short; run make test-boss for coverage")
	}
	h := tuitest.NewMock(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithCronJobs(testCronJob()), // enabled = true
	)

	navigateToCronList(t, h)

	// Wait for the list to have a selected row (action bar shows [d]elete).
	waitForCronListPopulated(t, h)

	// Press space to toggle enabled → UpdateCronJob should be called.
	if err := h.Driver.SendKey(' '); err != nil {
		t.Fatal(err)
	}

	// Wait for the RPC to be received.
	if err := h.Driver.WaitFor(waitTimeout, func(_ string) bool {
		return len(h.Daemon.UpdateCronJobCalls()) >= 1
	}); err != nil {
		t.Fatalf("UpdateCronJob not called after first toggle; screen:\n%s", h.Driver.Screen())
	}

	calls := h.Daemon.UpdateCronJobCalls()
	if len(calls) != 1 {
		t.Fatalf("UpdateCronJob called %d times after first toggle, want 1", len(calls))
	}
	// Verify the enabled field was toggled to false.
	if calls[0].IsEnabled == nil || *calls[0].IsEnabled != false {
		t.Fatalf("expected Enabled=false in first UpdateCronJob call, got %v", calls[0].IsEnabled)
	}

	// Wait for the view to process the response and re-render the row with
	// Enabled=false. Without this the second space-press would compute
	// newEnabled = !true = false again from stale local state, and the
	// concurrent in-flight marshal of response 1 would race with handler 2's
	// in-place mutation of the shared job pointer.
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Daily update") && !strings.Contains(screen, "yes")
	}); err != nil {
		t.Fatalf("ENABLED column did not flip to 'no' after first toggle; screen:\n%s", h.Driver.Screen())
	}

	// Press space again to re-enable.
	if err := h.Driver.SendKey(' '); err != nil {
		t.Fatal(err)
	}

	// Wait for the second RPC.
	if err := h.Driver.WaitFor(waitTimeout, func(_ string) bool {
		return len(h.Daemon.UpdateCronJobCalls()) >= 2
	}); err != nil {
		t.Fatalf("UpdateCronJob not called after second toggle; screen:\n%s", h.Driver.Screen())
	}

	calls = h.Daemon.UpdateCronJobCalls()
	if len(calls) != 2 {
		t.Fatalf("UpdateCronJob called %d times after second toggle, want 2", len(calls))
	}
	// Verify the enabled field was toggled back to true.
	if calls[1].IsEnabled == nil || *calls[1].IsEnabled != true {
		t.Fatalf("expected Enabled=true in second UpdateCronJob call, got %v", calls[1].IsEnabled)
	}
}

// TestCron_RunNow verifies that pressing 'r' fires RunCronJobNow and shows a
// toast. It also tests the skip-reason path.
func TestCron_RunNow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow TUI test in -short; run make test-boss for coverage")
	}
	t.Run("alwaysRun", func(t *testing.T) {
		h := tuitest.NewMock(t,
			tuitest.WithRepos(testRepos()...),
			tuitest.WithCronJobs(testCronJob()),
		)

		navigateToCronList(t, h)
		waitForCronListPopulated(t, h)

		if err := h.Driver.SendKey('r'); err != nil {
			t.Fatal(err)
		}

		// Wait for RunCronJobNow to be called.
		if err := h.Driver.WaitFor(waitTimeout, func(_ string) bool {
			return h.Daemon.RunCronJobNowCallCount() >= 1
		}); err != nil {
			t.Fatalf("RunCronJobNow not called; screen:\n%s", h.Driver.Screen())
		}

		// Toast should appear with "Firing" text (the job name may not be visible
		// in the table row, but the toast is a separate UI element).
		if err := h.Driver.WaitForText(waitTimeout, "Firing"); err != nil {
			t.Fatalf("expected Firing toast; screen:\n%s", h.Driver.Screen())
		}

		if n := h.Daemon.RunCronJobNowCallCount(); n != 1 {
			t.Fatalf("RunCronJobNow called %d times, want 1", n)
		}
	})

	t.Run("alwaysSkip", func(t *testing.T) {
		h := tuitest.NewMock(t,
			tuitest.WithRepos(testRepos()...),
			tuitest.WithCronJobs(testCronJob()),
		)
		h.Daemon.SetRunCronJobNowMode("alwaysSkip", "overlap")

		navigateToCronList(t, h)
		waitForCronListPopulated(t, h)

		if err := h.Driver.SendKey('r'); err != nil {
			t.Fatal(err)
		}

		// Toast should contain the skip reason.
		if err := h.Driver.WaitForText(waitTimeout, "overlap"); err != nil {
			t.Fatalf("expected skip reason 'overlap' in toast; screen:\n%s", h.Driver.Screen())
		}

		if n := h.Daemon.RunCronJobNowCallCount(); n != 1 {
			t.Fatalf("RunCronJobNow called %d times, want 1", n)
		}
	})
}

// cronJobWithStatus returns a seeded cron job with a specific LastRunStatus.
// Each test gets its own ID so jobs can coexist across subtests if needed.
func cronJobWithStatus(id, name string, status pb.CronJobStatus) *pb.CronJob {
	return &pb.CronJob{
		Id:            id,
		RepoId:        "repo-1",
		Name:          name,
		Prompt:        "Run something.",
		Schedule:      "0 9 * * 1-5",
		IsEnabled:     true,
		LastRunStatus: status,
		CreatedAt:     timestamppb.Now(),
		UpdatedAt:     timestamppb.Now(),
	}
}

// TestCron_StatusColumn verifies the STATUS column renders the right text for
// each derived state: Running (server-side), Failed, Idle, and the local
// m.running bridge that fires immediately on 'r' press.
func TestCron_StatusColumn(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow TUI test in -short; run make test-boss for coverage")
	}
	t.Run("server_running", func(t *testing.T) {
		h := tuitest.NewMock(t,
			tuitest.WithRepos(testRepos()...),
			tuitest.WithCronJobs(cronJobWithStatus(
				"cron-running", "Server Running Job",
				pb.CronJobStatus_CRON_JOB_STATUS_RUNNING,
			)),
		)

		navigateToCronList(t, h)
		waitForCronListPopulated(t, h)

		// STATUS column header should be present.
		if err := h.Driver.WaitForText(waitTimeout, "STATUS"); err != nil {
			t.Fatalf("expected STATUS column header; screen:\n%s", h.Driver.Screen())
		}

		// Server reports RUNNING — row should render "Running" without any
		// key press.
		if err := h.Driver.WaitForText(waitTimeout, "Running"); err != nil {
			t.Fatalf("expected 'Running' status from server-derived state; screen:\n%s", h.Driver.Screen())
		}
	})

	t.Run("server_failed", func(t *testing.T) {
		h := tuitest.NewMock(t,
			tuitest.WithRepos(testRepos()...),
			tuitest.WithCronJobs(cronJobWithStatus(
				"cron-failed", "Failed Job",
				pb.CronJobStatus_CRON_JOB_STATUS_FAILED,
			)),
		)

		navigateToCronList(t, h)
		waitForCronListPopulated(t, h)

		if err := h.Driver.WaitForText(waitTimeout, "STATUS"); err != nil {
			t.Fatalf("expected STATUS column header; screen:\n%s", h.Driver.Screen())
		}

		if err := h.Driver.WaitForText(waitTimeout, "failed"); err != nil {
			t.Fatalf("expected 'failed' status; screen:\n%s", h.Driver.Screen())
		}
	})

	t.Run("server_idle", func(t *testing.T) {
		h := tuitest.NewMock(t,
			tuitest.WithRepos(testRepos()...),
			tuitest.WithCronJobs(cronJobWithStatus(
				"cron-idle", "Idle Job",
				pb.CronJobStatus_CRON_JOB_STATUS_IDLE,
			)),
		)

		navigateToCronList(t, h)
		waitForCronListPopulated(t, h)

		if err := h.Driver.WaitForText(waitTimeout, "STATUS"); err != nil {
			t.Fatalf("expected STATUS column header; screen:\n%s", h.Driver.Screen())
		}

		if err := h.Driver.WaitForText(waitTimeout, "idle"); err != nil {
			t.Fatalf("expected 'idle' status; screen:\n%s", h.Driver.Screen())
		}
	})

	t.Run("server_unspecified_renders_idle", func(t *testing.T) {
		// A never-run job has UNSPECIFIED — the default switch arm should
		// still render "idle".
		h := tuitest.NewMock(t,
			tuitest.WithRepos(testRepos()...),
			tuitest.WithCronJobs(cronJobWithStatus(
				"cron-unspec", "Unspecified Job",
				pb.CronJobStatus_CRON_JOB_STATUS_UNSPECIFIED,
			)),
		)

		navigateToCronList(t, h)
		waitForCronListPopulated(t, h)

		if err := h.Driver.WaitForText(waitTimeout, "idle"); err != nil {
			t.Fatalf("expected 'idle' status for unspecified; screen:\n%s", h.Driver.Screen())
		}
	})

	// The local m.running bridge (set on 'r' press, cleared on cronRunNowMsg)
	// is implicitly exercised by TestCron_RunNow. Asserting on the rendered
	// "Running" text directly is racy here because the mock RPC returns
	// near-instantly: the bridge is cleared before the 50ms screen poller
	// observes the intermediate frame. The server-side RUNNING path above
	// covers the steady-state "Running" rendering.
}

// TestCron_DeleteConfirm verifies the delete overlay behaviour.
func TestCron_DeleteConfirm(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow TUI test in -short; run make test-boss for coverage")
	}
	t.Run("cancel_with_n", func(t *testing.T) {
		h := tuitest.NewMock(t,
			tuitest.WithRepos(testRepos()...),
			tuitest.WithCronJobs(testCronJob()),
		)

		navigateToCronList(t, h)
		waitForCronListPopulated(t, h)

		// Press 'd' — should open confirm overlay.
		if err := h.Driver.SendKey('d'); err != nil {
			t.Fatal(err)
		}
		if err := h.Driver.WaitForText(waitTimeout, "[y/enter] confirm"); err != nil {
			t.Fatalf("expected confirm overlay; screen:\n%s", h.Driver.Screen())
		}
		if err := h.Driver.WaitForText(waitTimeout, `Delete "Daily update"?`); err != nil {
			t.Fatalf("expected confirm prompt to include job name; screen:\n%s", h.Driver.Screen())
		}

		// Press 'n' — should cancel without deleting.
		if err := h.Driver.SendKey('n'); err != nil {
			t.Fatal(err)
		}

		// Overlay should disappear; action bar should revert to normal.
		if err := h.Driver.WaitForText(waitTimeout, "[d]elete"); err != nil {
			t.Fatalf("expected normal action bar after cancel; screen:\n%s", h.Driver.Screen())
		}

		if n := h.Daemon.DeleteCronJobCallCount(); n != 0 {
			t.Fatalf("DeleteCronJob called %d times after cancel, want 0", n)
		}
		// Daemon should still have the job.
		if jobs := h.Daemon.CronJobs(); len(jobs) != 1 {
			t.Fatalf("expected 1 job in daemon after cancel, got %d", len(jobs))
		}
	})

	t.Run("cancel_with_esc", func(t *testing.T) {
		h := tuitest.NewMock(t,
			tuitest.WithRepos(testRepos()...),
			tuitest.WithCronJobs(testCronJob()),
		)

		navigateToCronList(t, h)
		waitForCronListPopulated(t, h)

		if err := h.Driver.SendKey('d'); err != nil {
			t.Fatal(err)
		}
		if err := h.Driver.WaitForText(waitTimeout, "[y/enter] confirm"); err != nil {
			t.Fatalf("expected confirm overlay; screen:\n%s", h.Driver.Screen())
		}

		if err := h.Driver.SendEscape(); err != nil {
			t.Fatal(err)
		}

		// Overlay should disappear; action bar should revert to normal.
		if err := h.Driver.WaitForText(waitTimeout, "[d]elete"); err != nil {
			t.Fatalf("expected normal action bar after esc; screen:\n%s", h.Driver.Screen())
		}

		if n := h.Daemon.DeleteCronJobCallCount(); n != 0 {
			t.Fatalf("DeleteCronJob called %d times after esc, want 0", n)
		}
		if jobs := h.Daemon.CronJobs(); len(jobs) != 1 {
			t.Fatalf("expected 1 job in daemon after esc, got %d", len(jobs))
		}
	})

	t.Run("confirm_with_y", func(t *testing.T) {
		h := tuitest.NewMock(t,
			tuitest.WithRepos(testRepos()...),
			tuitest.WithCronJobs(testCronJob()),
		)

		navigateToCronList(t, h)
		waitForCronListPopulated(t, h)

		if err := h.Driver.SendKey('d'); err != nil {
			t.Fatal(err)
		}
		if err := h.Driver.WaitForText(waitTimeout, "[y/enter] confirm"); err != nil {
			t.Fatalf("expected confirm overlay; screen:\n%s", h.Driver.Screen())
		}

		// Confirm with 'y'.
		if err := h.Driver.SendKey('y'); err != nil {
			t.Fatal(err)
		}

		// Wait for the DeleteCronJob RPC to be received.
		if err := h.Driver.WaitFor(waitTimeout, func(_ string) bool {
			return h.Daemon.DeleteCronJobCallCount() == 1
		}); err != nil {
			t.Fatalf("DeleteCronJob was never called; screen:\n%s", h.Driver.Screen())
		}

		// Wait for the daemon store to be empty (list re-fetches in background).
		if err := h.Driver.WaitFor(waitTimeout, func(_ string) bool {
			return len(h.Daemon.CronJobs()) == 0
		}); err != nil {
			t.Fatalf("daemon still has cron jobs after delete; screen:\n%s", h.Driver.Screen())
		}
	})

	t.Run("confirm_with_enter", func(t *testing.T) {
		h := tuitest.NewMock(t,
			tuitest.WithRepos(testRepos()...),
			tuitest.WithCronJobs(testCronJob()),
		)

		navigateToCronList(t, h)
		waitForCronListPopulated(t, h)

		if err := h.Driver.SendKey('d'); err != nil {
			t.Fatal(err)
		}
		if err := h.Driver.WaitForText(waitTimeout, "[y/enter] confirm"); err != nil {
			t.Fatalf("expected confirm overlay; screen:\n%s", h.Driver.Screen())
		}

		// Confirm with Enter.
		if err := h.Driver.SendEnter(); err != nil {
			t.Fatal(err)
		}

		// Wait for the DeleteCronJob RPC to be received.
		if err := h.Driver.WaitFor(waitTimeout, func(_ string) bool {
			return h.Daemon.DeleteCronJobCallCount() == 1
		}); err != nil {
			t.Fatalf("DeleteCronJob was never called after Enter confirm; screen:\n%s", h.Driver.Screen())
		}

		// Wait for the daemon store to be empty.
		if err := h.Driver.WaitFor(waitTimeout, func(_ string) bool {
			return len(h.Daemon.CronJobs()) == 0
		}); err != nil {
			t.Fatalf("daemon still has cron jobs after Enter confirm; screen:\n%s", h.Driver.Screen())
		}
	})
}

// The CLI and the open TUI drive one daemon: a job added from the terminal
// shows up in the Scheduled Jobs list, and what the TUI then toggles and adds
// is what `boss cron ls` reports back.
func TestJourney_ScheduledJobs_AC1_AC2_AC3_CLIAndTUIShareJobs(t *testing.T) {
	h := tuitest.NewMock(t, tuitest.WithRepos(testRepos()...))
	cli := newJourneyCLI(t, h)

	t.Log("open the Scheduled Jobs list")
	waitText(t, h, "[s]ettings")
	sendKeys(t, h, 's')
	waitText(t, h, "Cron jobs")
	sendKeys(t, h, 'c')
	waitText(t, h, "No cron jobs")

	if !t.Run("AC1 a job added with boss cron add is listed by the CLI and the open TUI", func(t *testing.T) {
		cli.run(t, "cron", "add", "--repo", "repo-1", "--name", "nightly report",
			"--schedule", "0 5 * * *", "--prompt", "write the nightly report")
		calls := h.Daemon.CreateCronJobCalls()
		if len(calls) != 1 || calls[0].GetRepoId() != "repo-1" || calls[0].GetName() != "nightly report" ||
			calls[0].GetSchedule() != "0 5 * * *" || !calls[0].GetIsEnabled() {
			t.Fatalf("CreateCronJob calls = %v", calls)
		}
		waitEnabled(t, h, "nightly report", "yes")
		if job := cli.job(t, "nightly report"); !job.Enabled || job.Schedule != "0 5 * * *" {
			t.Fatalf("boss cron ls lists %+v", job)
		}
	}) {
		return
	}

	if !t.Run("AC2 space in the TUI list disables the job for the CLI too", func(t *testing.T) {
		sendKeys(t, h, ' ')
		calls := waitCalls(t, h, h.Daemon.UpdateCronJobCalls, 1)
		if calls[0].GetId() != "cron-1" || calls[0].IsEnabled == nil || calls[0].GetIsEnabled() {
			t.Fatalf("UpdateCronJob request = %v, want cron-1 disabled", calls[0])
		}
		waitEnabled(t, h, "nightly report", "no")
		if job := cli.job(t, "nightly report"); job.Enabled {
			t.Fatalf("boss cron ls still lists the job enabled: %+v", job)
		}
	}) {
		return
	}

	t.Run("AC3 the New Scheduled Job form adds a job the CLI lists", func(t *testing.T) {
		sendKeys(t, h, 'n')
		waitText(t, h, "New Scheduled Job")
		waitText(t, h, "Name")
		fill := func(text string, advance byte, settle time.Duration) {
			t.Helper()
			if text != "" {
				if err := h.Driver.SendString(text); err != nil {
					t.Fatal(err)
				}
			}
			sendKeys(t, h, advance)
			time.Sleep(settle)
		}
		const enter, tab = '\r', '\t'
		fill("tui weekly", enter, 150*time.Millisecond)
		fill("", enter, 150*time.Millisecond) // repo
		fill("", enter, 150*time.Millisecond) // model
		fill("summarise the week", tab, 500*time.Millisecond)
		fill("0 9 * * 1", enter, 150*time.Millisecond)
		// timezone, gate, concurrency, run setup, zero output, enabled, save
		for range 7 {
			fill("", enter, 150*time.Millisecond)
		}
		calls := waitCalls(t, h, h.Daemon.CreateCronJobCalls, 2)
		if c := calls[1]; c.GetRepoId() != "repo-1" || c.GetName() != "tui weekly" ||
			c.GetSchedule() != "0 9 * * 1" || c.GetPrompt() != "summarise the week" || !c.GetIsEnabled() {
			t.Fatalf("CreateCronJob request from the form = %v", c)
		}
		waitEnabled(t, h, "tui weekly", "yes")
		waitEnabled(t, h, "nightly report", "no")
		if job := cli.job(t, "tui weekly"); !job.Enabled || job.Schedule != "0 9 * * 1" {
			t.Fatalf("boss cron ls lists %+v", job)
		}
		if n := len(h.Daemon.UpdateCronJobCalls()); n != 1 {
			t.Fatalf("UpdateCronJob calls = %d, want only the toggle", n)
		}
	})
}

// waitEnabled waits for the job row named name to show want ("yes" or "no")
// as a whole cell.
func waitEnabled(t *testing.T, h *tuitest.Harness, name, want string) {
	t.Helper()
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return slices.Contains(strings.Fields(screenRow(screen, name)), want)
	}); err != nil {
		t.Fatalf("job %q never showed enabled=%s: %v\n%s", name, want, err, h.Driver.Screen())
	}
}

// journeyCLI runs the boss binary the TUI runs against the same mock daemon.
type journeyCLI struct {
	bin string
	env []string
}

func newJourneyCLI(t *testing.T, h *tuitest.Harness) journeyCLI {
	t.Helper()
	bin, err := tuitest.BossBinaryPath()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	env := append(tuitest.BaseHarnessEnv(os.Environ()),
		"BOSS_SOCKET="+h.Daemon.SocketPath(),
		"BOSS_SKIP_SKILLS=1",
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"BOSS_SETTINGS_PATH="+filepath.Join(home, "settings.json"),
		"BOSS_REFUSE_DEFAULT_SETTINGS=1",
	)
	return journeyCLI{bin: bin, env: env}
}

func (c journeyCLI) run(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.bin, args...) // #nosec G204 -- test runs the compiled boss binary with fixed args
	cmd.Env = c.env
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("boss %s: %v\nstdout: %s\nstderr: %s", strings.Join(args, " "), err, out, stderr.String())
	}
	return string(out)
}

type journeyCronJob struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Schedule string `json:"schedule"`
	Enabled  bool   `json:"enabled"`
}

// job returns the job named name from `boss cron ls --json`.
func (c journeyCLI) job(t *testing.T, name string) journeyCronJob {
	t.Helper()
	out := c.run(t, "cron", "ls", "--json")
	var jobs []journeyCronJob
	if err := json.Unmarshal([]byte(out), &jobs); err != nil {
		t.Fatalf("boss cron ls --json: %v\n%s", err, out)
	}
	for _, j := range jobs {
		if j.Name == name {
			return j
		}
	}
	t.Fatalf("boss cron ls --json has no %q:\n%s", name, out)
	return journeyCronJob{}
}
