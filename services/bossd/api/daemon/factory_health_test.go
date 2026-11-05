package server

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/status"
)

type healthRepos struct {
	repoStore
	repo *domain.Repo
}

func (r healthRepos) Get(_ context.Context, id string) (*domain.Repo, error) {
	if id != r.repo.ID {
		return nil, domain.ErrNotFound
	}
	return r.repo, nil
}
func (r healthRepos) List(context.Context) ([]*domain.Repo, error) {
	return []*domain.Repo{r.repo}, nil
}

type healthSessions struct {
	sessionStore
	rows []*domain.Session
	err  error
}

func (s healthSessions) List(context.Context, string) ([]*domain.Session, error) {
	return s.rows, s.err
}
func (s healthSessions) ListActive(context.Context, string) ([]*domain.Session, error) {
	return s.rows, s.err
}

type healthNotes struct {
	noteStore
	rows []*domain.Note
	err  error
}

func (n healthNotes) List(_ context.Context, f domain.ListNotesFilter) ([]*domain.Note, error) {
	if f.RepoID != nil || len(f.Tags) != 1 || f.Tags[0] != "second-model" {
		return nil, errors.New("not host-wide")
	}
	return n.rows, n.err
}

type healthCrons struct {
	cronJobStore
	rows []*domain.CronJob
	err  error
}

func (c healthCrons) ListByRepo(context.Context, string) ([]*domain.CronJob, error) {
	return c.rows, c.err
}

type healthSnapshots struct {
	checkSnapshotReader
	tracked bool
	status  int
	err     error
}

func (s healthSnapshots) GetAttentionStartsBySessionHead(context.Context, string, string) (int, time.Time, time.Time, bool, error) {
	return s.status, time.Now().Add(-2 * time.Hour), time.Time{}, s.tracked, s.err
}

// newHealthServer serves one repository whose checkout is a fresh git repo
// with no verify admission file.
func newHealthServer(t *testing.T) *Server {
	t.Helper()
	repoPath := t.TempDir()
	cmd := exec.CommandContext(t.Context(), "git", "init", "--quiet", repoPath)
	cmd.WaitDelay = time.Second
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %s: %v", out, err)
	}
	return &Server{repos: healthRepos{repo: &domain.Repo{ID: "a", LocalPath: repoPath}}, sessions: healthSessions{}, notes: healthNotes{}, cronJobs: healthCrons{}}
}

// writeHealthAdmission writes content as the health server's verify admission file.
func writeHealthAdmission(t *testing.T, s *Server, content string) {
	t.Helper()
	dir := filepath.Join(s.repos.(healthRepos).repo.LocalPath, ".git", "boss-verify")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "verify-admission.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestFactoryHealthRemoteSessions_Sessions_AC208201(t *testing.T) {
	s := newHealthServer(t)
	writeHealthAdmission(t, s, `{"version":1,"scopes":{"shared":{"scannedAt":"2026-10-24T00:00:00.000Z","prs":{`+
		`"42":{"head":"a","state":"admitted","reason":"ticket"},"43":{"head":"b","state":"pending","reason":"draft"}},"remote":[43,42]}}}`)
	resp, err := s.GetFactoryHealth(context.Background(), connect.NewRequest(&pb.GetFactoryHealthRequest{RepoId: "a"}))
	if err != nil {
		t.Fatal(err)
	}
	problems := resp.Msg.Problems
	if len(problems) != 1 || problems[0].Code != "remote-sessions" || problems[0].StartedAt != nil ||
		!strings.Contains(problems[0].Text, "(#42)") || !strings.HasPrefix(problems[0].Text, "1 PR ") {
		t.Fatalf("problems: %v", problems)
	}
}
func TestFactoryHealthSelectors(t *testing.T) {
	s := newHealthServer(t)
	for _, tc := range []struct {
		req  *pb.GetFactoryHealthRequest
		code connect.Code
	}{{&pb.GetFactoryHealthRequest{}, connect.CodeInvalidArgument}, {&pb.GetFactoryHealthRequest{RepoId: "a", WorkspacePath: "/tmp"}, connect.CodeInvalidArgument}, {&pb.GetFactoryHealthRequest{RepoId: "missing"}, connect.CodeNotFound}, {&pb.GetFactoryHealthRequest{WorkspacePath: t.TempDir()}, connect.CodePermissionDenied}} {
		_, err := s.GetFactoryHealth(context.Background(), connect.NewRequest(tc.req))
		if connect.CodeOf(err) != tc.code {
			t.Fatalf("request %v: %v", tc.req, err)
		}
	}
}
func TestFactoryHealthHealthyAndHostWide_Sessions_AC208201(t *testing.T) {
	s := newHealthServer(t)
	for _, req := range []*pb.GetFactoryHealthRequest{{RepoId: "a"}, {WorkspacePath: s.repos.(healthRepos).repo.LocalPath}} {
		resp, err := s.GetFactoryHealth(context.Background(), connect.NewRequest(req))
		if err != nil || len(resp.Msg.Problems) != 0 || resp.Msg.CheckedAt == nil {
			t.Fatalf("healthy: %v, %v", resp, err)
		}
	}
	s.notes = healthNotes{rows: []*domain.Note{{RepoID: "b", Tags: []string{"health", "second-model"}, CreatedAt: time.Now(), Body: "second-model review unavailable: claude not_authed since 2026-10-19T00:00:00Z, 3 consecutive failures on host private"}}}
	resp, err := s.GetFactoryHealth(context.Background(), connect.NewRequest(&pb.GetFactoryHealthRequest{RepoId: "a"}))
	if err != nil || len(resp.Msg.Problems) != 1 || resp.Msg.Problems[0].Code != "second-model-off" {
		t.Fatalf("host-wide: %v, %v", resp, err)
	}
}
func TestFactoryHealthPartialAndBacklog(t *testing.T) {
	s := newHealthServer(t)
	s.cronJobs = healthCrons{err: errors.New("private error")}
	pr := 42
	s.sessions = healthSessions{rows: []*domain.Session{{ID: "session", PRNumber: &pr}}}
	s.displayTracker = status.NewDisplayTracker()
	s.displayTracker.Set("session", vcs.DisplayInfo{Status: vcs.DisplayStatusVerifying, HeadSHA: "head"})
	for _, tracked := range []bool{false, true} {
		s.checkSnapshots = healthSnapshots{tracked: tracked, status: int(vcs.DisplayStatusVerifying)}
		resp, err := s.GetFactoryHealth(context.Background(), connect.NewRequest(&pb.GetFactoryHealthRequest{RepoId: "a"}))
		if err != nil {
			t.Fatal(err)
		}
		want := 1
		if tracked {
			want = 2
		}
		if len(resp.Msg.Problems) != want || resp.Msg.Problems[want-1].Code != "health-partial" {
			t.Fatalf("tracked %v: %v", tracked, resp.Msg)
		}
	}
}

