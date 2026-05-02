package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
)

// CreateCronJob registers a new scheduled prompt and adds it to the live
// scheduler when enabled. The schedule is validated by the scheduler's parser
// when AddJob runs; an invalid expression is reported as InvalidArgument and
// the row is rolled back.
func (s *Server) CreateCronJob(ctx context.Context, req *connect.Request[pb.CreateCronJobRequest]) (*connect.Response[pb.CreateCronJobResponse], error) {
	msg := req.Msg
	if strings.TrimSpace(msg.RepoId) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("repo_id is required"))
	}
	if strings.TrimSpace(msg.Name) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("name is required"))
	}
	if strings.TrimSpace(msg.Prompt) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("prompt is required"))
	}
	if strings.TrimSpace(msg.Schedule) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("schedule is required"))
	}

	params := db.CreateCronJobParams{
		RepoID:   msg.RepoId,
		Name:     strings.TrimSpace(msg.Name),
		Prompt:   msg.Prompt,
		Schedule: strings.TrimSpace(msg.Schedule),
		Enabled:  msg.Enabled,
	}
	if tz := strings.TrimSpace(msg.Timezone); tz != "" {
		params.Timezone = &tz
	}

	job, err := s.cronJobs.Create(ctx, params)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create cron job: %w", err))
	}

	if job.Enabled {
		if err := s.cronScheduler.AddJob(job); err != nil {
			// Roll back so the DB doesn't keep an unparseable schedule that
			// the scheduler refuses to load on next startup.
			if delErr := s.cronJobs.Delete(ctx, job.ID); delErr != nil {
				s.logger.Warn().Err(delErr).Str("cron_job_id", job.ID).
					Msg("CreateCronJob: rollback delete failed after AddJob error")
			}
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("schedule cron job: %w", err))
		}
	}

	return connect.NewResponse(&pb.CreateCronJobResponse{CronJob: cronJobToProto(ctx, job, s.sessions)}), nil
}

// ListCronJobs returns all cron jobs, optionally filtered by repo_id.
func (s *Server) ListCronJobs(ctx context.Context, req *connect.Request[pb.ListCronJobsRequest]) (*connect.Response[pb.ListCronJobsResponse], error) {
	var (
		jobs []*models.CronJob
		err  error
	)
	if req.Msg.RepoId != nil && *req.Msg.RepoId != "" {
		jobs, err = s.cronJobs.ListByRepo(ctx, *req.Msg.RepoId)
	} else {
		jobs, err = s.cronJobs.List(ctx)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list cron jobs: %w", err))
	}

	out := make([]*pb.CronJob, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, cronJobToProto(ctx, j, s.sessions))
	}
	return connect.NewResponse(&pb.ListCronJobsResponse{CronJobs: out}), nil
}

// GetCronJob returns a single cron job by id.
func (s *Server) GetCronJob(ctx context.Context, req *connect.Request[pb.GetCronJobRequest]) (*connect.Response[pb.GetCronJobResponse], error) {
	if strings.TrimSpace(req.Msg.Id) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}
	job, err := s.cronJobs.Get(ctx, req.Msg.Id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("cron job not found: %s", req.Msg.Id))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get cron job: %w", err))
	}
	return connect.NewResponse(&pb.GetCronJobResponse{CronJob: cronJobToProto(ctx, job, s.sessions)}), nil
}

// UpdateCronJob mutates an existing cron job and refreshes its scheduler
// registration. A field is only updated when the request has set the
// corresponding optional pointer.
func (s *Server) UpdateCronJob(ctx context.Context, req *connect.Request[pb.UpdateCronJobRequest]) (*connect.Response[pb.UpdateCronJobResponse], error) {
	msg := req.Msg
	if strings.TrimSpace(msg.Id) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}

	params := db.UpdateCronJobParams{}
	if msg.Name != nil {
		v := strings.TrimSpace(*msg.Name)
		if v == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("name cannot be empty"))
		}
		params.Name = &v
	}
	if msg.Prompt != nil {
		params.Prompt = msg.Prompt
	}
	if msg.Schedule != nil {
		v := strings.TrimSpace(*msg.Schedule)
		if v == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("schedule cannot be empty"))
		}
		params.Schedule = &v
	}
	if msg.Timezone != nil {
		// Empty timezone clears the field (uses daemon-local). UpdateCronJobParams
		// uses **string: outer non-nil = update, inner nil = SET NULL.
		var tz *string
		if v := strings.TrimSpace(*msg.Timezone); v != "" {
			tz = &v
		}
		params.Timezone = &tz
	}
	if msg.Enabled != nil {
		params.Enabled = msg.Enabled
	}

	job, err := s.cronJobs.Update(ctx, msg.Id, params)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("cron job not found: %s", msg.Id))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("update cron job: %w", err))
	}

	// Sync the scheduler. Always RemoveJob first; then AddJob iff enabled.
	// UpdateJob would also work but keeping the two steps explicit makes the
	// disabled-job case (no AddJob, no spurious schedule parse error) obvious.
	s.cronScheduler.RemoveJob(job.ID)
	if job.Enabled {
		if err := s.cronScheduler.AddJob(job); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("schedule cron job: %w", err))
		}
	}

	return connect.NewResponse(&pb.UpdateCronJobResponse{CronJob: cronJobToProto(ctx, job, s.sessions)}), nil
}

// DeleteCronJob removes a cron job from the scheduler and the database.
func (s *Server) DeleteCronJob(ctx context.Context, req *connect.Request[pb.DeleteCronJobRequest]) (*connect.Response[pb.DeleteCronJobResponse], error) {
	if strings.TrimSpace(req.Msg.Id) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}
	s.cronScheduler.RemoveJob(req.Msg.Id)
	if err := s.cronJobs.Delete(ctx, req.Msg.Id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("cron job not found: %s", req.Msg.Id))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("delete cron job: %w", err))
	}
	return connect.NewResponse(&pb.DeleteCronJobResponse{}), nil
}

// RunCronJobNow fires a cron job immediately, bypassing the schedule but
// honoring the same overlap and concurrency-cap rules as scheduled fires.
func (s *Server) RunCronJobNow(ctx context.Context, req *connect.Request[pb.RunCronJobNowRequest]) (*connect.Response[pb.RunCronJobNowResponse], error) {
	if strings.TrimSpace(req.Msg.Id) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}
	sess, skipped, err := s.cronScheduler.RunNow(ctx, req.Msg.Id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("run cron job now: %w", err))
	}
	resp := &pb.RunCronJobNowResponse{SkippedReason: skipped}
	if sess != nil {
		resp.Session = SessionToProto(sess)
	}
	return connect.NewResponse(resp), nil
}
