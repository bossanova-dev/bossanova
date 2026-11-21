package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	server "github.com/recurser/bossd/api/daemon"
	"github.com/recurser/bossd/internal/adapter/r2upload"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/domain"
	"github.com/rs/zerolog"
)

type hostedProofRPC struct {
	starts   []*connect.Request[pb.StartProofUploadRequest]
	puts     []*connect.Request[pb.PutProofObjectRequest]
	startErr error
	putErr   error
}

func (r *hostedProofRPC) StartProofUpload(_ context.Context, req *connect.Request[pb.StartProofUploadRequest]) (*connect.Response[pb.StartProofUploadResponse], error) {
	r.starts = append(r.starts, req)
	if r.startErr != nil {
		return nil, r.startErr
	}
	return connect.NewResponse(&pb.StartProofUploadResponse{UploadId: "upload-7", OrgLabel: "acme-org", PageToken: "page-token"}), nil
}

func (r *hostedProofRPC) PutProofObject(_ context.Context, req *connect.Request[pb.PutProofObjectRequest]) (*connect.Response[pb.PutProofObjectResponse], error) {
	r.puts = append(r.puts, req)
	if r.putErr != nil {
		return nil, r.putErr
	}
	return connect.NewResponse(&pb.PutProofObjectResponse{}), nil
}

const hostedTestSHA = "0123456789abcdef0123456789abcdef01234567"

func TestHostedProofClientSendsDaemonBearer_Sessions_AC260202(t *testing.T) {
	rpc := &hostedProofRPC{}
	client := &hostedProofClient{rpc: rpc, token: func() string { return "daemon-session-token" }, reviewOrigin: "https://app.bossanova.dev"}
	upload, err := client.StartHostedProof(context.Background(), server.HostedProofStart{RepoOriginURL: "https://github.com/acme/widgets", HeadSHA: hostedTestSHA, PR: 9, Manifest: []server.HostedProofEntry{{Name: "media/a.png", Size: 3}, {Name: "index.html", Size: 15}}})
	if err != nil {
		t.Fatal(err)
	}
	if upload.UploadID != "upload-7" || upload.ReviewOrigin != "https://app.bossanova.dev" {
		t.Fatalf("upload = %+v", upload)
	}
	start := rpc.starts[0]
	if start.Header().Get("Authorization") != "Bearer daemon-session-token" {
		t.Fatal("start is missing the daemon bearer")
	}
	m := start.Msg
	if m.RepoOriginUrl != "https://github.com/acme/widgets" || m.PrNumber != 9 || m.HeadSha != hostedTestSHA || len(m.Manifest) != 2 || m.Manifest[0].Name != "media/a.png" || m.Manifest[0].SizeBytes != 3 || m.Manifest[1].Name != "index.html" || m.Manifest[1].SizeBytes != 15 {
		t.Fatalf("start = %v", m)
	}
	if err := client.PutHostedProofObject(context.Background(), server.HostedProofPut{UploadID: "upload-7", RepoOriginURL: "https://github.com/acme/widgets", Name: "index.html", Body: []byte("<!doctype html>")}); err != nil {
		t.Fatal(err)
	}
	put := rpc.puts[0]
	if put.Header().Get("Authorization") != "Bearer daemon-session-token" || put.Msg.UploadId != "upload-7" || put.Msg.RepoOriginUrl != "https://github.com/acme/widgets" || put.Msg.Name != "index.html" || string(put.Msg.Content) != "<!doctype html>" {
		t.Fatalf("put = %v (auth %q)", put.Msg, put.Header().Get("Authorization"))
	}
	rpc.putErr = connect.NewError(connect.CodeUnavailable, errors.New("reset"))
	if err := client.PutHostedProofObject(context.Background(), server.HostedProofPut{UploadID: "upload-7", Name: "index.html"}); !errors.Is(err, rpc.putErr) {
		t.Fatalf("put error = %v, want the upstream error", err)
	}
}

