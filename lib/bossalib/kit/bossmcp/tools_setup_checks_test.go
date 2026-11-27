package bossmcp

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type setupCheckFakeBackend struct {
	*fakeBackend
	listSetupChecks func(ctx context.Context, repoID, workspacePath string) (*pb.ListSetupChecksResponse, error)
}

func (s *setupCheckFakeBackend) ListSetupChecks(ctx context.Context, repoID, workspacePath string) (*pb.ListSetupChecksResponse, error) {
	return s.listSetupChecks(ctx, repoID, workspacePath)
}

func sampleSetupChecksResponse() *pb.ListSetupChecksResponse {
	return &pb.ListSetupChecksResponse{
		Checks: []*pb.SetupCheck{
			{
				Id:             "gh-auth",
				Area:           pb.SetupCheckArea_SETUP_CHECK_AREA_MACHINE,
				Status:         pb.SetupCheckStatus_SETUP_CHECK_STATUS_MISSING,
				Title:          "GitHub CLI authenticated",
				Evidence:       "gh is not authenticated",
				Recommendation: "Authenticate gh on the daemon host.",
				Fix: &pb.SetupCheckFix{
					Kind:    pb.SetupCheckFixKind_SETUP_CHECK_FIX_KIND_COMMAND,
					Command: "gh auth login",
				},
				AddedInVersion: "0.9.0",
			},
			{
				Id:             "node-on-daemon-path",
				Area:           pb.SetupCheckArea_SETUP_CHECK_AREA_MACHINE,
				Status:         pb.SetupCheckStatus_SETUP_CHECK_STATUS_OK,
				Title:          "Node.js on daemon PATH",
				Evidence:       "/usr/local/bin/node",
				AddedInVersion: "0.9.0",
			},
		},
		CheckedAt: timestamppb.New(time.Date(2026, 11, 24, 19, 0, 0, 0, time.UTC)),
		RepoId:    "repo-1",
	}
}

func TestListSetupChecksTool_McpServer_AC268301(t *testing.T) {
	t.Parallel()

	t.Run("registered read-only with ReadOnlyHint", func(t *testing.T) {
		cs := newConnectedClient(t, &fakeBackend{}, Options{ReadOnly: true})
		res, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
		if err != nil {
			t.Fatalf("ListTools: %v", err)
		}
		var found *mcp.Tool
		for _, tool := range res.Tools {
			if tool.Name == "list_setup_checks" {
				found = tool
				break
			}
		}
		if found == nil {
			t.Fatal("list_setup_checks not registered under Options{ReadOnly: true}")
		}
		if found.Annotations == nil || !found.Annotations.ReadOnlyHint {
			t.Fatalf("list_setup_checks annotations = %+v, want ReadOnlyHint=true", found.Annotations)
		}
	})

	t.Run("output equals MarshalSetupChecks byte-for-byte, uses proto names, and is deterministic", func(t *testing.T) {
		resp := sampleSetupChecksResponse()
		var gotRepoID, gotWorkspacePath string
		backend := &setupCheckFakeBackend{
			fakeBackend: &fakeBackend{},
			listSetupChecks: func(_ context.Context, repoID, workspacePath string) (*pb.ListSetupChecksResponse, error) {
				gotRepoID = repoID
				gotWorkspacePath = workspacePath
				return resp, nil
			},
		}
		cs := newConnectedClient(t, backend, Options{ReadOnly: true})

		wantBytes, err := MarshalSetupChecks(resp)
		if err != nil {
			t.Fatalf("MarshalSetupChecks: %v", err)
		}
		if !bytes.HasSuffix(wantBytes, []byte("\n")) {
			t.Fatalf("MarshalSetupChecks output must end with a newline, got %q", wantBytes)
		}

		// Repeated calls must produce identical bytes (no protojson detrand jitter).
		for i := range 20 {
			again, err := MarshalSetupChecks(resp)
			if err != nil {
				t.Fatalf("MarshalSetupChecks[%d]: %v", i, err)
			}
			if !bytes.Equal(again, wantBytes) {
				t.Fatalf("MarshalSetupChecks[%d] diverged:\ngot:\n%s\nwant:\n%s", i, again, wantBytes)
			}
		}

		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name: "list_setup_checks",
			Arguments: map[string]any{
				"repo_id":        "repo-42",
				"workspace_path": "/tmp/example",
			},
		})
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
		if res.IsError {
			t.Fatalf("unexpected tool error: %s", textOf(t, res))
		}
		if gotRepoID != "repo-42" || gotWorkspacePath != "/tmp/example" {
			t.Fatalf("args forwarded = (%q, %q), want (%q, %q)", gotRepoID, gotWorkspacePath, "repo-42", "/tmp/example")
		}

		gotText := textOf(t, res)
		if gotText != string(wantBytes) {
			t.Fatalf("MCP tool output differs from MarshalSetupChecks:\ngot:\n%s\nwant:\n%s", gotText, wantBytes)
		}

		for _, token := range []string{
			`"checked_at"`,
			`"repo_id"`,
			`"added_in_version"`,
			`"SETUP_CHECK_AREA_MACHINE"`,
			`"SETUP_CHECK_STATUS_MISSING"`,
			`"SETUP_CHECK_FIX_KIND_COMMAND"`,
		} {
			if !strings.Contains(gotText, token) {
				t.Errorf("serialized output missing proto token %s:\n%s", token, gotText)
			}
		}
	})

	t.Run("MarshalSetupChecks handles nil response", func(t *testing.T) {
		b, err := MarshalSetupChecks(nil)
		if err != nil {
			t.Fatalf("MarshalSetupChecks(nil): %v", err)
		}
		if !strings.Contains(string(b), `"checks": []`) {
			t.Fatalf("MarshalSetupChecks(nil) = %s, want empty checks array", b)
		}
	})

	t.Run("backend error returns IsError", func(t *testing.T) {
		backend := &setupCheckFakeBackend{
			fakeBackend: &fakeBackend{},
			listSetupChecks: func(_ context.Context, _, _ string) (*pb.ListSetupChecksResponse, error) {
				return nil, errors.New("daemon unavailable")
			},
		}
		cs := newConnectedClient(t, backend, Options{ReadOnly: true})
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "list_setup_checks",
			Arguments: map[string]any{},
		})
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
		if !res.IsError || !strings.Contains(textOf(t, res), "daemon unavailable") {
			t.Fatalf("result = (isError=%v, text=%q), want error naming daemon unavailable", res.IsError, textOf(t, res))
		}
	})

	t.Run("unsupported backend returns IsError", func(t *testing.T) {
		cs := newConnectedClient(t, &fakeBackend{}, Options{ReadOnly: true})
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "list_setup_checks",
			Arguments: map[string]any{},
		})
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
		if !res.IsError || !strings.Contains(textOf(t, res), errSetupChecksUnsupported.Error()) {
			t.Fatalf("result = (isError=%v, text=%q), want unsupported error", res.IsError, textOf(t, res))
		}
	})
}
