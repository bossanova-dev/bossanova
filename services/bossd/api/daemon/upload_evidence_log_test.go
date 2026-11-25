package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog"
)

// TestUploadEvidenceLogsItsFailures proves a refused upload leaves a warn-level
// daemon trace naming the repo, PR, head, publisher and the withheld cause, while
// the caller gets only the fixed message and the failing stage's reason.
func TestUploadEvidenceLogsItsFailures(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bucket is read-only", http.StatusInternalServerError)
	}))
	t.Cleanup(endpoint.Close)
	s, dir := evidenceServer(t, endpoint.URL)
	var logs bytes.Buffer
	s.logger = zerolog.New(&logs)

	_, err := s.UploadEvidence(context.Background(), connect.NewRequest(&pb.UploadEvidenceRequest{
		EvidenceDir: dir,
		PrNumber:    144,
		HeadSha:     evidenceTestSHA,
		Destination: pb.EvidenceDestination_EVIDENCE_DESTINATION_LOCAL,
		Objects:     []*pb.EvidenceObject{{Name: "manifest.json", Path: "manifest.json"}},
	}))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("code = %v, want unavailable", connect.CodeOf(err))
	}
	var rpcErr *connect.Error
	if !errors.As(err, &rpcErr) || rpcErr.Message() != "evidence object upload failed" {
		t.Fatalf("RPC message = %q, want the fixed text", rpcErr.Message())
	}
	if got := rpcErr.Meta().Get(EvidenceReasonHeader); got != "object-upload-failed" {
		t.Errorf("reason header = %q, want the failing stage", got)
	}

	var entry map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var candidate map[string]any
		if json.Unmarshal([]byte(line), &candidate) == nil && candidate["level"] == "warn" {
			entry = candidate
		}
	}
	if entry == nil {
		t.Fatalf("no warn entry in logs:\n%s", logs.String())
	}
	want := map[string]any{
		"repo":      "acme/widgets",
		"repo_id":   "repo",
		"pr_number": float64(144),
		"head_sha":  evidenceTestSHA,
		"publisher": "EVIDENCE_DESTINATION_LOCAL",
		"code":      "unavailable",
	}
	for key, value := range want {
		if entry[key] != value {
			t.Errorf("log %s = %v, want %v", key, entry[key], value)
		}
	}
	if cause, _ := entry["cause"].(string); !strings.Contains(cause, "R2 upload: HTTP 500") {
		t.Errorf("log cause = %q, want the bucket's own error", cause)
	}
}
