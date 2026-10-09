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

// fakeTriggerOrchestrator records each trigger RPC in its OWN field so a
// RemoteClient method wired to a sibling RPC fails its own assertion.
type fakeTriggerOrchestrator struct {
	bossanovav1connect.UnimplementedOrchestratorServiceHandler

	gotAuth    string
	catalogHit bool
	listReq    *pb.ListTriggersRequest
	getReq     *pb.GetTriggerRequest
	createReq  *pb.CreateTriggerRequest
	updateReq  *pb.UpdateTriggerRequest
	deleteReq  *pb.DeleteTriggerRequest
	rotateReq  *pb.RotateTriggerSecretRequest
	testReq    *pb.TestTriggerRequest
	historyReq *pb.ListTriggerInvocationsRequest

	deleteErr error
}

func (f *fakeTriggerOrchestrator) GetTriggerCatalog(_ context.Context, req *connect.Request[pb.GetTriggerCatalogRequest]) (*connect.Response[pb.GetTriggerCatalogResponse], error) {
	f.gotAuth = req.Header().Get("Authorization")
	f.catalogHit = true
	return connect.NewResponse(&pb.GetTriggerCatalogResponse{Catalog: &pb.TriggerCatalog{Types: []*pb.TriggerTypeSpec{{Name: "http"}}}}), nil
}

func (f *fakeTriggerOrchestrator) ListTriggers(_ context.Context, req *connect.Request[pb.ListTriggersRequest]) (*connect.Response[pb.ListTriggersResponse], error) {
	f.listReq = req.Msg
	return connect.NewResponse(&pb.ListTriggersResponse{Triggers: []*pb.Trigger{{Id: "tr-1"}}}), nil
}

func (f *fakeTriggerOrchestrator) GetTrigger(_ context.Context, req *connect.Request[pb.GetTriggerRequest]) (*connect.Response[pb.GetTriggerResponse], error) {
	f.getReq = req.Msg
	return connect.NewResponse(&pb.GetTriggerResponse{Trigger: &pb.Trigger{Id: req.Msg.GetId()}}), nil
}

func (f *fakeTriggerOrchestrator) CreateTrigger(_ context.Context, req *connect.Request[pb.CreateTriggerRequest]) (*connect.Response[pb.CreateTriggerResponse], error) {
	f.createReq = req.Msg
	return connect.NewResponse(&pb.CreateTriggerResponse{Trigger: &pb.Trigger{Id: "tr-new"}, Secret: "s3cret"}), nil
}

func (f *fakeTriggerOrchestrator) UpdateTrigger(_ context.Context, req *connect.Request[pb.UpdateTriggerRequest]) (*connect.Response[pb.UpdateTriggerResponse], error) {
	f.updateReq = req.Msg
	return connect.NewResponse(&pb.UpdateTriggerResponse{Trigger: &pb.Trigger{Id: req.Msg.GetId()}}), nil
}

func (f *fakeTriggerOrchestrator) DeleteTrigger(_ context.Context, req *connect.Request[pb.DeleteTriggerRequest]) (*connect.Response[pb.DeleteTriggerResponse], error) {
	f.deleteReq = req.Msg
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	return connect.NewResponse(&pb.DeleteTriggerResponse{}), nil
}

func (f *fakeTriggerOrchestrator) RotateTriggerSecret(_ context.Context, req *connect.Request[pb.RotateTriggerSecretRequest]) (*connect.Response[pb.RotateTriggerSecretResponse], error) {
	f.rotateReq = req.Msg
	return connect.NewResponse(&pb.RotateTriggerSecretResponse{Trigger: &pb.Trigger{Id: req.Msg.GetId()}, Secret: "rotated"}), nil
}

func (f *fakeTriggerOrchestrator) TestTrigger(_ context.Context, req *connect.Request[pb.TestTriggerRequest]) (*connect.Response[pb.TestTriggerResponse], error) {
	f.testReq = req.Msg
	return connect.NewResponse(&pb.TestTriggerResponse{Invocation: &pb.TriggerInvocation{Id: "inv-1", TriggerId: req.Msg.GetTriggerId()}}), nil
}

func (f *fakeTriggerOrchestrator) ListTriggerInvocations(_ context.Context, req *connect.Request[pb.ListTriggerInvocationsRequest]) (*connect.Response[pb.ListTriggerInvocationsResponse], error) {
	f.historyReq = req.Msg
	return connect.NewResponse(&pb.ListTriggerInvocationsResponse{Invocations: []*pb.TriggerInvocation{{Id: "inv-1"}, {Id: "inv-2"}}}), nil
}

func newTestTriggerRemote(t *testing.T, fake *fakeTriggerOrchestrator) *RemoteClient {
	t.Helper()
	path, handler := bossanovav1connect.NewOrchestratorServiceHandler(fake)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return NewRemote(srv.URL, "tok")
}

