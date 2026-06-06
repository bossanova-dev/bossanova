package views

import (
	"testing"
	"time"

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

// TestHasRunningStatus covers both directions of the LastRunStatus equality
// check (CONDITIONALS_NEGATION on `== RUNNING`): a lone running job must report
// true, and a lone non-running job must report false.
func TestHasRunningStatus(t *testing.T) {
	running := &pb.CronJob{Id: "r", LastRunStatus: pb.CronJobStatus_CRON_JOB_STATUS_RUNNING}
	idle := &pb.CronJob{Id: "i"} // zero status == UNSPECIFIED, not RUNNING

	cases := []struct {
		name string
		jobs []*pb.CronJob
		want bool
	}{
		{"nil slice", nil, false},
		{"empty slice", []*pb.CronJob{}, false},
		{"single idle", []*pb.CronJob{idle}, false},
		{"single running", []*pb.CronJob{running}, true},
		{"running among idle", []*pb.CronJob{idle, running, idle}, true},
		{"all idle", []*pb.CronJob{idle, idle}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasRunningStatus(tc.jobs); got != tc.want {
				t.Errorf("hasRunningStatus(%v) = %v, want %v", tc.jobs, got, tc.want)
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
