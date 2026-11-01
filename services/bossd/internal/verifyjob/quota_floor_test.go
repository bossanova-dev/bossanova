package verifyjob

import (
	"testing"

	"github.com/recurser/bossd/internal/domain"
)

func TestQuotaFloorReservesBudgetForVerifyJobsOnly(t *testing.T) {
	for _, tt := range []struct {
		name string
		job  *domain.CronJob
		want string
	}{
		{"verify prompt", &domain.CronJob{Prompt: "/boss-verify"}, "0.1"},
		{"codex verify prompt", &domain.CronJob{Prompt: "$boss-verify --batch 3"}, "0.1"},
		{"verify gate script", &domain.CronJob{Prompt: "judge PRs", GateCommand: "node ~/.claude/skills/boss-verify/toolbox/cron-gates/boss-verify.mjs"}, "0.1"},
		{"build", &domain.CronJob{Prompt: "/boss-build", GateCommand: "node toolbox/cron-gates/boss-build.mjs"}, "0.35"},
		{"plain sweep", &domain.CronJob{Prompt: "Tidy the docs"}, "0.35"},
		{"nil", nil, "0.35"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := QuotaFloorValue(tt.job); got != tt.want {
				t.Fatalf("QuotaFloorValue = %q, want %q", got, tt.want)
			}
		})
	}
}
