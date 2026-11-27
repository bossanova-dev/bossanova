package server

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/plugin"
	"github.com/recurser/bossd/internal/setupcheck"
	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

type setupCheckHostService struct {
	names []string
}

func (*setupCheckHostService) Register(*grpc.Server) {}
func (*setupCheckHostService) Validate() error       { return nil }
func (h *setupCheckHostService) AgentClientNames() []string {
	return h.names
}
func (*setupCheckHostService) AgentLogsDir() string { return "" }

type setupCheckRepos struct {
	repoStore
	repos   []*domain.Repo
	getErr  error
	listErr error
}

func (r *setupCheckRepos) Get(_ context.Context, id string) (*domain.Repo, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	for _, repo := range r.repos {
		if repo.ID == id {
			return repo, nil
		}
	}
	return nil, domain.ErrNotFound
}

func (r *setupCheckRepos) List(context.Context) ([]*domain.Repo, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.repos, nil
}

type setupCheckSessions struct {
	sessionStore
	rows    []*domain.Session
	listErr error
}

func (s *setupCheckSessions) List(context.Context, string) ([]*domain.Session, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.rows, nil
}

type setupCheckCrons struct {
	cronJobStore
	rows []*domain.CronJob
}

func (c *setupCheckCrons) ListByRepo(context.Context, string) ([]*domain.CronJob, error) {
	return c.rows, nil
}

func newSetupCheckTestServer(t *testing.T, repoPath string) (*Server, *setupCheckRepos, *setupCheckCrons) {
	t.Helper()
	repos := &setupCheckRepos{
		repos: []*domain.Repo{{ID: "repo-1", LocalPath: repoPath}},
	}
	crons := &setupCheckCrons{
		rows: []*domain.CronJob{{ID: "cron-1", RepoID: "repo-1", Name: "plan"}},
	}
	bins := map[string]string{
		"claude": "/usr/local/bin/claude",
		"codex":  "/usr/local/bin/codex",
		"gh":     "/opt/homebrew/bin/gh",
		"node":   "/opt/homebrew/bin/node",
		"tmux":   "/opt/homebrew/bin/tmux",
	}
	srv := New(Config{
		Repos:      repos,
		Sessions:   &setupCheckSessions{},
		CronJobs:   crons,
		PluginHost: plugin.New(nil, &setupCheckHostService{names: []string{"claude", "codex"}}, zerolog.Nop()),
		SetupProbes: SetupProbes{
			LookPath: func(name string) (string, error) {
				if p, ok := bins[name]; ok {
					return p, nil
				}
				return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
			},
			GHAuth: func(context.Context) *pb.RepairDoctorCheck {
				return &pb.RepairDoctorCheck{Name: "github auth", Ok: true, Detail: "bossd's gh CLI authenticates to GitHub."}
			},
			Skills: func(context.Context) setupcheck.SkillsFact {
				return setupcheck.SkillsFact{
					State:         setupcheck.SkillsCurrent,
					DaemonVersion: "v1.198.0 (abcdef1)",
				}
			},
		},
	})
	return srv, repos, crons
}

func findProtoCheck(t *testing.T, checks []*pb.SetupCheck, id string) *pb.SetupCheck {
	t.Helper()
	for _, c := range checks {
		if c.GetId() == id {
			return c
		}
	}
	t.Fatalf("check %q not found in %v", id, checks)
	return nil
}

