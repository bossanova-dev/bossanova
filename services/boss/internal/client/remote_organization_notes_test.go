package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

// fakeOrgNotesOrchestrator records each organization-note RPC in its OWN field
// so a RemoteClient method wired to a sibling RPC fails its own assertion.
type fakeOrgNotesOrchestrator struct {
	bossanovav1connect.UnimplementedOrchestratorServiceHandler

	gotAuth   string
	createReq *pb.CreateOrganizationNoteRequest
	getReq    *pb.GetOrganizationNoteRequest
	listReq   *pb.ListOrganizationNotesRequest
	updateReq *pb.UpdateOrganizationNoteRequest
	deleteReq *pb.DeleteOrganizationNoteRequest
	quotaReq  *pb.GetOrganizationNoteQuotaRequest

	deleteErr error
}

func (f *fakeOrgNotesOrchestrator) CreateOrganizationNote(_ context.Context, req *connect.Request[pb.CreateOrganizationNoteRequest]) (*connect.Response[pb.CreateOrganizationNoteResponse], error) {
	f.gotAuth = req.Header().Get("Authorization")
	f.createReq = req.Msg
	return connect.NewResponse(&pb.CreateOrganizationNoteResponse{Note: &pb.OrganizationNote{Id: "on-created", Body: req.Msg.GetBody()}}), nil
}

func (f *fakeOrgNotesOrchestrator) GetOrganizationNote(_ context.Context, req *connect.Request[pb.GetOrganizationNoteRequest]) (*connect.Response[pb.GetOrganizationNoteResponse], error) {
	f.getReq = req.Msg
	return connect.NewResponse(&pb.GetOrganizationNoteResponse{Note: &pb.OrganizationNote{Id: req.Msg.GetId()}}), nil
}

func (f *fakeOrgNotesOrchestrator) ListOrganizationNotes(_ context.Context, req *connect.Request[pb.ListOrganizationNotesRequest]) (*connect.Response[pb.ListOrganizationNotesResponse], error) {
	f.listReq = req.Msg
	return connect.NewResponse(&pb.ListOrganizationNotesResponse{
		Notes:         []*pb.OrganizationNote{{Id: "on-1"}},
		NextPageToken: "tok-next",
	}), nil
}

func (f *fakeOrgNotesOrchestrator) UpdateOrganizationNote(_ context.Context, req *connect.Request[pb.UpdateOrganizationNoteRequest]) (*connect.Response[pb.UpdateOrganizationNoteResponse], error) {
	f.updateReq = req.Msg
	return connect.NewResponse(&pb.UpdateOrganizationNoteResponse{Note: &pb.OrganizationNote{Id: req.Msg.GetId()}}), nil
}

func (f *fakeOrgNotesOrchestrator) DeleteOrganizationNote(_ context.Context, req *connect.Request[pb.DeleteOrganizationNoteRequest]) (*connect.Response[pb.DeleteOrganizationNoteResponse], error) {
	f.deleteReq = req.Msg
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	return connect.NewResponse(&pb.DeleteOrganizationNoteResponse{}), nil
}

func (f *fakeOrgNotesOrchestrator) GetOrganizationNoteQuota(_ context.Context, req *connect.Request[pb.GetOrganizationNoteQuotaRequest]) (*connect.Response[pb.GetOrganizationNoteQuotaResponse], error) {
	f.quotaReq = req.Msg
	return connect.NewResponse(&pb.GetOrganizationNoteQuotaResponse{Quota: &pb.OrganizationNoteQuota{HourlyLimit: 100, UsedCount: 4}}), nil
}

func newTestOrgNotesRemote(t *testing.T, fake *fakeOrgNotesOrchestrator) *RemoteClient {
	t.Helper()
	path, handler := bossanovav1connect.NewOrchestratorServiceHandler(fake)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return NewRemote(srv.URL, "tok")
}

// TestRemoteClient_OrganizationNotes proves each method calls its own RPC with
// the organization and id it was given, the bearer token attached, and the
// list's page token returned.
func TestRemoteClient_OrganizationNotes(t *testing.T) {
	t.Parallel()
	fake := &fakeOrgNotesOrchestrator{}
	c := newTestOrgNotesRemote(t, fake)
	ctx := context.Background()

	note, err := c.CreateOrganizationNote(ctx, &pb.CreateOrganizationNoteRequest{OrganizationId: "org-1", Body: "body"})
	if err != nil {
		t.Fatalf("CreateOrganizationNote: %v", err)
	}
	if fake.gotAuth != "Bearer tok" || fake.createReq.GetOrganizationId() != "org-1" || note.GetId() != "on-created" {
		t.Errorf("create: auth=%q req=%v note=%v", fake.gotAuth, fake.createReq, note)
	}

	if _, err := c.GetOrganizationNote(ctx, "org-1", "on-7"); err != nil {
		t.Fatalf("GetOrganizationNote: %v", err)
	}
	if fake.getReq.GetOrganizationId() != "org-1" || fake.getReq.GetId() != "on-7" {
		t.Errorf("get request = %v", fake.getReq)
	}

	page, err := c.ListOrganizationNotes(ctx, &pb.ListOrganizationNotesRequest{OrganizationId: "org-1", PageToken: "tok-prev", PageSize: 5})
	if err != nil {
		t.Fatalf("ListOrganizationNotes: %v", err)
	}
	if fake.listReq.GetPageToken() != "tok-prev" || fake.listReq.GetPageSize() != 5 || page.GetNextPageToken() != "tok-next" {
		t.Errorf("list request = %v, page = %v", fake.listReq, page)
	}

	if _, err := c.UpdateOrganizationNote(ctx, &pb.UpdateOrganizationNoteRequest{OrganizationId: "org-1", Id: "on-7", Tags: &pb.NoteTagSet{}}); err != nil {
		t.Fatalf("UpdateOrganizationNote: %v", err)
	}
	if fake.updateReq.GetId() != "on-7" || fake.updateReq.Tags == nil {
		t.Errorf("update request = %v, want the set-but-empty tag set preserved", fake.updateReq)
	}

	if err := c.DeleteOrganizationNote(ctx, "org-1", "on-7"); err != nil {
		t.Fatalf("DeleteOrganizationNote: %v", err)
	}
	if fake.deleteReq.GetOrganizationId() != "org-1" || fake.deleteReq.GetId() != "on-7" {
		t.Errorf("delete request = %v", fake.deleteReq)
	}

	quota, err := c.GetOrganizationNoteQuota(ctx, "org-2")
	if err != nil {
		t.Fatalf("GetOrganizationNoteQuota: %v", err)
	}
	if fake.quotaReq.GetOrganizationId() != "org-2" || quota.GetUsedCount() != 4 {
		t.Errorf("quota request = %v, quota = %v", fake.quotaReq, quota)
	}
}

// TestRemoteClient_OrganizationNoteErrorPassesThrough proves an API refusal
// reaches the caller with its code intact.
func TestRemoteClient_OrganizationNoteErrorPassesThrough(t *testing.T) {
	t.Parallel()
	fake := &fakeOrgNotesOrchestrator{deleteErr: connect.NewError(connect.CodePermissionDenied, errors.New("only the author or an owner may delete"))}
	c := newTestOrgNotesRemote(t, fake)

	err := c.DeleteOrganizationNote(context.Background(), "org-1", "on-7")
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("code = %v (%v), want PermissionDenied", connect.CodeOf(err), err)
	}
}
