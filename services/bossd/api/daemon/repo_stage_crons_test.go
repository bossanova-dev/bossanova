package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/projectconfig"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/cron"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/domain"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/proto"
)

type fakeStageCronHost struct {
	bins       map[string]string
	skillRoots map[string]string
	skillErr   map[string]error
	homeDir    string
	files      map[string]bool
}

func (f *fakeStageCronHost) LookPath(name string) (string, bool) {
	p, ok := f.bins[name]
	return p, ok && p != ""
}

func (f *fakeStageCronHost) SkillRoot(agent string) (string, error) {
	if err := f.skillErr[agent]; err != nil {
		return "", err
	}
	return f.skillRoots[agent], nil
}

func (f *fakeStageCronHost) HomeDir() string { return f.homeDir }

func (f *fakeStageCronHost) FileExists(path string) bool { return f.files[path] }

func readyStageCronHost(home, agentName, root string) *fakeStageCronHost {
	files := make(map[string]bool)
	for _, st := range projectconfig.DefaultFactoryStages() {
		if !st.DefaultOn {
			continue
		}
		files[filepath.Join(root, filepath.FromSlash(st.GatePath()))] = true
		files[filepath.Join(root, filepath.FromSlash(st.ModulePath()))] = true
	}
	bins := map[string]string{"node": "/usr/local/bin/node"}
	for _, a := range projectconfig.FactoryAgents() {
		if a.Name == agentName {
			bins[a.CLI] = "/usr/local/bin/" + a.CLI
		}
	}
	return &fakeStageCronHost{
		bins:       bins,
		skillRoots: map[string]string{agentName: root},
		homeDir:    home,
		files:      files,
	}
}

func newStageCronTestServer(t *testing.T, host stageCronHost, agents []string, logBuf *bytes.Buffer) (*Server, db.RepoStore, db.CronJobStore) {
	t.Helper()
	seedWorktreeBaseSettings(t)

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

	logger := zerolog.Nop()
	if logBuf != nil {
		logger = zerolog.New(logBuf)
	}
	agentClients := make(map[string]agent.RunnerClient, len(agents))
	for _, name := range agents {
		agentClients[name] = nil
	}

	srv := New(Config{
		Repos:         repos,
		Sessions:      sessions,
		CronJobs:      cronJobs,
		CronScheduler: scheduler,
		AgentClients:  agentClients,
		Worktrees:     &repoMgmtWorktree{isGitRepo: true, originURL: "git@github.com:acme/app.git"},
		StageCronHost: host,
		Logger:        logger,
	})
	return srv, repos, cronJobs
}

func parseJSONLogLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var entries []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("unmarshal log line %q: %v", line, err)
		}
		entries = append(entries, entry)
	}
	return entries
}

