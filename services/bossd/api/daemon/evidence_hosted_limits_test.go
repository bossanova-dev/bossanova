package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

type limitsOpener struct {
	*fakeHostedOpener
	limits HostedProofLimits
	fresh  bool
	asked  []string
}

func (o *limitsOpener) StartHostedProof(ctx context.Context, in HostedProofStart) (HostedProofUpload, error) {
	upload, err := o.fakeHostedOpener.StartHostedProof(ctx, in)
	upload.Limits = o.limits
	return upload, err
}

func (o *limitsOpener) HostedProofLimits(origin string) (HostedProofLimits, bool) {
	o.asked = append(o.asked, origin)
	return o.limits, o.fresh
}

func callLimits(t *testing.T, s *Server, workspace string) (*pb.GetHostedProofLimitsResponse, error) {
	t.Helper()
	route, handler := bossanovav1connect.NewDaemonServiceHandler(s)
	mux := http.NewServeMux()
	mux.Handle(route, handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	resp, err := bossanovav1connect.NewDaemonServiceClient(srv.Client(), srv.URL).GetHostedProofLimits(context.Background(), connect.NewRequest(&pb.GetHostedProofLimitsRequest{WorkspacePath: workspace}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

var knownLimits = HostedProofLimits{FileCount: 64, FileBytes: 50 << 20, PageBytes: 200 << 20}

func TestGetHostedProofLimitsReadsTheCache_Sessions_AC278401(t *testing.T) {
	s, dir := evidenceServer(t, "http://127.0.0.1:1")
	opener := &limitsOpener{fakeHostedOpener: &fakeHostedOpener{}, limits: knownLimits, fresh: true}
	s.evidenceBucketOpener = opener
	got, err := callLimits(t, s, filepath.Dir(filepath.Dir(dir)))
	if err != nil {
		t.Fatal(err)
	}
	want := &pb.HostedProofPageLimits{MaxFileCount: 64, MaxFileBytes: 50 << 20, MaxPageBytes: 200 << 20}
	if got.Limits.GetMaxFileCount() != want.MaxFileCount || got.Limits.GetMaxFileBytes() != want.MaxFileBytes || got.Limits.GetMaxPageBytes() != want.MaxPageBytes {
		t.Fatalf("limits = %v, want %v", got.Limits, want)
	}
	if len(opener.asked) != 1 || opener.asked[0] != "git@github.com:acme/widgets.git" {
		t.Fatalf("asked for %v, want the repository's origin", opener.asked)
	}
	opener.fresh = false
	if got, err := callLimits(t, s, dir); err != nil || got.Limits != nil {
		t.Fatalf("expired cache = %v, %v; want no limits", got, err)
	}
	opener.fresh, opener.limits = true, HostedProofLimits{}
	if got, err := callLimits(t, s, dir); err != nil || got.Limits != nil {
		t.Fatalf("empty limits = %v, %v; want none", got, err)
	}
}

func TestGetHostedProofLimitsWithoutAHostedPublisherReturnsNone_Sessions_AC278401(t *testing.T) {
	s, dir := evidenceServer(t, "http://127.0.0.1:1")
	if got, err := callLimits(t, s, dir); err != nil || got.Limits != nil {
		t.Fatalf("local-only daemon = %v, %v; want no limits", got, err)
	}
	s.evidenceBucketOpener = &fakeHostedOpener{}
	if got, err := callLimits(t, s, dir); err != nil || got.Limits != nil {
		t.Fatalf("hosted publisher without a cache = %v, %v; want no limits", got, err)
	}
}

func TestGetHostedProofLimitsRefusesUnregisteredPaths(t *testing.T) {
	s, _ := evidenceServer(t, "http://127.0.0.1:1")
	s.evidenceBucketOpener = &limitsOpener{fakeHostedOpener: &fakeHostedOpener{}, limits: knownLimits, fresh: true}
	for name, tc := range map[string]struct {
		path string
		code connect.Code
	}{
		"empty":   {"", connect.CodeInvalidArgument},
		"outside": {t.TempDir(), connect.CodePermissionDenied},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := callLimits(t, s, tc.path)
			var rpcErr *connect.Error
			if !errors.As(err, &rpcErr) || rpcErr.Code() != tc.code {
				t.Fatalf("error = %v, want %v", err, tc.code)
			}
		})
	}
}

func TestUploadEvidenceHostedReturnsTheLimits_Sessions_AC278401(t *testing.T) {
	s, dir := evidenceServer(t, "http://127.0.0.1:1")
	s.evidenceBucketOpener = &limitsOpener{fakeHostedOpener: &fakeHostedOpener{}, limits: knownLimits, fresh: true}
	objects := hostedPage(t, dir, hostedContents, "index.html", "media/a.png")
	got, rpcErr := callEvidence(t, s, &pb.UploadEvidenceRequest{EvidenceDir: dir, PrNumber: 42, HeadSha: evidenceTestSHA, Objects: objects, Destination: pb.EvidenceDestination_EVIDENCE_DESTINATION_HOSTED})
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if l := got.HostedLimits; l.GetMaxFileCount() != 64 || l.GetMaxFileBytes() != 50<<20 || l.GetMaxPageBytes() != 200<<20 {
		t.Fatalf("hosted limits = %v", l)
	}
}
