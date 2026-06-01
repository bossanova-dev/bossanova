package taskorchestrator

import (
	"testing"
	"time"

	"github.com/recurser/bossalib/models"
)

func TestFailedAutoMergeRetryReady(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	cooldown := 30 * time.Minute

	tests := []struct {
		name      string
		updatedAt time.Time
		want      bool
	}{
		{"just failed", now.Add(-1 * time.Minute), false},
		{"within cooldown", now.Add(-29 * time.Minute), false},
		{"exactly at cooldown", now.Add(-30 * time.Minute), true},
		{"well past cooldown", now.Add(-2 * time.Hour), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &models.TaskMapping{UpdatedAt: tt.updatedAt}
			if got := failedAutoMergeRetryReady(m, now, cooldown); got != tt.want {
				t.Fatalf("failedAutoMergeRetryReady = %v, want %v", got, tt.want)
			}
		})
	}
}
