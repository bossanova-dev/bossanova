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

// TestCountPlanFlightLegsRealPlan verifies the function against a realistic
// 6-leg plan file to prevent regressions where the count silently falls back
// to a hardcoded default (e.g. 20).
func TestCountPlanFlightLegsRealPlan(t *testing.T) {
	plan := `# Stitcher Service Implementation Plan

**Flight ID:** fp-2026-04-06-1725-stitcher-service

## Overview

Build a new service from scratch.

---

## Flight Leg 1: Scaffold + Schema

### Tasks

- [ ] Create package.json
- [ ] Create schemas

### Post-Flight Checks for Flight Leg 1

- [ ] Tests pass

### [HANDOFF] Review Flight Leg 1

Human reviews schema design.

---

## Flight Leg 2: Components

### Tasks

- [ ] Create components

### Post-Flight Checks for Flight Leg 2

- [ ] Lint passes

### [HANDOFF] Review Flight Leg 2

Human reviews component structure.

---

## Flight Leg 3: Transcription

### Tasks

- [ ] Create whisper wrapper

### Post-Flight Checks for Flight Leg 3

- [ ] Tests pass

### [HANDOFF] Review Flight Leg 3

Human reviews transcription logic.

---

## Flight Leg 4: Pipeline + CLI

### Tasks

- [ ] Create pipeline orchestrator

### Post-Flight Checks for Flight Leg 4

- [ ] Tests pass

### [HANDOFF] Review Flight Leg 4

Human reviews pipeline ordering.

---

## Flight Leg 5: Test Fixtures

### Tasks

- [ ] Create test fixtures

### Post-Flight Checks for Flight Leg 5

- [ ] Integration test passes

### [HANDOFF] Review Flight Leg 5

Human reviews test quality.

---

## Flight Leg 6: Final Verification

### Tasks

- [ ] Run full test suite

### Post-Flight Checks for Final Verification

- [ ] End-to-end test passes

### [HANDOFF] Final Review

Human reviews complete service.
`

	dir := t.TempDir()
	planPath := filepath.Join(dir, "plan.md")
	if err := os.WriteFile(planPath, []byte(plan), 0o644); err != nil {
		t.Fatal(err)
	}

	got := countPlanFlightLegs(planPath)
	if got != 6 {
		t.Errorf("countPlanFlightLegs() = %d, want 6", got)
	}
}

// TestCountPlanFlightLegsWorkingDirFallback verifies the StartAutopilot
// logic where countPlanFlightLegs should succeed when called with the
// working directory path even if the rootDir-based path fails.
func TestCountPlanFlightLegsWorkingDirFallback(t *testing.T) {
	// Simulate the scenario: plan exists under the working directory
	// but NOT under the rootDir (e.g. worktree path mismatch).
	workDir := t.TempDir()
	wrongRootDir := t.TempDir()

	planRelPath := filepath.Join("docs", "plans", "test-plan.md")
	planContent := "## Flight Leg 1: Setup\n### [HANDOFF]\n\n## Flight Leg 2: Build\n### [HANDOFF]\n\n## Flight Leg 3: Test\n### [HANDOFF]\n"

	// Create the plan file under workDir only.
	planFullPath := filepath.Join(workDir, planRelPath)
	if err := os.MkdirAll(filepath.Dir(planFullPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planFullPath, []byte(planContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// rootDir-based path should fail (plan doesn't exist there).
	rootDirPath := filepath.Join(wrongRootDir, planRelPath)
	if got := countPlanFlightLegs(rootDirPath); got != -1 {
		t.Fatalf("expected -1 for wrong rootDir, got %d", got)
	}

	// workDir-based path should succeed.
	workDirPath := filepath.Join(workDir, planRelPath)
	if got := countPlanFlightLegs(workDirPath); got != 3 {
		t.Errorf("countPlanFlightLegs(workDir) = %d, want 3", got)
	}
}

func TestResolvePlanFile(t *testing.T) {
	workDir := t.TempDir()
	rootDir := t.TempDir()

	planRel := filepath.Join("docs", "plans", "plan.md")
	planInRoot := filepath.Join(rootDir, planRel)
	if err := os.MkdirAll(filepath.Dir(planInRoot), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planInRoot, []byte("# plan"), 0o644); err != nil {
		t.Fatal(err)
	}

	planInWork := filepath.Join(workDir, planRel)
	if err := os.MkdirAll(filepath.Dir(planInWork), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planInWork, []byte("# plan"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("prefers first candidate when present", func(t *testing.T) {
		got, err := resolvePlanFile(planRel, rootDir, workDir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != planInRoot {
			t.Errorf("got %q, want %q", got, planInRoot)
		}
	})

	t.Run("falls back to second candidate", func(t *testing.T) {
		got, err := resolvePlanFile(planRel, t.TempDir(), workDir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != planInWork {
			t.Errorf("got %q, want %q", got, planInWork)
		}
	})

	t.Run("errors when file missing in all candidates", func(t *testing.T) {
		_, err := resolvePlanFile("missing.md", rootDir, workDir)
		if err == nil {
			t.Fatal("expected error for missing plan file")
		}
	})

	t.Run("errors when plan_path is empty", func(t *testing.T) {
		_, err := resolvePlanFile("", rootDir, workDir)
		if err == nil {
			t.Fatal("expected error for empty plan path")
		}
	})

	t.Run("skips empty candidate directories", func(t *testing.T) {
		got, err := resolvePlanFile(planRel, "", rootDir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != planInRoot {
			t.Errorf("got %q, want %q", got, planInRoot)
		}
	})

	t.Run("rejects when path resolves to a directory", func(t *testing.T) {
		_, err := resolvePlanFile("docs", rootDir)
		if err == nil {
			t.Fatal("expected error when path resolves to a directory")
		}
	})
}
