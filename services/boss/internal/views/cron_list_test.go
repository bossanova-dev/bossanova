package views

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// TestFormatRelAgo pins the threshold ladder in formatRelAgo. Each boundary is
// tested at its exact value (e.g. exactly time.Minute) so the CONDITIONALS_BOUNDARY
// mutants (`<` -> `<=`) are killed, plus values inside each band to kill the
// CONDITIONALS_NEGATION mutants and the day-arithmetic (`/24`).
func TestFormatRelAgo(t *testing.T) {
	cases := []struct {
		name string
		d    time.Duration
		want string
	}{
		{"zero is just now", 0, "just now"},
		{"sub-minute is just now", 30 * time.Second, "just now"},
		{"exactly one minute rolls over", time.Minute, "1m ago"},
		{"mid minutes", 30 * time.Minute, "30m ago"},
		{"just under an hour", 59 * time.Minute, "59m ago"},
		{"exactly one hour rolls over", time.Hour, "1h ago"},
		{"mid hours", 5 * time.Hour, "5h ago"},
		{"just under a day", 23 * time.Hour, "23h ago"},
		{"exactly one day rolls over", 24 * time.Hour, "1d ago"},
		{"just over a day still one day", 25 * time.Hour, "1d ago"},
		{"two days uses integer day division", 48 * time.Hour, "2d ago"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatRelAgo(tc.d); got != tc.want {
				t.Errorf("formatRelAgo(%s) = %q, want %q", tc.d, got, tc.want)
			}
		})
	}
}

// TestFormatRelFuture pins the threshold ladder in formatRelFuture, including
// the `d <= 0` guard boundary (0 must be "now", not "in 0s") and the day
// arithmetic.
func TestFormatRelFuture(t *testing.T) {
	cases := []struct {
		name string
		d    time.Duration
		want string
	}{
		{"negative is now", -5 * time.Second, "now"},
		{"exactly zero is now", 0, "now"},
		{"sub-minute seconds", 30 * time.Second, "in 30s"},
		{"just under a minute", 59 * time.Second, "in 59s"},
		{"exactly one minute rolls over", time.Minute, "in 1m"},
		{"mid minutes", 30 * time.Minute, "in 30m"},
		{"exactly one hour rolls over", time.Hour, "in 1h"},
		{"mid hours", 5 * time.Hour, "in 5h"},
		{"just under a day", 23 * time.Hour, "in 23h"},
		{"exactly one day rolls over", 24 * time.Hour, "in 1d"},
		{"two days uses integer day division", 48 * time.Hour, "in 2d"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatRelFuture(tc.d); got != tc.want {
				t.Errorf("formatRelFuture(%s) = %q, want %q", tc.d, got, tc.want)
			}
		})
	}
}

// TestRelTimeWrappers smoke-tests the wall-clock wrappers so the time.Since /
// time.Until plumbing stays covered. Bands are chosen far from any boundary so
// the few microseconds of elapsed time between setup and call cannot flip the
// result.
func TestRelTimeWrappers(t *testing.T) {
	if got := relTimeAgo(time.Now().Add(-2 * time.Hour)); got != "2h ago" {
		t.Errorf("relTimeAgo(2h in the past) = %q, want %q", got, "2h ago")
	}
	if got := relTimeFuture(time.Now().Add(3 * time.Hour)); got != "in 2h" && got != "in 3h" {
		// time.Until shaves a sliver off, so 3h reads as "in 2h" (int-truncated).
		t.Errorf("relTimeFuture(3h in the future) = %q, want \"in 2h\" or \"in 3h\"", got)
	}
}

func responsiveCronFixture() []*pb.CronJob {
	return []*pb.CronJob{{
		Id:        "cron-1",
		Schedule:  strings.Repeat("*", 20),
		Name:      strings.Repeat("n", 40),
		RepoId:    strings.Repeat("r", 30),
		AgentName: strings.Repeat("a", 20),
		IsEnabled: true,
	}}
}

