package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossd/internal/adapter/r2upload"
	"github.com/recurser/bossd/internal/domain"
)

// fakeHostedOpener is an evidence opener that also reaches a fake hosted proof
// host. It records whether the local bucket was opened, every Start and every Put.
type fakeHostedOpener struct {
	local      evidenceBucketOpener
	opened     int
	starts     []HostedProofStart
	puts       []HostedProofPut
	aborts     []hostedAbort
	startErr   error
	putErr     error
	abortErr   error
	failPutAt  int // 1-based Put that fails with putErr; 0 fails every Put
	afterStart func()
}

type hostedAbort struct {
	uploadID, repoOriginURL string
	ctxErr                  error
}

func (f *fakeHostedOpener) OpenEvidenceBucket() (domain.EvidenceBucket, error) {
	f.opened++
	if f.local == nil {
		return nil, domain.ErrEvidenceDestinationNotConfigured
	}
	return f.local.OpenEvidenceBucket()
}

func (f *fakeHostedOpener) StartHostedProof(_ context.Context, in HostedProofStart) (HostedProofUpload, error) {
	f.starts = append(f.starts, in)
	if f.afterStart != nil {
		f.afterStart()
	}
	if f.startErr != nil {
		return HostedProofUpload{}, f.startErr
	}
	return HostedProofUpload{UploadID: "upload-1", ReviewOrigin: "https://app.example"}, nil
}

func (f *fakeHostedOpener) PutHostedProofObject(_ context.Context, in HostedProofPut) error {
	f.puts = append(f.puts, in)
	if f.failPutAt != 0 && len(f.puts) != f.failPutAt {
		return nil
	}
	return f.putErr
}

func (f *fakeHostedOpener) AbortHostedProof(ctx context.Context, uploadID, repoOriginURL string) error {
	f.aborts = append(f.aborts, hostedAbort{uploadID, repoOriginURL, ctx.Err()})
	return f.abortErr
}

// hostedPage writes a proof page's objects into dir and returns them in the
// given request order.
func hostedPage(t *testing.T, dir string, contents map[string]string, order ...string) []*pb.EvidenceObject {
	t.Helper()
	objects := make([]*pb.EvidenceObject, 0, len(order))
	for _, name := range order {
		file := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(contents[name]), 0600); err != nil {
			t.Fatal(err)
		}
		objects = append(objects, &pb.EvidenceObject{Name: name, Path: name})
	}
	return objects
}

var hostedContents = map[string]string{"index.html": "<!doctype html>", "player.js": "play()", "media/a.png": "png", "manifest.json": "{}"}