// TestRemoteClient_Triggers proves each trigger method calls its own RPC with
// the fields it was given and returns that RPC's answer, bearer token attached.
func TestRemoteClient_Triggers(t *testing.T) {
	t.Parallel()
	fake := &fakeTriggerOrchestrator{}
	c := newTestTriggerRemote(t, fake)
	ctx := context.Background()

	catalog, err := c.GetTriggerCatalog(ctx)
	if err != nil {
		t.Fatalf("GetTriggerCatalog: %v", err)
	}
	if !fake.catalogHit || fake.gotAuth != "Bearer tok" || len(catalog.GetTypes()) != 1 {
		t.Errorf("catalog: hit=%v auth=%q catalog=%v", fake.catalogHit, fake.gotAuth, catalog)
	}

	org := "org-1"
	triggers, err := c.ListTriggers(ctx, &pb.ListTriggersRequest{OrganizationId: &org})
	if err != nil {
		t.Fatalf("ListTriggers: %v", err)
	}
	if fake.listReq.GetOrganizationId() != "org-1" || len(triggers) != 1 {
		t.Errorf("list request = %v, triggers = %v", fake.listReq, triggers)
	}

	if _, err := c.GetTrigger(ctx, "tr-7"); err != nil {
		t.Fatalf("GetTrigger: %v", err)
	}
	if fake.getReq.GetId() != "tr-7" {
		t.Errorf("get request = %v", fake.getReq)
	}

	created, err := c.CreateTrigger(ctx, &pb.CreateTriggerRequest{OrganizationId: "org-1", Name: "n", TriggerType: "http"})
	if err != nil {
		t.Fatalf("CreateTrigger: %v", err)
	}
	if fake.createReq.GetName() != "n" || created.GetSecret() != "s3cret" || created.GetTrigger().GetId() != "tr-new" {
		t.Errorf("create request = %v, response = %v", fake.createReq, created)
	}

	enabled := false
	if _, err := c.UpdateTrigger(ctx, &pb.UpdateTriggerRequest{Id: "tr-7", IsEnabled: &enabled}); err != nil {
		t.Fatalf("UpdateTrigger: %v", err)
	}
	if fake.updateReq.GetId() != "tr-7" || fake.updateReq.IsEnabled == nil || fake.updateReq.GetIsEnabled() {
		t.Errorf("update request = %v, want is_enabled explicitly false", fake.updateReq)
	}

	if err := c.DeleteTrigger(ctx, "tr-7"); err != nil {
		t.Fatalf("DeleteTrigger: %v", err)
	}
	if fake.deleteReq.GetId() != "tr-7" {
		t.Errorf("delete request = %v", fake.deleteReq)
	}

	rotated, err := c.RotateTriggerSecret(ctx, "tr-7")
	if err != nil {
		t.Fatalf("RotateTriggerSecret: %v", err)
	}
	if fake.rotateReq.GetId() != "tr-7" || rotated.GetSecret() != "rotated" {
		t.Errorf("rotate request = %v, response = %v", fake.rotateReq, rotated)
	}

	inv, err := c.TestTrigger(ctx, &pb.TestTriggerRequest{TriggerId: "tr-7", SamplePayloadJson: "{}", ShouldLaunch: true})
	if err != nil {
		t.Fatalf("TestTrigger: %v", err)
	}
	if fake.testReq.GetTriggerId() != "tr-7" || !fake.testReq.GetShouldLaunch() || inv.GetId() != "inv-1" {
		t.Errorf("test request = %v, invocation = %v", fake.testReq, inv)
	}

	history, err := c.ListTriggerInvocations(ctx, &pb.ListTriggerInvocationsRequest{TriggerId: "tr-7", Limit: 20})
	if err != nil {
		t.Fatalf("ListTriggerInvocations: %v", err)
	}
	if fake.historyReq.GetTriggerId() != "tr-7" || fake.historyReq.GetLimit() != 20 || len(history) != 2 {
		t.Errorf("history request = %v, invocations = %v", fake.historyReq, history)
	}
}

// TestRemoteClient_TriggerErrorPassesThrough proves an API refusal reaches the
// caller with its connect code intact.
func TestRemoteClient_TriggerErrorPassesThrough(t *testing.T) {
	t.Parallel()
	fake := &fakeTriggerOrchestrator{deleteErr: connect.NewError(connect.CodeNotFound, errors.New("trigger not found"))}
	c := newTestTriggerRemote(t, fake)

	err := c.DeleteTrigger(context.Background(), "tr-missing")
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("DeleteTrigger err = %v, want a NotFound connect error", err)
	}
}
