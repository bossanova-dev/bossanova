package views

import (
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestFormatFlightLeg(t *testing.T) {
	tests := []struct {
		name string
		w    *pb.AutopilotWorkflow
		want string
	}{
		{
			name: "completed with leg 0 shows max/max",
			w: &pb.AutopilotWorkflow{
				Status:    pb.WorkflowStatus_WORKFLOW_STATUS_COMPLETED,
				FlightLeg: 0,
				MaxLegs:   1,
			},
			want: "1/1",
		},
		{
			name: "completed with leg 0 and max 10 shows 10/10",
			w: &pb.AutopilotWorkflow{
				Status:    pb.WorkflowStatus_WORKFLOW_STATUS_COMPLETED,
				FlightLeg: 0,
				MaxLegs:   10,
			},
			want: "10/10",
		},
		{
			name: "completed with non-zero leg preserves value",
			w: &pb.AutopilotWorkflow{
				Status:    pb.WorkflowStatus_WORKFLOW_STATUS_COMPLETED,
				FlightLeg: 10,
				MaxLegs:   10,
			},
			want: "10/10",
		},
		{
			name: "running shows actual leg",
			w: &pb.AutopilotWorkflow{
				Status:    pb.WorkflowStatus_WORKFLOW_STATUS_RUNNING,
				FlightLeg: 3,
				MaxLegs:   10,
			},
			want: "3/10",
		},
		{
			name: "running at leg 0 shows 0",
			w: &pb.AutopilotWorkflow{
				Status:    pb.WorkflowStatus_WORKFLOW_STATUS_RUNNING,
				FlightLeg: 0,
				MaxLegs:   5,
			},
			want: "0/5",
		},
		{
			name: "paused incomplete shows suffix",
			w: &pb.AutopilotWorkflow{
				Status:    pb.WorkflowStatus_WORKFLOW_STATUS_PAUSED,
				FlightLeg: 4,
				MaxLegs:   8,
			},
			want: "4/8 (incomplete)",
		},
		{
			name: "paused at max shows no suffix",
			w: &pb.AutopilotWorkflow{
				Status:    pb.WorkflowStatus_WORKFLOW_STATUS_PAUSED,
				FlightLeg: 8,
				MaxLegs:   8,
			},
			want: "8/8",
		},
		{
			name: "failed shows actual leg",
			w: &pb.AutopilotWorkflow{
				Status:    pb.WorkflowStatus_WORKFLOW_STATUS_FAILED,
				FlightLeg: 0,
				MaxLegs:   1,
			},
			want: "0/1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatFlightLeg(tt.w)
			if got != tt.want {
				t.Errorf("FormatFlightLeg() = %q, want %q", got, tt.want)
			}
		})
	}
}
