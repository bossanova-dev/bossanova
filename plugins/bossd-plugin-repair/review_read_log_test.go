package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/recurser/bossalib/kit/clocktest"
)

// TestReviewFingerprintQuotaRefusalWarnsOncePerWindow pins that repeated
// ResourceExhausted review comment reads log one Warn, then Debug, and warn
// again once the five-minute window ends.
func TestReviewFingerprintQuotaRefusalWarnsOncePerWindow(t *testing.T) {
	mock := newTestMock()
	mock.reviewCommentsErr = grpcstatus.Error(codes.ResourceExhausted, "GitHub graphql quota exhausted")
	var logs bytes.Buffer
	rm := newRepairMonitor(mock, zerolog.New(&logs).Level(zerolog.DebugLevel))
	clock := clocktest.New(time.Unix(1_800_000_000, 0))
	rm.reviewReadLog.Now = clock.Now
	info := sessionInfo{RepoOriginURL: "git@github.com:o/r.git", PRNumber: 7}
	const msg = "failed to fetch review comments; using legacy duplicate-repair guard"

	for i := 0; i < 3; i++ {
		if _, ok := rm.currentReviewFingerprint(context.Background(), info); ok {
			t.Fatalf("read %d: fingerprint reported authoritative on a refused read", i)
		}
	}
	if warn, debug := levelCounts(logs.String(), msg); warn != 1 || debug != 2 {
		t.Fatalf("logged %d Warn and %d Debug lines for three refusals, want 1 and 2:\n%s", warn, debug, logs.String())
	}
	clock.Advance(5 * time.Minute)
	rm.currentReviewFingerprint(context.Background(), info)
	if warn, _ := levelCounts(logs.String(), msg); warn != 2 {
		t.Fatalf("logged %d Warn lines after the window ended, want 2", warn)
	}
}

func levelCounts(logs, msg string) (warn, debug int) {
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, `"message":"`+msg) {
			continue
		}
		switch {
		case strings.Contains(line, `"level":"warn"`):
			warn++
		case strings.Contains(line, `"level":"debug"`):
			debug++
		}
	}
	return warn, debug
}
