package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func trackerLeaseRequest(holder string) *pb.AcquireTrackerIssueLeaseRequest {
	return &pb.AcquireTrackerIssueLeaseRequest{RepoOriginUrl: "https://github.com/example/repo", IssueKey: "linear:ISSUE-1", Purpose: pb.TrackerIssueLeasePurpose_TRACKER_ISSUE_LEASE_PURPOSE_BUILD, HolderId: holder, TtlSeconds: 60}
}
func trackerLeaseRelease(r *pb.AcquireTrackerIssueLeaseRequest) *pb.ReleaseTrackerIssueLeaseRequest {
	return &pb.ReleaseTrackerIssueLeaseRequest{RepoOriginUrl: r.RepoOriginUrl, IssueKey: r.IssueKey, Purpose: r.Purpose, HolderId: r.HolderId}
}

func TestTrackerIssueLeaseLocalLifecycle_MultiSurfaceJourneys_AC227201(t *testing.T) {
	now := time.Date(2026, 11, 5, 1, 0, 0, 0, time.UTC)
	s := New(Config{TrackerIssueLeaseNow: func() time.Time { return now }})
	ctx := context.Background()
	first := trackerLeaseRequest(strings.Repeat("a", 32))
	other := trackerLeaseRequest(strings.Repeat("b", 32))
	acquire := func(r *pb.AcquireTrackerIssueLeaseRequest, acquired bool, expiry time.Time) {
		t.Helper()
		out, err := s.AcquireTrackerIssueLease(ctx, connect.NewRequest(r))
		if err != nil {
			t.Fatal(err)
		}
		if out.Msg.IsAcquired != acquired || out.Msg.Scope != pb.TrackerIssueLeaseScope_TRACKER_ISSUE_LEASE_SCOPE_DAEMON || !out.Msg.ExpiresAt.AsTime().Equal(expiry) {
			t.Fatalf("acquire = %v; want acquired %v expiry %v", out.Msg, acquired, expiry)
		}
	}
	release := func(r *pb.AcquireTrackerIssueLeaseRequest, released bool) {
		t.Helper()
		out, err := s.ReleaseTrackerIssueLease(ctx, connect.NewRequest(trackerLeaseRelease(r)))
		if err != nil || out.Msg.IsReleased != released {
			t.Fatalf("release = %v, %v; want %v", out, err, released)
		}
	}
	expiry := now.Add(time.Minute)
	acquire(first, true, expiry)
	// Local leases deliberately span repository origins for one tracker issue.
	other.RepoOriginUrl = "https://github.com/example/other"
	acquire(other, false, expiry)
	release(other, false)
	now = now.Add(10 * time.Second)
	expiry = now.Add(time.Minute)
	acquire(first, true, expiry)
	now = expiry
	acquire(other, true, now.Add(time.Minute))
	release(first, false)
	release(other, true)
	release(other, false)
	first.TtlSeconds = 0
	acquire(first, true, now.Add(2*time.Hour))
	first.Purpose = pb.TrackerIssueLeasePurpose_TRACKER_ISSUE_LEASE_PURPOSE_PLAN
	acquire(first, true, now.Add(2*time.Hour))
	now = now.Add(2 * time.Hour)
	release(first, false)
}

func TestTrackerIssueLeaseConcurrentLocalAcquire_MultiSurfaceJourneys_AC227201(t *testing.T) {
	s := New(Config{})
	var winners atomic.Int32
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() {
			out, err := s.AcquireTrackerIssueLease(context.Background(), connect.NewRequest(trackerLeaseRequest(fmt.Sprintf("%032x", i))))
			if err != nil {
				t.Error(err)
				return
			}
			if out.Msg.Scope != pb.TrackerIssueLeaseScope_TRACKER_ISSUE_LEASE_SCOPE_DAEMON {
				t.Errorf("scope = %v", out.Msg.Scope)
			}
			if out.Msg.IsAcquired {
				winners.Add(1)
			}
		})
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("winners = %d; want 1", winners.Load())
	}
}