func TestListSetupChecksRegisteredAndDeterministic_BossInit_AC268302(t *testing.T) {
	repoDir := t.TempDir()
	srv, repos, crons := newSetupCheckTestServer(t, repoDir)

	first, err := srv.ListSetupChecks(t.Context(), connect.NewRequest(&pb.ListSetupChecksRequest{WorkspacePath: repoDir}))
	if err != nil {
		t.Fatalf("first ListSetupChecks: %v", err)
	}
	second, err := srv.ListSetupChecks(t.Context(), connect.NewRequest(&pb.ListSetupChecksRequest{WorkspacePath: repoDir}))
	if err != nil {
		t.Fatalf("second ListSetupChecks: %v", err)
	}

	if first.Msg.GetRepoId() != "repo-1" || second.Msg.GetRepoId() != "repo-1" {
		t.Fatalf("repo_id = (%q, %q), want repo-1", first.Msg.GetRepoId(), second.Msg.GetRepoId())
	}
	if first.Msg.GetCheckedAt() == nil || second.Msg.GetCheckedAt() == nil {
		t.Fatal("checked_at must be populated")
	}
	if len(first.Msg.GetChecks()) != len(setupcheck.IDs()) {
		t.Fatalf("check count = %d, want %d", len(first.Msg.GetChecks()), len(setupcheck.IDs()))
	}
	for i, c := range first.Msg.GetChecks() {
		if c.GetStatus() != pb.SetupCheckStatus_SETUP_CHECK_STATUS_OK {
			t.Errorf("check %q status = %v, want OK (evidence=%q)", c.GetId(), c.GetStatus(), c.GetEvidence())
		}
		if !proto.Equal(c, second.Msg.GetChecks()[i]) {
			t.Errorf("check[%d] differed between runs:\nfirst:  %v\nsecond: %v", i, c, second.Msg.GetChecks()[i])
		}
	}
	// Verify repo rows and cron jobs are untouched.
	if len(repos.repos) != 1 || repos.repos[0].ID != "repo-1" {
		t.Fatalf("repos mutated: %+v", repos.repos)
	}
	if len(crons.rows) != 1 || crons.rows[0].ID != "cron-1" {
		t.Fatalf("cron jobs mutated: %+v", crons.rows)
	}
}

func TestListSetupChecksUnregisteredWorkspace_BossInit_AC268304(t *testing.T) {
	registeredDir := t.TempDir()
	unregisteredDir := t.TempDir()
	srv, _, _ := newSetupCheckTestServer(t, registeredDir)

	resp, err := srv.ListSetupChecks(t.Context(), connect.NewRequest(&pb.ListSetupChecksRequest{WorkspacePath: unregisteredDir}))
	if err != nil {
		t.Fatalf("ListSetupChecks on unregistered path returned error: %v", err)
	}
	if resp.Msg.GetRepoId() != "" {
		t.Fatalf("repo_id = %q, want empty for unregistered workspace", resp.Msg.GetRepoId())
	}

	repoCheck := findProtoCheck(t, resp.Msg.GetChecks(), "repo-registered")
	if repoCheck.GetStatus() != pb.SetupCheckStatus_SETUP_CHECK_STATUS_MISSING {
		t.Fatalf("repo-registered status = %v, want MISSING", repoCheck.GetStatus())
	}
	wantCmd := "boss init --register --merge --dir " + unregisteredDir
	if repoCheck.GetFix().GetKind() != pb.SetupCheckFixKind_SETUP_CHECK_FIX_KIND_COMMAND ||
		repoCheck.GetFix().GetCommand() != wantCmd {
		t.Fatalf("repo-registered fix = %v, want COMMAND %q", repoCheck.GetFix(), wantCmd)
	}

	// Machine checks must still run and report OK.
	for _, id := range []string{"daemon-running", "agent-runner", "agent-cli-on-path", "gh-auth", "node-on-daemon-path", "tmux-on-daemon-path", "skills-current"} {
		c := findProtoCheck(t, resp.Msg.GetChecks(), id)
		if c.GetStatus() != pb.SetupCheckStatus_SETUP_CHECK_STATUS_OK {
			t.Errorf("machine check %q status = %v, want OK", id, c.GetStatus())
		}
	}
}

