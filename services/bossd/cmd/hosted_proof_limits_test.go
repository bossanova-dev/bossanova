package main

import (
	"context"
	"errors"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	server "github.com/recurser/bossd/api/daemon"
	"github.com/recurser/bossd/internal/adapter/r2upload"
)

const limitsTestOrigin = "https://github.com/acme/widgets"

type manualClock struct{ at time.Time }

func (c *manualClock) now() time.Time { return c.at }

func limitsTestClient(rpc *hostedProofRPC, clock *manualClock) *hostedProofClient {
	client := &hostedProofClient{rpc: rpc, token: func() string { return "token" }, reviewOrigin: "https://app.bossanova.dev"}
	client.limits.now = clock.now
	return client
}

func startLimitsTest(t *testing.T, client *hostedProofClient, origin string) (server.HostedProofUpload, error) {
	t.Helper()
	return client.StartHostedProof(context.Background(), server.HostedProofStart{RepoOriginURL: origin, HeadSHA: hostedTestSHA, PR: 9, Manifest: []server.HostedProofEntry{{Name: "index.html", Size: 15}}})
}

func TestHostedProofClientKeepsTheLimitsBossoReturned_Sessions_AC278401(t *testing.T) {
	clock := &manualClock{at: time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)}
	rpc := &hostedProofRPC{limits: &pb.ProofUploadLimits{AllowedExtensions: []string{"png"}, MaxFileCount: 64, MaxFileBytes: 50 << 20, MaxPageBytes: 200 << 20, MaxPathDepth: 4, MaxPathLength: 200}}
	client := limitsTestClient(rpc, clock)
	opener := evidenceDestinations{Uploader: r2upload.New(missingEvidenceTarget{}), hosted: client}
	if _, ok := opener.HostedProofLimits(limitsTestOrigin); ok {
		t.Fatal("an empty cache returned limits")
	}
	upload, err := startLimitsTest(t, client, limitsTestOrigin)
	if err != nil {
		t.Fatal(err)
	}
	want := server.HostedProofLimits{FileCount: 64, FileBytes: 50 << 20, PageBytes: 200 << 20}
	if upload.Limits != want {
		t.Fatalf("upload limits = %+v, want %+v", upload.Limits, want)
	}
	if got, ok := opener.HostedProofLimits(limitsTestOrigin); !ok || got != want {
		t.Fatalf("cached limits = %+v, %v; want %+v", got, ok, want)
	}
	if _, ok := opener.HostedProofLimits("https://github.com/acme/other"); ok {
		t.Fatal("another repository read this repository's limits")
	}
	clock.at = clock.at.Add(hostedProofLimitsTTL - time.Second)
	if _, ok := opener.HostedProofLimits(limitsTestOrigin); !ok {
		t.Fatal("limits expired before their TTL")
	}
	clock.at = clock.at.Add(time.Second)
	if got, ok := opener.HostedProofLimits(limitsTestOrigin); ok {
		t.Fatalf("expired limits returned: %+v", got)
	}
	if _, ok := (evidenceDestinations{}).HostedProofLimits(limitsTestOrigin); ok {
		t.Fatal("a daemon with no hosted client returned limits")
	}
}

func TestHostedProofClientStartWithoutLimitsKeepsTheCache_Sessions_AC278401(t *testing.T) {
	clock := &manualClock{at: time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)}
	rpc := &hostedProofRPC{limits: &pb.ProofUploadLimits{MaxFileCount: 64}}
	client := limitsTestClient(rpc, clock)
	if _, err := startLimitsTest(t, client, limitsTestOrigin); err != nil {
		t.Fatal(err)
	}
	rpc.limits = nil
	upload, err := startLimitsTest(t, client, limitsTestOrigin)
	if err != nil {
		t.Fatal(err)
	}
	if upload.Limits != (server.HostedProofLimits{}) {
		t.Fatalf("a response with no limits reported %+v", upload.Limits)
	}
	if got, ok := client.HostedProofLimits(limitsTestOrigin); !ok || got.FileCount != 64 {
		t.Fatalf("cached limits = %+v, %v", got, ok)
	}
}

func TestHostedProofClientLearnsLimitsFromRefusals_Sessions_AC278401(t *testing.T) {
	clock := &manualClock{at: time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)}
	rpc := &hostedProofRPC{limits: &pb.ProofUploadLimits{MaxFileCount: 64, MaxFileBytes: 900, MaxPageBytes: 5000}}
	client := limitsTestClient(rpc, clock)
	if _, err := startLimitsTest(t, client, limitsTestOrigin); err != nil {
		t.Fatal(err)
	}
	refuse := func(limit pb.ProofUploadLimit, maxValue int64) {
		t.Helper()
		rpc.startErr = limitRefusal(t, &pb.ProofUploadLimitExceeded{Limit: limit, MaxValue: maxValue, ActualValue: maxValue + 1})
		var refusal *server.HostedProofLimitError
		if _, err := startLimitsTest(t, client, limitsTestOrigin); !errors.As(err, &refusal) {
			t.Fatalf("refusal = %v", err)
		}
	}
	clock.at = clock.at.Add(30 * time.Minute)
	refuse(pb.ProofUploadLimit_PROOF_UPLOAD_LIMIT_PAGE_BYTES, 4000)
	refuse(pb.ProofUploadLimit_PROOF_UPLOAD_LIMIT_ORG_PAGES_PER_DAY, 10)
	want := server.HostedProofLimits{FileCount: 64, FileBytes: 900, PageBytes: 4000}
	if got, ok := client.HostedProofLimits(limitsTestOrigin); !ok || got != want {
		t.Fatalf("limits after refusals = %+v, %v; want %+v", got, ok, want)
	}
	clock.at = clock.at.Add(45 * time.Minute)
	if _, ok := client.HostedProofLimits(limitsTestOrigin); !ok {
		t.Fatal("a refusal did not refresh the limits")
	}
	other := "https://github.com/acme/other"
	rpc.startErr = limitRefusal(t, &pb.ProofUploadLimitExceeded{Limit: pb.ProofUploadLimit_PROOF_UPLOAD_LIMIT_FILE_COUNT, MaxValue: 12, ActualValue: 13})
	if _, err := startLimitsTest(t, client, other); err == nil {
		t.Fatal("refusal succeeded")
	}
	if got, ok := client.HostedProofLimits(other); !ok || got != (server.HostedProofLimits{FileCount: 12}) {
		t.Fatalf("refusal-only limits = %+v, %v", got, ok)
	}
}
