package factoryhealth

import (
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestComputeRules(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * time.Hour)
	failure := Note{Body: "second-model review unavailable: claude not_authed since 2026-10-19T01:02:03Z, 302 consecutive failures on host private-host", CreatedAt: old}
	for _, tt := range []struct {
		name    string
		facts   Facts
		code    string
		started time.Time
	}{
		{name: "healthy", facts: Facts{VerifyEnabled: true, PublishingConfigured: true}},
		{name: "second model", facts: Facts{SecondModelNotes: []Note{failure}}, code: "second-model-off", started: time.Date(2026, 10, 19, 1, 2, 3, 0, time.UTC)},
		{name: "malformed note", facts: Facts{SecondModelNotes: []Note{{Body: "private-host malformed", CreatedAt: old}}}, code: "second-model-off", started: old},
		{name: "recovered", facts: Facts{SecondModelNotes: []Note{failure, {Body: failure.Body, Tags: []string{"recovered"}, CreatedAt: now}}}},
		{name: "failed again", facts: Facts{SecondModelNotes: []Note{failure, {Body: failure.Body, Tags: []string{"recovered"}, CreatedAt: old.Add(-time.Minute)}}}, code: "second-model-off", started: time.Date(2026, 10, 19, 1, 2, 3, 0, time.UTC)},
		{name: "cron failed", facts: Facts{VerifyCrons: []CronFact{{Name: "verify", Failing: true, LastRunAt: &old, LastOutcome: "failed"}}}, code: "verify-cron-failing", started: old},
		{name: "gated cron", facts: Facts{VerifyCrons: []CronFact{{LastOutcome: "gated"}, {LastOutcome: "pr_created"}}}},
		{name: "publishing", facts: Facts{VerifyEnabled: true}, code: "publishing-off"},
		{name: "no verify", facts: Facts{}},
		{name: "backlog", facts: Facts{Verifying: []VerifyingHead{{PR: 4, StartedAt: old}}}, code: "verify-backlog", started: old},
		{name: "recent", facts: Facts{Verifying: []VerifyingHead{{PR: 4, StartedAt: now.Add(-59 * time.Minute)}}}},
		{name: "boundary", facts: Facts{Verifying: []VerifyingHead{{PR: 4, StartedAt: now.Add(-time.Hour)}}}},
		{name: "missing start", facts: Facts{Verifying: []VerifyingHead{{PR: 4}}}},
		{name: "partial", facts: Facts{Unreadable: []string{"notes"}}, code: "health-partial"},
		{name: "older daemon", facts: Facts{DaemonVersion: "v1.9.9", SkillsVersion: "v1.10.0", DaemonOutdated: true}, code: "daemon-outdated"},
		{name: "same daemon", facts: Facts{DaemonVersion: "v1.10.0", SkillsVersion: "v1.10.0"}},
		{name: "newer daemon", facts: Facts{DaemonVersion: "v2.0.0", SkillsVersion: "v1.10.0"}},
		{name: "unknown daemon", facts: Facts{DaemonVersion: "dev", SkillsVersion: "v1.10.0"}},
		{name: "no remote PRs", facts: Facts{RemotePRs: []int{}}},
		{name: "remote PR", facts: Facts{RemotePRs: []int{42}}, code: "remote-sessions"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := Compute(tt.facts, now)
			if tt.code == "" {
				if len(got) != 0 {
					t.Fatalf("unexpected problems: %+v", got)
				}
				return
			}
			if len(got) != 1 || got[0].Code != tt.code || !got[0].StartedAt.Equal(tt.started) {
				t.Fatalf("got %+v, want %s at %s", got, tt.code, tt.started)
			}
			if strings.Contains(got[0].Text, "private-host") {
				t.Fatal("leaked host")
			}
			if tt.code == "daemon-outdated" && (!strings.Contains(got[0].Text, tt.facts.DaemonVersion) || !strings.Contains(got[0].Text, tt.facts.SkillsVersion)) {
				t.Fatal("missing versions")
			}
		})
	}
}