func TestListSetupChecksValidationAndRepoSelectors(t *testing.T) {
	repoDir := t.TempDir()
	srv, _, _ := newSetupCheckTestServer(t, repoDir)

	// Both repo_id and workspace_path set -> InvalidArgument.
	if _, err := srv.ListSetupChecks(t.Context(), connect.NewRequest(&pb.ListSetupChecksRequest{
		RepoId:        "repo-1",
		WorkspacePath: repoDir,
	})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("both selectors: code = %v, want InvalidArgument", connect.CodeOf(err))
	}

	// Unknown repo_id -> NotFound.
	if _, err := srv.ListSetupChecks(t.Context(), connect.NewRequest(&pb.ListSetupChecksRequest{
		RepoId: "unknown-repo",
	})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown repo_id: code = %v, want NotFound", connect.CodeOf(err))
	}

	// Known repo_id -> OK with RepoId populated.
	byID, err := srv.ListSetupChecks(t.Context(), connect.NewRequest(&pb.ListSetupChecksRequest{
		RepoId: "repo-1",
	}))
	if err != nil {
		t.Fatalf("known repo_id: %v", err)
	}
	if byID.Msg.GetRepoId() != "repo-1" {
		t.Fatalf("known repo_id: got repo_id=%q", byID.Msg.GetRepoId())
	}
	if got := findProtoCheck(t, byID.Msg.GetChecks(), "repo-registered").GetStatus(); got != pb.SetupCheckStatus_SETUP_CHECK_STATUS_OK {
		t.Fatalf("known repo_id: repo-registered=%v, want OK", got)
	}

	// Neither set -> repo-registered BLOCKED, machine checks still run.
	neither, err := srv.ListSetupChecks(t.Context(), connect.NewRequest(&pb.ListSetupChecksRequest{}))
	if err != nil {
		t.Fatalf("neither selector: %v", err)
	}
	if neither.Msg.GetRepoId() != "" {
		t.Fatalf("neither selector: repo_id=%q, want empty", neither.Msg.GetRepoId())
	}
	if got := findProtoCheck(t, neither.Msg.GetChecks(), "repo-registered").GetStatus(); got != pb.SetupCheckStatus_SETUP_CHECK_STATUS_BLOCKED {
		t.Fatalf("neither selector: repo-registered=%v, want BLOCKED", got)
	}
	if got := findProtoCheck(t, neither.Msg.GetChecks(), "node-on-daemon-path").GetStatus(); got != pb.SetupCheckStatus_SETUP_CHECK_STATUS_OK {
		t.Fatalf("neither selector: node-on-daemon-path=%v, want OK", got)
	}
}

