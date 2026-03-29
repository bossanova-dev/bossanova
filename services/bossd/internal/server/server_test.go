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
		missing bool // if true, test with a nonexistent path
	}{
		{
			name:    "three flight legs",
			content: "# Plan\n\n## Flight Leg 1: Setup\nDo stuff\n\n## Flight Leg 2: Build\nMore stuff\n\n## Flight Leg 3: Test\nTest stuff\n",
			want:    3,
		},
		{
			name:    "no flight legs returns 1 (single-leg plan)",
			content: "# Plan\n\nJust some notes\n## Other heading\n",
			want:    1,
		},
		{
			name:    "case insensitive",
			content: "## flight leg 1: lower case\n## FLIGHT LEG 2: upper case\n## Flight Leg 3: mixed\n",
			want:    3,
		},
		{
			name:    "empty file returns 1 (single-leg plan)",
			content: "",
			want:    1,
		},
		{
			name:    "missing file returns -1",
			missing: true,
			want:    -1,
		},
		{
			name:    "handoff markers only (h3)",
			content: "# Plan\n\n### [HANDOFF] Phase 1 complete\n\n### [HANDOFF] Phase 2 complete\n\n### [HANDOFF] Phase 3 complete\n",
			want:    3,
		},
		{
			name:    "handoff markers only (h4)",
			content: "# Plan\n\n#### [HANDOFF] Done\n\n#### [HANDOFF] Also done\n",
			want:    2,
		},
		{
			name:    "handoff case insensitive",
			content: "# Plan\n\n### [Handoff] Phase 1\n### [handoff] Phase 2\n### [HANDOFF] Phase 3\n",
			want:    3,
		},
		{
			name:    "both flight legs and handoff markers (takes max)",
			content: "## Flight Leg 1: Setup\n### [HANDOFF]\n\n## Flight Leg 2: Build\n### [HANDOFF]\n",
			want:    2,
		},
		{
			name:    "more handoff markers than flight legs",
			content: "## Flight Leg 1: All\n### [HANDOFF] First\n### [HANDOFF] Second\n### [HANDOFF] Third\n",
			want:    3,
		},
		{
			name:    "handoff in non-heading line ignored",
			content: "# Plan\n\nThis line mentions [HANDOFF] but is not a heading\n",
			want:    1,
		},
		{
			name:    "h3 leg headings (### Leg N)",
			content: "# Plan\n\n### Leg 1: Scaffold\nDo stuff\n\n### Leg 2: Build\nMore stuff\n\n### Leg 3: Test\nTest stuff\n",
			want:    3,
		},
		{
			name:    "h4 leg headings",
			content: "#### Leg 1: A\n#### Leg 2: B\n",
			want:    2,
		},
		{
			name:    "mixed heading levels for legs",
			content: "## Flight Leg 1: Setup\n### Leg 2: Build\n#### Leg 3: Polish\n",
			want:    3,
		},
		{
			name:    "extra whitespace between leg and number",
			content: "## Leg  1: A\n## Leg   2: B\n",
			want:    2,
		},
		{
			name:    "section header '## Flight Legs' without number not counted",
			content: "## Flight Legs\n\n### Leg 1: Scaffold\n### Leg 2: Build\n",
			want:    2,
		},
		{
			name:    "sub-headings referencing leg number not counted",
			content: "## Flight Leg 1: Setup\n### Post-Flight Checks for Flight Leg 1\n### [HANDOFF] Review Flight Leg 1\n\n## Flight Leg 2: Build\n### Post-Flight Checks for Flight Leg 2\n### [HANDOFF] Review Flight Leg 2\n",
			want:    2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.missing {
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
