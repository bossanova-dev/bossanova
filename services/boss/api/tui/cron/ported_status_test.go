package cron

import (
	"strings"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// Stored status variants belong at the screen boundary: no scheduler run is
// required to verify the text a historical daemon result renders.
func TestCronListPortedStatusColumn(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status pb.CronJobStatus
		want   string
	}{
		{"server_running", pb.CronJobStatus_CRON_JOB_STATUS_RUNNING, "Running"},
		{"server_failed", pb.CronJobStatus_CRON_JOB_STATUS_FAILED, "failed"},
		{"server_idle", pb.CronJobStatus_CRON_JOB_STATUS_IDLE, "idle"},
		{"server_unspecified_renders_idle", pb.CronJobStatus_CRON_JOB_STATUS_UNSPECIFIED, "idle"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := newCronListForUpdate([]*pb.CronJob{{Id: "cron-1", Name: "Historical job", Schedule: "@daily", IsEnabled: true, LastRunStatus: tt.status}})
			m.width = 120
			m.height = 30
			m.rebuildTable()
			view := stripANSI(m.View().Content)
			if !strings.Contains(view, "STATUS") || !strings.Contains(view, tt.want) {
				t.Fatalf("status %v: want STATUS column containing %q; view:\n%s", tt.status, tt.want, view)
			}
		})
	}
}