func TestListSetupChecksDegradedAndUnreadableProbes(t *testing.T) {
	repoDir := t.TempDir()
	srv, _, _ := newSetupCheckTestServer(t, repoDir)
	srv.setupProbes.Skills = func(context.Context) setupcheck.SkillsFact {
		return setupcheck.SkillsFact{
			State: setupcheck.SkillsUnreadable,
			Err:   "revision history unavailable",
		}
	}
	srv.setupProbes.LookPath = func(name string) (string, error) {
		switch name {
		case "claude":
			return "/usr/local/bin/claude", nil
		case "codex":
			return "/usr/local/bin/codex", nil
		case "node", "tmux":
			return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
		default:
			return "", errors.New("unexpected lookup error")
		}
	}
	srv.setupProbes.GHAuth = func(context.Context) *pb.RepairDoctorCheck {
		return &pb.RepairDoctorCheck{
			Name:   "github auth",
			Ok:     false,
			Detail: "cannot reach GitHub with `gh api -i /user`: dial tcp timeout",
		}
	}

	resp, err := srv.ListSetupChecks(t.Context(), connect.NewRequest(&pb.ListSetupChecksRequest{WorkspacePath: repoDir}))
	if err != nil {
		t.Fatalf("ListSetupChecks degraded: %v", err)
	}

	want := map[string]pb.SetupCheckStatus{
		"daemon-running":      pb.SetupCheckStatus_SETUP_CHECK_STATUS_OK,
		"repo-registered":     pb.SetupCheckStatus_SETUP_CHECK_STATUS_OK,
		"agent-runner":        pb.SetupCheckStatus_SETUP_CHECK_STATUS_OK,
		"agent-cli-on-path":   pb.SetupCheckStatus_SETUP_CHECK_STATUS_OK,
		"gh-auth":             pb.SetupCheckStatus_SETUP_CHECK_STATUS_BLOCKED,
		"node-on-daemon-path": pb.SetupCheckStatus_SETUP_CHECK_STATUS_MISSING,
		"tmux-on-daemon-path": pb.SetupCheckStatus_SETUP_CHECK_STATUS_MISSING,
		"skills-current":      pb.SetupCheckStatus_SETUP_CHECK_STATUS_BLOCKED,
	}
	gotIDs := make([]string, 0, len(resp.Msg.GetChecks()))
	for _, c := range resp.Msg.GetChecks() {
		gotIDs = append(gotIDs, c.GetId())
		if c.GetStatus() != want[c.GetId()] {
			t.Errorf("check %q: status = %v, want %v (evidence=%q)", c.GetId(), c.GetStatus(), want[c.GetId()], c.GetEvidence())
		}
	}
	if !reflect.DeepEqual(gotIDs, setupcheck.IDs()) {
		t.Fatalf("check order = %v, want %v", gotIDs, setupcheck.IDs())
	}
	if sc := findProtoCheck(t, resp.Msg.GetChecks(), "skills-current"); !strings.Contains(sc.GetEvidence(), "revision history unavailable") {
		t.Errorf("skills-current evidence = %q, want error detail", sc.GetEvidence())
	}
}

func TestListSetupChecksGHAuthClassification(t *testing.T) {
	cases := []struct {
		name                  string
		probe                 *pb.RepairDoctorCheck
		useLookPathNotFound   bool
		wantStatus            pb.SetupCheckStatus
		wantFixKind           pb.SetupCheckFixKind
		wantCmd               string
		wantAgentTaskContains string
	}{
		{
			name: "not logged in",
			probe: &pb.RepairDoctorCheck{
				Name:   "github auth",
				Ok:     false,
				Detail: "bossd's gh CLI is not authenticated to GitHub, so PR creation, checks and merges all fail. gh said: HTTP 401. run `gh auth login`.",
			},
			wantStatus:  pb.SetupCheckStatus_SETUP_CHECK_STATUS_MISSING,
			wantFixKind: pb.SetupCheckFixKind_SETUP_CHECK_FIX_KIND_COMMAND,
			wantCmd:     "gh auth login",
		},
		{
			name: "not installed from doctor detail",
			probe: &pb.RepairDoctorCheck{
				Name:   "github auth",
				Ok:     false,
				Detail: `cannot reach GitHub with 'gh api -i /user': exec: "gh": executable file not found in $PATH`,
			},
			wantStatus:            pb.SetupCheckStatus_SETUP_CHECK_STATUS_MISSING,
			wantFixKind:           pb.SetupCheckFixKind_SETUP_CHECK_FIX_KIND_AGENT_TASK,
			wantAgentTaskContains: "daemon_path_extra",
		},
		{
			name:                  "not installed from LookPath",
			useLookPathNotFound:   true,
			wantStatus:            pb.SetupCheckStatus_SETUP_CHECK_STATUS_MISSING,
			wantFixKind:           pb.SetupCheckFixKind_SETUP_CHECK_FIX_KIND_AGENT_TASK,
			wantAgentTaskContains: "daemon_path_extra",
		},
		{
			name:        "nil probe fails closed",
			probe:       nil,
			wantStatus:  pb.SetupCheckStatus_SETUP_CHECK_STATUS_BLOCKED,
			wantFixKind: pb.SetupCheckFixKind_SETUP_CHECK_FIX_KIND_COMMAND,
			wantCmd:     "boss daemon doctor",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repoDir := t.TempDir()
			srv, _, _ := newSetupCheckTestServer(t, repoDir)
			if tc.useLookPathNotFound {
				srv.setupProbes.GHAuth = nil
				origLookPath := srv.setupProbes.LookPath
				srv.setupProbes.LookPath = func(name string) (string, error) {
					if name == "gh" {
						return "", &exec.Error{Name: "gh", Err: exec.ErrNotFound}
					}
					return origLookPath(name)
				}
			} else {
				srv.setupProbes.GHAuth = func(context.Context) *pb.RepairDoctorCheck { return tc.probe }
			}

			resp, err := srv.ListSetupChecks(t.Context(), connect.NewRequest(&pb.ListSetupChecksRequest{WorkspacePath: repoDir}))
			if err != nil {
				t.Fatalf("ListSetupChecks: %v", err)
			}
			gh := findProtoCheck(t, resp.Msg.GetChecks(), "gh-auth")
			if gh.GetStatus() != tc.wantStatus {
				t.Fatalf("gh-auth status = %v, want %v", gh.GetStatus(), tc.wantStatus)
			}
			if gh.GetFix().GetKind() != tc.wantFixKind {
				t.Fatalf("gh-auth fix kind = %v, want %v", gh.GetFix().GetKind(), tc.wantFixKind)
			}
			if gh.GetFix().GetCommand() != tc.wantCmd {
				t.Fatalf("gh-auth fix command = %q, want %q", gh.GetFix().GetCommand(), tc.wantCmd)
			}
			if tc.wantAgentTaskContains != "" && !strings.Contains(gh.GetFix().GetAgentTask(), tc.wantAgentTaskContains) {
				t.Fatalf("gh-auth fix agent_task = %q, want substring %q", gh.GetFix().GetAgentTask(), tc.wantAgentTaskContains)
			}
		})
	}
}