type trackerLeaseUpstreamFake struct {
	acquire func(context.Context, *connect.Request[pb.AcquireTrackerIssueLeaseRequest]) (*connect.Response[pb.AcquireTrackerIssueLeaseResponse], error)
	release func(context.Context, *connect.Request[pb.ReleaseTrackerIssueLeaseRequest]) (*connect.Response[pb.ReleaseTrackerIssueLeaseResponse], error)
}

func (f trackerLeaseUpstreamFake) AcquireTrackerIssueLease(ctx context.Context, r *connect.Request[pb.AcquireTrackerIssueLeaseRequest]) (*connect.Response[pb.AcquireTrackerIssueLeaseResponse], error) {
	return f.acquire(ctx, r)
}
func (f trackerLeaseUpstreamFake) ReleaseTrackerIssueLease(ctx context.Context, r *connect.Request[pb.ReleaseTrackerIssueLeaseRequest]) (*connect.Response[pb.ReleaseTrackerIssueLeaseResponse], error) {
	return f.release(ctx, r)
}

func TestTrackerIssueLeaseAuthRouting_MultiSurfaceJourneys_AC227201(t *testing.T) {
	reporter := &stubAuthStateReporter{state: DaemonAuthState{NeedsLogin: true}}
	calls := 0
	want := trackerLeaseRequest(strings.Repeat("a", 32))
	upstreamError := connect.NewError(connect.CodeUnavailable, errors.New("offline"))
	fail := false
	token := "first-token"
	expected := &pb.AcquireTrackerIssueLeaseResponse{IsAcquired: true, Scope: pb.TrackerIssueLeaseScope_TRACKER_ISSUE_LEASE_SCOPE_ORGANIZATION, ExpiresAt: timestamppb.New(time.Now().Add(time.Hour))}
	fake := trackerLeaseUpstreamFake{acquire: func(_ context.Context, r *connect.Request[pb.AcquireTrackerIssueLeaseRequest]) (*connect.Response[pb.AcquireTrackerIssueLeaseResponse], error) {
		calls++
		if !proto.Equal(r.Msg, want) || r.Header().Get("Authorization") != "Bearer "+token {
			t.Fatalf("forwarded request = %v, authorization match = %v", r.Msg, r.Header().Get("Authorization") == "Bearer "+token)
		}
		if fail {
			return nil, upstreamError
		}
		return connect.NewResponse(expected), nil
	}, release: func(_ context.Context, r *connect.Request[pb.ReleaseTrackerIssueLeaseRequest]) (*connect.Response[pb.ReleaseTrackerIssueLeaseResponse], error) {
		calls++
		if !proto.Equal(r.Msg, trackerLeaseRelease(want)) || r.Header().Get("Authorization") != "Bearer "+token {
			t.Fatal("incorrect forwarded release")
		}
		if fail {
			return nil, upstreamError
		}
		return connect.NewResponse(&pb.ReleaseTrackerIssueLeaseResponse{}), nil
	}}
	s := New(Config{AuthStateReporter: reporter, TrackerIssueLeaseUpstream: fake, TrackerIssueLeaseSessionToken: func() string { return token }})
	ctx := context.Background()
	out, err := s.AcquireTrackerIssueLease(ctx, connect.NewRequest(want))
	if err != nil || !out.Msg.IsAcquired || out.Msg.Scope != pb.TrackerIssueLeaseScope_TRACKER_ISSUE_LEASE_SCOPE_DAEMON || calls != 0 {
		t.Fatalf("signed-out acquire = %v, %v calls %d", out, err, calls)
	}
	reporter.state.NeedsLogin = false
	out, err = s.AcquireTrackerIssueLease(ctx, connect.NewRequest(want))
	if err != nil || !proto.Equal(out.Msg, expected) || calls != 1 {
		t.Fatalf("signed-in acquire = %v, %v calls %d", out, err, calls)
	}
	token = "rotated-token"
	released, err := s.ReleaseTrackerIssueLease(ctx, connect.NewRequest(trackerLeaseRelease(want)))
	if err != nil || !released.Msg.IsReleased || calls != 2 {
		t.Fatalf("release = %v, %v calls %d", released, err, calls)
	}
	reporter.state.NeedsLogin = true
	peer := trackerLeaseRequest(strings.Repeat("b", 32))
	out, err = s.AcquireTrackerIssueLease(ctx, connect.NewRequest(peer))
	if err != nil || !out.Msg.IsAcquired {
		t.Fatalf("local entry survived signed-in release: %v %v", out, err)
	}
	reporter.state.NeedsLogin = false
	fail = true
	if _, err = s.AcquireTrackerIssueLease(ctx, connect.NewRequest(want)); !errors.Is(err, upstreamError) {
		t.Fatalf("upstream acquire error = %v", err)
	}
	if _, err = s.ReleaseTrackerIssueLease(ctx, connect.NewRequest(trackerLeaseRelease(want))); !errors.Is(err, upstreamError) {
		t.Fatalf("upstream release error = %v", err)
	}
	reporter.state.NeedsLogin = true
	out, err = s.AcquireTrackerIssueLease(ctx, connect.NewRequest(want))
	if err != nil || out.Msg.IsAcquired {
		t.Fatalf("failed upstream acquire mutated local entry: %v %v", out, err)
	}
}

