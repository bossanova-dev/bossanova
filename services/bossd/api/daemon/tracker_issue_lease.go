package server

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TrackerIssueLeaseUpstream is the daemon-authenticated issue coordination transport.
type TrackerIssueLeaseUpstream interface {
	AcquireTrackerIssueLease(context.Context, *connect.Request[pb.AcquireTrackerIssueLeaseRequest]) (*connect.Response[pb.AcquireTrackerIssueLeaseResponse], error)
	ReleaseTrackerIssueLease(context.Context, *connect.Request[pb.ReleaseTrackerIssueLeaseRequest]) (*connect.Response[pb.ReleaseTrackerIssueLeaseResponse], error)
}

type trackerIssueLeaseKey struct {
	issue   string
	purpose pb.TrackerIssueLeasePurpose
}
type trackerIssueLeaseEntry struct {
	holder    string
	expiresAt time.Time
}
type trackerIssueLeases struct {
	mu           sync.Mutex
	entries      map[trackerIssueLeaseKey]trackerIssueLeaseEntry
	now          func() time.Time
	upstream     TrackerIssueLeaseUpstream
	sessionToken func() string
}

func (l *trackerIssueLeases) prune() time.Time {
	now := l.now()
	for key, entry := range l.entries {
		if !now.Before(entry.expiresAt) {
			delete(l.entries, key)
		}
	}
	return now
}
func (l *trackerIssueLeases) acquire(r *pb.AcquireTrackerIssueLeaseRequest, ttl time.Duration) *pb.AcquireTrackerIssueLeaseResponse {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.prune()
	key := trackerIssueLeaseKey{r.IssueKey, r.Purpose}
	entry, exists := l.entries[key]
	acquired := !exists || entry.holder == r.HolderId
	if acquired {
		entry = trackerIssueLeaseEntry{r.HolderId, now.Add(ttl)}
		l.entries[key] = entry
	}
	return &pb.AcquireTrackerIssueLeaseResponse{IsAcquired: acquired, ExpiresAt: timestamppb.New(entry.expiresAt), Scope: pb.TrackerIssueLeaseScope_TRACKER_ISSUE_LEASE_SCOPE_DAEMON}
}
func (l *trackerIssueLeases) release(r *pb.ReleaseTrackerIssueLeaseRequest) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune()
	key := trackerIssueLeaseKey{r.IssueKey, r.Purpose}
	if entry, exists := l.entries[key]; exists && entry.holder == r.HolderId {
		delete(l.entries, key)
		return true
	}
	return false
}
func (s *Server) trackerIssueLeaseSignedIn(ctx context.Context) bool {
	return s.trackerIssueLeases.upstream != nil && s.authStateReporter != nil && !s.authStateReporter.AuthState(ctx).NeedsLogin
}
func (l *trackerIssueLeases) authorize(header interface{ Set(string, string) }) {
	if l.sessionToken != nil {
		header.Set("Authorization", "Bearer "+l.sessionToken())
	}
}

// AcquireTrackerIssueLease arbitrates locally only when upstream authentication is unavailable.
// These additive daemon RPCs do not change existing orchestrator behavior, so no
// version transform is needed; see docs/structure/contracts.md#skipping-a-bump-justify-it-in-the-diff.
func (s *Server) AcquireTrackerIssueLease(ctx context.Context, req *connect.Request[pb.AcquireTrackerIssueLeaseRequest]) (*connect.Response[pb.AcquireTrackerIssueLeaseResponse], error) {
	r := req.Msg
	ttl, err := validateTrackerIssueLease(r.GetIssueKey(), r.GetPurpose(), r.GetHolderId(), r.GetTtlSeconds())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if s.trackerIssueLeaseSignedIn(ctx) {
		request := connect.NewRequest(r)
		s.trackerIssueLeases.authorize(request.Header())
		return s.trackerIssueLeases.upstream.AcquireTrackerIssueLease(ctx, request)
	}
	return connect.NewResponse(s.trackerIssueLeases.acquire(r, ttl)), nil
}

// ReleaseTrackerIssueLease removes the caller's local lease even after an auth transition.
// A signed-in release also reaches upstream; its errors remain visible to the caller.
func (s *Server) ReleaseTrackerIssueLease(ctx context.Context, req *connect.Request[pb.ReleaseTrackerIssueLeaseRequest]) (*connect.Response[pb.ReleaseTrackerIssueLeaseResponse], error) {
	r := req.Msg
	if _, err := validateTrackerIssueLease(r.GetIssueKey(), r.GetPurpose(), r.GetHolderId(), 0); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	released := s.trackerIssueLeases.release(r)
	if s.trackerIssueLeaseSignedIn(ctx) {
		request := connect.NewRequest(r)
		s.trackerIssueLeases.authorize(request.Header())
		out, err := s.trackerIssueLeases.upstream.ReleaseTrackerIssueLease(ctx, request)
		if err != nil {
			return nil, err
		}
		out.Msg.IsReleased = out.Msg.IsReleased || released
		return out, nil
	}
	return connect.NewResponse(&pb.ReleaseTrackerIssueLeaseResponse{IsReleased: released}), nil
}

func validateTrackerIssueLease(key string, purpose pb.TrackerIssueLeasePurpose, holder string, ttl int32) (time.Duration, error) {
	if len(key) == 0 || len(key) > 256 || strings.ContainsFunc(key, unicode.IsControl) {
		return 0, errors.New("issue_key must be 1–256 bytes without control characters")
	}
	if purpose != pb.TrackerIssueLeasePurpose_TRACKER_ISSUE_LEASE_PURPOSE_PLAN && purpose != pb.TrackerIssueLeasePurpose_TRACKER_ISSUE_LEASE_PURPOSE_BUILD {
		return 0, errors.New("purpose must be PLAN or BUILD")
	}
	if len(holder) != 32 || strings.ContainsFunc(holder, func(r rune) bool { return (r < '0' || r > '9') && (r < 'a' || r > 'f') }) {
		return 0, errors.New("holder_id must be 32 lowercase hex characters")
	}
	if ttl == 0 {
		ttl = 7200
	} else if ttl < 60 || ttl > 14400 {
		return 0, errors.New("ttl_seconds must be zero or 60–14400")
	}
	return time.Duration(ttl) * time.Second, nil
}
