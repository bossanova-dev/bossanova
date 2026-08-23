package bossmcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// TestReadTools drives every read-only tool over an in-memory transport,
// asserting it forwards to the configured Backend method and returns a
// sentinel value in the JSON result.
func TestReadTools(t *testing.T) {
	cases := []struct {
		tool     string
		args     map[string]any
		backend  *fakeBackend
		sentinel string // substring expected in the JSON text result
	}{
		{
			tool: "resolve_context",
			args: map[string]any{"working_dir": "/tmp/x"},
			backend: &fakeBackend{resolveContext: func(_ context.Context, wd string) (*pb.ResolveContextResponse, error) {
				if wd != "/tmp/x" {
					t.Errorf("working_dir not forwarded: %q", wd)
				}
				return &pb.ResolveContextResponse{Repo: &pb.Repo{Id: "repo-rc"}}, nil
			}},
			sentinel: "repo-rc",
		},
		{
			tool: "validate_repo_path",
			args: map[string]any{"local_path": "/code/x"},
			backend: &fakeBackend{validateRepoPath: func(_ context.Context, p string) (*pb.ValidateRepoPathResponse, error) {
				if p != "/code/x" {
					t.Errorf("local_path not forwarded: %q", p)
				}
				return &pb.ValidateRepoPathResponse{OriginUrl: "git@vrp"}, nil
			}},
			sentinel: "git@vrp",
		},
		{
			tool: "list_repos",
			args: map[string]any{},
			backend: &fakeBackend{listRepos: func(_ context.Context) ([]*pb.Repo, error) {
				return []*pb.Repo{{Id: "repo-lr"}}, nil
			}},
			sentinel: "repo-lr",
		},
		{
			tool: "list_repo_prs",
			args: map[string]any{"repo_id": "r1"},
			backend: &fakeBackend{listRepoPRs: func(_ context.Context, id string) ([]*pb.PRSummary, error) {
				if id != "r1" {
					t.Errorf("repo_id not forwarded: %q", id)
				}
				return []*pb.PRSummary{{Title: "pr-sentinel"}}, nil
			}},
			sentinel: "pr-sentinel",
		},
		{
			tool: "list_tracker_issues",
			args: map[string]any{"repo_id": "r1", "query": "q", "source": "linear"},
			backend: &fakeBackend{listTrackerIssues: func(_ context.Context, repoID, query, source string) ([]*pb.TrackerIssue, error) {
				if repoID != "r1" || query != "q" || source != "linear" {
					t.Errorf("args not forwarded: %q %q %q", repoID, query, source)
				}
				return []*pb.TrackerIssue{{ExternalId: "issue-ti"}}, nil
			}},
			sentinel: "issue-ti",
		},
		{
			tool: "get_session",
			args: map[string]any{"id": "s1"},
			backend: &fakeBackend{getSession: func(_ context.Context, id string) (*pb.Session, error) {
				if id != "s1" {
					t.Errorf("id not forwarded: %q", id)
				}
				return &pb.Session{Id: "sess-gs"}, nil
			}},
			sentinel: "sess-gs",
		},
		{
			tool: "list_chats",
			args: map[string]any{"session_id": "s1"},
			backend: &fakeBackend{listChats: func(_ context.Context, sid string) ([]*pb.ClaudeChat, error) {
				if sid != "s1" {
					t.Errorf("session_id not forwarded: %q", sid)
				}
				return []*pb.ClaudeChat{{AgentSessionId: "chat-lc"}}, nil
			}},
			sentinel: "chat-lc",
		},
		{
			tool: "get_chat_statuses",
			args: map[string]any{"session_id": "s1"},
			backend: &fakeBackend{getChatStatuses: func(_ context.Context, sid string) ([]*pb.ChatStatusEntry, error) {
				if sid != "s1" {
					t.Errorf("session_id not forwarded: %q", sid)
				}
				return []*pb.ChatStatusEntry{{AgentSessionId: "chat-gcs"}}, nil
			}},
			sentinel: "chat-gcs",
		},
		{
			tool: "get_session_statuses",
			args: map[string]any{"session_ids": []any{"s1", "s2"}},
			backend: &fakeBackend{getSessionStatuses: func(_ context.Context, ids []string) ([]*pb.SessionStatusEntry, error) {
				if len(ids) != 2 || ids[0] != "s1" || ids[1] != "s2" {
					t.Errorf("session_ids not forwarded: %v", ids)
				}
				return []*pb.SessionStatusEntry{{SessionId: "sess-gss"}}, nil
			}},
			sentinel: "sess-gss",
		},
		{
			tool: "list_check_snapshots",
			args: map[string]any{"session_id": "s1", "limit": 5},
			backend: &fakeBackend{listCheckSnapshots: func(_ context.Context, sid string, limit int32) (*pb.ListCheckSnapshotsResponse, error) {
				if sid != "s1" || limit != 5 {
					t.Errorf("args not forwarded: %q %d", sid, limit)
				}
				return &pb.ListCheckSnapshotsResponse{Snapshots: []*pb.CheckSnapshot{{HeadSha: "snap-lcs"}}}, nil
			}},
			sentinel: "snap-lcs",
		},
		{
			tool: "repair_doctor",
			args: map[string]any{},
			backend: &fakeBackend{repairDoctor: func(_ context.Context) (*pb.RepairDoctorResponse, error) {
				return &pb.RepairDoctorResponse{Checks: []*pb.RepairDoctorCheck{{Name: "check-rd"}}}, nil
			}},
			sentinel: "check-rd",
		},
		{
			tool: "list_agents",
			args: map[string]any{},
			backend: &fakeBackend{listAgents: func(_ context.Context) ([]*pb.AgentInfo, error) {
				return []*pb.AgentInfo{{Name: "agent-la"}}, nil
			}},
			sentinel: "agent-la",
		},
		{
			tool: "list_plugins",
			args: map[string]any{},
			backend: &fakeBackend{listPlugins: func(_ context.Context) ([]*pb.InstalledPlugin, error) {
				return []*pb.InstalledPlugin{{Name: "plugin-lp"}}, nil
			}},
			sentinel: "plugin-lp",
		},
		{
			tool: "list_cron_jobs",
			args: map[string]any{},
			backend: &fakeBackend{listCronJobs: func(_ context.Context) ([]*pb.CronJob, error) {
				return []*pb.CronJob{{Id: "cron-lcj"}}, nil
			}},
			sentinel: "cron-lcj",
		},
		{
			tool: "get_cron_job",
			args: map[string]any{"id": "c1"},
			backend: &fakeBackend{getCronJob: func(_ context.Context, id string) (*pb.CronJob, error) {
				if id != "c1" {
					t.Errorf("id not forwarded: %q", id)
				}
				return &pb.CronJob{Id: "cron-gcj"}, nil
			}},
			sentinel: "cron-gcj",
		},
	}

	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			cs := newConnectedClient(t, tc.backend, Options{})
			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tc.tool, Arguments: tc.args})
			if err != nil {
				t.Fatalf("call %s: %v", tc.tool, err)
			}
			if res.IsError {
				t.Fatalf("%s returned error result: %s", tc.tool, textOf(t, res))
			}
			if got := textOf(t, res); !strings.Contains(got, tc.sentinel) {
				t.Fatalf("%s result missing sentinel %q: %s", tc.tool, tc.sentinel, got)
			}
		})
	}
}