func TestCronListRebuildTable_FitsColumnsToTerminalWidth(t *testing.T) {
	wantTitles := map[int][]string{
		0:   {" ", "CRON", "NAME", "REPO", "AGENT", "ENABLED", "LAST RUN", "NEXT RUN", "STATUS"},
		60:  {" ", "NAME", "ENABLED"},
		72:  {" ", "CRON", "NAME", "ENABLED"},
		80:  {" ", "CRON", "NAME", "ENABLED", "STATUS"},
		100: {" ", "CRON", "NAME", "ENABLED", "LAST RUN", "NEXT RUN", "STATUS"},
		140: {" ", "CRON", "NAME", "REPO", "AGENT", "ENABLED", "LAST RUN", "NEXT RUN", "STATUS"},
	}

	var unfitted []table.Column
	for _, width := range []int{0, 60, 72, 80, 100, 140} {
		t.Run(fmt.Sprintf("width-%d", width), func(t *testing.T) {
			m := newCronListForUpdate(responsiveCronFixture())
			m.rebuildTable()
			if width > 0 {
				updated, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: 40})
				m = updated.(CronListModel)
			}

			cols := m.table.Columns()
			assertTableRowsMatchColumns(t, cols, m.table.Rows())
			if width > 0 && columnsWidth(cols) > width {
				t.Fatalf("columns width = %d, want <= terminal width %d", columnsWidth(cols), width)
			}
			if !slices.Contains(columnTitles(cols), "NAME") {
				t.Fatalf("titles = %v, want priority-0 NAME retained", columnTitles(cols))
			}
			if width == 72 && !slices.Contains(columnTitles(cols), "ENABLED") {
				t.Fatalf("titles = %v, want ENABLED retained at 72 columns", columnTitles(cols))
			}
			if width == 0 {
				unfitted = append([]table.Column(nil), cols...)
			}
			if got := columnTitles(cols); !slices.Equal(got, wantTitles[width]) {
				t.Fatalf("width %d titles = %v, want %v", width, got, wantTitles[width])
			}
			if width == 140 && !reflect.DeepEqual(cols, unfitted) {
				t.Fatalf("140-column set = %#v, want byte-identical unfitted %#v", cols, unfitted)
			}
		})
	}
}

func TestCronListRebuildTable_ResizeKeepsSelectedRowVisible(t *testing.T) {
	const cursor = 25
	jobs := make([]*pb.CronJob, 50)
	for i := range jobs {
		jobs[i] = &pb.CronJob{Id: fmt.Sprintf("cron-%d", i), Name: fmt.Sprintf("cron-%02d", i), Schedule: "@daily", IsEnabled: true}
	}

	m := newCronListForUpdate(jobs)
	m.width, m.height = 140, 13
	m.rebuildTable()
	m.table.SetCursor(cursor)
	m.table.MoveDown(0)
	updateCursorColumn(&m.table)

	for _, width := range []int{72, 140} {
		m.width = width
		m.rebuildTable()
		if got := m.table.Cursor(); got != cursor {
			t.Fatalf("cursor after resize to %d = %d, want %d", width, got, cursor)
		}
		if got := stripANSI(m.table.View()); !strings.Contains(got, "cron-25") {
			t.Fatalf("selected row is outside the %d-column viewport:\n%s", width, got)
		}
	}
}

// TestHasRunningStatus covers both directions of the LastRunStatus equality
// check (CONDITIONALS_NEGATION on `== RUNNING`): a lone running job must report
// true, and a lone non-running job must report false.
func TestHasRunningStatus(t *testing.T) {
	assertCronStatusPredicate(
		t,
		"hasRunningStatus",
		"running",
		pb.CronJobStatus_CRON_JOB_STATUS_RUNNING,
		hasRunningStatus,
	)
}

// TestHasGatingStatus covers both directions of the LastRunStatus equality
// check for GATING: a lone gating job must report true, and a non-gating job
// must report false.
func TestHasGatingStatus(t *testing.T) {
	assertCronStatusPredicate(
		t,
		"hasGatingStatus",
		"gating",
		pb.CronJobStatus_CRON_JOB_STATUS_GATING,
		hasGatingStatus,
	)
}