func TestSeedStageCronsOnRegistration_Repos_AC270501(t *testing.T) {
	const (
		home = "/Users/tester"
		root = "/Users/tester/.claude/skills"
	)
	defaultStages := make([]projectconfig.FactoryStage, 0, 3)
	for _, st := range projectconfig.DefaultFactoryStages() {
		if st.DefaultOn {
			defaultStages = append(defaultStages, st)
		}
	}

	cases := []struct {
		name     string
		register func(t *testing.T, srv *Server) *pb.Repo
	}{
		{
			name: "RegisterRepo",
			register: func(t *testing.T, srv *Server) *pb.Repo {
				t.Helper()
				resp, err := srv.RegisterRepo(context.Background(), connect.NewRequest(&pb.RegisterRepoRequest{
					LocalPath: t.TempDir(),
					IsTrusted: proto.Bool(true),
				}))
				if err != nil {
					t.Fatalf("RegisterRepo: %v", err)
				}
				return resp.Msg.GetRepo()
			},
		},
		{
			name: "CloneAndRegisterRepo",
			register: func(t *testing.T, srv *Server) *pb.Repo {
				t.Helper()
				target := filepath.Join(t.TempDir(), "cloned")
				resp, err := srv.CloneAndRegisterRepo(context.Background(), connect.NewRequest(&pb.CloneAndRegisterRepoRequest{
					CloneUrl:  "git@github.com:acme/app.git",
					LocalPath: target,
					IsTrusted: proto.Bool(true),
				}))
				if err != nil {
					t.Fatalf("CloneAndRegisterRepo: %v", err)
				}
				return resp.Msg.GetRepo()
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := readyStageCronHost(home, "claude", root)
			srv, repos, _ := newStageCronTestServer(t, host, []string{"claude", "codex"}, nil)

			repo := tc.register(t, srv)
			if repo.GetCanAutoMerge() {
				t.Fatal("registered repo CanAutoMerge = true, want false")
			}
			storedRepo, err := repos.Get(context.Background(), repo.GetId())
			if err != nil {
				t.Fatalf("repos.Get: %v", err)
			}
			if storedRepo.CanAutoMerge {
				t.Fatal("stored repo CanAutoMerge = true, want false")
			}

			repoID := repo.GetId()
			listed, err := srv.ListCronJobs(context.Background(), connect.NewRequest(&pb.ListCronJobsRequest{
				RepoId: &repoID,
			}))
			if err != nil {
				t.Fatalf("ListCronJobs: %v", err)
			}
			jobs := listed.Msg.GetCronJobs()
			if len(jobs) != len(defaultStages) {
				t.Fatalf("seeded %d jobs, want %d (plan, build, verify; no retro)", len(jobs), len(defaultStages))
			}

			byName := make(map[string]*pb.CronJob, len(jobs))
			for _, j := range jobs {
				byName[j.GetName()] = j
			}
			if _, hasRetro := byName["factory-retro"]; hasRetro {
				t.Fatal("factory-retro must not be seeded by default")
			}

			for _, st := range defaultStages {
				j, ok := byName[st.JobName]
				if !ok {
					t.Fatalf("missing seeded job %q", st.JobName)
				}
				if !j.GetIsEnabled() {
					t.Errorf("%s IsEnabled = false, want true", st.JobName)
				}
				if got := j.GetAgentName(); got != "claude" {
					t.Errorf("%s AgentName = %q, want claude", st.JobName, got)
				}
				if got := j.GetPrompt(); got != st.Prompt("claude") {
					t.Errorf("%s Prompt = %q, want %q", st.JobName, got, st.Prompt("claude"))
				}
				if got := j.GetSchedule(); got != st.DefaultSchedule {
					t.Errorf("%s Schedule = %q, want %q", st.JobName, got, st.DefaultSchedule)
				}
				wantGate := st.GateCommand("node", root, home)
				if got := j.GetGateCommand(); got != wantGate {
					t.Errorf("%s GateCommand = %q, want %q", st.JobName, got, wantGate)
				}
				if got := j.GetManagedBy(); got != projectconfig.DefaultsMarker {
					t.Errorf("%s ManagedBy = %q, want %q", st.JobName, got, projectconfig.DefaultsMarker)
				}
			}
		})
	}
}

func TestSeedStageCronsPreflightFailureDisablesAndLogs_Repos_AC270502(t *testing.T) {
	const (
		home = "/Users/tester"
		root = "/Users/tester/.claude/skills"
	)

	t.Run("missing gate script disables that stage and logs structured warning", func(t *testing.T) {
		host := readyStageCronHost(home, "claude", root)
		verifyStage, _ := projectconfig.DefaultFactoryStages().Lookup("verify")
		missingGate := filepath.Join(root, filepath.FromSlash(verifyStage.GatePath()))
		delete(host.files, missingGate)

		var logBuf bytes.Buffer
		srv, _, _ := newStageCronTestServer(t, host, []string{"claude"}, &logBuf)

		resp, err := srv.RegisterRepo(context.Background(), connect.NewRequest(&pb.RegisterRepoRequest{
			LocalPath: t.TempDir(),
			IsTrusted: proto.Bool(true),
		}))
		if err != nil {
			t.Fatalf("RegisterRepo must succeed even on preflight failure: %v", err)
		}
		repoID := resp.Msg.GetRepo().GetId()

		listed, err := srv.ListCronJobs(context.Background(), connect.NewRequest(&pb.ListCronJobsRequest{
			RepoId: &repoID,
		}))
		if err != nil {
			t.Fatalf("ListCronJobs: %v", err)
		}
		if len(listed.Msg.GetCronJobs()) != 3 {
			t.Fatalf("seeded %d jobs, want 3", len(listed.Msg.GetCronJobs()))
		}
		for _, j := range listed.Msg.GetCronJobs() {
			wantEnabled := j.GetName() != "factory-verify"
			if j.GetIsEnabled() != wantEnabled {
				t.Errorf("%s IsEnabled = %v, want %v", j.GetName(), j.GetIsEnabled(), wantEnabled)
			}
		}

		entries := parseJSONLogLines(t, &logBuf)
		var matched map[string]any
		for _, e := range entries {
			if e["job"] == "factory-verify" {
				matched = e
				break
			}
		}
		if matched == nil {
			t.Fatalf("expected structured Warn log for factory-verify, got logs:\n%s", logBuf.String())
		}
		if matched["level"] != "warn" {
			t.Errorf("log level = %v, want warn", matched["level"])
		}
		if matched["repo_id"] != repoID {
			t.Errorf("log repo_id = %v, want %q", matched["repo_id"], repoID)
		}
		if matched["stage"] != "verify" {
			t.Errorf("log stage = %v, want verify", matched["stage"])
		}
		preflight, _ := matched["preflight"].(string)
		if !strings.Contains(preflight, missingGate) {
			t.Errorf("log preflight = %q, want it to name %q", preflight, missingGate)
		}
	})

	t.Run("missing node disables all default stages and keeps bare node gate command", func(t *testing.T) {
		host := readyStageCronHost(home, "claude", root)
		delete(host.bins, "node")

		var logBuf bytes.Buffer
		srv, _, _ := newStageCronTestServer(t, host, []string{"claude"}, &logBuf)

		resp, err := srv.RegisterRepo(context.Background(), connect.NewRequest(&pb.RegisterRepoRequest{
			LocalPath: t.TempDir(),
			IsTrusted: proto.Bool(true),
		}))
		if err != nil {
			t.Fatalf("RegisterRepo: %v", err)
		}
		repoID := resp.Msg.GetRepo().GetId()

		listed, err := srv.ListCronJobs(context.Background(), connect.NewRequest(&pb.ListCronJobsRequest{
			RepoId: &repoID,
		}))
		if err != nil {
			t.Fatalf("ListCronJobs: %v", err)
		}
		if len(listed.Msg.GetCronJobs()) != 3 {
			t.Fatalf("seeded %d jobs, want 3", len(listed.Msg.GetCronJobs()))
		}
		for _, j := range listed.Msg.GetCronJobs() {
			if j.GetIsEnabled() {
				t.Errorf("%s IsEnabled = true without node, want false", j.GetName())
			}
			if !strings.HasPrefix(j.GetGateCommand(), "node ") {
				t.Errorf("%s GateCommand = %q, want bare node prefix", j.GetName(), j.GetGateCommand())
			}
		}

		entries := parseJSONLogLines(t, &logBuf)
		warnCount := 0
		for _, e := range entries {
			if e["level"] == "warn" && e["repo_id"] == repoID && strings.Contains(e["preflight"].(string), "node") {
				warnCount++
			}
		}
		if warnCount != 3 {
			t.Fatalf("logged %d node preflight warnings, want 3; logs:\n%s", warnCount, logBuf.String())
		}
	})
}

