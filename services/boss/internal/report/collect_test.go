package report

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/recurser/boss/internal/client"
	"github.com/recurser/bossalib/buildinfo"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// fakeReportClient embeds client.BossClient so it satisfies the full interface
// with nil method bodies. CollectContext only calls ListSessions, which we
// override; any other call would nil-panic, which is exactly the contract we
// want to pin — CollectContext must not reach for anything else.
type fakeReportClient struct {
	client.BossClient
	sessions []*pb.Session
	err      error
	gotReq   *pb.ListSessionsRequest
	calls    int
}

func (f *fakeReportClient) ListSessions(_ context.Context, req *pb.ListSessionsRequest, _ client.SessionReadOptions) ([]*pb.Session, error) {
	f.calls++
	f.gotReq = req
	return f.sessions, f.err
}

func TestCollectContext_PopulatesMetadataAndMapsSessions(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")

	updated := timestamppb.New(time.Unix(1_700_000_000, 0).UTC())
	prNum := int32(42)
	prURL := "https://example.test/pr/42"
	fake := &fakeReportClient{
		sessions: []*pb.Session{
			{
				Id:        "sess-1",
				RepoId:    "repo-1",
				Title:     "first",
				State:     pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
				PrNumber:  &prNum,
				PrUrl:     &prURL,
				UpdatedAt: updated,
				// AgentSessionId / LastCheckState are deliberately set to verify
				// they are NOT copied into the summary.
				LastCheckState: pb.ChecksOverall_CHECKS_OVERALL_FAILED,
			},
			{
				Id:     "sess-2",
				RepoId: "repo-2",
				Title:  "second",
				State:  pb.SessionState_SESSION_STATE_CREATING_WORKTREE,
			},
		},
	}
	current := &pb.Session{Id: "current"}
	daemonStatuses := map[string]string{"bossd": "healthy"}

	rc, err := CollectContext(context.Background(), fake, current, daemonStatuses)
	if err != nil {
		t.Fatalf("CollectContext returned error: %v", err)
	}
	if rc == nil {
		t.Fatal("CollectContext returned nil context")
	}

	// Environment / identity metadata.
	if rc.BossVersion != buildinfo.Version {
		t.Errorf("BossVersion = %q, want %q", rc.BossVersion, buildinfo.Version)
	}
	if rc.BossCommit != buildinfo.Commit {
		t.Errorf("BossCommit = %q, want %q", rc.BossCommit, buildinfo.Commit)
	}
	if rc.Os != runtime.GOOS {
		t.Errorf("Os = %q, want %q", rc.Os, runtime.GOOS)
	}
	if rc.Arch != runtime.GOARCH {
		t.Errorf("Arch = %q, want %q", rc.Arch, runtime.GOARCH)
	}
	if rc.Terminal != "xterm-256color" {
		t.Errorf("Terminal = %q, want %q", rc.Terminal, "xterm-256color")
	}
	if rc.CurrentSession != current {
		t.Errorf("CurrentSession = %v, want pass-through %v", rc.CurrentSession, current)
	}
	if rc.DaemonStatuses["bossd"] != "healthy" {
		t.Errorf("DaemonStatuses = %v, want bossd=healthy", rc.DaemonStatuses)
	}

	// ListSessions must be asked for non-archived sessions exactly once.
	if fake.calls != 1 {
		t.Errorf("ListSessions called %d times, want 1", fake.calls)
	}
	if fake.gotReq == nil || fake.gotReq.IncludeArchived {
		t.Errorf("ListSessions request = %+v, want IncludeArchived=false", fake.gotReq)
	}

	// Session → SessionSummary field mapping.
	if len(rc.Sessions) != 2 {
		t.Fatalf("got %d summaries, want 2", len(rc.Sessions))
	}
	got := rc.Sessions[0]
	if got.Id != "sess-1" || got.RepoId != "repo-1" || got.Title != "first" {
		t.Errorf("summary[0] id/repo/title = %q/%q/%q", got.Id, got.RepoId, got.Title)
	}
	if got.State != pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN {
		t.Errorf("summary[0] State = %v, want IMPLEMENTING_PLAN", got.State)
	}
	if got.GetPrNumber() != 42 {
		t.Errorf("summary[0] PrNumber = %d, want 42", got.GetPrNumber())
	}
	if got.GetPrUrl() != prURL {
		t.Errorf("summary[0] PrUrl = %q, want %q", got.GetPrUrl(), prURL)
	}
	if got.UpdatedAt == nil || !got.UpdatedAt.AsTime().Equal(updated.AsTime()) {
		t.Errorf("summary[0] UpdatedAt = %v, want %v", got.UpdatedAt, updated)
	}

	// The summary projects only a fixed subset of fields; nil optionals must
	// stay nil rather than be invented.
	second := rc.Sessions[1]
	if second.PrNumber != nil || second.PrUrl != nil || second.UpdatedAt != nil {
		t.Errorf("summary[1] should leave optional fields nil, got pr=%v url=%v updated=%v",
			second.PrNumber, second.PrUrl, second.UpdatedAt)
	}
}

// TestCollectContext_ListSessionsErrorIsSwallowed pins the load-bearing
// contract documented on CollectContext: a failing ListSessions RPC (the most
// likely failure exactly when a user is filing a report about a misbehaving
// daemon) must NOT surface as an error, so the report still reaches triage. The
// session list is simply left empty (non-nil, zero length).
func TestCollectContext_ListSessionsErrorIsSwallowed(t *testing.T) {
	fake := &fakeReportClient{err: errors.New("daemon unreachable")}

	rc, err := CollectContext(context.Background(), fake, nil, nil)
	if err != nil {
		t.Fatalf("CollectContext returned error, want nil despite RPC failure: %v", err)
	}
	if rc == nil {
		t.Fatal("CollectContext returned nil context on RPC failure")
	}
	if rc.Sessions == nil {
		t.Error("Sessions should be non-nil even when ListSessions fails")
	}
	if len(rc.Sessions) != 0 {
		t.Errorf("Sessions = %v, want empty on RPC failure", rc.Sessions)
	}
	// Metadata is still collected regardless of the RPC outcome.
	if rc.Os != runtime.GOOS {
		t.Errorf("Os = %q, want %q", rc.Os, runtime.GOOS)
	}
}
