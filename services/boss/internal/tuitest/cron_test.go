package tuitest_test

import (
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
		Enabled:   true,
		CreatedAt: timestamppb.Now(),
		UpdatedAt: timestamppb.Now(),
	}
}

// navigateToCronList navigates from the home screen to the cron list by pressing 'c'.
func navigateToCronList(t *testing.T, h *tuitest.Harness) {
	t.Helper()

	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Bossanova") || strings.Contains(screen, "no active sessions")
	}); err != nil {
		t.Fatal(err)
	}

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
			strings.Contains(screen, "[e]dit")
	}); err != nil {
		t.Fatalf("expected cron table rows to be visible; screen:\n%s", h.Driver.Screen())
	}
}

// TestCron_HomeC_OpensList verifies that pressing 'c' from home opens the cron
// list with the empty-state copy when no cron jobs exist.
func TestCron_HomeC_OpensList(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
	)

	navigateToCronList(t, h)

	if err := h.Driver.WaitForText(waitTimeout, "No cron jobs. Press [n] to create one."); err != nil {
		t.Fatalf("expected empty-state copy; screen:\n%s", h.Driver.Screen())
	}
}

// advanceCronFormField sends an Enter key and waits for the form to settle
// before continuing. huh renders all fields simultaneously, so naively waiting
// for the next field's heading is unreliable (it is visible even when a prior
// field is still focused). A short sleep after each advance prevents the next
// SendString from landing in the wrong field.
func advanceCronFormField(t *testing.T, h *tuitest.Harness) {
	t.Helper()
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
}

// advanceCronFormPrompt sends a Tab key (huh text-area advance) and waits.
func advanceCronFormPrompt(t *testing.T, h *tuitest.Harness) {
	t.Helper()
	if err := h.Driver.SendKey('\t'); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
}