func TestSeedStageCronsIdempotent_Repos_AC270503(t *testing.T) {
	const (
		home = "/Users/tester"
		root = "/Users/tester/.claude/skills"
	)

	cases := []struct {
		name         string
		existingName string
		existingCmd  string
	}{
		{
			name:         "existing factory-verify by canonical name",
			existingName: "factory-verify",
			existingCmd:  "custom verify prompt",
		},
		{
			name:         "existing hand-named job running /boss-verify",
			existingName: "my-custom-verify",
			existingCmd:  "/boss-verify",
		},
		{
			name:         "existing hand-named job running $boss-verify",
			existingName: "codex-verify-custom",
			existingCmd:  "$boss-verify",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := readyStageCronHost(home, "claude", root)
			srv, repos, cronJobs := newStageCronTestServer(t, host, []string{"claude"}, nil)
			ctx := context.Background()

			repo, err := repos.Create(ctx, db.CreateRepoParams{
				DisplayName:       "app",
				LocalPath:         t.TempDir(),
				OriginURL:         "git@github.com:acme/app.git",
				DefaultBaseBranch: "main",
				WorktreeBaseDir:   seededWorktreeBase,
				IsTrusted:         true,
			})
			if err != nil {
				t.Fatalf("repos.Create: %v", err)
			}
			if _, err := cronJobs.Create(ctx, domain.CreateCronJobParams{
				RepoID:    repo.ID,
				Name:      tc.existingName,
				Prompt:    tc.existingCmd,
				Schedule:  "*/5 * * * *",
				AgentName: "claude",
				IsEnabled: true,
			}); err != nil {
				t.Fatalf("cronJobs.Create: %v", err)
			}

			srv.seedStageCrons(ctx, repo)
			srv.seedStageCrons(ctx, repo)

			jobs, err := cronJobs.ListByRepo(ctx, repo.ID)
			if err != nil {
				t.Fatalf("ListByRepo: %v", err)
			}
			if len(jobs) != 3 {
				t.Fatalf("got %d jobs after seeding, want 3 (existing verify + seeded plan & build)", len(jobs))
			}

			verifyStage, _ := projectconfig.DefaultFactoryStages().Lookup("verify")
			verifyMatches := 0
			for _, j := range jobs {
				if verifyStage.MatchesJob(j.Name, j.Prompt) {
					verifyMatches++
				}
			}
			if verifyMatches != 1 {
				t.Fatalf("verify stage matched %d jobs, want 1 (no duplicate)", verifyMatches)
			}
		})
	}
}

