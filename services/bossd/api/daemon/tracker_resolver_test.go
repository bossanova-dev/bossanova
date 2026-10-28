package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/session"
)

type recordingGitHubIssueReader struct {
	calls []string
	found bool
	err   error
}

func (f *recordingGitHubIssueReader) OpenIssueURL(_ context.Context, host, owner, repo string, number int) (string, bool, error) {
	f.calls = append(f.calls, fmt.Sprintf("%s/%s/%s/%d", host, owner, repo, number))
	return "https://github.com/o/r/issues/2", f.found, f.err
}
func trackerConfiguredRepo(t *testing.T, adapter, repository string) *models.Repo {
	t.Helper()
	dir := t.TempDir()
	body := fmt.Sprintf(`{"adapters":{"tracker":%q},"trackerConfig":{"github":{"repository":%q}}}`, adapter, repository)
	if err := os.WriteFile(filepath.Join(dir, ".boss-skills.json"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return &models.Repo{LocalPath: dir}
}
func TestTrackerResolver_GitHub_Sessions_AC116(t *testing.T) {
	for _, input := range []string{"GH-2", "#2", "2", "https://GitHub.com/O/R/issues/2"} {
		t.Run(input, func(t *testing.T) {
			reader := &recordingGitHubIssueReader{found: true}
			r := NewTrackerResolver(nil, reader)
			repo := trackerConfiguredRepo(t, "github", "o/r")
			if err := r.CheckCandidate(repo, input); err != nil {
				t.Fatal(err)
			}
			id, url, err := r.ResolveTrackerIssue(context.Background(), repo, input)
			if err != nil || id != "GH-2" || url != "https://github.com/o/r/issues/2" {
				t.Fatalf("resolved=(%q,%q,%v)", id, url, err)
			}
			if len(reader.calls) != 1 || reader.calls[0] != "github.com/o/r/2" {
				t.Fatalf("lookup=%v", reader.calls)
			}
		})
	}
}
func TestTrackerResolver_CheckCandidate(t *testing.T) {
	for _, tt := range []struct {
		name, adapter, repository, input string
		want                             error
	}{
		{"wrong repository", "github", "o/r", "https://github.com/other/r/issues/2", session.ErrTrackerIssueNotFound},
		{"linear candidate", "github", "o/r", "BOS-12", session.ErrTrackerIssueNotFound},
		{"missing repository", "github", "", "GH-2", session.ErrTrackerNotConfigured},
		{"invalid repository", "github", "https://github.com/o/r", "GH-2", session.ErrTrackerNotConfigured},
		{"unsupported adapter", "custom", "o/r", "GH-2", session.ErrTrackerNotConfigured},
		{"linear without key", "linear", "", "BOS-12", session.ErrTrackerNotConfigured},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repo := trackerConfiguredRepo(t, tt.adapter, tt.repository)
			reader := &recordingGitHubIssueReader{found: true}
			r := NewTrackerResolver(nil, reader)
			check := r.CheckCandidate(repo, tt.input)
			_, _, err := r.ResolveTrackerIssue(context.Background(), repo, tt.input)
			if !errors.Is(check, tt.want) || !errors.Is(err, tt.want) {
				t.Fatalf("check=%v resolve=%v want %v", check, err, tt.want)
			}
			if len(reader.calls) != 0 {
				t.Fatalf("unexpected calls %v", reader.calls)
			}
			if errors.Is(err, session.ErrTrackerNotConfigured) && (!strings.Contains(err.Error(), tt.adapter) || strings.Contains(err.Error(), "Linear")) {
				t.Fatalf("adapter missing: %v", err)
			}
		})
	}
}
func TestTrackerResolver_GitHubLookupOutcomes(t *testing.T) {
	lookupErr := errors.New("gh failed")
	for _, tt := range []struct {
		name   string
		reader *recordingGitHubIssueReader
		want   error
	}{
		{"not found", &recordingGitHubIssueReader{}, session.ErrTrackerIssueNotFound},
		{"provider error", &recordingGitHubIssueReader{err: lookupErr}, lookupErr},
		{"missing provider", nil, session.ErrTrackerNotConfigured},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var reader GitHubIssueReader
			if tt.reader != nil {
				reader = tt.reader
			}
			_, _, err := NewTrackerResolver(nil, reader).ResolveTrackerIssue(context.Background(), trackerConfiguredRepo(t, "github", "ghe.example/o/r"), "GH-2")
			if !errors.Is(err, tt.want) {
				t.Fatalf("err=%v want %v", err, tt.want)
			}
		})
	}
}
