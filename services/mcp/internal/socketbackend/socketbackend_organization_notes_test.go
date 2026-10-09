package socketbackend

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/recurser/bossalib/bossmcp"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// TestOrganizationNotesRefusedWithGuidance proves every organization-note
// method on the local socket backend returns a typed FailedPrecondition that
// names the surfaces which do serve organization notes. None of them touches
// the daemon, so a zero Backend (no socket) is enough: a method that tried to
// dial would panic on the nil client.
func TestOrganizationNotesRefusedWithGuidance(t *testing.T) {
	t.Parallel()
	b := &Backend{}
	ctx := context.Background()

	calls := map[string]func() error{
		"CreateOrganizationNote": func() error {
			_, err := b.CreateOrganizationNote(ctx, &pb.CreateOrganizationNoteRequest{OrganizationId: "org-1", Body: "b"})
			return err
		},
		"GetOrganizationNote": func() error {
			_, err := b.GetOrganizationNote(ctx, &pb.GetOrganizationNoteRequest{OrganizationId: "org-1", Id: "on-1"})
			return err
		},
		"ListOrganizationNotes": func() error {
			_, err := b.ListOrganizationNotes(ctx, &pb.ListOrganizationNotesRequest{OrganizationId: "org-1"})
			return err
		},
		"UpdateOrganizationNote": func() error {
			_, err := b.UpdateOrganizationNote(ctx, &pb.UpdateOrganizationNoteRequest{OrganizationId: "org-1", Id: "on-1"})
			return err
		},
		"DeleteOrganizationNote": func() error {
			return b.DeleteOrganizationNote(ctx, &pb.DeleteOrganizationNoteRequest{OrganizationId: "org-1", Id: "on-1"})
		},
		"GetOrganizationNoteQuota": func() error {
			_, err := b.GetOrganizationNoteQuota(ctx, &pb.GetOrganizationNoteQuotaRequest{OrganizationId: "org-1"})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			err := call()
			if connect.CodeOf(err) != connect.CodeFailedPrecondition {
				t.Fatalf("%s code = %v (%v), want FailedPrecondition", name, connect.CodeOf(err), err)
			}
			for _, want := range []string{"boss notes org", "hosted MCP endpoint"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%s error %q does not name %q", name, err.Error(), want)
				}
			}
		})
	}
}

// TestOrganizationNoteToolSurfacesGuidance proves the refusal reaches an agent
// through the real tool layer as a tool error result carrying the guidance,
// not as a transport failure.
func TestOrganizationNoteToolSurfacesGuidance(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "bossanova-test", Version: "test"}, nil)
	bossmcp.RegisterTools(server, &Backend{}, bossmcp.Options{})
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("connect server: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "bossanova-test-client", Version: "test"}, nil)
	cs, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "list_organization_notes",
		Arguments: map[string]any{"organization_id": "org-1"},
	})
	if err != nil {
		t.Fatalf("call list_organization_notes: %v", err)
	}
	if !res.IsError || len(res.Content) == 0 {
		t.Fatalf("result = %+v, want an error result", res)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok || !strings.Contains(text.Text, "failed_precondition") || !strings.Contains(text.Text, "boss notes org") {
		t.Fatalf("tool error = %+v, want failed_precondition naming boss notes org", res.Content[0])
	}
}
