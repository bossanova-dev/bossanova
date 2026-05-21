package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
