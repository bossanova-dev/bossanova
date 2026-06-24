package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossalib/models"

	"github.com/recurser/bossd/internal/db"
)

func TestParseGitHubOAuthScopes(t *testing.T) {
	scopes, found := parseGitHubOAuthScopes("HTTP/2 200\r\nx-oauth-scopes: repo, workflow, read:org\r\nx-accepted-oauth-scopes: user\r\n")

	if !found {
		t.Fatal("expected x-oauth-scopes header to be found")
	}
	for _, scope := range []string{"repo", "workflow", "read:org"} {
		if !scopes[scope] {
			t.Errorf("expected scope %q to be present in %v", scope, scopes)
		}
	}
}

func TestParseGitHubOAuthScopesMissingHeader(t *testing.T) {
	scopes, found := parseGitHubOAuthScopes("HTTP/2 200\r\nx-accepted-oauth-scopes: user\r\n")

	if found {
		t.Fatal("expected x-oauth-scopes header to be absent")
	}
	if len(scopes) != 0 {
		t.Fatalf("expected no scopes, got %v", scopes)
	}
}

func TestGitHubWorkflowScopeCheckDoesNotTreatAcceptedPermissionsAsGrants(t *testing.T) {
	binDir := t.TempDir()
	ghPath := filepath.Join(binDir, "gh")
	if err := os.WriteFile(ghPath, []byte("#!/bin/sh\nprintf 'HTTP/2 200\\r\\nx-accepted-github-permissions: workflows=write; contents=write; pull_requests=write\\r\\n\\r\\n{}\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	check := githubWorkflowScopeCheck(context.Background())

	if check.GetOk() {
		t.Fatalf("expected accepted endpoint permissions without OAuth scopes to fail, detail=%q", check.GetDetail())
	}
	if !strings.Contains(check.GetDetail(), "cannot verify fine-grained") {
		t.Fatalf("detail = %q, want fine-grained verification limit detail", check.GetDetail())
	}
}

func TestGitHubWorkflowScopeCheckFailsWhenWorkflowPermissionIsUnknown(t *testing.T) {
	binDir := t.TempDir()
	ghPath := filepath.Join(binDir, "gh")
	if err := os.WriteFile(ghPath, []byte("#!/bin/sh\nprintf 'HTTP/2 200\\r\\nx-accepted-oauth-scopes: user\\r\\n\\r\\n{}\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	check := githubWorkflowScopeCheck(context.Background())

	if check.GetOk() {
		t.Fatalf("expected missing oauth scopes without fine-grained workflow permission to fail, detail=%q", check.GetDetail())
	}
	if !strings.Contains(check.GetDetail(), "cannot verify fine-grained") {
		t.Fatalf("detail = %q, want unknown permission detail", check.GetDetail())
	}
}

type repairDoctorTaskMappingStore struct {
	failures []*models.TaskMapping
	err      error
}

func (s *repairDoctorTaskMappingStore) Create(context.Context, db.CreateTaskMappingParams) (*models.TaskMapping, error) {
	return nil, errors.New("not implemented")
}

func (s *repairDoctorTaskMappingStore) Get(context.Context, string) (*models.TaskMapping, error) {
	return nil, errors.New("not implemented")
}

func (s *repairDoctorTaskMappingStore) GetByExternalID(context.Context, string) (*models.TaskMapping, error) {
	return nil, errors.New("not implemented")
}

func (s *repairDoctorTaskMappingStore) GetBySessionID(context.Context, string) (*models.TaskMapping, error) {
	return nil, errors.New("not implemented")
}

func (s *repairDoctorTaskMappingStore) Update(context.Context, string, db.UpdateTaskMappingParams) (*models.TaskMapping, error) {
	return nil, errors.New("not implemented")
}

func (s *repairDoctorTaskMappingStore) Delete(context.Context, string) error {
	return errors.New("not implemented")
}

func (s *repairDoctorTaskMappingStore) ListPending(context.Context) ([]*models.TaskMapping, error) {
	return nil, errors.New("not implemented")
}

func (s *repairDoctorTaskMappingStore) ListRecentFailures(context.Context, int) ([]*models.TaskMapping, error) {
	return s.failures, s.err
}

func (s *repairDoctorTaskMappingStore) FailOrphanedMappings(context.Context) (int64, error) {
	return 0, errors.New("not implemented")
}

func TestRecentTaskMappingFailuresCheckReportsFailuresWithActionableDetail(t *testing.T) {
	lastError := "auto-merge failed: workflow scope denied; fix command: gh workflow enable ci.yml"
	srv := &Server{
		taskMappings: &repairDoctorTaskMappingStore{
			failures: []*models.TaskMapping{{
				ExternalID: "dependabot:pr:https://github.com/acme/widgets:42",
				LastError:  &lastError,
			}},
		},
	}

	check := srv.recentTaskMappingFailuresCheck(context.Background())

	if check.GetOk() {
		t.Fatal("expected check to fail")
	}
	detail := check.GetDetail()
	for _, want := range []string{
		"recent task automation failures need attention",
		"PR #42",
		"dependabot:pr:https://github.com/acme/widgets:42",
		"auto-merge failed: workflow scope denied",
		"gh workflow enable ci.yml",
	} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail missing %q: %q", want, detail)
		}
	}
}

func TestRecentTaskMappingFailuresCheckPassesWhenNoFailuresExist(t *testing.T) {
	srv := &Server{taskMappings: &repairDoctorTaskMappingStore{}}

	check := srv.recentTaskMappingFailuresCheck(context.Background())

	if !check.GetOk() {
		t.Fatal("expected check to pass")
	}
	if !strings.Contains(check.GetDetail(), "no recent failed task mappings") {
		t.Fatalf("detail = %q, want no recent failed task mappings", check.GetDetail())
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestAgentLogsDirCheck(t *testing.T) {
	t.Run("empty dir reports unset", func(t *testing.T) {
		got := agentLogsDirCheck("")
		if got.GetOk() {
			t.Fatalf("empty dir should not be Ok, got %+v", got)
		}
		if !strings.Contains(got.GetDetail(), "unset") {
			t.Errorf("detail = %q, want mention of unset", got.GetDetail())
		}
	})

	t.Run("missing dir fails stat", func(t *testing.T) {
		got := agentLogsDirCheck(filepath.Join(t.TempDir(), "does-not-exist"))
		if got.GetOk() {
			t.Fatalf("missing dir should not be Ok, got %+v", got)
		}
		if !strings.Contains(got.GetDetail(), "stat") {
			t.Errorf("detail = %q, want mention of stat", got.GetDetail())
		}
	})

	t.Run("file path is not a directory", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "afile")
		mustWriteFile(t, file, "x")
		got := agentLogsDirCheck(file)
		if got.GetOk() {
			t.Fatalf("file should not be Ok, got %+v", got)
		}
		if !strings.Contains(got.GetDetail(), "not a directory") {
			t.Errorf("detail = %q, want mention of not a directory", got.GetDetail())
		}
	})

	t.Run("writable dir passes and cleans up the probe", func(t *testing.T) {
		dir := t.TempDir()
		got := agentLogsDirCheck(dir)
		if !got.GetOk() {
			t.Fatalf("writable dir should be Ok, got %+v", got)
		}
		if got.GetDetail() != dir {
			t.Errorf("detail = %q, want %q", got.GetDetail(), dir)
		}
		// The writability probe must not leave a temp file behind.
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Errorf("probe file left behind: %v", entries)
		}
	})

	t.Run("unwritable dir fails the probe", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses directory permissions")
		}
		dir := filepath.Join(t.TempDir(), "ro")
		if err := os.Mkdir(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		got := agentLogsDirCheck(dir)
		if got.GetOk() {
			t.Fatalf("unwritable dir should not be Ok, got %+v", got)
		}
		if !strings.Contains(got.GetDetail(), "probe") {
			t.Errorf("detail = %q, want mention of probe", got.GetDetail())
		}
	})
}