type stageCronFailingCreateStore struct {
	cronJobStore
	failName string
	failErr  error
}

func (s *stageCronFailingCreateStore) Create(ctx context.Context, params domain.CreateCronJobParams) (*domain.CronJob, error) {
	if params.Name == s.failName {
		return nil, s.failErr
	}
	return s.cronJobStore.Create(ctx, params)
}

func TestSeedStageCronsAgentSelectionAndGuards(t *testing.T) {
	const home = "/Users/tester"

	t.Run("codex-only loaded with codex on PATH seeds $boss-* prompts", func(t *testing.T) {
		root := "/Users/tester/.codex/skills"
		host := readyStageCronHost(home, "codex", root)
		srv, _, cronJobs := newStageCronTestServer(t, host, []string{"codex"}, nil)

		resp, err := srv.RegisterRepo(context.Background(), connect.NewRequest(&pb.RegisterRepoRequest{
			LocalPath: t.TempDir(),
			IsTrusted: proto.Bool(true),
		}))
		if err != nil {
			t.Fatalf("RegisterRepo: %v", err)
		}
		jobs, err := cronJobs.ListByRepo(context.Background(), resp.Msg.GetRepo().GetId())
		if err != nil {
			t.Fatalf("ListByRepo: %v", err)
		}
		if len(jobs) != 3 {
			t.Fatalf("seeded %d jobs, want 3", len(jobs))
		}
		for _, j := range jobs {
			if j.AgentName != "codex" {
				t.Errorf("%s AgentName = %q, want codex", j.Name, j.AgentName)
			}
			if !strings.HasPrefix(j.Prompt, "$boss-") {
				t.Errorf("%s Prompt = %q, want $boss-* prefix", j.Name, j.Prompt)
			}
			if !j.IsEnabled {
				t.Errorf("%s IsEnabled = false, want true", j.Name)
			}
		}
	})

	t.Run("no loaded factory runner seeds nothing and logs warning", func(t *testing.T) {
		host := readyStageCronHost(home, "claude", "/Users/tester/.claude/skills")
		var logBuf bytes.Buffer
		srv, _, cronJobs := newStageCronTestServer(t, host, []string{"stub"}, &logBuf)

		resp, err := srv.RegisterRepo(context.Background(), connect.NewRequest(&pb.RegisterRepoRequest{
			LocalPath: t.TempDir(),
			IsTrusted: proto.Bool(true),
		}))
		if err != nil {
			t.Fatalf("RegisterRepo: %v", err)
		}
		repoID := resp.Msg.GetRepo().GetId()
		jobs, err := cronJobs.ListByRepo(context.Background(), repoID)
		if err != nil {
			t.Fatalf("ListByRepo: %v", err)
		}
		if len(jobs) != 0 {
			t.Fatalf("seeded %d jobs with only stub runner, want 0", len(jobs))
		}
		entries := parseJSONLogLines(t, &logBuf)
		if len(entries) != 1 || entries[0]["level"] != "warn" || entries[0]["repo_id"] != repoID {
			t.Fatalf("expected 1 warn log with repo_id=%q, got %s", repoID, logBuf.String())
		}
	})

	t.Run("loaded factory runner with missing CLI on PATH seeds nothing and logs warning", func(t *testing.T) {
		host := readyStageCronHost(home, "claude", "/Users/tester/.claude/skills")
		delete(host.bins, "claude")
		var logBuf bytes.Buffer
		srv, _, cronJobs := newStageCronTestServer(t, host, []string{"claude"}, &logBuf)

		resp, err := srv.RegisterRepo(context.Background(), connect.NewRequest(&pb.RegisterRepoRequest{
			LocalPath: t.TempDir(),
			IsTrusted: proto.Bool(true),
		}))
		if err != nil {
			t.Fatalf("RegisterRepo: %v", err)
		}
		jobs, err := cronJobs.ListByRepo(context.Background(), resp.Msg.GetRepo().GetId())
		if err != nil {
			t.Fatalf("ListByRepo: %v", err)
		}
		if len(jobs) != 0 {
			t.Fatalf("seeded %d jobs when claude CLI missing from PATH, want 0", len(jobs))
		}
		if entries := parseJSONLogLines(t, &logBuf); len(entries) != 1 || entries[0]["level"] != "warn" {
			t.Fatalf("expected 1 warn log, got %s", logBuf.String())
		}
	})

	t.Run("nil stageCronHost seeds nothing", func(t *testing.T) {
		srv, _, cronJobs := newStageCronTestServer(t, nil, []string{"claude"}, nil)

		resp, err := srv.RegisterRepo(context.Background(), connect.NewRequest(&pb.RegisterRepoRequest{
			LocalPath: t.TempDir(),
			IsTrusted: proto.Bool(true),
		}))
		if err != nil {
			t.Fatalf("RegisterRepo: %v", err)
		}
		jobs, err := cronJobs.ListByRepo(context.Background(), resp.Msg.GetRepo().GetId())
		if err != nil {
			t.Fatalf("ListByRepo: %v", err)
		}
		if len(jobs) != 0 {
			t.Fatalf("seeded %d jobs with nil host, want 0", len(jobs))
		}
	})

	t.Run("CreateCronJob error logs warning and continues seeding remaining stages", func(t *testing.T) {
		host := readyStageCronHost(home, "claude", "/Users/tester/.claude/skills")
		var logBuf bytes.Buffer
		srv, _, cronJobs := newStageCronTestServer(t, host, []string{"claude"}, &logBuf)
		srv.cronJobs = &stageCronFailingCreateStore{
			cronJobStore: cronJobs,
			failName:     "factory-build",
			failErr:      errors.New("disk full"),
		}

		resp, err := srv.RegisterRepo(context.Background(), connect.NewRequest(&pb.RegisterRepoRequest{
			LocalPath: t.TempDir(),
			IsTrusted: proto.Bool(true),
		}))
		if err != nil {
			t.Fatalf("RegisterRepo: %v", err)
		}
		repoID := resp.Msg.GetRepo().GetId()
		jobs, err := cronJobs.ListByRepo(context.Background(), repoID)
		if err != nil {
			t.Fatalf("ListByRepo: %v", err)
		}
		if len(jobs) != 2 {
			t.Fatalf("seeded %d jobs after build failure, want 2 (plan and verify)", len(jobs))
		}
		entries := parseJSONLogLines(t, &logBuf)
		if len(entries) != 1 || entries[0]["job"] != "factory-build" || entries[0]["stage"] != "build" || entries[0]["repo_id"] != repoID {
			t.Fatalf("expected 1 warn log for factory-build failure, got %s", logBuf.String())
		}
	})
}