func TestHostedProofClientRefusesBeforeCalling_Sessions_AC260204(t *testing.T) {
	for _, tc := range []struct {
		name, token, origin string
	}{
		{name: "not signed in", origin: "https://app.bossanova.dev"},
		{name: "no review origin", token: "token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rpc := &hostedProofRPC{}
			client := &hostedProofClient{rpc: rpc, token: func() string { return tc.token }, reviewOrigin: tc.origin}
			if _, err := client.StartHostedProof(context.Background(), server.HostedProofStart{}); !errors.Is(err, server.ErrHostedProofUnavailable) {
				t.Fatalf("error = %v", err)
			}
			if len(rpc.starts) != 0 {
				t.Fatal("refused start still called bosso")
			}
		})
	}
	rpc := &hostedProofRPC{}
	client := &hostedProofClient{rpc: rpc, reviewOrigin: "https://app.bossanova.dev"}
	if err := client.PutHostedProofObject(context.Background(), server.HostedProofPut{}); !errors.Is(err, server.ErrHostedProofUnavailable) || len(rpc.puts) != 0 {
		t.Fatalf("tokenless put = %v, calls %d", err, len(rpc.puts))
	}
}

func limitRefusal(t *testing.T, detail *pb.ProofUploadLimitExceeded) error {
	t.Helper()
	rpcErr := connect.NewError(connect.CodeResourceExhausted, errors.New("limit"))
	if detail != nil {
		d, err := connect.NewErrorDetail(detail)
		if err != nil {
			t.Fatal(err)
		}
		rpcErr.AddDetail(d)
	}
	return rpcErr
}

func TestHostedProofClientMapsLimits_Sessions_AC260204(t *testing.T) {
	want := map[pb.ProofUploadLimit]string{
		pb.ProofUploadLimit_PROOF_UPLOAD_LIMIT_FILE_COUNT:           "file-count",
		pb.ProofUploadLimit_PROOF_UPLOAD_LIMIT_FILE_BYTES:           "file-bytes",
		pb.ProofUploadLimit_PROOF_UPLOAD_LIMIT_PAGE_BYTES:           "page-bytes",
		pb.ProofUploadLimit_PROOF_UPLOAD_LIMIT_ORG_PAGES_PER_HOUR:   "org-pages-per-hour",
		pb.ProofUploadLimit_PROOF_UPLOAD_LIMIT_ORG_PAGES_PER_DAY:    "org-pages-per-day",
		pb.ProofUploadLimit_PROOF_UPLOAD_LIMIT_ORG_BYTES_PER_DAY:    "org-bytes-per-day",
		pb.ProofUploadLimit_PROOF_UPLOAD_LIMIT_GLOBAL_BYTES_PER_DAY: "global-bytes-per-day",
		pb.ProofUploadLimit_PROOF_UPLOAD_LIMIT_UNSPECIFIED:          "unknown",
	}
	for value := range pb.ProofUploadLimit_name {
		limit := pb.ProofUploadLimit(value)
		if _, ok := want[limit]; !ok {
			t.Fatalf("ProofUploadLimit %v has no expected kind", limit)
		}
	}
	start := func(err error) error {
		client := &hostedProofClient{rpc: &hostedProofRPC{startErr: err}, token: func() string { return "token" }, reviewOrigin: "https://app.bossanova.dev"}
		_, got := client.StartHostedProof(context.Background(), server.HostedProofStart{})
		return got
	}
	for limit, kind := range want {
		t.Run(kind+"/"+limit.String(), func(t *testing.T) {
			var got *server.HostedProofLimitError
			if err := start(limitRefusal(t, &pb.ProofUploadLimitExceeded{Limit: limit, MaxValue: 40, ActualValue: 41})); !errors.As(err, &got) || got.Limit != kind {
				t.Fatalf("error = %v, want limit %q", err, kind)
			}
			if kind != "unknown" && got.Max != 40 {
				t.Fatalf("max = %d", got.Max)
			}
		})
	}
	var got *server.HostedProofLimitError
	if err := start(limitRefusal(t, nil)); !errors.As(err, &got) || got.Limit != "unknown" {
		t.Fatalf("detail-less refusal = %v", err)
	}
	denied := connect.NewError(connect.CodePermissionDenied, errors.New("not a member"))
	if err := start(denied); !errors.Is(err, denied) {
		t.Fatalf("non-limit error = %v, want it unchanged", err)
	}
}

