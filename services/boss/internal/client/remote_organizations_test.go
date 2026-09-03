package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

// fakeOrgOrchestrator records the organization / repo-organization calls it
// receives. Unimplemented RPCs inherit CodeUnimplemented from the embedded base.
type fakeOrgOrchestrator struct {
	bossanovav1connect.UnimplementedOrchestratorServiceHandler

	setReq   *pb.SetRepoOrganizationRequest
	getReq   *pb.GetRepoOrganizationRequest
	clearReq *pb.ClearRepoOrganizationRequest

	orgs []*pb.Organization
	// getMapping, when nil, models an unmapped origin: the server answers an
	// unset mapping rather than an error.
	getMapping *pb.RepoOrganizationMapping
	// setErr, when set, is returned by SetRepoOrganization — used to model the
	// server's fail-closed non-member refusal.
	setErr error
}

func (f *fakeOrgOrchestrator) ListOrganizations(_ context.Context, _ *connect.Request[pb.ListOrganizationsRequest]) (*connect.Response[pb.ListOrganizationsResponse], error) {
	return connect.NewResponse(&pb.ListOrganizationsResponse{Organizations: f.orgs}), nil
}

func (f *fakeOrgOrchestrator) GetRepoOrganization(_ context.Context, req *connect.Request[pb.GetRepoOrganizationRequest]) (*connect.Response[pb.GetRepoOrganizationResponse], error) {
	f.getReq = req.Msg
	return connect.NewResponse(&pb.GetRepoOrganizationResponse{Mapping: f.getMapping}), nil
}

func (f *fakeOrgOrchestrator) SetRepoOrganization(_ context.Context, req *connect.Request[pb.SetRepoOrganizationRequest]) (*connect.Response[pb.SetRepoOrganizationResponse], error) {
	f.setReq = req.Msg
	if f.setErr != nil {
		return nil, f.setErr
	}
	return connect.NewResponse(&pb.SetRepoOrganizationResponse{Mapping: &pb.RepoOrganizationMapping{
		Id:             "map-1",
		RepoOriginUrl:  req.Msg.GetRepoOriginUrl(),
		OrganizationId: req.Msg.GetOrganizationId(),
	}}), nil
}

func (f *fakeOrgOrchestrator) ClearRepoOrganization(_ context.Context, req *connect.Request[pb.ClearRepoOrganizationRequest]) (*connect.Response[pb.ClearRepoOrganizationResponse], error) {
	f.clearReq = req.Msg
	return connect.NewResponse(&pb.ClearRepoOrganizationResponse{}), nil
}

func newTestOrgRemote(t *testing.T, fake *fakeOrgOrchestrator) *RemoteClient {
	t.Helper()
	path, handler := bossanovav1connect.NewOrchestratorServiceHandler(fake)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return NewRemote(srv.URL, "tok")
}

// TestRemoteClient_ListOrganizations verifies the caller's organizations come
// back verbatim — the server already scopes the list to the caller, so the
// client must not filter, reorder, or synthesize entries.
func TestRemoteClient_ListOrganizations(t *testing.T) {
	t.Parallel()
	fake := &fakeOrgOrchestrator{orgs: []*pb.Organization{
		{Id: "org-1", Name: "Acme"},
		{Id: "org-2", Name: "Recurse", IsPersonal: true},
	}}
	c := newTestOrgRemote(t, fake)

	got, err := c.ListOrganizations(context.Background())
	if err != nil {
		t.Fatalf("ListOrganizations: %v", err)
	}
	if len(got) != 2 || got[0].GetId() != "org-1" || got[1].GetId() != "org-2" {
		t.Fatalf("ListOrganizations = %v, want the two seeded organizations in order", got)
	}
}

// TestRemoteClient_RepoOrganizationSendsCanonicalOrigin is the load-bearing one:
// bosso keys mappings by a canonical https://host/owner/repo origin and refuses
// anything it cannot normalize to that fixed point. A repo's stored origin is
// frequently ssh shorthand with a ".git" suffix, so every one of the three
// origin-keyed RPCs must canonicalize before it reaches the wire — otherwise a
// set and a later get key different rows.
func TestRemoteClient_RepoOrganizationSendsCanonicalOrigin(t *testing.T) {
	t.Parallel()
	const raw = "git@github.com:recurser/bossanova.git"
	const canonical = "https://github.com/recurser/bossanova"

	fake := &fakeOrgOrchestrator{}
	c := newTestOrgRemote(t, fake)
	ctx := context.Background()

	if _, err := c.SetRepoOrganization(ctx, raw, "org-1"); err != nil {
		t.Fatalf("SetRepoOrganization: %v", err)
	}
	if got := fake.setReq.GetRepoOriginUrl(); got != canonical {
		t.Errorf("Set repo_origin_url = %q, want canonical %q", got, canonical)
	}
	if got := fake.setReq.GetOrganizationId(); got != "org-1" {
		t.Errorf("Set organization_id = %q, want %q", got, "org-1")
	}

	if _, err := c.GetRepoOrganization(ctx, raw); err != nil {
		t.Fatalf("GetRepoOrganization: %v", err)
	}
	if got := fake.getReq.GetRepoOriginUrl(); got != canonical {
		t.Errorf("Get repo_origin_url = %q, want canonical %q", got, canonical)
	}

	if err := c.ClearRepoOrganization(ctx, raw, "org-1"); err != nil {
		t.Fatalf("ClearRepoOrganization: %v", err)
	}
	if got := fake.clearReq.GetRepoOriginUrl(); got != canonical {
		t.Errorf("Clear repo_origin_url = %q, want canonical %q", got, canonical)
	}
	// The delete is organization-scoped as an authorization backstop, so the
	// organization has to reach the wire alongside the key.
	if got := fake.clearReq.GetOrganizationId(); got != "org-1" {
		t.Errorf("Clear organization_id = %q, want %q", got, "org-1")
	}
}