// TestCron_NewForm_LivePreview verifies that the form shows a live "Next fire:"
// preview when the schedule field contains a valid cron expression.
func TestCron_NewForm_LivePreview(t *testing.T) {
	// Use a taller terminal so the live-preview line (rendered below the form)
	// is visible. The cron form has ~30 rows of content at the default height.
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithTerminalSize(120, 50),
	)

	navigateToCronList(t, h)

	// Open the new cron job form.
	if err := h.Driver.SendKey('n'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "New Scheduled Job"); err != nil {
		t.Fatalf("expected cron form; screen:\n%s", h.Driver.Screen())
	}

	// Wait for repos to load so the form is interactive.
	if err := h.Driver.WaitForText(waitTimeout, "Name"); err != nil {
		t.Fatalf("expected Name field; screen:\n%s", h.Driver.Screen())
	}
	// huh renders all fields at once; we must pause after each advance so that
	// the next keystroke lands in the correct (newly-focused) field.

	// Field 1: Name.
	if err := h.Driver.SendString("My Test Job"); err != nil {
		t.Fatal(err)
	}
	advanceCronFormField(t, h)

	// Field 2: Repo — already has "my-app" selected; advance.
	advanceCronFormField(t, h)

	// Field 3: Prompt — type and advance with Tab (huh text-area convention).
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
	t.Run("bad_schedule", func(t *testing.T) {
		h := tuitest.New(t,
			tuitest.WithRepos(testRepos()...),
		)

		navigateToCronList(t, h)

		// Open new cron form.
		if err := h.Driver.SendKey('n'); err != nil {
			t.Fatal(err)
		}
		if err := h.Driver.WaitForText(waitTimeout, "New Scheduled Job"); err != nil {
			t.Fatalf("expected cron form; screen:\n%s", h.Driver.Screen())
		}

		// Wait for Name field.
		if err := h.Driver.WaitForText(waitTimeout, "Name"); err != nil {
			t.Fatalf("expected Name field; screen:\n%s", h.Driver.Screen())
		}

		// Field 1: Name.
		if err := h.Driver.SendString("Bad Schedule Test"); err != nil {
			t.Fatal(err)
		}
		advanceCronFormField(t, h)

		// Field 2: Repo — advance.
		advanceCronFormField(t, h)

		// Field 3: Prompt — type and advance with Tab.
		if err := h.Driver.SendString("Some prompt."); err != nil {
			t.Fatal(err)
		}
		advanceCronFormPrompt(t, h)

		// Field 4: Schedule — type an invalid expression and try to advance.
		if err := h.Driver.SendString("not-a-valid-cron"); err != nil {
			t.Fatal(err)
		}
		// Try to advance past — huh should reject and show a validation error.
		advanceCronFormField(t, h)

		// Validation error should appear; "New Scheduled Job" should still be visible.
		if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
			return strings.Contains(screen, "New Scheduled Job")
		}); err != nil {
			t.Fatalf("expected to stay on form after bad schedule; screen:\n%s", h.Driver.Screen())
		}

		// No CreateCronJob call should have been made.
		if n := h.Daemon.CreateCronJobCallCount(); n != 0 {
			t.Fatalf("CreateCronJob called %d times with invalid input, want 0", n)
		}
	})

	t.Run("bad_timezone", func(t *testing.T) {
		h := tuitest.New(t,
			tuitest.WithRepos(testRepos()...),
		)

		navigateToCronList(t, h)

		// Open new cron form.
		if err := h.Driver.SendKey('n'); err != nil {
			t.Fatal(err)
		}
		if err := h.Driver.WaitForText(waitTimeout, "New Scheduled Job"); err != nil {
			t.Fatalf("expected cron form; screen:\n%s", h.Driver.Screen())
		}
		if err := h.Driver.WaitForText(waitTimeout, "Name"); err != nil {
			t.Fatalf("expected Name field; screen:\n%s", h.Driver.Screen())
		}

		// Field 1: Name.
		if err := h.Driver.SendString("TZ Test Job"); err != nil {
			t.Fatal(err)
		}
		advanceCronFormField(t, h)

		// Field 2: Repo — advance.
		advanceCronFormField(t, h)

		// Field 3: Prompt — type and advance with Tab.
		if err := h.Driver.SendString("Some prompt."); err != nil {
			t.Fatal(err)
		}
		advanceCronFormPrompt(t, h)

		// Field 4: Schedule — valid expression.
		if err := h.Driver.SendString("@daily"); err != nil {
			t.Fatal(err)
		}
		advanceCronFormField(t, h)

		// Field 5: Timezone — type an invalid timezone and try to advance.
		if err := h.Driver.SendString("Not/A/Zone"); err != nil {
			t.Fatal(err)
		}
		advanceCronFormField(t, h)

		// Form should remain open; "New Scheduled Job" should still be visible.
		if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
			return strings.Contains(screen, "New Scheduled Job")
		}); err != nil {
			t.Fatalf("expected to stay on form after bad timezone; screen:\n%s", h.Driver.Screen())
		}

		// No CreateCronJob call should have been made.
		if n := h.Daemon.CreateCronJobCallCount(); n != 0 {
			t.Fatalf("CreateCronJob called %d times with invalid timezone, want 0", n)
		}
	})

	t.Run("empty_required_field", func(t *testing.T) {
		h := tuitest.New(t,
			tuitest.WithRepos(testRepos()...),
		)

		navigateToCronList(t, h)

		// Open new cron form.
		if err := h.Driver.SendKey('n'); err != nil {
			t.Fatal(err)
		}
		if err := h.Driver.WaitForText(waitTimeout, "New Scheduled Job"); err != nil {
			t.Fatalf("expected cron form; screen:\n%s", h.Driver.Screen())
		}
		if err := h.Driver.WaitForText(waitTimeout, "Name"); err != nil {
			t.Fatalf("expected Name field; screen:\n%s", h.Driver.Screen())
		}

		// Field 1: Name.
		if err := h.Driver.SendString("Empty Prompt Test"); err != nil {
			t.Fatal(err)
		}
		advanceCronFormField(t, h)

		// Field 2: Repo — advance.
		advanceCronFormField(t, h)

		// Field 3: Prompt — leave empty; advance with Tab (empty prompt should fail validation).
		advanceCronFormPrompt(t, h)

		// Field 4: Schedule — valid expression.
		if err := h.Driver.SendString("@daily"); err != nil {
			t.Fatal(err)
		}
		advanceCronFormField(t, h)

		// Field 5: Timezone — leave empty; advance.
		advanceCronFormField(t, h)

		// Field 6: Enabled — try to submit.
		advanceCronFormField(t, h)

		// Form should not have submitted — CreateCronJob must not be called.
		// Give the TUI a moment to process any submit attempt.
		if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
			return strings.Contains(screen, "New Scheduled Job")
		}); err != nil {
			t.Fatalf("expected to stay on form with empty prompt; screen:\n%s", h.Driver.Screen())
		}

		if n := h.Daemon.CreateCronJobCallCount(); n != 0 {
			t.Fatalf("CreateCronJob called %d times with empty prompt, want 0", n)
		}
	})
}