func TestListSetupChecksWorktreeOwnershipErrorReportsBlocked(t *testing.T) {
	repoDir := t.TempDir()
	worktreeDir := t.TempDir()
	srv, _, _ := newSetupCheckTestServer(t, repoDir)
	sessions := &setupCheckSessions{
		rows: []*domain.Session{{ID: "sess-1", RepoID: "repo-1", WorktreePath: worktreeDir}},
	}
	srv.sessions = sessions

	// When session lookup succeeds, a registered session worktree resolves as OK.
	okResp, err := srv.ListSetupChecks(t.Context(), connect.NewRequest(&pb.ListSetupChecksRequest{WorkspacePath: worktreeDir}))
	if err != nil {
		t.Fatalf("ListSetupChecks in worktree: %v", err)
	}
	if okResp.Msg.GetRepoId() != "repo-1" {
		t.Fatalf("repo_id = %q, want repo-1", okResp.Msg.GetRepoId())
	}
	if got := findProtoCheck(t, okResp.Msg.GetChecks(), "repo-registered").GetStatus(); got != pb.SetupCheckStatus_SETUP_CHECK_STATUS_OK {
		t.Fatalf("repo-registered status = %v, want OK", got)
	}

	// When session lookup fails, worktree ownership is unreadable -> BLOCKED (never MISSING).
	sessions.listErr = errors.New("sqlite database is locked")
	blockedResp, err := srv.ListSetupChecks(t.Context(), connect.NewRequest(&pb.ListSetupChecksRequest{WorkspacePath: worktreeDir}))
	if err != nil {
		t.Fatalf("ListSetupChecks with session list error: %v", err)
	}
	if blockedResp.Msg.GetRepoId() != "" {
		t.Fatalf("repo_id = %q, want empty when unreadable", blockedResp.Msg.GetRepoId())
	}
	repoCheck := findProtoCheck(t, blockedResp.Msg.GetChecks(), "repo-registered")
	if repoCheck.GetStatus() != pb.SetupCheckStatus_SETUP_CHECK_STATUS_BLOCKED {
		t.Fatalf("repo-registered status = %v, want BLOCKED (evidence=%q)", repoCheck.GetStatus(), repoCheck.GetEvidence())
	}
	if !strings.Contains(repoCheck.GetEvidence(), "sqlite database is locked") {
		t.Fatalf("repo-registered evidence = %q, want session store error detail", repoCheck.GetEvidence())
	}
	if got := findProtoCheck(t, blockedResp.Msg.GetChecks(), "node-on-daemon-path").GetStatus(); got != pb.SetupCheckStatus_SETUP_CHECK_STATUS_OK {
		t.Fatalf("machine check node-on-daemon-path = %v, want OK", got)
	}
}