type healthBucket struct{ err error }

func (b healthBucket) OpenEvidenceBucket() (domain.EvidenceBucket, error) { return nil, b.err }

func TestFactoryHealthMissingPublishingConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"destination", domain.ErrEvidenceDestinationNotConfigured},
		{"wrapped destination", errors.Join(errors.New("open destination"), domain.ErrEvidenceDestinationNotConfigured)},
		{"token", domain.ErrEvidenceUploadNotConfigured},
		{"wrapped token", errors.Join(errors.New("open credential"), domain.ErrEvidenceUploadNotConfigured)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newHealthServer(t)
			s.cronJobs = healthCrons{rows: []*domain.CronJob{{Prompt: "/boss-verify", IsEnabled: true}}}
			s.evidenceBucketOpener = healthBucket{err: tc.err}
			resp, err := s.GetFactoryHealth(t.Context(), connect.NewRequest(&pb.GetFactoryHealthRequest{RepoId: "a"}))
			if err != nil {
				t.Fatal(err)
			}
			if len(resp.Msg.Problems) != 1 || resp.Msg.Problems[0].Code != "publishing-off" {
				t.Fatalf("problems: %v", resp.Msg.Problems)
			}
		})
	}
}

func TestFactoryHealthDegradedSources(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*Server)
		codes []string
	}{
		{"verify failure and publishing off", func(s *Server) {
			outcome := domain.CronJobOutcomeGateFailed
			s.cronJobs = healthCrons{rows: []*domain.CronJob{{Name: "verify", Prompt: "/boss-verify", IsEnabled: true, LastRunOutcome: &outcome}}}
		}, []string{"verify-cron-failing", "publishing-off"}},
		{"healthy verify skips", func(s *Server) {
			outcome := domain.CronJobOutcomeGated
			s.cronJobs = healthCrons{rows: []*domain.CronJob{{Name: "verify", Prompt: "/boss-verify", IsEnabled: true, LastRunOutcome: &outcome}}}
			s.evidenceBucketOpener = healthBucket{}
		}, nil},
		{"unreadable publishing", func(s *Server) {
			s.cronJobs = healthCrons{rows: []*domain.CronJob{{Prompt: "/boss-verify", IsEnabled: true}}}
			s.evidenceBucketOpener = healthBucket{err: errors.New("private token error")}
		}, []string{"health-partial"}},
		{"unreadable notes", func(s *Server) { s.notes = healthNotes{err: errors.New("read failed")} }, []string{"health-partial"}},
		{"unreadable sessions", func(s *Server) { s.sessions = healthSessions{err: errors.New("read failed")} }, []string{"health-partial"}},
		{"older daemon", func(s *Server) {
			s.factoryHealthVersions = func(context.Context, string) (string, string, bool, error) { return "v1.0.0", "v2.0.0", true, nil }
		}, []string{"daemon-outdated"}},
		{"corrupt admission", func(s *Server) { writeHealthAdmission(t, s, "{") }, []string{"health-partial"}},
		{"unsupported admission version", func(s *Server) {
			writeHealthAdmission(t, s, `{"version":2,"scopes":{}}`)
		}, []string{"health-partial"}},
		{"admission path unresolvable", func(s *Server) {
			s.repos = healthRepos{repo: &domain.Repo{ID: "a", LocalPath: t.TempDir()}}
		}, []string{"health-partial"}},
		{"unreadable version", func(s *Server) {
			s.factoryHealthVersions = func(context.Context, string) (string, string, bool, error) {
				return "", "", false, errors.New("unreadable")
			}
		}, []string{"health-partial"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newHealthServer(t)
			tc.setup(s)
			resp, err := s.GetFactoryHealth(context.Background(), connect.NewRequest(&pb.GetFactoryHealthRequest{RepoId: "a"}))
			if err != nil {
				t.Fatal(err)
			}
			if len(resp.Msg.Problems) != len(tc.codes) {
				t.Fatalf("problems: %v", resp.Msg.Problems)
			}
			for i, code := range tc.codes {
				if resp.Msg.Problems[i].Code != code {
					t.Fatalf("problems: %v", resp.Msg.Problems)
				}
			}
		})
	}
}