// callEvidence sends the request over Connect, so response headers are the wire ones.
func callEvidence(t *testing.T, s *Server, req *pb.UploadEvidenceRequest) (*pb.UploadEvidenceResponse, *connect.Error) {
	t.Helper()
	route, handler := bossanovav1connect.NewDaemonServiceHandler(s)
	mux := http.NewServeMux()
	mux.Handle(route, handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	resp, err := bossanovav1connect.NewDaemonServiceClient(srv.Client(), srv.URL).UploadEvidence(context.Background(), connect.NewRequest(req))
	if err != nil {
		var rpcErr *connect.Error
		if !errors.As(err, &rpcErr) {
			t.Fatalf("non-connect error: %v", err)
		}
		return nil, rpcErr
	}
	return resp.Msg, nil
}

func TestUploadEvidenceHostedPublishesPlaintextWhileSealingIsOff(t *testing.T) {
	rec := newRecordingEndpoint(t)
	s, dir := evidenceServer(t, rec.server.URL)
	local := s.evidenceBucketOpener
	fake := &fakeHostedOpener{local: local}
	s.evidenceBucketOpener = fake
	objects := hostedPage(t, dir, hostedContents, "index.html", "media/a.png", "player.js", "manifest.json")
	got, rpcErr := callEvidence(t, s, &pb.UploadEvidenceRequest{EvidenceDir: dir, PrNumber: 42, HeadSha: evidenceTestSHA, Objects: objects, Destination: pb.EvidenceDestination_EVIDENCE_DESTINATION_HOSTED})
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	if fake.opened != 0 || len(rec.puts) != 0 {
		t.Fatalf("hosted publish touched the local bucket: opened %d, PUTs %v", fake.opened, rec.puts)
	}
	if len(fake.starts) != 1 {
		t.Fatalf("starts = %d", len(fake.starts))
	}
	start := fake.starts[0]
	wantManifest := []HostedProofEntry{{"index.html", 15}, {"media/a.png", 3}, {"player.js", 6}, {"manifest.json", 2}}
	if start.RepoOriginURL != "git@github.com:acme/widgets.git" || start.PR != 42 || start.HeadSHA != evidenceTestSHA || !slices.Equal(start.Manifest, wantManifest) {
		t.Fatalf("start = %+v", start)
	}
	var order []string
	for _, put := range fake.puts {
		if put.UploadID != "upload-1" || put.RepoOriginURL != start.RepoOriginURL || string(put.Body) != hostedContents[put.Name] {
			t.Fatalf("put = %+v", put)
		}
		order = append(order, put.Name)
	}
	if want := []string{"media/a.png", "player.js", "manifest.json", "index.html"}; !slices.Equal(order, want) {
		t.Fatalf("put order = %v, want %v", order, want)
	}
	if got.Destination != pb.EvidenceDestination_EVIDENCE_DESTINATION_HOSTED || got.PageUrl != "https://app.example/review/acme/widgets/42?head="+evidenceTestSHA || got.KeyPrefix != "" || got.PublicBaseUrl != "" {
		t.Fatalf("response = %v", got)
	}
	if want := []string{"index.html", "media/a.png", "player.js", "manifest.json"}; !slices.Equal(got.ObjectKeys, want) {
		t.Fatalf("object keys = %v", got.ObjectKeys)
	}
	t.Logf("hosted page %s, put order %v", got.PageUrl, order)
}

func TestUploadEvidenceLocalOrHosted_Sessions_AC260203(t *testing.T) {
	t.Run("local complete", func(t *testing.T) {
		rec := newRecordingEndpoint(t)
		s, dir := evidenceServer(t, rec.server.URL)
		fake := &fakeHostedOpener{local: s.evidenceBucketOpener}
		s.evidenceBucketOpener = fake
		got, rpcErr := callEvidence(t, s, &pb.UploadEvidenceRequest{EvidenceDir: dir, PrNumber: 42, HeadSha: evidenceTestSHA, Objects: []*pb.EvidenceObject{{Name: "manifest.json", Path: "manifest.json"}}, Destination: pb.EvidenceDestination_EVIDENCE_DESTINATION_LOCAL_OR_HOSTED})
		if rpcErr != nil {
			t.Fatal(rpcErr)
		}
		want := "acme/widgets/42/" + evidenceTestSHA + "/"
		if len(fake.starts) != 0 || got.Destination != pb.EvidenceDestination_EVIDENCE_DESTINATION_LOCAL || got.KeyPrefix != want || got.PublicBaseUrl != "https://proof.example" || got.PageUrl != "" {
			t.Fatalf("response = %v, hosted starts %d", got, len(fake.starts))
		}
		if _, ok := rec.puts[want+"manifest.json"]; !ok || len(rec.puts) != 1 {
			t.Fatalf("local PUTs = %v", rec.puts)
		}
	})
	for _, missing := range []error{domain.ErrEvidenceDestinationNotConfigured, domain.ErrEvidenceUploadNotConfigured} {
		t.Run("local missing: "+missing.Error(), func(t *testing.T) {
			s, dir := evidenceServer(t, "http://unused.invalid")
			fake := &fakeHostedOpener{local: r2upload.New(evidenceTargetSource{err: missing})}
			s.evidenceBucketOpener = fake
			objects := hostedPage(t, dir, hostedContents, "index.html", "manifest.json")
			got, rpcErr := callEvidence(t, s, &pb.UploadEvidenceRequest{EvidenceDir: dir, PrNumber: 42, HeadSha: evidenceTestSHA, Objects: objects, Destination: pb.EvidenceDestination_EVIDENCE_DESTINATION_LOCAL_OR_HOSTED})
			if rpcErr != nil {
				t.Fatal(rpcErr)
			}
			if fake.opened != 1 || len(fake.starts) != 1 || len(fake.puts) != 2 || got.Destination != pb.EvidenceDestination_EVIDENCE_DESTINATION_HOSTED || got.PageUrl == "" {
				t.Fatalf("response = %v, opened %d, starts %d, puts %d", got, fake.opened, len(fake.starts), len(fake.puts))
			}
		})
	}
	t.Run("local broken", func(t *testing.T) {
		s, dir := evidenceServer(t, "http://unused.invalid")
		fake := &fakeHostedOpener{local: r2upload.New(evidenceTargetSource{err: errors.New("keyring locked")})}
		s.evidenceBucketOpener = fake
		_, rpcErr := callEvidence(t, s, &pb.UploadEvidenceRequest{EvidenceDir: dir, PrNumber: 42, HeadSha: evidenceTestSHA, Objects: []*pb.EvidenceObject{{Name: "manifest.json", Path: "manifest.json"}}, Destination: pb.EvidenceDestination_EVIDENCE_DESTINATION_LOCAL_OR_HOSTED})
		if rpcErr == nil || rpcErr.Code() != connect.CodeUnavailable || len(fake.starts) != 0 {
			t.Fatalf("error = %v, starts %d", rpcErr, len(fake.starts))
		}
	})
}

func TestUploadEvidenceLocalDestination_Sessions_AC260201(t *testing.T) {
	for _, destination := range []pb.EvidenceDestination{pb.EvidenceDestination_EVIDENCE_DESTINATION_UNSPECIFIED, pb.EvidenceDestination_EVIDENCE_DESTINATION_LOCAL} {
		t.Run(destination.String(), func(t *testing.T) {
			rec := newRecordingEndpoint(t)
			s, dir := evidenceServer(t, rec.server.URL)
			fake := &fakeHostedOpener{local: s.evidenceBucketOpener}
			s.evidenceBucketOpener = fake
			req := &pb.UploadEvidenceRequest{EvidenceDir: dir, PrNumber: 42, HeadSha: evidenceTestSHA, Objects: []*pb.EvidenceObject{{Name: "manifest.json", Path: "manifest.json"}}, Destination: destination}
			got, rpcErr := callEvidence(t, s, req)
			if rpcErr != nil {
				t.Fatal(rpcErr)
			}
			if got.Destination != pb.EvidenceDestination_EVIDENCE_DESTINATION_LOCAL || got.KeyPrefix != "acme/widgets/42/"+evidenceTestSHA+"/" || len(rec.puts) != 1 {
				t.Fatalf("response = %v, PUTs %v", got, rec.puts)
			}
			// A missing local destination is still refused, never sent to the hosted host.
			fake.local = nil
			_, rpcErr = callEvidence(t, s, req)
			if rpcErr == nil || rpcErr.Code() != connect.CodeFailedPrecondition || rpcErr.Meta().Get(EvidenceReasonHeader) != "not-configured" || len(fake.starts) != 0 {
				t.Fatalf("error = %v, starts %d", rpcErr, len(fake.starts))
			}
		})
	}
	t.Run("unknown destination", func(t *testing.T) {
		s, dir := evidenceServer(t, "http://unused.invalid")
		_, rpcErr := callEvidence(t, s, &pb.UploadEvidenceRequest{EvidenceDir: dir, Objects: []*pb.EvidenceObject{{Name: "manifest.json", Path: "manifest.json"}}, Destination: 9})
		if rpcErr == nil || rpcErr.Code() != connect.CodeInvalidArgument {
			t.Fatalf("error = %v", rpcErr)
		}
	})
}

func TestUploadEvidenceHostedRefusals_Sessions_AC260204(t *testing.T) {
	page := []string{"manifest.json", "index.html"}
	for _, tc := range []struct {
		name     string
		opener   func(*fakeHostedOpener) evidenceBucketOpener
		startErr error
		putErr   error
		shrink   bool
		keyed    bool
		origin   string
		objects  []string
		code     connect.Code
		reason   string
		limit    string
		limitMax string
		started  bool
	}{
		{name: "no hosted port", opener: func(*fakeHostedOpener) evidenceBucketOpener { return r2upload.New(evidenceTargetSource{}) }, keyed: true, objects: page, code: connect.CodeFailedPrecondition, reason: "not-configured"},
		{name: "no opener", opener: func(*fakeHostedOpener) evidenceBucketOpener { return nil }, keyed: true, objects: page, code: connect.CodeFailedPrecondition, reason: "not-configured"},
		{name: "not signed in", startErr: ErrHostedProofUnavailable, keyed: true, objects: page, code: connect.CodeFailedPrecondition, reason: "not-configured", started: true},
		{name: "hosting turned off by an owner", startErr: connect.NewError(connect.CodeFailedPrecondition, errors.New("hosted proof pages are turned off for this organization")), keyed: true, objects: page, code: connect.CodeFailedPrecondition, reason: "not-configured", started: true},
		{name: "not a member", startErr: connect.NewError(connect.CodePermissionDenied, errors.New("denied")), keyed: true, objects: page, code: connect.CodeFailedPrecondition, reason: "not-configured", started: true},
		{name: "unauthenticated", startErr: connect.NewError(connect.CodeUnauthenticated, errors.New("expired")), keyed: true, objects: page, code: connect.CodeFailedPrecondition, reason: "not-configured", started: true},
		{name: "offline", startErr: connect.NewError(connect.CodeUnavailable, errors.New("offline")), keyed: true, objects: page, code: connect.CodeFailedPrecondition, reason: "not-configured", started: true},
		{name: "transport", startErr: errors.New("dial tcp: refused"), keyed: true, objects: page, code: connect.CodeFailedPrecondition, reason: "not-configured", started: true},
		{name: "file count limit", startErr: &HostedProofLimitError{Limit: "file-count", Max: 40}, keyed: true, objects: page, code: connect.CodeResourceExhausted, reason: "over-limit", limit: "file-count", limitMax: "40", started: true},
		{name: "file bytes limit", startErr: &HostedProofLimitError{Limit: "file-bytes", Max: 1000}, keyed: true, objects: page, code: connect.CodeResourceExhausted, reason: "over-limit", limit: "file-bytes", limitMax: "1000", started: true},
		{name: "page bytes limit", startErr: &HostedProofLimitError{Limit: "page-bytes", Max: 5000}, keyed: true, objects: page, code: connect.CodeResourceExhausted, reason: "over-limit", limit: "page-bytes", limitMax: "5000", started: true},
		{name: "org window", startErr: &HostedProofLimitError{Limit: "org-pages-per-hour", Max: 20}, keyed: true, objects: page, code: connect.CodeFailedPrecondition, reason: "quota-exhausted", started: true},
		{name: "global window", startErr: &HostedProofLimitError{Limit: "global-bytes-per-day"}, keyed: true, objects: page, code: connect.CodeFailedPrecondition, reason: "quota-exhausted", started: true},
		{name: "unknown limit", startErr: &HostedProofLimitError{Limit: "unknown"}, keyed: true, objects: page, code: connect.CodeFailedPrecondition, reason: "quota-exhausted", started: true},
		{name: "manifest shape", startErr: connect.NewError(connect.CodeInvalidArgument, errors.New("bad name")), keyed: true, objects: page, code: connect.CodeInvalidArgument, started: true},
		{name: "put failure", putErr: errors.New("reset"), keyed: true, objects: page, code: connect.CodeUnavailable, reason: "object-upload-failed", started: true},
		{name: "server has no proof storage", startErr: ErrHostedProofServerNotConfigured, keyed: true, objects: page, code: connect.CodeUnavailable, reason: "server-not-configured", started: true},
		{name: "server lost its proof storage mid-upload", putErr: ErrHostedProofServerNotConfigured, keyed: true, objects: page, code: connect.CodeUnavailable, reason: "server-not-configured", started: true},
		{name: "short read", shrink: true, keyed: true, objects: page, code: connect.CodeUnavailable, reason: "object-changed", started: true},
		{name: "random prefix without page objects", objects: []string{"manifest.json"}, code: connect.CodeFailedPrecondition, reason: "not-configured"},
		{name: "random prefix with index.html", objects: page, code: connect.CodeInvalidArgument},
		{name: "keyed without index.html", keyed: true, objects: []string{"manifest.json", "player.js"}, code: connect.CodeResourceExhausted, reason: "over-limit", limit: "file-count", limitMax: "64"},
		{name: "nested origin", keyed: true, origin: "https://gitlab.example/group/sub/repo.git", objects: page, code: connect.CodeFailedPrecondition},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, dir := evidenceServer(t, "http://unused.invalid")
			fake := &fakeHostedOpener{startErr: tc.startErr, putErr: tc.putErr}
			if tc.shrink {
				fake.afterStart = func() {
					if err := os.Truncate(filepath.Join(dir, "index.html"), 1); err != nil {
						t.Fatal(err)
					}
				}
			}
			s.evidenceBucketOpener = fake
			if tc.opener != nil {
				s.evidenceBucketOpener = tc.opener(fake)
			}
			if tc.origin != "" {
				s.repos.(evidenceRepoList).repo.OriginURL = tc.origin
			}
			req := &pb.UploadEvidenceRequest{EvidenceDir: dir, Objects: hostedPage(t, dir, hostedContents, tc.objects...), Destination: pb.EvidenceDestination_EVIDENCE_DESTINATION_HOSTED}
			if tc.keyed {
				req.PrNumber, req.HeadSha = 42, evidenceTestSHA
			}
			_, rpcErr := callEvidence(t, s, req)
			if rpcErr == nil || rpcErr.Code() != tc.code {
				t.Fatalf("error = %v, want code %v", rpcErr, tc.code)
			}
			meta := rpcErr.Meta()
			if meta.Get(EvidenceReasonHeader) != tc.reason || meta.Get(EvidenceLimitHeader) != tc.limit || meta.Get(EvidenceLimitMaxHeader) != tc.limitMax {
				t.Fatalf("headers reason=%q limit=%q max=%q, want %q %q %q", meta.Get(EvidenceReasonHeader), meta.Get(EvidenceLimitHeader), meta.Get(EvidenceLimitMaxHeader), tc.reason, tc.limit, tc.limitMax)
			}
			if started := len(fake.starts) == 1; started != tc.started || fake.opened != 0 {
				t.Fatalf("started = %v, want %v; local opened %d", started, tc.started, fake.opened)
			}
			if tc.putErr == nil && !tc.shrink && len(fake.puts) != 0 {
				t.Fatalf("refusal wrote %d objects", len(fake.puts))
			}
			if slices.ContainsFunc(fake.puts, func(p HostedProofPut) bool { return tc.shrink && p.Name == "index.html" }) {
				t.Fatal("a short read was still uploaded")
			}
			if wantAborts := tc.putErr != nil || tc.shrink; (len(fake.aborts) == 1) != wantAborts || len(fake.aborts) > 1 {
				t.Fatalf("aborts = %+v, want one only after a failed object", fake.aborts)
			}
		})
	}
}