func TestSeedStageCronsGateCommandMatchesProjectconfigHelper(t *testing.T) {
	cases := []struct {
		name string
		home string
		root string
	}{
		{
			name: "root inside home uses $HOME prefix",
			home: "/Users/tester",
			root: "/Users/tester/.claude/skills",
		},
		{
			name: "root outside home keeps absolute path",
			home: "/Users/tester",
			root: "/opt/shared/skills",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := readyStageCronHost(tc.home, "claude", tc.root)
			srv, _, cronJobs := newStageCronTestServer(t, host, []string{"claude"}, nil)

			resp, err := srv.RegisterRepo(context.Background(), connect.NewRequest(&pb.RegisterRepoRequest{
				LocalPath: t.TempDir(),
				IsTrusted: proto.Bool(true),
			}))
			if err != nil {
				t.Fatalf("RegisterRepo: %v", err)
			}
			jobs, err := cronJobs.ListByRepo(context.Background(), resp.Msg.GetRepo().GetId())
			if err != nil {
				t.Fatalf("ListByRepo: %v", err)
			}
			byName := make(map[string]*domain.CronJob, len(jobs))
			for _, j := range jobs {
				byName[j.Name] = j
			}
			for _, st := range projectconfig.DefaultFactoryStages() {
				if !st.DefaultOn {
					continue
				}
				got := byName[st.JobName].GateCommand
				want := st.GateCommand("node", tc.root, tc.home)
				if got != want {
					t.Errorf("%s GateCommand = %q, want %q", st.JobName, got, want)
				}
			}
		})
	}
}

type stageCronProjectClient struct {
	srv             *Server
	origin          string
	trustOnRegister bool
	seededIDsByName map[string]string
	createdNames    []string
	updatedIDs      []string
}

