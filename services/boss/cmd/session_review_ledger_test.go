package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func writeReviewLedgerTestFile(t *testing.T, path, runID string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir ledger dir: %v", err)
	}
	body := `{
  "schemaVersion": 1,
  "runId": "` + runID + `",
  "seededAtMs": 100,
  "rows": [{
    "name": "lens:go",
    "phase": "Phase 1",
    "tier": "tier2",
    "mode": "dispatched",
    "outcome": "completed",
    "cause": null,
    "completedAtMs": 150,
    "durationMs": 50
  }]
}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write ledger: %v", err)
	}
}

func initReviewLedgerGitRepo(t *testing.T, worktree string) string {
	t.Helper()
	if out, err := exec.Command("git", "init", "--quiet", worktree).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	gitDir, err := checkoutGitOutput(worktree, "rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil {
		t.Fatalf("git rev-parse --git-dir: %v", err)
	}
	return gitDir
}

func commitReviewLedgerGitRepo(t *testing.T, worktree string) {
	t.Helper()
	if out, err := exec.Command("git", "-C", worktree, "config", "user.email", "test@example.com").CombinedOutput(); err != nil {
		t.Fatalf("git config user.email: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", worktree, "config", "user.name", "Test User").CombinedOutput(); err != nil {
		t.Fatalf("git config user.name: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", worktree, "commit", "--allow-empty", "-m", "init").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
}

func TestLoadReviewLedgerJSONKeySetIsStable(t *testing.T) {
	worktree := t.TempDir()
	gitDir := initReviewLedgerGitRepo(t, worktree)
	path := filepath.Join(gitDir, "boss-review-ledgers", "ledger-run-a.json")
	writeReviewLedgerTestFile(t, path, "run-a")
	got, err := loadReviewLedger(worktree, "run-a")
	if err != nil {
		t.Fatalf("loadReviewLedger: %v", err)
	}
	raw, err := json.Marshal(got.Rows[0])
	if err != nil {
		t.Fatalf("marshal row: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode row: %v", err)
	}
	keys := make([]string, 0, len(decoded))
	for key := range decoded {
		keys = append(keys, key)
	}
	want := []string{"cause", "completedAtMs", "durationMs", "mode", "name", "outcome", "phase", "tier"}
	if !reflect.DeepEqual(sortedStrings(keys), want) {
		t.Fatalf("row keys = %v, want %v", sortedStrings(keys), want)
	}
}

func TestLoadReviewLedgerHonorsConfiguredDirectory(t *testing.T) {
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, ".boss-skills.json"), []byte(`{"reviewLedger":{"dir":"custom/ledgers"}}`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	path := filepath.Join(worktree, "custom", "ledgers", "ledger-run-b.json")
	writeReviewLedgerTestFile(t, path, "run-b")
	got, err := loadReviewLedger(worktree, "run-b")
	if err != nil {
		t.Fatalf("loadReviewLedger: %v", err)
	}
	if got.Path != path {
		t.Fatalf("path = %q, want %q", got.Path, path)
	}
}

func TestLoadReviewLedgerHonorsAncestorConfiguredDirectory(t *testing.T) {
	parent := t.TempDir()
	worktree := filepath.Join(parent, "nested", "worktree")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(parent, ".boss-skills.json"), []byte(`{"reviewLedger":{"dir":"custom/ledgers"}}`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	path := filepath.Join(worktree, "custom", "ledgers", "ledger-run-parent.json")
	writeReviewLedgerTestFile(t, path, "run-parent")
	got, err := loadReviewLedger(worktree, "run-parent")
	if err != nil {
		t.Fatalf("loadReviewLedger: %v", err)
	}
	if got.Path != path {
		t.Fatalf("path = %q, want %q", got.Path, path)
	}
}

func TestLoadReviewLedgerDefaultsToNewestLedger(t *testing.T) {
	worktree := t.TempDir()
	gitDir := initReviewLedgerGitRepo(t, worktree)
	oldPath := filepath.Join(gitDir, "boss-review-ledgers", "ledger-old.json")
	newPath := filepath.Join(gitDir, "boss-review-ledgers", "ledger-new.json")
	writeReviewLedgerTestFile(t, oldPath, "old")
	writeReviewLedgerTestFile(t, newPath, "new")
	oldTime := time.Now().Add(-time.Hour)
	newTime := time.Now()
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatalf("chtimes old: %v", err)
	}
	if err := os.Chtimes(newPath, newTime, newTime); err != nil {
		t.Fatalf("chtimes new: %v", err)
	}
	got, err := loadReviewLedger(worktree, "")
	if err != nil {
		t.Fatalf("loadReviewLedger: %v", err)
	}
	if got.RunID != "new" {
		t.Fatalf("run id = %q, want newest run", got.RunID)
	}
}

func TestLoadReviewLedgerNotFoundNamesPath(t *testing.T) {
	worktree := t.TempDir()
	initReviewLedgerGitRepo(t, worktree)
	_, err := loadReviewLedger(worktree, "missing")
	if err == nil || !strings.Contains(err.Error(), "NOT_FOUND:") || !strings.Contains(err.Error(), "ledger-missing.json") {
		t.Fatalf("error = %v, want NOT_FOUND naming ledger path", err)
	}
}

func TestLoadReviewLedgerResolvesGitMetadataDirectoryInLinkedWorktree(t *testing.T) {
	repo := t.TempDir()
	initReviewLedgerGitRepo(t, repo)
	commitReviewLedgerGitRepo(t, repo)
	worktree := filepath.Join(t.TempDir(), "linked")
	if out, err := exec.Command("git", "-C", repo, "worktree", "add", "--quiet", worktree).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".boss-skills.json"), []byte(`{"reviewLedger":{"dir":"./.git/boss-review-ledgers"}}`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	ledgerDir, err := checkoutGitOutput(worktree, "rev-parse", "--path-format=absolute", "--git-path", "boss-review-ledgers")
	if err != nil {
		t.Fatalf("git rev-parse --git-path: %v", err)
	}
	path := filepath.Join(ledgerDir, "ledger-linked.json")
	writeReviewLedgerTestFile(t, path, "linked")
	got, err := loadReviewLedger(worktree, "linked")
	if err != nil {
		t.Fatalf("loadReviewLedger: %v", err)
	}
	if got.Path != path {
		t.Fatalf("path = %q, want %q", got.Path, path)
	}
}

func TestLoadReviewLedgerMalformedSurfacesParseFailure(t *testing.T) {
	worktree := t.TempDir()
	gitDir := initReviewLedgerGitRepo(t, worktree)
	path := filepath.Join(gitDir, "boss-review-ledgers", "ledger-bad.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir ledger dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"schemaVersion":1,"runId":"bad","seededAtMs":100,"rows":[{"name":""}]}`), 0o644); err != nil {
		t.Fatalf("write ledger: %v", err)
	}
	_, err := loadReviewLedger(worktree, "bad")
	if err == nil || !strings.Contains(err.Error(), "missing name") {
		t.Fatalf("error = %v, want row validation failure", err)
	}
}