func TestListSetupChecksSkillsStates(t *testing.T) {
	cases := []struct {
		name             string
		probe            func(context.Context) setupcheck.SkillsFact
		wantStatus       pb.SetupCheckStatus
		wantCmd          string
		wantEvidContains string
	}{
		{
			name: "missing skills",
			probe: func(context.Context) setupcheck.SkillsFact {
				return setupcheck.SkillsFact{
					State:         setupcheck.SkillsMissing,
					DaemonVersion: "v1.198.0 (abcdef1)",
				}
			},
			wantStatus:       pb.SetupCheckStatus_SETUP_CHECK_STATUS_MISSING,
			wantCmd:          "boss skills install",
			wantEvidContains: "no Bossanova skills",
		},
		{
			name: "installed skills older than daemon",
			probe: func(context.Context) setupcheck.SkillsFact {
				return setupcheck.SkillsFact{
					State:         setupcheck.SkillsOutdated,
					DaemonVersion: "v1.198.0 (abcdef1)",
					SkillsVersion: "v1.197.0 (1234567)",
				}
			},
			wantStatus:       pb.SetupCheckStatus_SETUP_CHECK_STATUS_OUTDATED,
			wantCmd:          "boss skills sync",
			wantEvidContains: "older than daemon",
		},
		{
			name: "daemon older than installed skills",
			probe: func(context.Context) setupcheck.SkillsFact {
				return setupcheck.SkillsFact{
					State:         setupcheck.SkillsDaemonOutdated,
					DaemonVersion: "v1.198.0 (abcdef1)",
					SkillsVersion: "v1.199.0 (7654321)",
				}
			},
			wantStatus:       pb.SetupCheckStatus_SETUP_CHECK_STATUS_OUTDATED,
			wantCmd:          "boss daemon restart",
			wantEvidContains: "running daemon v1.198.0 (abcdef1) is older than installed skills v1.199.0 (7654321)",
		},
		{
			name:             "nil skills probe fails closed",
			probe:            nil,
			wantStatus:       pb.SetupCheckStatus_SETUP_CHECK_STATUS_BLOCKED,
			wantCmd:          "boss skills sync",
			wantEvidContains: "skill version probe unavailable",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repoDir := t.TempDir()
			srv, _, _ := newSetupCheckTestServer(t, repoDir)
			srv.setupProbes.Skills = tc.probe

			resp, err := srv.ListSetupChecks(t.Context(), connect.NewRequest(&pb.ListSetupChecksRequest{WorkspacePath: repoDir}))
			if err != nil {
				t.Fatalf("ListSetupChecks: %v", err)
			}
			sc := findProtoCheck(t, resp.Msg.GetChecks(), "skills-current")
			if sc.GetStatus() != tc.wantStatus {
				t.Fatalf("skills-current status = %v, want %v (evidence=%q)", sc.GetStatus(), tc.wantStatus, sc.GetEvidence())
			}
			if sc.GetFix().GetCommand() != tc.wantCmd {
				t.Fatalf("skills-current fix command = %q, want %q", sc.GetFix().GetCommand(), tc.wantCmd)
			}
			if !strings.Contains(sc.GetEvidence(), tc.wantEvidContains) {
				t.Fatalf("skills-current evidence = %q, want substring %q", sc.GetEvidence(), tc.wantEvidContains)
			}
		})
	}
}