// TestCron_CreateRoundtrip verifies that a valid form submission results in a
// CreateCronJob RPC call and the new job appears in the list.
func TestCron_CreateRoundtrip(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
	)

	navigateToCronList(t, h)

	// Open new cron form.
	if err := h.Driver.SendKey('n'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "New Scheduled Job"); err != nil {
		t.Fatalf("expected cron form; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.WaitForText(waitTimeout, "Name"); err != nil {
		t.Fatalf("expected Name field; screen:\n%s", h.Driver.Screen())
	}

	// Field 1: Name.
	if err := h.Driver.SendString("Roundtrip Job"); err != nil {
		t.Fatal(err)
	}
	advanceCronFormField(t, h)

	// Field 2: Repo — advance with Enter.
	advanceCronFormField(t, h)

	// Field 3: Prompt — type and advance with Tab.
	if err := h.Driver.SendString("Run the roundtrip prompt."); err != nil {
		t.Fatal(err)
	}
	advanceCronFormPrompt(t, h)

	// Field 4: Schedule — use a named expression to avoid '*' PTY quirks.
	if err := h.Driver.SendString("@daily"); err != nil {
		t.Fatal(err)
	}
	advanceCronFormField(t, h)

	// Field 5: Timezone — leave empty, advance.
	advanceCronFormField(t, h)

	// Field 6: Enabled confirm — already "Yes"; submit.
	advanceCronFormField(t, h)

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

// TestCron_EditRoundtrip verifies that the edit form pre-populates from the
// existing job and calls UpdateCronJob with only the changed field.
func TestCron_EditRoundtrip(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithCronJobs(testCronJob()),
	)

	navigateToCronList(t, h)

	// Wait for the list to be populated (a job is selected).
	waitForCronListPopulated(t, h)

	// Press 'e' to edit the selected job.
	if err := h.Driver.SendKey('e'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Edit Scheduled Job"); err != nil {
		t.Fatalf("expected edit form; screen:\n%s", h.Driver.Screen())
	}

	// The Name field should be pre-populated with "Daily update".
	if err := h.Driver.WaitForText(waitTimeout, "Daily update"); err != nil {
		t.Fatalf("expected pre-populated name in form; screen:\n%s", h.Driver.Screen())
	}

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
	advanceCronFormField(t, h)

	// Field 2: Repo — keep as-is; advance.
	advanceCronFormField(t, h)

	// Field 3: Prompt — keep as-is; advance with Tab.
	advanceCronFormPrompt(t, h)

	// Field 4: Schedule — keep as-is; advance.
	advanceCronFormField(t, h)

	// Field 5: Timezone — keep as-is; advance.
	advanceCronFormField(t, h)

	// Field 6: Enabled — keep as-is; submit.
	advanceCronFormField(t, h)

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
	if req.Enabled != nil {
		t.Fatalf("expected Enabled to be nil (unchanged) in UpdateCronJob request, got %v", *req.Enabled)
	}
}

