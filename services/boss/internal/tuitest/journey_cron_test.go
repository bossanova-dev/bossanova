package tuitest_test

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
)

// The CLI and the open TUI drive one daemon: a job added from the terminal
// shows up in the Scheduled Jobs list, and what the TUI then toggles and adds
// is what `boss cron ls` reports back.
func TestJourney_ScheduledJobs_AC1_AC2_AC3_CLIAndTUIShareJobs(t *testing.T) {
	h := tuitest.New(t, tuitest.WithRepos(testRepos()...))
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