func TestReviewAppOrigin_Sessions_AC260204(t *testing.T) {
	for in, want := range map[string]string{
		"https://orchestrator.bossanova.dev":          "https://app.bossanova.dev",
		"https://orchestrator.bossanova.dev/":         "https://app.bossanova.dev",
		"HTTPS://Orchestrator.Bossanova.dev":          "https://app.bossanova.dev",
		"https://orchestrator-staging.bossanova.dev":  "https://app-staging.bossanova.dev",
		"https://orchestrator-staging.bossanova.dev/": "https://app-staging.bossanova.dev",
		"http://orchestrator.bossanova.dev":           "",
		"http://localhost:8080":                       "",
		"https://orchestrator.example.com":            "",
		"https://orchestrator.bossanova.dev.evil.com": "",
		"":          "",
		"not a url": "",
	} {
		if got := reviewAppOrigin(in); got != want {
			t.Errorf("reviewAppOrigin(%q) = %q, want %q", in, got, want)
		}
	}
}

type evidenceRepos struct {
	db.RepoStore
	repo *domain.Repo
}

func (r evidenceRepos) List(context.Context) ([]*domain.Repo, error) {
	return []*domain.Repo{r.repo}, nil
}

type evidenceSessions struct{ db.SessionStore }

func (evidenceSessions) List(context.Context, string) ([]*domain.Session, error) { return nil, nil }

type missingEvidenceTarget struct{}

func (missingEvidenceTarget) UploadTarget() (domain.EvidenceUploadTarget, error) {
	return domain.EvidenceUploadTarget{}, domain.ErrEvidenceDestinationNotConfigured
}

// TestEvidenceDestinations drives the daemon's UploadEvidence through the
// wired opener, so the door finds the hosted host on it.
func TestEvidenceDestinations_Sessions_AC260203(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".verify-evidence", "run")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<!doctype html>"), 0600); err != nil {
		t.Fatal(err)
	}
	repo := &domain.Repo{ID: "repo", LocalPath: root, IsTrusted: true, OriginURL: "https://github.com/acme/widgets.git"}
	req := &pb.UploadEvidenceRequest{EvidenceDir: dir, PrNumber: 9, HeadSha: hostedTestSHA, Objects: []*pb.EvidenceObject{{Name: "index.html", Path: "index.html"}}, Destination: pb.EvidenceDestination_EVIDENCE_DESTINATION_LOCAL_OR_HOSTED}
	rpc := &hostedProofRPC{}
	hosted := &hostedProofClient{rpc: rpc, token: func() string { return "token" }, reviewOrigin: "https://app.bossanova.dev"}
	for _, tc := range []struct {
		name   string
		hosted *hostedProofClient
		code   connect.Code
	}{
		{name: "hosted wired", hosted: hosted},
		{name: "no upstream", code: connect.CodeFailedPrecondition},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opener := evidenceDestinations{Uploader: r2upload.New(missingEvidenceTarget{}), hosted: tc.hosted}
			srv := server.New(server.Config{Repos: evidenceRepos{repo: repo}, Sessions: evidenceSessions{}, EvidenceBucketOpener: opener, Logger: zerolog.Nop()})
			resp, err := srv.UploadEvidence(context.Background(), connect.NewRequest(req))
			if tc.code != 0 {
				if connect.CodeOf(err) != tc.code {
					t.Fatalf("error = %v, want %v", err, tc.code)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if resp.Msg.PageUrl != "https://app.bossanova.dev/review/acme/widgets/9?head="+hostedTestSHA || resp.Msg.Destination != pb.EvidenceDestination_EVIDENCE_DESTINATION_HOSTED {
				t.Fatalf("response = %v", resp.Msg)
			}
			if names := []string{rpc.puts[0].Msg.Name}; len(rpc.starts) != 1 || !slices.Equal(names, []string{"index.html"}) {
				t.Fatalf("starts %d, puts %v", len(rpc.starts), names)
			}
		})
	}
}