func assertCronStatusPredicate(
	t *testing.T,
	predicateName string,
	activeName string,
	activeStatus pb.CronJobStatus,
	predicate func([]*pb.CronJob) bool,
) {
	t.Helper()

	active := &pb.CronJob{Id: "active", LastRunStatus: activeStatus}
	idle := &pb.CronJob{Id: "idle"} // zero status == UNSPECIFIED

	cases := []struct {
		name string
		jobs []*pb.CronJob
		want bool
	}{
		{"nil slice", nil, false},
		{"empty slice", []*pb.CronJob{}, false},
		{"single idle", []*pb.CronJob{idle}, false},
		{"single " + activeName, []*pb.CronJob{active}, true},
		{activeName + " among idle", []*pb.CronJob{idle, active, idle}, true},
		{"all idle", []*pb.CronJob{idle, idle}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := predicate(tc.jobs); got != tc.want {
				t.Errorf("%s(%v) = %v, want %v", predicateName, tc.jobs, got, tc.want)
			}
		})
	}
}

// TestCronListRebuildTable_GatingAndGatedStatuses builds a list with one GATING
// job and one GATED job, then renders View() and asserts that "gating" and
// "gated" both appear in the output.
func TestCronListRebuildTable_GatingAndGatedStatuses(t *testing.T) {
	jobs := []*pb.CronJob{
		{Id: "a", Name: "Gate runner", Schedule: "@daily", LastRunStatus: pb.CronJobStatus_CRON_JOB_STATUS_GATING},
		{Id: "b", Name: "Gate blocked", Schedule: "@daily", LastRunStatus: pb.CronJobStatus_CRON_JOB_STATUS_GATED},
	}
	m := newCronListForUpdate(jobs)
	m.width = 120
	m.height = 40

	view := m.View()
	if !strings.Contains(view.Content, "gating") {
		t.Errorf("View() missing %q:\n%s", "gating", view.Content)
	}
	if !strings.Contains(view.Content, "gated") {
		t.Errorf("View() missing %q:\n%s", "gated", view.Content)
	}
}

// TestCronListRebuildTable_GateFailedRendersAsFailedBesideGated is the BOS-881
// TUI half: a job whose gate could not run derives CRON_JOB_STATUS_FAILED, so
// the cron list must show it as a red `failed` row while a genuinely gated job
// on the same screen keeps its warning-styled `gated`. The two must be
// distinguishable at a glance — that is the whole point of the ticket.
func TestCronListRebuildTable_GateFailedRendersAsFailedBesideGated(t *testing.T) {
	jobs := []*pb.CronJob{
		{Id: "a", Name: "Healthy gate", Schedule: "@daily", IsEnabled: true, LastRunStatus: pb.CronJobStatus_CRON_JOB_STATUS_GATED, LastRunOutcome: "gated"},
		{Id: "b", Name: "Broken gate", Schedule: "@daily", IsEnabled: true, LastRunStatus: pb.CronJobStatus_CRON_JOB_STATUS_FAILED, LastRunOutcome: "gate_failed"},
	}
	m := newCronListForUpdate(jobs)
	m.width = 120
	m.height = 40

	view := m.View().Content
	if !strings.Contains(view, styleStatusWarning.Render("gated")) {
		t.Errorf("the healthy gated job lost its warning-styled 'gated' label:\n%s", view)
	}
	if !strings.Contains(view, styleStatusDanger.Render("failed")) {
		t.Errorf("the gate_failed job did not render as a danger-styled 'failed' row:\n%s", view)
	}
}

// TestRunNowSkipLabel covers the run-now status line, where the two gate
// outcomes must also read differently rather than the new reason falling
// through the default branch verbatim.
func TestRunNowSkipLabel(t *testing.T) {
	tests := []struct {
		reason string
		want   string
	}{
		{"gated", "blocked by gate command"},
		{"gate_failed", "gate command could not run — check the daemon log"},
		{"overlap_prev_active", "previous run still active"},
		{"disabled", "job is disabled"},
		{"something_new", "something_new"},
	}
	for _, tt := range tests {
		if got := runNowSkipLabel(tt.reason); got != tt.want {
			t.Errorf("runNowSkipLabel(%q) = %q, want %q", tt.reason, got, tt.want)
		}
	}
	if runNowSkipLabel("gate_failed") == runNowSkipLabel("gated") {
		t.Error("gate_failed and gated must not share a run-now label")
	}
}

