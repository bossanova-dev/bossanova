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
		IsEnabled: false,
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

// newCronTestServer builds a Server wired to a real SQLite DB and scheduler
// plus one repo, mirroring the setup the agent-name tests use. It returns the
// server, the repo ID, and a context for the request handlers.
func newCronTestServer(t *testing.T) (*Server, string, context.Context) {
	t.Helper()
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
	return srv, repo.ID, ctx
}

// mustCreateDisabledCronJob creates a valid, disabled cron job and returns its
// ID. Disabled keeps the scheduler out of the picture so update/delete tests
// don't depend on schedule parsing.
func mustCreateDisabledCronJob(t *testing.T, srv *Server, ctx context.Context, repoID string) string {
	t.Helper()
	created, err := srv.CreateCronJob(ctx, connect.NewRequest(&pb.CreateCronJobRequest{
		RepoId:    repoID,
		Name:      "Job",
		Prompt:    "do it",
		Schedule:  "@daily",
		AgentName: "codex",
		IsEnabled: false,
	}))
	if err != nil {
		t.Fatalf("CreateCronJob: %v", err)
	}
	return created.Msg.CronJob.Id
}

// TestCreateCronJobRejectsUnschedulableExpression covers the enabled-job branch
// where AddJob rejects an unparseable schedule (cron.go:60). The handler must
// surface InvalidArgument and roll back the just-created row so a restart
// doesn't choke on a schedule the scheduler refuses to load.
func TestCreateCronJobRejectsUnschedulableExpression(t *testing.T) {
	srv, repoID, ctx := newCronTestServer(t)

	_, err := srv.CreateCronJob(ctx, connect.NewRequest(&pb.CreateCronJobRequest{
		RepoId:    repoID,
		Name:      "Bad",
		Prompt:    "do it",
		Schedule:  "this is not a cron expression",
		AgentName: "codex",
		IsEnabled: true,
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("err code = %v, want InvalidArgument (err=%v)", connect.CodeOf(err), err)
	}

	// The failed AddJob must roll the row back, leaving no jobs behind.
	listed, lerr := srv.ListCronJobs(ctx, connect.NewRequest(&pb.ListCronJobsRequest{}))
	if lerr != nil {
		t.Fatalf("ListCronJobs: %v", lerr)
	}
	if len(listed.Msg.CronJobs) != 0 {
		t.Fatalf("want rollback to leave 0 jobs, got %d", len(listed.Msg.CronJobs))
	}
}

// TestUpdateCronJobRejectsBlankNameAndSchedule covers the empty-after-trim
// guards in UpdateCronJob (cron.go:133 and cron.go:143): a name or schedule
// that is set but trims to empty must be rejected rather than persisted blank.
func TestUpdateCronJobRejectsBlankNameAndSchedule(t *testing.T) {
	srv, repoID, ctx := newCronTestServer(t)
	id := mustCreateDisabledCronJob(t, srv, ctx, repoID)

	blank := "   "
	if _, err := srv.UpdateCronJob(ctx, connect.NewRequest(&pb.UpdateCronJobRequest{
		Id:   id,
		Name: &blank,
	})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("blank name code = %v, want InvalidArgument", connect.CodeOf(err))
	}

	if _, err := srv.UpdateCronJob(ctx, connect.NewRequest(&pb.UpdateCronJobRequest{
		Id:       id,
		Schedule: &blank,
	})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("blank schedule code = %v, want InvalidArgument", connect.CodeOf(err))
	}
}

// TestUpdateCronJobSetsTimezone covers the non-empty timezone branch
// (cron.go:152): a non-blank timezone is persisted and echoed back, where the
// blank case would clear it to daemon-local.
func TestUpdateCronJobSetsTimezone(t *testing.T) {
	srv, repoID, ctx := newCronTestServer(t)
	id := mustCreateDisabledCronJob(t, srv, ctx, repoID)

	tz := "America/New_York"
	updated, err := srv.UpdateCronJob(ctx, connect.NewRequest(&pb.UpdateCronJobRequest{
		Id:       id,
		Timezone: &tz,
	}))
	if err != nil {
		t.Fatalf("UpdateCronJob: %v", err)
	}
	if got := updated.Msg.CronJob.Timezone; got != tz {
		t.Fatalf("timezone = %q, want %q", got, tz)
	}
}

// TestUpdateCronJobRejectsEnableWithUnschedulableExpression covers the
// AddJob-failure branch in UpdateCronJob (cron.go:188): enabling a job with an
// unparseable schedule must surface InvalidArgument.
func TestUpdateCronJobRejectsEnableWithUnschedulableExpression(t *testing.T) {
	srv, repoID, ctx := newCronTestServer(t)
	id := mustCreateDisabledCronJob(t, srv, ctx, repoID)

	enable := true
	badSchedule := "not a real schedule"
	if _, err := srv.UpdateCronJob(ctx, connect.NewRequest(&pb.UpdateCronJobRequest{
		Id:        id,
		Schedule:  &badSchedule,
		IsEnabled: &enable,
	})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("err code = %v, want InvalidArgument (err=%v)", connect.CodeOf(err), err)
	}
}

// TestDeleteCronJob covers the empty-id guard (cron.go:198), the
// ErrNoRows→NotFound mapping for an unknown id (cron.go:202), and a successful
// delete that removes the row.
func TestDeleteCronJob(t *testing.T) {
	srv, repoID, ctx := newCronTestServer(t)
	id := mustCreateDisabledCronJob(t, srv, ctx, repoID)

	if _, err := srv.DeleteCronJob(ctx, connect.NewRequest(&pb.DeleteCronJobRequest{
		Id: "  ",
	})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("blank id code = %v, want InvalidArgument", connect.CodeOf(err))
	}

	if _, err := srv.DeleteCronJob(ctx, connect.NewRequest(&pb.DeleteCronJobRequest{
		Id: "does-not-exist",
	})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown id code = %v, want NotFound", connect.CodeOf(err))
	}

	if _, err := srv.DeleteCronJob(ctx, connect.NewRequest(&pb.DeleteCronJobRequest{
		Id: id,
	})); err != nil {
		t.Fatalf("DeleteCronJob: %v", err)
	}
	if _, err := srv.GetCronJob(ctx, connect.NewRequest(&pb.GetCronJobRequest{
		Id: id,
	})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("after delete, Get code = %v, want NotFound", connect.CodeOf(err))
	}
}

// TestRunCronJobNowUnknownJob covers the empty-id guard (cron.go:214) and the
// no-session path (cron.go:218 and cron.go:222): an unknown job is reported via
// SkippedReason with no error and no attached session, rather than erroring.
func TestRunCronJobNowUnknownJob(t *testing.T) {
	srv, _, ctx := newCronTestServer(t)

	if _, err := srv.RunCronJobNow(ctx, connect.NewRequest(&pb.RunCronJobNowRequest{
		Id: " ",
	})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("blank id code = %v, want InvalidArgument", connect.CodeOf(err))
	}

	resp, err := srv.RunCronJobNow(ctx, connect.NewRequest(&pb.RunCronJobNowRequest{
		Id: "missing",
	}))
	if err != nil {
		t.Fatalf("RunCronJobNow: %v", err)
	}
	if resp.Msg.Session != nil {
		t.Fatalf("want no session for an unknown job, got %+v", resp.Msg.Session)
	}
	if resp.Msg.SkippedReason == "" {
		t.Fatalf("want a skipped reason for an unknown job")
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
		IsEnabled: false,
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
		IsEnabled: false,
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
		IsEnabled: false,
	})
	if err != nil {
		t.Fatalf("create stale cron job: %v", err)
	}

	enable := true
	_, err = srv.UpdateCronJob(ctx, connect.NewRequest(&pb.UpdateCronJobRequest{
		Id:        stale.ID,
		IsEnabled: &enable,
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("UpdateCronJob re-enable error code = %v, want %v (err=%v)", connect.CodeOf(err), connect.CodeInvalidArgument, err)
	}
}

// TestCreateCronJobRunSetupCommandDefault verifies that omitting ShouldRunSetupCommand
// (nil) defaults to true: setup should run by default.
func TestCreateCronJobRunSetupCommandDefault(t *testing.T) {
	srv, repoID, ctx := newCronTestServer(t)

	created, err := srv.CreateCronJob(ctx, connect.NewRequest(&pb.CreateCronJobRequest{
		RepoId:    repoID,
		Name:      "Default setup",
		Prompt:    "do it",
		Schedule:  "@daily",
		AgentName: "codex",
		IsEnabled: false,
		// ShouldRunSetupCommand intentionally omitted (nil).
	}))
	if err != nil {
		t.Fatalf("CreateCronJob: %v", err)
	}
	if !created.Msg.CronJob.ShouldRunSetupCommand {
		t.Fatalf("ShouldRunSetupCommand = false, want true (default)")
	}
}

// TestCreateCronJobRunSetupCommandExplicitFalse verifies that passing
// ShouldRunSetupCommand=false is honoured (opt-out of setup).
func TestCreateCronJobRunSetupCommandExplicitFalse(t *testing.T) {
	srv, repoID, ctx := newCronTestServer(t)
	falseVal := false

	created, err := srv.CreateCronJob(ctx, connect.NewRequest(&pb.CreateCronJobRequest{
		RepoId:                repoID,
		Name:                  "No setup",
		Prompt:                "do it",
		Schedule:              "@daily",
		AgentName:             "codex",
		IsEnabled:             false,
		ShouldRunSetupCommand: &falseVal,
	}))
	if err != nil {
		t.Fatalf("CreateCronJob: %v", err)
	}
	if created.Msg.CronJob.ShouldRunSetupCommand {
		t.Fatalf("ShouldRunSetupCommand = true, want false (explicit opt-out)")
	}
}

// TestCreateCronJobGateCommand verifies that a GateCommand set on creation is
// reflected in the returned proto.
func TestCreateCronJobGateCommand(t *testing.T) {
	srv, repoID, ctx := newCronTestServer(t)

	created, err := srv.CreateCronJob(ctx, connect.NewRequest(&pb.CreateCronJobRequest{
		RepoId:      repoID,
		Name:        "Gated job",
		Prompt:      "do it",
		Schedule:    "@daily",
		AgentName:   "codex",
		IsEnabled:   false,
		GateCommand: "make gate-check",
	}))
	if err != nil {
		t.Fatalf("CreateCronJob: %v", err)
	}
	if got := created.Msg.CronJob.GateCommand; got != "make gate-check" {
		t.Fatalf("GateCommand = %q, want %q", got, "make gate-check")
	}
}

// TestUpdateCronJobGateCommandAndRunSetupCommand verifies that UpdateCronJob
// correctly maps and persists GateCommand and ShouldRunSetupCommand changes.
func TestUpdateCronJobGateCommandAndRunSetupCommand(t *testing.T) {
	srv, repoID, ctx := newCronTestServer(t)

	// Create without a gate command; run_setup defaults to true.
	created, err := srv.CreateCronJob(ctx, connect.NewRequest(&pb.CreateCronJobRequest{
		RepoId:    repoID,
		Name:      "Update target",
		Prompt:    "do it",
		Schedule:  "@daily",
		AgentName: "codex",
		IsEnabled: false,
	}))
	if err != nil {
		t.Fatalf("CreateCronJob: %v", err)
	}
	jobID := created.Msg.CronJob.Id

	gateCmd := "make ci-gate"
	falseVal := false
	updated, err := srv.UpdateCronJob(ctx, connect.NewRequest(&pb.UpdateCronJobRequest{
		Id:                    jobID,
		GateCommand:           &gateCmd,
		ShouldRunSetupCommand: &falseVal,
	}))
	if err != nil {
		t.Fatalf("UpdateCronJob: %v", err)
	}
	if got := updated.Msg.CronJob.GateCommand; got != "make ci-gate" {
		t.Fatalf("GateCommand = %q, want %q", got, "make ci-gate")
	}
	if updated.Msg.CronJob.ShouldRunSetupCommand {
		t.Fatalf("ShouldRunSetupCommand = true, want false after update")
	}
}