func (c *stageCronProjectClient) ListRepos(ctx context.Context) ([]*pb.Repo, error) {
	resp, err := c.srv.ListRepos(ctx, connect.NewRequest(&pb.ListReposRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetRepos(), nil
}

func (c *stageCronProjectClient) ListCronJobs(ctx context.Context, repoID string) ([]*pb.CronJob, error) {
	resp, err := c.srv.ListCronJobs(ctx, connect.NewRequest(&pb.ListCronJobsRequest{RepoId: &repoID}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetCronJobs(), nil
}

func (c *stageCronProjectClient) ValidateRepoPath(_ context.Context, _ string) (*pb.ValidateRepoPathResponse, error) {
	return &pb.ValidateRepoPathResponse{IsValid: true, OriginUrl: c.origin, DefaultBranch: "main"}, nil
}

func (c *stageCronProjectClient) RegisterRepo(ctx context.Context, req *pb.RegisterRepoRequest) (*pb.Repo, error) {
	if c.trustOnRegister {
		req = proto.CloneOf(req)
		req.IsTrusted = proto.Bool(true)
	}
	resp, err := c.srv.RegisterRepo(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	repo := resp.Msg.GetRepo()
	if jobs, lerr := c.ListCronJobs(ctx, repo.GetId()); lerr == nil {
		c.seededIDsByName = make(map[string]string, len(jobs))
		for _, j := range jobs {
			if j.GetManagedBy() == projectconfig.DefaultsMarker {
				c.seededIDsByName[j.GetName()] = j.GetId()
			}
		}
	}
	return repo, nil
}

func (c *stageCronProjectClient) UpdateRepo(ctx context.Context, req *pb.UpdateRepoRequest) (*pb.Repo, error) {
	resp, err := c.srv.UpdateRepo(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetRepo(), nil
}

func (c *stageCronProjectClient) CreateCronJob(ctx context.Context, req *pb.CreateCronJobRequest) (*pb.CronJob, error) {
	c.createdNames = append(c.createdNames, req.GetName())
	resp, err := c.srv.CreateCronJob(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetCronJob(), nil
}

func (c *stageCronProjectClient) UpdateCronJob(ctx context.Context, req *pb.UpdateCronJobRequest) (*pb.CronJob, error) {
	c.updatedIDs = append(c.updatedIDs, req.GetId())
	resp, err := c.srv.UpdateCronJob(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetCronJob(), nil
}

func (c *stageCronProjectClient) DeleteCronJob(ctx context.Context, id string) error {
	_, err := c.srv.DeleteCronJob(ctx, connect.NewRequest(&pb.DeleteCronJobRequest{Id: id}))
	return err
}

type stageCronFactoryResolver struct {
	agent, root, home string
}

func (r stageCronFactoryResolver) Resolve(stage, agent string) (projectconfig.FactoryCron, error) {
	st, ok := projectconfig.DefaultFactoryStages().Lookup(stage)
	if !ok {
		return projectconfig.FactoryCron{}, projectconfig.ErrUnknownFactoryStage
	}
	if agent == "" {
		agent = r.agent
	}
	return projectconfig.FactoryCron{
		Skill:           st.Skill,
		Agent:           agent,
		Prompt:          st.Prompt(agent),
		GateCommand:     st.GateCommand("node", r.root, r.home),
		DefaultSchedule: st.DefaultSchedule,
	}, nil
}

func TestFirstProjectApplyAdoptsRegistrationSeededStageCrons(t *testing.T) {
	const (
		home      = "/Users/tester"
		root      = "/Users/tester/.claude/skills"
		originURL = "git@github.com:acme/app.git"
		marker    = "boss-project:app"
	)
	strPtr := func(s string) *string { return &s }
	boolPtr := func(b bool) *bool { return &b }

	t.Run("default factory names succeed without unique-name collision and converge", func(t *testing.T) {
		host := readyStageCronHost(home, "claude", root)
		srv, _, cronJobs := newStageCronTestServer(t, host, []string{"claude"}, nil)
		repoDir := t.TempDir()
		client := &stageCronProjectClient{srv: srv, origin: originURL, trustOnRegister: true}
		resolver := stageCronFactoryResolver{agent: "claude", root: root, home: home}

		cfg := &projectconfig.Config{
			Version: projectconfig.SchemaVersion,
			Project: "app",
			Repo:    projectconfig.Repo{OriginURL: originURL},
			Daemons: []projectconfig.Daemon{{Name: "dev", Path: strPtr(repoDir)}},
			Crons: []projectconfig.Cron{
				{Name: "factory-plan", FactoryStage: strPtr("plan")},
				{Name: "factory-build", FactoryStage: strPtr("build")},
				{Name: "factory-verify", FactoryStage: strPtr("verify")},
				{Name: "nightly", Prompt: strPtr("/nightly"), Schedule: strPtr("0 3 * * *")},
			},
		}
		opts := projectconfig.LocalOptions{
			Daemon:       projectconfig.DaemonIdentity{DisplayName: "dev"},
			CheckoutPath: repoDir,
			HomeDir:      home,
		}

		ctx := context.Background()
		st, err := projectconfig.ObserveLocal(ctx, cfg, client, resolver, opts)
		if err != nil {
			t.Fatalf("ObserveLocal: %v", err)
		}
		plan := projectconfig.PlanLocal(cfg, st, opts)
		res, err := projectconfig.ApplyLocal(ctx, client, plan)
		if err != nil {
			t.Fatalf("ApplyLocal failed on unregistered repo with seeded defaults: %v\n%s", err, res.Text())
		}
		if len(res.Failed) != 0 || len(res.NotReached) != 0 {
			t.Fatalf("unexpected failures/unreached: %+v", res)
		}
		if len(client.seededIDsByName) != 3 {
			t.Fatalf("seeded defaults = %v, want 3 seeded stage crons before adoption", client.seededIDsByName)
		}
		if !slices.Equal(client.createdNames, []string{"nightly"}) {
			t.Fatalf("created crons = %v, want only [nightly] (seeded stage crons must be adopted)", client.createdNames)
		}

		repos, err := client.ListRepos(ctx)
		if err != nil || len(repos) != 1 {
			t.Fatalf("ListRepos: len=%d err=%v", len(repos), err)
		}
		jobs, err := cronJobs.ListByRepo(ctx, repos[0].GetId())
		if err != nil {
			t.Fatalf("ListByRepo: %v", err)
		}
		if len(jobs) != 4 {
			t.Fatalf("got %d cron jobs, want 4", len(jobs))
		}
		for _, j := range jobs {
			if j.ManagedBy != marker {
				t.Errorf("%s ManagedBy = %q, want %q", j.Name, j.ManagedBy, marker)
			}
			if seededID, ok := client.seededIDsByName[j.Name]; ok {
				if j.ID != seededID || !slices.Contains(client.updatedIDs, seededID) {
					t.Errorf("%s id=%q updated=%v, want adopted seeded id %q", j.Name, j.ID, client.updatedIDs, seededID)
				}
			}
		}

		st2, err := projectconfig.ObserveLocal(ctx, cfg, client, resolver, opts)
		if err != nil {
			t.Fatalf("second ObserveLocal: %v", err)
		}
		if plan2 := projectconfig.PlanLocal(cfg, st2, opts); !plan2.Converged() {
			t.Fatalf("expected converged plan on re-apply, got:\n%s", plan2.Text())
		}
	})

	t.Run("custom factory stage name adopts seeded job without leaving duplicate verify job", func(t *testing.T) {
		host := readyStageCronHost(home, "claude", root)
		srv, _, cronJobs := newStageCronTestServer(t, host, []string{"claude"}, nil)
		repoDir := t.TempDir()
		client := &stageCronProjectClient{srv: srv, origin: originURL, trustOnRegister: true}
		resolver := stageCronFactoryResolver{agent: "claude", root: root, home: home}

		cfg := &projectconfig.Config{
			Version: projectconfig.SchemaVersion,
			Project: "app",
			Repo:    projectconfig.Repo{OriginURL: originURL},
			Daemons: []projectconfig.Daemon{{Name: "dev", Path: strPtr(repoDir)}},
			Crons: []projectconfig.Cron{
				{Name: "bs-verify", FactoryStage: strPtr("verify"), Enabled: boolPtr(false)},
				{Name: "nightly", Prompt: strPtr("/nightly"), Schedule: strPtr("0 3 * * *")},
			},
		}
		opts := projectconfig.LocalOptions{
			Daemon:       projectconfig.DaemonIdentity{DisplayName: "dev"},
			CheckoutPath: repoDir,
			HomeDir:      home,
		}

		ctx := context.Background()
		st, err := projectconfig.ObserveLocal(ctx, cfg, client, resolver, opts)
		if err != nil {
			t.Fatalf("ObserveLocal: %v", err)
		}
		res, err := projectconfig.ApplyLocal(ctx, client, projectconfig.PlanLocal(cfg, st, opts))
		if err != nil {
			t.Fatalf("ApplyLocal: %v\n%s", err, res.Text())
		}
		seededVerifyID := client.seededIDsByName["factory-verify"]
		if seededVerifyID == "" || !slices.Equal(client.createdNames, []string{"nightly"}) || !slices.Equal(client.updatedIDs, []string{seededVerifyID}) {
			t.Fatalf("adoption mismatch: seeded=%v created=%v updated=%v", client.seededIDsByName, client.createdNames, client.updatedIDs)
		}

		repos, err := client.ListRepos(ctx)
		if err != nil || len(repos) != 1 {
			t.Fatalf("ListRepos: len=%d err=%v", len(repos), err)
		}
		jobs, err := cronJobs.ListByRepo(ctx, repos[0].GetId())
		if err != nil {
			t.Fatalf("ListByRepo: %v", err)
		}

		verifyStage, _ := projectconfig.DefaultFactoryStages().Lookup("verify")
		var verifyJobs []*domain.CronJob
		for _, j := range jobs {
			if verifyStage.MatchesJob(j.Name, j.Prompt) {
				verifyJobs = append(verifyJobs, j)
			}
		}
		if len(verifyJobs) != 1 {
			t.Fatalf("got %d verify jobs, want 1 (no duplicate seeded factory-verify)", len(verifyJobs))
		}
		if verifyJobs[0].ID != seededVerifyID || verifyJobs[0].Name != "bs-verify" || verifyJobs[0].ManagedBy != marker || verifyJobs[0].IsEnabled {
			t.Fatalf("unexpected adopted verify job: %+v (want id %q)", verifyJobs[0], seededVerifyID)
		}

		st2, err := projectconfig.ObserveLocal(ctx, cfg, client, resolver, opts)
		if err != nil {
			t.Fatalf("second ObserveLocal: %v", err)
		}
		if plan2 := projectconfig.PlanLocal(cfg, st2, opts); !plan2.Converged() {
			t.Fatalf("expected converged plan on re-apply, got:\n%s", plan2.Text())
		}
	})
}

func TestSeedStageCronsSkipsUntrustedRepo(t *testing.T) {
	const (
		home = "/Users/tester"
		root = "/Users/tester/.claude/skills"
	)

	cases := []struct {
		name     string
		register func(t *testing.T, srv *Server) *pb.Repo
	}{
		{
			name: "RegisterRepo with omitted trust",
			register: func(t *testing.T, srv *Server) *pb.Repo {
				t.Helper()
				resp, err := srv.RegisterRepo(context.Background(), connect.NewRequest(&pb.RegisterRepoRequest{
					LocalPath: t.TempDir(),
				}))
				if err != nil {
					t.Fatalf("RegisterRepo: %v", err)
				}
				return resp.Msg.GetRepo()
			},
		},
		{
			name: "RegisterRepo with explicit untrusted",
			register: func(t *testing.T, srv *Server) *pb.Repo {
				t.Helper()
				resp, err := srv.RegisterRepo(context.Background(), connect.NewRequest(&pb.RegisterRepoRequest{
					LocalPath: t.TempDir(),
					IsTrusted: proto.Bool(false),
				}))
				if err != nil {
					t.Fatalf("RegisterRepo: %v", err)
				}
				return resp.Msg.GetRepo()
			},
		},
		{
			name: "CloneAndRegisterRepo with omitted trust and setup detection",
			register: func(t *testing.T, srv *Server) *pb.Repo {
				t.Helper()
				resp, err := srv.CloneAndRegisterRepo(context.Background(), connect.NewRequest(&pb.CloneAndRegisterRepoRequest{
					CloneUrl:                "git@github.com:acme/app.git",
					LocalPath:               filepath.Join(t.TempDir(), "cloned"),
					ShouldDetectSetupScript: proto.Bool(true),
				}))
				if err != nil {
					t.Fatalf("CloneAndRegisterRepo: %v", err)
				}
				return resp.Msg.GetRepo()
			},
		},
		{
			name: "CloneAndRegisterRepo with explicit untrusted",
			register: func(t *testing.T, srv *Server) *pb.Repo {
				t.Helper()
				resp, err := srv.CloneAndRegisterRepo(context.Background(), connect.NewRequest(&pb.CloneAndRegisterRepoRequest{
					CloneUrl:  "git@github.com:acme/app.git",
					LocalPath: filepath.Join(t.TempDir(), "cloned"),
					IsTrusted: proto.Bool(false),
				}))
				if err != nil {
					t.Fatalf("CloneAndRegisterRepo: %v", err)
				}
				return resp.Msg.GetRepo()
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := readyStageCronHost(home, "claude", root)
			var logBuf bytes.Buffer
			srv, _, cronJobs := newStageCronTestServer(t, host, []string{"claude"}, &logBuf)

			repo := tc.register(t, srv)
			if repo.GetIsTrusted() {
				t.Fatal("registered repo IsTrusted = true, want false")
			}

			jobs, err := cronJobs.ListByRepo(context.Background(), repo.GetId())
			if err != nil {
				t.Fatalf("ListByRepo: %v", err)
			}
			if len(jobs) != 0 {
				t.Fatalf("seeded %d cron jobs on untrusted repo, want 0", len(jobs))
			}

			entries := parseJSONLogLines(t, &logBuf)
			if len(entries) != 1 || entries[0]["level"] != "info" || entries[0]["repo_id"] != repo.GetId() {
				t.Fatalf("expected 1 info skip log for repo_id=%q, got:\n%s", repo.GetId(), logBuf.String())
			}
		})
	}

	t.Run("nil repo fails closed without seeding", func(t *testing.T) {
		host := readyStageCronHost(home, "claude", root)
		var logBuf bytes.Buffer
		srv, _, cronJobs := newStageCronTestServer(t, host, []string{"claude"}, &logBuf)

		srv.seedStageCrons(context.Background(), nil)

		jobs, err := cronJobs.List(context.Background())
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(jobs) != 0 {
			t.Fatalf("seeded %d cron jobs for nil repo, want 0", len(jobs))
		}
	})
}
