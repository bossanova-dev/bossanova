package server

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/factoryhealth"
	"github.com/recurser/bossd/internal/verifyadmission"
	"github.com/recurser/bossd/internal/verifyjob"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// GetFactoryHealth reads each source independently so an unavailable source cannot imply health.
func (s *Server) GetFactoryHealth(ctx context.Context, req *connect.Request[pb.GetFactoryHealthRequest]) (*connect.Response[pb.GetFactoryHealthResponse], error) {
	repo, err := s.factoryHealthRepo(ctx, req.Msg)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	f := factoryhealth.Facts{}
	s.factoryHealthNotes(ctx, &f)
	s.factoryHealthCrons(ctx, repo.ID, &f)
	s.factoryHealthPublishing(&f)
	s.factoryHealthBacklog(ctx, repo.ID, &f)
	s.factoryHealthRemote(ctx, repo, &f)
	if s.factoryHealthVersions != nil {
		var versionErr error
		f.DaemonVersion, f.SkillsVersion, f.DaemonOutdated, versionErr = s.factoryHealthVersions(ctx, repo.LocalPath)
		if versionErr != nil {
			f.Unreadable = append(f.Unreadable, "daemon version")
		}
	}
	resp := &pb.GetFactoryHealthResponse{CheckedAt: timestamppb.New(now)}
	for _, p := range factoryhealth.Compute(f, now) {
		problem := &pb.FactoryHealthProblem{Code: p.Code, Text: p.Text}
		if !p.StartedAt.IsZero() {
			problem.StartedAt = timestamppb.New(p.StartedAt)
		}
		resp.Problems = append(resp.Problems, problem)
	}
	return connect.NewResponse(resp), nil
}

func (s *Server) factoryHealthRepo(ctx context.Context, req *pb.GetFactoryHealthRequest) (*domain.Repo, error) {
	if (req.RepoId == "") == (req.WorkspacePath == "") {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("set exactly one of repo_id or workspace_path"))
	}
	if req.RepoId != "" {
		repo, err := s.repos.Get(ctx, req.RepoId)
		if errors.Is(err, domain.ErrNotFound) || (err == nil && repo == nil) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("repository not found"))
		}
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get repository: %w", err))
		}
		return repo, nil
	}
	path, err := filepath.Abs(req.WorkspacePath)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid workspace path"))
	}
	repo, _, err := s.workspaceOwner(ctx, path)
	if err != nil {
		return nil, err
	}
	if repo == nil {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("path is outside registered workspaces"))
	}
	return repo, nil
}

func (s *Server) factoryHealthNotes(ctx context.Context, f *factoryhealth.Facts) {
	if s.notes == nil {
		f.Unreadable = append(f.Unreadable, "notes")
		return
	}
	notes, err := s.notes.List(ctx, domain.ListNotesFilter{Tags: []string{"second-model"}})
	if err != nil {
		f.Unreadable = append(f.Unreadable, "notes")
		return
	}
	for _, n := range notes {
		if n != nil && slices.Contains(n.Tags, "health") {
			f.SecondModelNotes = append(f.SecondModelNotes, factoryhealth.Note{Tags: n.Tags, Body: n.Body, CreatedAt: n.CreatedAt})
		}
	}
}

func (s *Server) factoryHealthCrons(ctx context.Context, repoID string, f *factoryhealth.Facts) {
	if s.cronJobs == nil {
		f.Unreadable = append(f.Unreadable, "cron jobs")
		return
	}
	jobs, err := s.cronJobs.ListByRepo(ctx, repoID)
	if err != nil {
		f.Unreadable = append(f.Unreadable, "cron jobs")
		return
	}
	for _, job := range jobs {
		if job == nil || !job.IsEnabled || !verifyjob.IsVerifyJob(job) {
			continue
		}
		f.VerifyEnabled = true
		fact := factoryhealth.CronFact{Name: job.Name, LastRunAt: job.LastRunAt}
		if job.LastRunOutcome != nil {
			fact.LastOutcome = string(*job.LastRunOutcome)
			fact.Failing = isCronFailureOutcome(*job.LastRunOutcome)
		}
		f.VerifyCrons = append(f.VerifyCrons, fact)
	}
}

func (s *Server) factoryHealthPublishing(f *factoryhealth.Facts) {
	if s.evidenceBucketOpener == nil {
		return
	}
	_, err := s.evidenceBucketOpener.OpenEvidenceBucket()
	switch {
	case err == nil:
		f.PublishingConfigured = true
	case errors.Is(err, domain.ErrEvidenceUploadNotConfigured), errors.Is(err, domain.ErrEvidenceDestinationNotConfigured):
	default:
		f.Unreadable = append(f.Unreadable, "publishing")
		f.PublishingConfigured = true
	}
}

func (s *Server) factoryHealthBacklog(ctx context.Context, repoID string, f *factoryhealth.Facts) {
	if s.sessions == nil {
		f.Unreadable = append(f.Unreadable, "sessions")
		return
	}
	sessions, err := s.sessions.ListActive(ctx, repoID)
	if err != nil {
		f.Unreadable = append(f.Unreadable, "sessions")
		return
	}
	if s.displayTracker == nil || s.checkSnapshots == nil {
		return
	}
	seen := make(map[int]bool)
	for _, sess := range sessions {
		if sess == nil || sess.PRNumber == nil || *sess.PRNumber <= 0 {
			continue
		}
		entry := s.displayTracker.Get(sess.ID)
		if entry == nil || entry.Status != vcs.DisplayStatusVerifying || entry.HeadSHA == "" {
			continue
		}
		current, start, _, tracked, err := s.checkSnapshots.GetAttentionStartsBySessionHead(ctx, sess.ID, entry.HeadSHA)
		if err != nil {
			if !slices.Contains(f.Unreadable, "check snapshots") {
				f.Unreadable = append(f.Unreadable, "check snapshots")
			}
			continue
		}
		if tracked && current == int(vcs.DisplayStatusVerifying) && !seen[*sess.PRNumber] {
			f.Verifying = append(f.Verifying, factoryhealth.VerifyingHead{PR: *sess.PRNumber, StartedAt: start})
			seen[*sess.PRNumber] = true
		}
	}
}

// factoryHealthRemote reports the admitted PRs this host's newest verify scan
// left to another machine's session. An unreadable admission file is named,
// never read as "nothing remote".
func (s *Server) factoryHealthRemote(ctx context.Context, repo *domain.Repo, f *factoryhealth.Facts) {
	path, err := verifyadmission.Path(ctx, repo.LocalPath)
	var snapshot verifyadmission.Snapshot
	if err == nil {
		_, snapshot, err = s.loadVerifyAdmission(ctx, repo.ID, path)
	}
	if err != nil {
		f.Unreadable = append(f.Unreadable, "verify admission")
		return
	}
	f.RemotePRs = snapshot.RemotePRs()
}