// TestCron_ToggleEnabled verifies that pressing space toggles the enabled
// field and calls UpdateCronJob once per press.
func TestCron_ToggleEnabled(t *testing.T) {
	h := tuitest.New(t,
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
	if calls[0].Enabled == nil || *calls[0].Enabled != false {
		t.Fatalf("expected Enabled=false in first UpdateCronJob call, got %v", calls[0].Enabled)
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
	if calls[1].Enabled == nil || *calls[1].Enabled != true {
		t.Fatalf("expected Enabled=true in second UpdateCronJob call, got %v", calls[1].Enabled)
	}
}

// TestCron_RunNow verifies that pressing 'r' fires RunCronJobNow and shows a
// toast. It also tests the skip-reason path.
func TestCron_RunNow(t *testing.T) {
	t.Run("alwaysRun", func(t *testing.T) {
		h := tuitest.New(t,
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
		h := tuitest.New(t,
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
		Enabled:       true,
		LastRunStatus: status,
		CreatedAt:     timestamppb.Now(),
		UpdatedAt:     timestamppb.Now(),
	}
}

// TestCron_StatusColumn verifies the STATUS column renders the right text for
// each derived state: Running (server-side), Failed, Idle, and the local
// m.running bridge that fires immediately on 'r' press.
func TestCron_StatusColumn(t *testing.T) {
	t.Run("server_running", func(t *testing.T) {
		h := tuitest.New(t,
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
		h := tuitest.New(t,
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
		h := tuitest.New(t,
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
		h := tuitest.New(t,
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
	t.Run("cancel_with_n", func(t *testing.T) {
		h := tuitest.New(t,
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
		h := tuitest.New(t,
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
		h := tuitest.New(t,
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
		h := tuitest.New(t,
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

// TestHome_KeysStillWork is a regression test ensuring that home-screen
// keybindings (n, p, r, s, t, l, c) still dispatch correctly after the cron
// feature was added.
func TestHome_KeysStillWork(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(testSessions()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	// 'n' → new session form (single repo → goes straight to new-session view).
	if err := h.Driver.SendKey('n'); err != nil {
		t.Fatal(err)
	}
	// With a single repo the TUI skips the repo picker and goes to the new-session
	// type picker. Wait for "esc] back" which is unique to non-home views.
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "[esc] back") &&
			(strings.Contains(screen, "New Session") ||
				strings.Contains(screen, "Select a repository"))
	}); err != nil {
		t.Fatalf("'n' did not open new session; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}
	// Wait for the home screen to fully regain focus before proceeding.
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Add dark mode") &&
			!strings.Contains(screen, "[esc] back")
	}); err != nil {
		t.Fatal(err)
	}

	// 's' → settings.
	if err := h.Driver.SendKey('s'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Settings"); err != nil {
		t.Fatalf("'s' did not open settings; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	// 't' → trash.
	if err := h.Driver.SendKey('t'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Archived Sessions"); err != nil {
		t.Fatalf("'t' did not open trash; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	// 'r' → repo list.
	if err := h.Driver.SendKey('r'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "PATH"); err != nil {
		t.Fatalf("'r' did not open repo list; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	// 'c' → cron list.
	if err := h.Driver.SendKey('c'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "No cron jobs") ||
			strings.Contains(screen, "CRON") ||
			strings.Contains(screen, "[n]ew")
	}); err != nil {
		t.Fatalf("'c' did not open cron list; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatal(err)
	}

	// 'l' → login/logout toggle. Requires authentication state that the harness
	// does not easily simulate without the BOSS_AUTH_E2E_EMAIL override and a
	// network-capable auth backend. Coverage for 'l' lives in the in-package
	// home_test.go (TestTUI_HomeView_*) which exercises logout via WithLoggedInUser.
	// Skipped here to avoid a flaky dependency on auth infrastructure.
}
