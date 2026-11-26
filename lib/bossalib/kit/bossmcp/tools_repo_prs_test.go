package bossmcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func callListRepoPRs(t *testing.T, backend *fakeBackend, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	cs := newConnectedClient(t, backend, Options{})
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_repo_prs", Arguments: args})
	if err != nil {
		t.Fatalf("call list_repo_prs: %v", err)
	}
	return res
}

// TestListRepoPRsTool_McpServer_AC276802 pins the argument mapping: every
// argument reaches the daemon query, owned PRs are included by default, and an
// unknown order is a tool error raised before any daemon call.
func TestListRepoPRsTool_McpServer_AC276802(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want *pb.PRListQuery
	}{
		{"defaults include owned PRs", map[string]any{"repo_id": "r1"}, &pb.PRListQuery{OwnerFilter: pb.PRListOwnerFilter_PR_LIST_OWNER_FILTER_ALL}},
		{
			"every argument maps",
			map[string]any{"repo_id": "r1", "author": "alice", "base_branch": "main", "order": "PR_LIST_ORDER_UPDATED_DESC", "limit": 5, "without_session": true},
			&pb.PRListQuery{
				Author: "alice", BaseBranch: "main", Order: pb.PRListOrder_PR_LIST_ORDER_UPDATED_DESC, Limit: 5,
				OwnerFilter: pb.PRListOwnerFilter_PR_LIST_OWNER_FILTER_UNOWNED,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got *pb.ListRepoPRsRequest
			backend := &fakeBackend{listRepoPRs: func(_ context.Context, req *pb.ListRepoPRsRequest) ([]*pb.PRSummary, error) {
				got = req
				return nil, nil
			}}
			if res := callListRepoPRs(t, backend, tc.args); res.IsError {
				t.Fatalf("tool error: %s", textOf(t, res))
			}
			if got.GetRepoId() != "r1" || !proto.Equal(got.GetQuery(), tc.want) {
				t.Fatalf("request = %v, want repo r1 with query %v", got, tc.want)
			}
		})
	}

	t.Run("unknown order is rejected before any daemon call", func(t *testing.T) {
		called := false
		backend := &fakeBackend{listRepoPRs: func(context.Context, *pb.ListRepoPRsRequest) ([]*pb.PRSummary, error) {
			called = true
			return nil, nil
		}}
		res := callListRepoPRs(t, backend, map[string]any{"repo_id": "r1", "order": "PR_LIST_ORDER_SIDEWAYS"})
		if !res.IsError || called {
			t.Fatalf("result error = %v, backend called = %v; want a tool error and no call", res.IsError, called)
		}
		if text := textOf(t, res); !strings.Contains(text, "UPDATED_DESC") {
			t.Fatalf("error %q does not list the valid orders", text)
		}
	})
}

// TestListRepoPRsTool_McpServer_AC276801 pins the output: new PR fields,
// RFC3339 times and enum names, with an unowned PR's session fields empty.
func TestListRepoPRsTool_McpServer_AC276801(t *testing.T) {
	created := time.Date(2026, 3, 1, 9, 30, 0, 0, time.UTC)
	backend := &fakeBackend{listRepoPRs: func(context.Context, *pb.ListRepoPRsRequest) ([]*pb.PRSummary, error) {
		return []*pb.PRSummary{
			{
				Number: 7, BaseBranch: "main", IsDraft: true, HeadSha: "abc", State: pb.PRState_PR_STATE_OPEN,
				CreatedAt: timestamppb.New(created), UpdatedAt: timestamppb.New(created.Add(time.Hour)),
				SessionId: "s1", SessionDisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_PASSING,
			},
			{Number: 8},
		}, nil
	}}
	res := callListRepoPRs(t, backend, map[string]any{"repo_id": "r1"})
	if res.IsError {
		t.Fatalf("tool error: %s", textOf(t, res))
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(textOf(t, res)), &rows); err != nil {
		t.Fatalf("decode result: %v\n%s", err, textOf(t, res))
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %v, want 2", rows)
	}
	owned := rows[0]
	want := map[string]any{
		"base_branch": "main", "is_draft": true, "head_sha": "abc", "state": "PR_STATE_OPEN",
		"created_at": "2026-03-01T09:30:00Z", "updated_at": "2026-03-01T10:30:00Z",
		"session_id": "s1", "session_display_status": "DISPLAY_STATUS_PASSING",
	}
	for key, value := range want {
		if owned[key] != value {
			t.Errorf("owned PR %s = %v, want %v", key, owned[key], value)
		}
	}
	unowned := rows[1]
	if unowned["session_id"] != "" || unowned["session_display_status"] != "DISPLAY_STATUS_UNSPECIFIED" {
		t.Errorf("unowned PR session fields = %v / %v, want empty and DISPLAY_STATUS_UNSPECIFIED", unowned["session_id"], unowned["session_display_status"])
	}
}
