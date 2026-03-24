package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCountPlanFlightLegs(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    int32
	}{
		{
			name:    "three flight legs",
			content: "# Plan\n\n## Flight Leg 1: Setup\nDo stuff\n\n## Flight Leg 2: Build\nMore stuff\n\n## Flight Leg 3: Test\nTest stuff\n",
			want:    3,
		},
		{
			name:    "no flight legs",
			content: "# Plan\n\nJust some notes\n## Other heading\n",
			want:    0,
		},
		{
			name:    "case insensitive",
			content: "## flight leg 1: lower case\n## FLIGHT LEG 2: upper case\n## Flight Leg 3: mixed\n",
			want:    3,
		},
		{
			name:    "empty file",
			content: "",
			want:    0,
		},
		{
			name:    "missing file",
			content: "", // won't be written
			want:    0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.name == "missing file" {
				got := countPlanFlightLegs("/nonexistent/path/plan.md")
				if got != tt.want {
					t.Errorf("countPlanFlightLegs() = %d, want %d", got, tt.want)
				}
				return
			}

			dir := t.TempDir()
			planPath := filepath.Join(dir, "plan.md")
			if err := os.WriteFile(planPath, []byte(tt.content), 0o644); err != nil {
				t.Fatal(err)
			}

			got := countPlanFlightLegs(planPath)
			if got != tt.want {
				t.Errorf("countPlanFlightLegs() = %d, want %d", got, tt.want)
			}
		})
	}
}