func TestRecentRepairLogs(t *testing.T) {
	t.Run("missing dir returns nil", func(t *testing.T) {
		if got := recentRepairLogs(filepath.Join(t.TempDir(), "nope")); got != nil {
			t.Errorf("missing dir should return nil, got %v", got)
		}
	})

	t.Run("filters non-matching entries and sorts newest first", func(t *testing.T) {
		dir := t.TempDir()
		// Entries that must be ignored: wrong prefix/suffix and a directory
		// that happens to match the naming pattern.
		mustWriteFile(t, filepath.Join(dir, "other.log"), "ignored")
		mustWriteFile(t, filepath.Join(dir, "repair-1.txt"), "ignored")
		if err := os.Mkdir(filepath.Join(dir, "repair-dir.log"), 0o700); err != nil {
			t.Fatal(err)
		}

		oldPath := filepath.Join(dir, "repair-old.log")
		newPath := filepath.Join(dir, "repair-new.log")
		mustWriteFile(t, oldPath, "old head\nrest")
		mustWriteFile(t, newPath, "new head\nrest")
		base := time.Now()
		if err := os.Chtimes(oldPath, base, base.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(newPath, base, base); err != nil {
			t.Fatal(err)
		}

		got := recentRepairLogs(dir)
		if len(got) != 2 {
			t.Fatalf("want 2 snapshots, got %d: %+v", len(got), got)
		}
		if !strings.HasSuffix(got[0].GetPath(), "repair-new.log") {
			t.Errorf("newest first: got[0].Path = %q", got[0].GetPath())
		}
		if got[0].GetHeadLine() != "new head" {
			t.Errorf("head line = %q, want %q", got[0].GetHeadLine(), "new head")
		}
		if got[1].GetSizeBytes() == 0 {
			t.Errorf("size should be non-zero for %q", got[1].GetPath())
		}
	})

	t.Run("caps at recentRepairLogsLimit", func(t *testing.T) {
		dir := t.TempDir()
		for i := range recentRepairLogsLimit + 2 {
			mustWriteFile(t, filepath.Join(dir, fmt.Sprintf("repair-%02d.log", i)), "head")
		}
		got := recentRepairLogs(dir)
		if len(got) != recentRepairLogsLimit {
			t.Fatalf("want %d snapshots, got %d", recentRepairLogsLimit, len(got))
		}
	})
}

func TestReadFirstNonEmptyLine(t *testing.T) {
	t.Run("missing file returns empty", func(t *testing.T) {
		if got := readFirstNonEmptyLine(filepath.Join(t.TempDir(), "nope")); got != "" {
			t.Errorf("missing file = %q, want empty", got)
		}
	})

	t.Run("returns first line trimmed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "f")
		mustWriteFile(t, path, "  first line  \nsecond line\n")
		if got := readFirstNonEmptyLine(path); got != "first line" {
			t.Errorf("got %q, want %q", got, "first line")
		}
	})

	t.Run("single line without trailing newline", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "f")
		mustWriteFile(t, path, "only line")
		if got := readFirstNonEmptyLine(path); got != "only line" {
			t.Errorf("got %q, want %q", got, "only line")
		}
	})

	t.Run("leading newline yields an empty first line", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "f")
		mustWriteFile(t, path, "\nsecond line")
		if got := readFirstNonEmptyLine(path); got != "" {
			t.Errorf("got %q, want empty (first line is blank)", got)
		}
	})

	t.Run("empty file returns empty", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "f")
		mustWriteFile(t, path, "")
		if got := readFirstNonEmptyLine(path); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}

