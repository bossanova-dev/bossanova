package server

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestCreateSessionBaseBranchRejectsBeforeSideEffects_Sessions_AC261403(t *testing.T) {
	for _, tc := range []struct {
		name, base string
		probe      func(context.Context, string, string) (bool, error)
		want       connect.Code
		wantProbes int
	}{
		{"missing", "integrtion", func(context.Context, string, string) (bool, error) { return false, nil }, connect.CodeInvalidArgument, 1},
		{"unreachable", "integration", func(context.Context, string, string) (bool, error) { return false, errors.New("origin unavailable") }, connect.CodeUnavailable, 1},
		{"invalid", "../bad", func(context.Context, string, string) (bool, error) { return true, nil }, connect.CodeInvalidArgument, 0},
		{"no probe", "integration", nil, connect.CodeInternal, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wt := &setupStreamWorktree{}
			h := newCreateSessionStreamHarness(t, wt, &setupStreamAgent{})
			probes := 0
			if tc.probe != nil {
				h.server.remoteBranchExists = func(ctx context.Context, path, branch string) (bool, error) {
					probes++
					if path != h.repo.LocalPath || branch != tc.base {
						t.Errorf("probe = %q, %q", path, branch)
					}
					return tc.probe(ctx, path, branch)
				}
			}
			err := h.server.StreamCreateSession(context.Background(), &pb.CreateSessionRequest{
				RepoId: h.repo.ID, Title: "named base", Plan: "work", BaseBranch: tc.base,
			}, func(*pb.CreateSessionResponse) error { return nil })
			if connect.CodeOf(err) != tc.want {
				t.Fatalf("code = %v (%v), want %v", connect.CodeOf(err), err, tc.want)
			}
			if probes != tc.wantProbes {
				t.Errorf("probes = %d, want %d", probes, tc.wantProbes)
			}
			rows, listErr := h.sessions.List(context.Background(), h.repo.ID)
			if listErr != nil {
				t.Fatal(listErr)
			}
			if len(rows) != 0 {
				t.Fatalf("rejected create left %d rows", len(rows))
			}
			creates, existing := wt.callRecords()
			if len(creates)+len(existing) != 0 {
				t.Fatal("rejected create touched worktrees")
			}
		})
	}
}

func TestCreateSessionBaseBranchDefaultAndPRSkipProbe(t *testing.T) {
	for _, existingPR := range []bool{false, true} {
		name := "default"
		if existingPR {
			name = "PR"
		}
		t.Run(name, func(t *testing.T) {
			wt := &setupStreamWorktree{}
			h := newCreateSessionStreamHarness(t, wt, &setupStreamAgent{})
			h.server.remoteBranchExists = func(context.Context, string, string) (bool, error) {
				t.Error("unexpected origin probe")
				return false, errors.New("must not probe")
			}
			req := &pb.CreateSessionRequest{RepoId: h.repo.ID, Title: "default base", Plan: "work"}
			if existingPR {
				n := int32(123)
				req.PrNumber = &n
				req.BaseBranch = "ignored"
			}
			err := h.server.StreamCreateSession(context.Background(), req, func(*pb.CreateSessionResponse) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			h.awaitDraftPRs(t)
			rows, listErr := h.sessions.List(context.Background(), h.repo.ID)
			if listErr != nil {
				t.Fatal(listErr)
			}
			if len(rows) != 1 || rows[0].BaseBranch != "main" {
				t.Fatalf("rows = %+v, want one with default main", rows)
			}
		})
	}
}
