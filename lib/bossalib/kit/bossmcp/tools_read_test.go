package bossmcp

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
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
			backend: &fakeBackend{listRepoPRs: func(_ context.Context, req *pb.ListRepoPRsRequest) ([]*pb.PRSummary, error) {
				if req.GetRepoId() != "r1" {
					t.Errorf("repo_id not forwarded: %q", req.GetRepoId())
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
			backend: &fakeBackend{listChats: func(_ context.Context, sid string) ([]*pb.AgentChat, error) {
				if sid != "s1" {
					t.Errorf("session_id not forwarded: %q", sid)
				}
				return []*pb.AgentChat{{AgentSessionId: "chat-lc"}}, nil
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

// Proto fields are forwarded by the read tools without adding schema/tool bytes.
func TestStatusReadToolsExposePhase(t *testing.T) {
	backend := &fakeBackend{
		getChatStatuses: func(context.Context, string) ([]*pb.ChatStatusEntry, error) {
			return []*pb.ChatStatusEntry{{AgentSessionId: "chat", Status: pb.ChatStatus_CHAT_STATUS_WORKING, Phase: "reviewing"}}, nil
		},
		getSessionStatuses: func(context.Context, []string) ([]*pb.SessionStatusEntry, error) {
			return []*pb.SessionStatusEntry{{SessionId: "session", Status: pb.ChatStatus_CHAT_STATUS_WORKING, Phase: "reviewing"}}, nil
		},
	}
	cs := newConnectedClient(t, backend, Options{})
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"get_chat_statuses", map[string]any{"session_id": "session"}},
		{"get_session_statuses", map[string]any{"session_ids": []string{"session"}}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			result, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tc.tool, Arguments: tc.args})
			if err != nil {
				t.Fatal(err)
			}
			if result.IsError {
				t.Fatal(textOf(t, result))
			}
			var rows []map[string]any
			if err := json.Unmarshal([]byte(textOf(t, result)), &rows); err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0]["phase"] != "reviewing" {
				t.Fatalf("phase missing: %v", rows)
			}
		})
	}
}

func TestListSessionsOrdersAndLimits_McpServer_AC277401(t *testing.T) {
	at := func(day int) *timestamppb.Timestamp {
		return timestamppb.New(time.Date(2026, 10, day, 0, 0, 0, 0, time.UTC))
	}
	// The backend order matches neither timestamp, so a default-order answer
	// cannot pass as an ordered one.
	backend := &fakeBackend{
		listSessions: func(context.Context, *pb.ListSessionsRequest) ([]*pb.Session, error) {
			return []*pb.Session{
				{Id: "mid", CreatedAt: at(2), UpdatedAt: at(9)},
				{Id: "newest", CreatedAt: at(4), UpdatedAt: at(1)},
				{Id: "oldest", CreatedAt: at(1), UpdatedAt: at(5)},
				{Id: "second", CreatedAt: at(3), UpdatedAt: at(3)},
			}, nil
		},
	}
	cs := newConnectedClient(t, backend, Options{})
	call := func(t *testing.T, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_sessions", Arguments: args})
		if err != nil {
			t.Fatalf("call list_sessions: %v", err)
		}
		return res
	}

	for _, tc := range []struct {
		name string
		args map[string]any
		want []string
	}{
		{"created oldest first, limit 2, returns the two oldest", map[string]any{"order_by": "created", "direction": "oldest_first", "limit": 2}, []string{"oldest", "mid"}},
		{"updated without direction is newest first", map[string]any{"order_by": "updated"}, []string{"mid", "oldest", "second", "newest"}},
		{"created newest first", map[string]any{"order_by": "created", "direction": "newest_first"}, []string{"newest", "second", "mid", "oldest"}},
		{"no arguments keep the backend order", map[string]any{}, []string{"mid", "newest", "oldest", "second"}},
		{"limit alone keeps the backend order", map[string]any{"limit": 3}, []string{"mid", "newest", "oldest"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := call(t, tc.args)
			if res.IsError {
				t.Fatalf("error result: %s", textOf(t, res))
			}
			var rows []struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal([]byte(textOf(t, res)), &rows); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got := make([]string, 0, len(rows))
			for _, row := range rows {
				got = append(got, row.ID)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("ids = %v, want %v", got, tc.want)
			}
		})
	}

	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"unknown order_by", map[string]any{"order_by": "rank"}},
		{"unknown direction", map[string]any{"order_by": "created", "direction": "sideways"}},
		{"direction without order_by", map[string]any{"direction": "oldest_first"}},
		{"negative limit", map[string]any{"limit": -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if res := call(t, tc.args); !res.IsError {
				t.Fatalf("want an error result, got %s", textOf(t, res))
			}
		})
	}
}