func TestLoadReviewLedgerRejectsUnsupportedSchemaVersion(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "missing",
			body: `{"runId":"bad","seededAtMs":100,"rows":[]}`,
		},
		{
			name: "future",
			body: `{"schemaVersion":2,"runId":"bad","seededAtMs":100,"rows":[]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			worktree := t.TempDir()
			gitDir := initReviewLedgerGitRepo(t, worktree)
			path := filepath.Join(gitDir, "boss-review-ledgers", "ledger-bad.json")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatalf("mkdir ledger dir: %v", err)
			}
			if err := os.WriteFile(path, []byte(tt.body), 0o644); err != nil {
				t.Fatalf("write ledger: %v", err)
			}
			_, err := loadReviewLedger(worktree, "bad")
			if err == nil || !strings.Contains(err.Error(), "unsupported schemaVersion") {
				t.Fatalf("error = %v, want unsupported schemaVersion", err)
			}
		})
	}
}

func TestReviewLedgerDefaultMatchesSkillConfig(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "skills-toolbox", "skill-config.mjs"))
	if err != nil {
		t.Fatalf("read skill-config: %v", err)
	}
	needle := `reviewLedger: { dir: '` + defaultReviewLedgerDir + `' }`
	if !strings.Contains(string(raw), needle) {
		t.Fatalf("skill-config default does not contain %q", needle)
	}
}

func sortedStrings(values []string) []string {
	out := append([]string(nil), values...)
	sortStrings(out)
	return out
}

func sortStrings(values []string) {
	for i := 0; i < len(values); i++ {
		for j := i + 1; j < len(values); j++ {
			if values[j] < values[i] {
				values[i], values[j] = values[j], values[i]
			}
		}
	}
}