// TestRemoteClient_RepoOrganizationSendsTheNormalizationFixedPoint pins the
// canonicalizer at the fixed point rather than one pass short of it.
// vcs.NormalizeRepoURL is not idempotent on this input: the first pass strips
// the trailing slash and leaves ".git", which a second pass then removes. bosso
// stores and compares the fixed point (validateRepoOriginURL iterates to it), so
// stopping after one pass would send a spelling the server accepts but rewrites
// — the same row by luck rather than by construction.
func TestRemoteClient_RepoOrganizationSendsTheNormalizationFixedPoint(t *testing.T) {
	t.Parallel()
	const raw = "https://github.com/recurser/bossanova.git/"
	const canonical = "https://github.com/recurser/bossanova"

	fake := &fakeOrgOrchestrator{}
	c := newTestOrgRemote(t, fake)

	if _, err := c.GetRepoOrganization(context.Background(), raw); err != nil {
		t.Fatalf("GetRepoOrganization: %v", err)
	}
	if got := fake.getReq.GetRepoOriginUrl(); got != canonical {
		t.Errorf("repo_origin_url = %q, want the fixed point %q", got, canonical)
	}
}

// TestRemoteClient_RepoOrganizationUnparseableOriginPassesThrough verifies an
// origin the canonicalizer cannot parse is sent as-is, so the user sees bosso's
// own InvalidArgument rather than an empty-origin error of the client's
// invention.
func TestRemoteClient_RepoOrganizationUnparseableOriginPassesThrough(t *testing.T) {
	t.Parallel()
	fake := &fakeOrgOrchestrator{}
	c := newTestOrgRemote(t, fake)

	if _, err := c.GetRepoOrganization(context.Background(), "not a repo origin"); err != nil {
		t.Fatalf("GetRepoOrganization: %v", err)
	}
	if got := fake.getReq.GetRepoOriginUrl(); got != "not a repo origin" {
		t.Errorf("repo_origin_url = %q, want the raw origin passed through", got)
	}
}

// TestRemoteClient_GetRepoOrganizationUnmapped verifies an unmapped origin is a
// nil mapping and no error — the miss signal bosso documents.
func TestRemoteClient_GetRepoOrganizationUnmapped(t *testing.T) {
	t.Parallel()
	c := newTestOrgRemote(t, &fakeOrgOrchestrator{getMapping: nil})

	mapping, err := c.GetRepoOrganization(context.Background(), "https://github.com/recurser/bossanova")
	if err != nil {
		t.Fatalf("GetRepoOrganization: %v", err)
	}
	if mapping != nil {
		t.Fatalf("mapping = %v, want nil for an unmapped origin", mapping)
	}
}

// TestRemoteClient_SetRepoOrganizationSurfacesRefusal verifies the server's
// fail-closed non-member refusal reaches the caller as a PermissionDenied
// connect error, which is what lets the TUI classify it into a distinct
// "not a member" line instead of a generic RPC failure.
func TestRemoteClient_SetRepoOrganizationSurfacesRefusal(t *testing.T) {
	t.Parallel()
	fake := &fakeOrgOrchestrator{
		setErr: connect.NewError(connect.CodePermissionDenied, errors.New("organization membership required")),
	}
	c := newTestOrgRemote(t, fake)

	_, err := c.SetRepoOrganization(context.Background(), "https://github.com/recurser/bossanova", "org-nope")
	if err == nil {
		t.Fatal("SetRepoOrganization succeeded, want a refusal")
	}
	if got := connect.CodeOf(err); got != connect.CodePermissionDenied {
		t.Errorf("connect.CodeOf(err) = %v, want %v", got, connect.CodePermissionDenied)
	}
}

// TestLocalClient_OrganizationsAreCloudOnly verifies the local daemon refuses
// the organization surface in a defined, non-panicking way. A local daemon
// holds no organizations and no mapping, so an Unimplemented refusal is the
// honest answer — and the message must not tell the caller to use a local
// daemon, which is what errLocalOnly would have said.
func TestLocalClient_OrganizationsAreCloudOnly(t *testing.T) {
	t.Parallel()
	c := &LocalClient{}
	ctx := context.Background()

	if _, err := c.ListOrganizations(ctx); !isCloudOnly(t, err) {
		t.Errorf("ListOrganizations err = %v, want a cloud-only Unimplemented refusal", err)
	}
	if _, err := c.GetRepoOrganization(ctx, "https://github.com/a/b"); !isCloudOnly(t, err) {
		t.Errorf("GetRepoOrganization err = %v, want a cloud-only Unimplemented refusal", err)
	}
	if _, err := c.SetRepoOrganization(ctx, "https://github.com/a/b", "org-1"); !isCloudOnly(t, err) {
		t.Errorf("SetRepoOrganization err = %v, want a cloud-only Unimplemented refusal", err)
	}
	if err := c.ClearRepoOrganization(ctx, "https://github.com/a/b", "org-1"); !isCloudOnly(t, err) {
		t.Errorf("ClearRepoOrganization err = %v, want a cloud-only Unimplemented refusal", err)
	}
}

func isCloudOnly(t *testing.T, err error) bool {
	t.Helper()
	if err == nil || connect.CodeOf(err) != connect.CodeUnimplemented {
		return false
	}
	// Guard the direction of the message: errLocalOnly's wording would be
	// exactly backwards coming from a LocalClient.
	if want := "signed in to Bossanova Cloud"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not mention %q", err.Error(), want)
		return false
	}
	return true
}