func TestComputeOrderingAndLimits(t *testing.T) {
	now := time.Now()
	old := now.Add(-61 * time.Minute)
	facts := Facts{SecondModelNotes: []Note{{Body: "second-model review unavailable: z failed since bad", CreatedAt: old}, {Body: "second-model review unavailable: a failed since bad", CreatedAt: old}}, VerifyCrons: []CronFact{{Name: strings.Repeat("界", 81), Failing: true, LastOutcome: strings.Repeat("x", 400)}}, VerifyEnabled: true, Verifying: []VerifyingHead{{PR: 8, StartedAt: old}, {PR: 9, StartedAt: now.Add(-62 * time.Minute)}}, Unreadable: []string{strings.Repeat("界", 400)}, DaemonVersion: "v1.0.0", SkillsVersion: "v1.1.0", DaemonOutdated: true, RemotePRs: []int{7}}
	got := Compute(facts, now)
	want := []string{"second-model-off", "second-model-off", "verify-cron-failing", "publishing-off", "verify-backlog", "remote-sessions", "daemon-outdated", "health-partial"}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i, p := range got {
		if p.Code != want[i] || !regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`).MatchString(p.Code) || utf8.RuneCountInString(p.Text) > 300 {
			t.Fatalf("invalid problem: %+v", p)
		}
	}
	if !strings.Contains(got[2].Text, strings.Repeat("界", 79)+"…") {
		t.Fatal("cron name not truncated")
	}
	if !strings.Contains(got[4].Text, "2 PR(s)") || !strings.Contains(got[4].Text, "#9") {
		t.Fatal("wrong backlog summary")
	}
	facts.VerifyCrons = nil
	for range 30 {
		facts.VerifyCrons = append(facts.VerifyCrons, CronFact{Failing: true})
	}
	got = Compute(facts, now)
	if len(got) != MaxProblems {
		t.Fatalf("got %d problems", len(got))
	}
}

func TestRemoteSessionsText(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name string
		prs  []int
		want string
	}{
		{"one PR is singular", []int{42}, "1 PR skipped here: session on another machine (#42)"},
		{"five PRs list all", []int{5, 4, 3, 2, 1}, "5 PRs skipped here: session on another machine (#1, #2, #3, #4, #5)"},
		{"seven PRs list five and elide", []int{70, 10, 60, 20, 50, 30, 40}, "7 PRs skipped here: session on another machine (#10, #20, #30, #40, #50, …)"},
		{"duplicates count once", []int{3, 3, 1}, "2 PRs skipped here: session on another machine (#1, #3)"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			prs := append([]int(nil), tt.prs...)
			got := Compute(Facts{RemotePRs: prs}, now)
			if len(got) != 1 || got[0].Code != "remote-sessions" || got[0].Text != tt.want || !got[0].StartedAt.IsZero() {
				t.Fatalf("got %+v, want %q", got, tt.want)
			}
			if utf8.RuneCountInString(got[0].Text) > 300 || strings.Contains(got[0].Text, "...") {
				t.Fatalf("text out of contract: %q", got[0].Text)
			}
			if !slices.Equal(prs, tt.prs) {
				t.Fatalf("Compute mutated its input: %v", prs)
			}
		})
	}
}

func TestSecondModelSafeTokens(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	got := Compute(Facts{SecondModelNotes: []Note{
		{Body: "second-model review unavailable: <private-host> secret/private-host since 2026-10-19T01:02:03Z, failures on host private-host", CreatedAt: now},
		{Body: "second-model review unavailable: claude not_authed since 2026-10-19T01:02:03Z, failures on host private-host", CreatedAt: now},
	}}, now)
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	for _, problem := range got {
		if strings.Contains(problem.Text, "private-host") {
			t.Fatalf("unsafe tokens leaked: %s", problem.Text)
		}
	}
	if got[0].Text != "Second model not running since 2026-10-19" {
		t.Fatalf("unexpected sanitized text %q", got[0].Text)
	}
	if !strings.Contains(got[1].Text, "(claude)") || !strings.Contains(got[1].Text, "(not_authed)") {
		t.Fatalf("safe tokens missing: %q", got[1].Text)
	}
}

func TestSecondModelActualRecoveryNote(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	failure := Note{Body: "second-model review unavailable: claude not_authed since 2026-10-19T01:02:03Z, 302 consecutive failures on host private-host", CreatedAt: now.Add(-time.Hour)}
	recovery := Note{Body: "second-model review recovered: claude ran again on host private-host after 302 consecutive failures (not_authed since 2026-10-19T01:02:03Z)", Tags: []string{"recovered"}, CreatedAt: now}
	if got := Compute(Facts{SecondModelNotes: []Note{failure, recovery}}, now); len(got) != 0 {
		t.Fatalf("recovery must clear agent: %+v", got)
	}
	failure.CreatedAt = now.Add(time.Minute)
	got := Compute(Facts{SecondModelNotes: []Note{failure, recovery}}, now)
	if len(got) != 1 || !strings.Contains(got[0].Text, "(claude)") {
		t.Fatalf("new failure must reopen agent: %+v", got)
	}
}