func TestTrackerIssueLeaseValidation_MultiSurfaceJourneys_AC227201(t *testing.T) {
	cases := []struct {
		name   string
		change func(*pb.AcquireTrackerIssueLeaseRequest)
	}{
		{"empty key", func(r *pb.AcquireTrackerIssueLeaseRequest) { r.IssueKey = "" }},
		{"long key", func(r *pb.AcquireTrackerIssueLeaseRequest) { r.IssueKey = strings.Repeat("é", 129) }},
		{"control key", func(r *pb.AcquireTrackerIssueLeaseRequest) { r.IssueKey = "issue\u0085" }},
		{"unset purpose", func(r *pb.AcquireTrackerIssueLeaseRequest) { r.Purpose = 0 }},
		{"unknown purpose", func(r *pb.AcquireTrackerIssueLeaseRequest) { r.Purpose = 99 }},
		{"short holder", func(r *pb.AcquireTrackerIssueLeaseRequest) { r.HolderId = "a" }},
		{"uppercase holder", func(r *pb.AcquireTrackerIssueLeaseRequest) { r.HolderId = strings.Repeat("A", 32) }},
		{"nonhex holder", func(r *pb.AcquireTrackerIssueLeaseRequest) { r.HolderId = strings.Repeat("g", 32) }},
		{"negative ttl", func(r *pb.AcquireTrackerIssueLeaseRequest) { r.TtlSeconds = -1 }},
		{"short ttl", func(r *pb.AcquireTrackerIssueLeaseRequest) { r.TtlSeconds = 59 }},
		{"long ttl", func(r *pb.AcquireTrackerIssueLeaseRequest) { r.TtlSeconds = 14401 }},
	}
	for _, signedIn := range []bool{false, true} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("%v/%s", signedIn, tc.name), func(t *testing.T) {
				fake := trackerLeaseUpstreamFake{acquire: func(context.Context, *connect.Request[pb.AcquireTrackerIssueLeaseRequest]) (*connect.Response[pb.AcquireTrackerIssueLeaseResponse], error) {
					t.Fatal("invalid acquire forwarded")
					return nil, nil
				}, release: func(context.Context, *connect.Request[pb.ReleaseTrackerIssueLeaseRequest]) (*connect.Response[pb.ReleaseTrackerIssueLeaseResponse], error) {
					t.Fatal("invalid release forwarded")
					return nil, nil
				}}
				cfg := Config{}
				if signedIn {
					cfg.TrackerIssueLeaseUpstream = fake
					cfg.AuthStateReporter = &stubAuthStateReporter{}
				}
				s := New(cfg)
				r := trackerLeaseRequest(strings.Repeat("a", 32))
				tc.change(r)
				if _, err := s.AcquireTrackerIssueLease(context.Background(), connect.NewRequest(r)); connect.CodeOf(err) != connect.CodeInvalidArgument {
					t.Fatalf("acquire error = %v", err)
				}
				if !strings.Contains(tc.name, "ttl") {
					if _, err := s.ReleaseTrackerIssueLease(context.Background(), connect.NewRequest(trackerLeaseRelease(r))); connect.CodeOf(err) != connect.CodeInvalidArgument {
						t.Fatalf("release error = %v", err)
					}
				}
			})
		}
	}
}