func TestClaudeVersionCheck(t *testing.T) {
	t.Run("missing claude on PATH fails via LookPath", func(t *testing.T) {
		// An empty PATH makes exec.LookPath("claude") fail, exercising the
		// first guard. The detail must name LookPath so the operator knows
		// the binary was never resolved (vs. resolved-but-broken).
		t.Setenv("PATH", t.TempDir())

		check := claudeVersionCheck(context.Background())

		if check.GetOk() {
			t.Fatalf("missing claude should fail, detail=%q", check.GetDetail())
		}
		if !strings.Contains(check.GetDetail(), "exec.LookPath") {
			t.Fatalf("detail = %q, want LookPath-specific failure (not the --version path)", check.GetDetail())
		}
	})

	t.Run("claude present but --version exits non-zero fails", func(t *testing.T) {
		binDir := t.TempDir()
		claudePath := filepath.Join(binDir, "claude")
		if err := os.WriteFile(claudePath, []byte("#!/bin/sh\necho boom >&2\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

		check := claudeVersionCheck(context.Background())

		if check.GetOk() {
			t.Fatalf("a non-zero `claude --version` must fail the check, detail=%q", check.GetDetail())
		}
		if !strings.Contains(check.GetDetail(), "--version") {
			t.Fatalf("detail = %q, want mention of the --version probe", check.GetDetail())
		}
	})

	t.Run("claude present and --version succeeds passes", func(t *testing.T) {
		binDir := t.TempDir()
		claudePath := filepath.Join(binDir, "claude")
		if err := os.WriteFile(claudePath, []byte("#!/bin/sh\necho '1.2.3 (Claude Code)'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

		check := claudeVersionCheck(context.Background())

		if !check.GetOk() {
			t.Fatalf("a healthy `claude --version` must pass, detail=%q", check.GetDetail())
		}
		if !strings.Contains(check.GetDetail(), "1.2.3") {
			t.Fatalf("detail = %q, want the version output echoed back", check.GetDetail())
		}
	})
}

func TestShortID(t *testing.T) {
	t.Run("truncates long id to 8", func(t *testing.T) {
		if got := shortID("1234567890"); got != "12345678" {
			t.Errorf("got %q, want %q", got, "12345678")
		}
	})

	t.Run("short id returned unchanged", func(t *testing.T) {
		if got := shortID("abc"); got != "abc" {
			t.Errorf("got %q, want %q", got, "abc")
		}
	})

	t.Run("exactly eight characters returned unchanged", func(t *testing.T) {
		if got := shortID("12345678"); got != "12345678" {
			t.Errorf("got %q, want %q", got, "12345678")
		}
	})
}
