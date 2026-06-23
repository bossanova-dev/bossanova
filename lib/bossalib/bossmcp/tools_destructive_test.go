package bossmcp

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestRemoveRepoRequiresConfirm(t *testing.T) {
	called := false
	backend := &fakeBackend{removeRepo: func(_ context.Context, _ string) error { called = true; return nil }}
	cs := newConnectedClient(t, backend, Options{})

	// Without confirm: must NOT call the backend; must return an error result.
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "remove_repo", Arguments: map[string]any{"id": "r1"},
	})
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected error result when confirm omitted")
	}
	if called {
		t.Fatal("backend.RemoveRepo must not run without confirm:true")
	}

	// With confirm: must call the backend.
	res, err = cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "remove_repo", Arguments: map[string]any{"id": "r1", "confirm": true},
	})
	if err != nil || res.IsError {
		t.Fatalf("confirmed call failed: err=%v res=%s", err, textOf(t, res))
	}
	if !called {
		t.Fatal("backend.RemoveRepo should run with confirm:true")
	}
}

// TestDestructiveToolsAllGated asserts every destructive tool refuses to touch
// the backend unless confirm:true is set, and runs when it is.
func TestDestructiveToolsAllGated(t *testing.T) {
	cases := []struct {
		tool    string
		args    map[string]any
		backend func(called *bool) *fakeBackend
	}{
		{
			tool: "remove_repo",
			args: map[string]any{"id": "r1"},
			backend: func(c *bool) *fakeBackend {
				return &fakeBackend{removeRepo: func(_ context.Context, _ string) error { *c = true; return nil }}
			},
		},
		{
			tool: "close_session",
			args: map[string]any{"id": "s1"},
			backend: func(c *bool) *fakeBackend {
				return &fakeBackend{closeSession: func(_ context.Context, _ string) (*pb.Session, error) { *c = true; return &pb.Session{Id: "x"}, nil }}
			},
		},
		{
			tool: "merge_session",
			args: map[string]any{"id": "s1"},
			backend: func(c *bool) *fakeBackend {
				return &fakeBackend{mergeSession: func(_ context.Context, _ string) (*pb.Session, error) { *c = true; return &pb.Session{Id: "x"}, nil }}
			},
		},
		{
			tool: "remove_session",
			args: map[string]any{"id": "s1"},
			backend: func(c *bool) *fakeBackend {
				return &fakeBackend{removeSession: func(_ context.Context, _ string) error { *c = true; return nil }}
			},
		},
		{
			tool: "archive_session",
			args: map[string]any{"id": "s1"},
			backend: func(c *bool) *fakeBackend {
				return &fakeBackend{archiveSession: func(_ context.Context, _ string) (*pb.Session, error) { *c = true; return &pb.Session{Id: "x"}, nil }}
			},
		},
		{
			tool: "resurrect_session",
			args: map[string]any{"id": "s1"},
			backend: func(c *bool) *fakeBackend {
				return &fakeBackend{resurrectSession: func(_ context.Context, _ string) (*pb.Session, error) { *c = true; return &pb.Session{Id: "x"}, nil }}
			},
		},
		{
			tool: "delete_chat",
			args: map[string]any{"agent_session_id": "a1"},
			backend: func(c *bool) *fakeBackend {
				return &fakeBackend{deleteChat: func(_ context.Context, _ string) error { *c = true; return nil }}
			},
		},
		{
			tool: "empty_trash",
			args: map[string]any{},
			backend: func(c *bool) *fakeBackend {
				return &fakeBackend{emptyTrash: func(_ context.Context, _ *pb.EmptyTrashRequest) (int32, error) { *c = true; return 3, nil }}
			},
		},
		{
			tool: "delete_cron_job",
			args: map[string]any{"id": "c1"},
			backend: func(c *bool) *fakeBackend {
				return &fakeBackend{deleteCronJob: func(_ context.Context, _ string) error { *c = true; return nil }}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			// Unconfirmed: must refuse and not call the backend.
			called := false
			cs := newConnectedClient(t, tc.backend(&called), Options{})
			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tc.tool, Arguments: tc.args})
			if err != nil {
				t.Fatalf("transport error: %v", err)
			}
			if !res.IsError {
				t.Fatalf("%s should refuse without confirm", tc.tool)
			}
			if called {
				t.Fatalf("%s ran the backend without confirm:true", tc.tool)
			}

			// Confirmed: must run.
			called2 := false
			cs2 := newConnectedClient(t, tc.backend(&called2), Options{})
			confirmedArgs := map[string]any{"confirm": true}
			for k, v := range tc.args {
				confirmedArgs[k] = v
			}
			res2, err := cs2.CallTool(context.Background(), &mcp.CallToolParams{Name: tc.tool, Arguments: confirmedArgs})
			if err != nil {
				t.Fatalf("confirmed transport error: %v", err)
			}
			if res2.IsError {
				t.Fatalf("%s confirmed call returned error: %s", tc.tool, textOf(t, res2))
			}
			if !called2 {
				t.Fatalf("%s did not run the backend with confirm:true", tc.tool)
			}
		})
	}
}