func TestCronJobReadToolsExposeLastRunAgentMismatch(t *testing.T) {
	backend := &fakeBackend{
		listCronJobs: func(_ context.Context) ([]*pb.CronJob, error) {
			return []*pb.CronJob{{
				Id:               "cron-list",
				AgentName:        "claude",
				LastRunAgentName: "codex",
			}}, nil
		},
		getCronJob: func(_ context.Context, id string) (*pb.CronJob, error) {
			if id != "cron-get" {
				t.Errorf("id not forwarded: %q", id)
			}
			return &pb.CronJob{
				Id:               "cron-get",
				AgentName:        "claude",
				LastRunAgentName: "codex",
			}, nil
		},
	}
	cs := newConnectedClient(t, backend, Options{})

	listRes, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_cron_jobs", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("call list_cron_jobs: %v", err)
	}
	if listRes.IsError {
		t.Fatalf("list_cron_jobs returned error result: %s", textOf(t, listRes))
	}
	var listPayload []map[string]any
	if err := json.Unmarshal([]byte(textOf(t, listRes)), &listPayload); err != nil {
		t.Fatalf("decode list_cron_jobs result: %v\n%s", err, textOf(t, listRes))
	}
	if len(listPayload) != 1 {
		t.Fatalf("list_cron_jobs returned %d jobs, want 1", len(listPayload))
	}
	assertCronMismatchPayload(t, listPayload[0], "cron-list")

	getRes, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_cron_job", Arguments: map[string]any{"id": "cron-get"}})
	if err != nil {
		t.Fatalf("call get_cron_job: %v", err)
	}
	if getRes.IsError {
		t.Fatalf("get_cron_job returned error result: %s", textOf(t, getRes))
	}
	var getPayload map[string]any
	if err := json.Unmarshal([]byte(textOf(t, getRes)), &getPayload); err != nil {
		t.Fatalf("decode get_cron_job result: %v\n%s", err, textOf(t, getRes))
	}
	assertCronMismatchPayload(t, getPayload, "cron-get")
}

func assertCronMismatchPayload(t *testing.T, payload map[string]any, wantID string) {
	t.Helper()
	if got := payload["id"]; got != wantID {
		t.Fatalf("id = %v, want %q; payload=%v", got, wantID, payload)
	}
	if got := payload["last_run_agent_name"]; got != "codex" {
		t.Fatalf("last_run_agent_name = %v, want codex; payload=%v", got, payload)
	}
	if got := payload["last_run_agent_label"]; got != "codex (mismatch: job claude)" {
		t.Fatalf("last_run_agent_label = %v, want mismatch label; payload=%v", got, payload)
	}
	if got := payload["last_run_agent_mismatch"]; got != true {
		t.Fatalf("last_run_agent_mismatch = %v, want true; payload=%v", got, payload)
	}
}
