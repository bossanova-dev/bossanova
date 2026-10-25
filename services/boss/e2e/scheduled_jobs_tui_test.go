package e2e_test

import (
	"context"
	"encoding/json"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/adapter/realstackrpc"
	"github.com/recurser/boss/internal/tuitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

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
	// The real daemon exposes its sole stub runner; retain the daemon default.
	if focusedCronField(h.Driver.Screen()) == "Agent" {
		advanceCronForm(t, h, h.Driver.SendEnter)
	}
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

// The CLI and the open TUI drive one daemon: a job added from the terminal
// shows up in the Scheduled Jobs list, and what the TUI then toggles and adds
// is what `boss cron ls` reports back.
func TestJourney_ScheduledJobs_AC1_AC2_AC3_CLIAndTUIShareJobs(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow TUI test in -short; run make test-boss for coverage")
	}
	h := tuitest.New(t, tuitest.WithRepos(testRepos()...))
	cli := newJourneyCLI(t, h)

	t.Log("open the Scheduled Jobs list")
	waitText(t, h, "[s]ettings")
	sendKeys(t, h, 's')
	waitText(t, h, "Cron jobs")
	sendKeys(t, h, 'c')
	waitText(t, h, "No cron jobs")

	if !t.Run("AC1 a job added with boss cron add is listed by the CLI and the open TUI", func(t *testing.T) {
		cli.run(t, "cron", "add", "--repo", h.RepoIDs["repo-1"], "--name", "nightly report",
			"--schedule", "0 5 * * *", "--prompt", "write the nightly report")
		waitStoredCronJobs(t, h, func(jobs []*pb.CronJob) bool {
			return len(jobs) == 1 && jobs[0].Name == "nightly report" && jobs[0].Schedule == "0 5 * * *" && jobs[0].IsEnabled
		})
		waitEnabled(t, h, "nightly report", "yes")
		if job := cli.job(t, "nightly report"); !job.Enabled || job.Schedule != "0 5 * * *" {
			t.Fatalf("boss cron ls lists %+v", job)
		}
	}) {
		return
	}

	if !t.Run("AC2 space in the TUI list disables the job for the CLI too", func(t *testing.T) {
		sendKeys(t, h, ' ')
		waitStoredCronJobs(t, h, func(jobs []*pb.CronJob) bool { return len(jobs) == 1 && !jobs[0].IsEnabled })
		waitEnabled(t, h, "nightly report", "no")
		if job := cli.job(t, "nightly report"); job.Enabled {
			t.Fatalf("boss cron ls still lists the job enabled: %+v", job)
		}
		// Retain the inverse transition from the standalone toggle proof.
		sendKeys(t, h, ' ')
		waitStoredCronJobs(t, h, func(jobs []*pb.CronJob) bool { return len(jobs) == 1 && jobs[0].IsEnabled })
		waitEnabled(t, h, "nightly report", "yes")
		if job := cli.job(t, "nightly report"); !job.Enabled {
			t.Fatalf("boss cron ls still lists the re-enabled job disabled: %+v", job)
		}
	}) {
		return
	}

	t.Run("AC3 the New Scheduled Job form adds a job the CLI lists", func(t *testing.T) {
		openCronForm(t, h, 'n', "New Scheduled Job")
		if err := h.Driver.SendString("tui weekly"); err != nil {
			t.Fatal(err)
		}
		advanceCronFormField(t, h) // Name
		advanceCronFormField(t, h) // Repo
		advanceCronFormField(t, h) // Model
		if err := h.Driver.SendString("summarise the week"); err != nil {
			t.Fatal(err)
		}
		advanceCronFormPrompt(t, h)
		if err := h.Driver.SendString("0 9 * * 1"); err != nil {
			t.Fatal(err)
		}
		for range 7 {
			advanceCronFormField(t, h)
		}
		submitCronForm(t, h, "Add Scheduled Job")
		waitStoredCronJobs(t, h, func(jobs []*pb.CronJob) bool {
			if len(jobs) != 2 {
				return false
			}
			for _, job := range jobs {
				if job.Name == "tui weekly" {
					return job.Prompt == "summarise the week" && job.Schedule == "0 9 * * 1" && job.IsEnabled
				}
			}
			return false
		})
		waitEnabled(t, h, "tui weekly", "yes")
		waitEnabled(t, h, "nightly report", "yes")
		if job := cli.job(t, "tui weekly"); !job.Enabled || job.Schedule != "0 9 * * 1" {
			t.Fatalf("boss cron ls lists %+v", job)
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

// journeyCLI runs the boss binary the TUI runs against the same isolated daemon.
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
	return journeyCLI{bin: bin, env: append(h.Stack.Env(), "BOSS_SOCKET="+h.Stack.SocketPath(), "BOSS_SKIP_SKILLS=1")}
}

func (c journeyCLI) run(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.bin, args...) // #nosec G204 -- test runs the compiled boss binary with fixed args
	cmd.Env = c.env
	cmd.WaitDelay = time.Second
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

func storedCronJobs(t *testing.T, h *tuitest.Harness) []*pb.CronJob {
	t.Helper()
	response, err := h.Stack.Client().ListCronJobs(context.Background(), realstackrpc.Request(&pb.ListCronJobsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	return response.Msg.CronJobs
}

func waitStoredCronJobs(t *testing.T, h *tuitest.Harness, predicate func([]*pb.CronJob) bool) {
	t.Helper()
	if err := h.Driver.WaitFor(waitTimeout, func(_ string) bool { return predicate(storedCronJobs(t, h)) }); err != nil {
		t.Fatalf("stored jobs did not reach expected state: %v; jobs=%v\n%s\n%s", err, storedCronJobs(t, h), h.Driver.Screen(), h.Stack.LogTail())
	}
}