// TestCronListRebuildTable_DisabledStatusLabelsAreMuted guards the BOS-313 fix:
// a disabled job's terminal STATUS label (gated/failed) must render in the muted
// grey of the rest of the row, not its status color. rebuildTable pre-colors the
// label and then mutes the whole row with muted.Render; lipgloss does not strip
// the inner foreground, so a pre-colored label would keep its status color under
// the outer muted span. cronStatusLabel therefore emits a plain label for
// disabled jobs. Enabled jobs must keep their status color (no over-muting).
//
// Assertions track the theme constants: the status-colored sequence is built
// from the same style the code uses, and the muted color is colorMuted's
// truecolor code (theme.go: #626262 -> 38;2;98;98;98).
func TestCronListRebuildTable_DisabledStatusLabelsAreMuted(t *testing.T) {
	const mutedCode = "38;2;98;98;98" // colorMuted (#626262) truecolor foreground

	cases := []struct {
		name    string
		status  pb.CronJobStatus
		label   string
		colored string // the status-colored render the enabled row must show
	}{
		{
			name:    "gated",
			status:  pb.CronJobStatus_CRON_JOB_STATUS_GATED,
			label:   "gated",
			colored: styleStatusWarning.Render("gated"), // orange #DBBD70
		},
		{
			name:    "failed",
			status:  pb.CronJobStatus_CRON_JOB_STATUS_FAILED,
			label:   "failed",
			colored: styleStatusDanger.Render("failed"), // red #FF6347
		},
	}

	for _, tc := range cases {
		t.Run("disabled "+tc.name+" is muted", func(t *testing.T) {
			m := newCronListForUpdate([]*pb.CronJob{
				{Id: "x", Name: "Job", Schedule: "@daily", IsEnabled: false, LastRunStatus: tc.status},
			})
			m.width = 120
			m.height = 40
			view := m.View().Content

			if strings.Contains(view, tc.colored) {
				t.Errorf("disabled %s row kept its status color; View() must not contain %q:\n%s", tc.name, tc.colored, view)
			}
			if !strings.Contains(view, mutedCode) {
				t.Errorf("disabled %s row is not muted; View() missing colorMuted code %q:\n%s", tc.name, mutedCode, view)
			}
			if !strings.Contains(view, tc.label) {
				t.Errorf("disabled %s row missing the %q label text:\n%s", tc.name, tc.label, view)
			}
		})

		t.Run("enabled "+tc.name+" keeps its color", func(t *testing.T) {
			m := newCronListForUpdate([]*pb.CronJob{
				{Id: "x", Name: "Job", Schedule: "@daily", IsEnabled: true, LastRunStatus: tc.status},
			})
			m.width = 120
			m.height = 40
			view := m.View().Content

			if !strings.Contains(view, tc.colored) {
				t.Errorf("enabled %s row lost its status color; View() must contain %q:\n%s", tc.name, tc.colored, view)
			}
		})
	}
}

// TestReplaceJob covers the nil guard, the ID-match replacement, the no-match
// passthrough, and that the input slice is not mutated in place.
func TestReplaceJob(t *testing.T) {
	t.Run("nil updated returns input unchanged", func(t *testing.T) {
		jobs := []*pb.CronJob{{Id: "a"}, {Id: "b"}}
		got := replaceJob(jobs, nil)
		if len(got) != 2 || got[0].Id != "a" || got[1].Id != "b" {
			t.Fatalf("replaceJob(jobs, nil) = %v, want unchanged input", got)
		}
	})

	t.Run("replaces matching id only", func(t *testing.T) {
		j1 := &pb.CronJob{Id: "a", Name: "first"}
		j2 := &pb.CronJob{Id: "b", Name: "second"}
		updated := &pb.CronJob{Id: "b", Name: "second-edited"}
		jobs := []*pb.CronJob{j1, j2}

		got := replaceJob(jobs, updated)
		if got[0] != j1 {
			t.Errorf("non-matching job at index 0 was changed: %v", got[0])
		}
		if got[1] != updated {
			t.Errorf("matching job at index 1 = %v, want the updated job", got[1])
		}
		// Input slice must not be mutated (replaceJob copies).
		if jobs[1] != j2 {
			t.Errorf("input slice was mutated: index 1 = %v, want original", jobs[1])
		}
	})

	t.Run("no match returns copy unchanged", func(t *testing.T) {
		j1 := &pb.CronJob{Id: "a"}
		jobs := []*pb.CronJob{j1}
		updated := &pb.CronJob{Id: "x"}
		got := replaceJob(jobs, updated)
		if len(got) != 1 || got[0] != j1 {
			t.Fatalf("replaceJob with no id match = %v, want copy of input", got)
		}
	})
}