// A Put that fails partway aborts the upload once and still returns the Put's
// own failure, whether or not the abort succeeds.
func TestUploadEvidenceHostedAbortsAfterAFailedPut_Organizations_AC278201(t *testing.T) {
	for _, tc := range []struct {
		name     string
		abortErr error
	}{
		{name: "abort succeeds"},
		{name: "abort fails", abortErr: connect.NewError(connect.CodeUnavailable, errors.New("bosso unreachable"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, dir := evidenceServer(t, "http://unused.invalid")
			fake := &fakeHostedOpener{putErr: errors.New("connection reset"), failPutAt: 2, abortErr: tc.abortErr}
			s.evidenceBucketOpener = fake
			objects := hostedPage(t, dir, hostedContents, "media/a.png", "player.js", "manifest.json", "index.html")
			_, rpcErr := callEvidence(t, s, &pb.UploadEvidenceRequest{EvidenceDir: dir, PrNumber: 42, HeadSha: evidenceTestSHA, Objects: objects, Destination: pb.EvidenceDestination_EVIDENCE_DESTINATION_HOSTED})
			if rpcErr == nil || rpcErr.Code() != connect.CodeUnavailable || rpcErr.Meta().Get(EvidenceReasonHeader) != "object-upload-failed" {
				t.Fatalf("error = %v, want the failed Put's Unavailable object-upload-failed", rpcErr)
			}
			if len(fake.puts) != 2 {
				t.Fatalf("puts = %d, want the publish to stop at the failed second Put", len(fake.puts))
			}
			want := []hostedAbort{{uploadID: "upload-1", repoOriginURL: "git@github.com:acme/widgets.git"}}
			if !slices.Equal(fake.aborts, want) {
				t.Fatalf("aborts = %+v, want %+v", fake.aborts, want)
			}
		})
	}
}

// A server with no proof page storage reaches the caller as its own stage
// with a plain-words message, and bosso's text stays in the daemon log.
func TestUploadEvidenceHostedNamesAServerWithNoProofStorage(t *testing.T) {
	s, dir := evidenceServer(t, "http://unused.invalid")
	upstream := errors.Join(ErrHostedProofServerNotConfigured, errors.New("bosso private detail"))
	s.evidenceBucketOpener = &fakeHostedOpener{startErr: upstream}
	req := &pb.UploadEvidenceRequest{EvidenceDir: dir, PrNumber: 42, HeadSha: evidenceTestSHA, Objects: hostedPage(t, dir, hostedContents, "index.html"), Destination: pb.EvidenceDestination_EVIDENCE_DESTINATION_HOSTED}
	_, rpcErr := callEvidence(t, s, req)
	if rpcErr == nil || rpcErr.Code() != connect.CodeUnavailable || rpcErr.Meta().Get(EvidenceReasonHeader) != "server-not-configured" {
		t.Fatalf("error = %v, want Unavailable server-not-configured", rpcErr)
	}
	if rpcErr.Message() != "hosted review pages aren't set up on this server" {
		t.Fatalf("message = %q, want the plain-words reason and no upstream detail", rpcErr.Message())
	}
}
