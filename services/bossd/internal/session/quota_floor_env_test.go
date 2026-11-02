package session

import (
	"testing"

	"github.com/recurser/bossalib/models"
)

// TestManagedSessionEnv_CronQuotaFloor proves a cron session tells the verify
// sweep the GitHub quota floor its job was admitted under, so a verify session
// keeps judging below the sweep's own 25% default, and that only cron
// sessions carry it.
func TestManagedSessionEnv_CronQuotaFloor(t *testing.T) {
	job := "cron-7"
	for _, tt := range []struct {
		name string
		sess *models.Session
		want string // "" means the variable must be absent
	}{
		{"verify cron", &models.Session{ID: "s1", CronJobID: &job, Plan: "/boss-verify"}, "0.1"},
		{"codex verify cron", &models.Session{ID: "s2", CronJobID: &job, Plan: "$boss-verify"}, "0.1"},
		{"build cron", &models.Session{ID: "s3", CronJobID: &job, Plan: "/boss-build"}, "0.35"},
		{"plain cron", &models.Session{ID: "s4", CronJobID: &job, Plan: "Tidy the docs"}, "0.35"},
		{"unattended, not cron", &models.Session{ID: "s5", IsTmuxUnattended: true, Plan: "/boss-verify"}, ""},
		{"attended", &models.Session{ID: "s6", Plan: "/boss-verify"}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ManagedSessionEnv(tt.sess, "agent-1", "claude", nil)["BOSS_GITHUB_QUOTA_FLOOR"]
			if tt.want == "" {
				if ok {
					t.Fatalf("BOSS_GITHUB_QUOTA_FLOOR = %q, want it unset", got)
				}
				return
			}
			if got != tt.want {
				t.Fatalf("BOSS_GITHUB_QUOTA_FLOOR = %q, want %q", got, tt.want)
			}
		})
	}
}
