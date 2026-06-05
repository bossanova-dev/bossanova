package server

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/cron"
	"github.com/recurser/bossd/internal/db"
	"github.com/rs/zerolog"
)

func TestCronJobAPIsPreserveAgentName(t *testing.T) {
	sqlDB := setupServerTestDB(t)
	repos := db.NewRepoStore(sqlDB)
	sessions := db.NewSessionStore(sqlDB)
	cronJobs := db.NewCronJobStore(sqlDB)
	scheduler := cron.New(cron.Config{
		Store:    cronJobs,
		Sessions: sessions,
		Repos:    repos,
		Logger:   zerolog.Nop(),
	})
	srv := &Server{
		cronJobs:      cronJobs,
		sessions:      sessions,
		cronScheduler: scheduler,
		agentClients: map[string]agent.AgentRunnerClient{
			"codex":    nil,
			"opencode": nil,
		},
		logger: zerolog.Nop(),
	}
	ctx := context.Background()

	repo, err := repos.Create(ctx, db.CreateRepoParams{
		DisplayName:       "repo",
		LocalPath:         "/tmp/repo",
		OriginURL:         "https://github.com/test/repo.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/wt/repo",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}

	created, err := srv.CreateCronJob(ctx, connect.NewRequest(&pb.CreateCronJobRequest{
		RepoId:    repo.ID,
		Name:      "Daily",
		Prompt:    "Run daily checks",
		Schedule:  "@daily",
		AgentName: "codex",
		Enabled:   false,
	}))
	if err != nil {
		t.Fatalf("CreateCronJob: %v", err)
	}
	jobID := created.Msg.CronJob.Id
	if created.Msg.CronJob.AgentName != "codex" {
		t.Fatalf("create agent_name = %q, want codex", created.Msg.CronJob.AgentName)
	}

	listed, err := srv.ListCronJobs(ctx, connect.NewRequest(&pb.ListCronJobsRequest{}))
	if err != nil {
		t.Fatalf("ListCronJobs: %v", err)
	}
	if len(listed.Msg.CronJobs) != 1 || listed.Msg.CronJobs[0].AgentName != "codex" {
		t.Fatalf("list cron_jobs = %+v, want one codex cron job", listed.Msg.CronJobs)
	}

	got, err := srv.GetCronJob(ctx, connect.NewRequest(&pb.GetCronJobRequest{Id: jobID}))
	if err != nil {
		t.Fatalf("GetCronJob: %v", err)
	}
	if got.Msg.CronJob.AgentName != "codex" {
		t.Fatalf("get agent_name = %q, want codex", got.Msg.CronJob.AgentName)
	}

	agentName := "opencode"
	updated, err := srv.UpdateCronJob(ctx, connect.NewRequest(&pb.UpdateCronJobRequest{
		Id:        jobID,
		AgentName: &agentName,
	}))
	if err != nil {
		t.Fatalf("UpdateCronJob: %v", err)
	}
	if updated.Msg.CronJob.AgentName != "opencode" {
		t.Fatalf("update agent_name = %q, want opencode", updated.Msg.CronJob.AgentName)
	}
}

func TestCronJobAPIsRejectUnknownAgentName(t *testing.T) {
	sqlDB := setupServerTestDB(t)
	repos := db.NewRepoStore(sqlDB)
	sessions := db.NewSessionStore(sqlDB)
	cronJobs := db.NewCronJobStore(sqlDB)
	scheduler := cron.New(cron.Config{
		Store:    cronJobs,
		Sessions: sessions,
		Repos:    repos,
		Logger:   zerolog.Nop(),
	})
	srv := &Server{
		cronJobs:      cronJobs,
		sessions:      sessions,
		cronScheduler: scheduler,
		agentClients: map[string]agent.AgentRunnerClient{
			"codex": nil,
		},
		logger: zerolog.Nop(),
	}
	ctx := context.Background()

	repo, err := repos.Create(ctx, db.CreateRepoParams{
		DisplayName:       "repo",
		LocalPath:         "/tmp/repo",
		OriginURL:         "https://github.com/test/repo.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/wt/repo",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}

	_, err = srv.CreateCronJob(ctx, connect.NewRequest(&pb.CreateCronJobRequest{
		RepoId:    repo.ID,
		Name:      "Daily",
		Prompt:    "Run daily checks",
		Schedule:  "@daily",
		AgentName: "missing",
		Enabled:   false,
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("CreateCronJob error code = %v, want %v (err=%v)", connect.CodeOf(err), connect.CodeInvalidArgument, err)
	}

	created, err := srv.CreateCronJob(ctx, connect.NewRequest(&pb.CreateCronJobRequest{
		RepoId:    repo.ID,
		Name:      "Valid",
		Prompt:    "Run daily checks",
		Schedule:  "@daily",
		AgentName: "codex",
		Enabled:   false,
	}))
	if err != nil {
		t.Fatalf("CreateCronJob valid: %v", err)
	}

	missing := "missing"
	_, err = srv.UpdateCronJob(ctx, connect.NewRequest(&pb.UpdateCronJobRequest{
		Id:        created.Msg.CronJob.Id,
		AgentName: &missing,
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("UpdateCronJob error code = %v, want %v (err=%v)", connect.CodeOf(err), connect.CodeInvalidArgument, err)
	}

	stale, err := cronJobs.Create(ctx, db.CreateCronJobParams{
		RepoID:    repo.ID,
		Name:      "Stale",
		Prompt:    "Run daily checks",
		Schedule:  "@daily",
		AgentName: "missing",
		Enabled:   false,
	})
	if err != nil {
		t.Fatalf("create stale cron job: %v", err)
	}

	enable := true
	_, err = srv.UpdateCronJob(ctx, connect.NewRequest(&pb.UpdateCronJobRequest{
		Id:      stale.ID,
		Enabled: &enable,
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("UpdateCronJob re-enable error code = %v, want %v (err=%v)", connect.CodeOf(err), connect.CodeInvalidArgument, err)
	}
}
