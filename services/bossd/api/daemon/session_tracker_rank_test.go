package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/domain"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestSetSessionTrackerRank(t *testing.T) {
	for _, tc := range []struct {
		name   string
		id     string
		ticket string
		rank   *pb.TrackerRank
		code   connect.Code
	}{
		{name: "success", ticket: " bos-1539 ", rank: &pb.TrackerRank{Tier: 1, TierSource: "blocks BOS-2", DeadlineOn: rankString("2026-11-30"), DeadlineSource: "project", IsScheduled: true, UnblockedTicketCount: 3, RankedAt: timestamppb.New(time.Unix(1, 0))}},
		{name: "tier", ticket: "BOS-1539", rank: &pb.TrackerRank{Tier: 5}, code: connect.CodeInvalidArgument},
		{name: "deadline", ticket: "BOS-1539", rank: &pb.TrackerRank{DeadlineOn: rankString("2026-13-01")}, code: connect.CodeInvalidArgument},
		{name: "negative count", ticket: "BOS-1539", rank: &pb.TrackerRank{UnblockedTicketCount: -1}, code: connect.CodeInvalidArgument},
		{name: "missing rank", ticket: "BOS-1539", code: connect.CodeInvalidArgument},
		{name: "bad ticket", ticket: "bad!", rank: &pb.TrackerRank{}, code: connect.CodeInvalidArgument},
		{name: "unknown", id: "unknown", ticket: "BOS-1539", rank: &pb.TrackerRank{}, code: connect.CodeNotFound},
		{name: "mismatch", ticket: "BOS-1540", rank: &pb.TrackerRank{}, code: connect.CodeFailedPrecondition},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newSetTrackerHarness(t)
			ctx := context.Background()
			ticket := "BOS-1539"
			ptr := &ticket
			if _, err := h.sessions.Update(ctx, h.sessID, domain.UpdateSessionParams{TrackerID: &ptr}); err != nil {
				t.Fatal(err)
			}
			id := tc.id
			if id == "" {
				id = h.sessID
			}
			before := time.Now().Add(-time.Second)
			resp, err := h.srv.SetSessionTrackerRank(ctx, connect.NewRequest(&pb.SetSessionTrackerRankRequest{Id: id, TrackerId: tc.ticket, TrackerRank: tc.rank}))
			if tc.code != 0 {
				if connect.CodeOf(err) != tc.code {
					t.Fatalf("error=%v want %v", err, tc.code)
				}
				if tc.name == "mismatch" && (!strings.Contains(err.Error(), "BOS-1539") || !strings.Contains(err.Error(), "BOS-1540")) {
					t.Fatal(err)
				}
				stored, e := h.sessions.Get(ctx, h.sessID)
				if e != nil || stored.TrackerRank != nil || len(h.updated) != 0 {
					t.Fatalf("error changed session: %+v %v", stored, e)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				rank := resp.Msg.GetSession().GetTrackerRank()
				if rank == nil || rank.Tier != 1 || rank.TierSource != "blocks BOS-2" || rank.GetDeadlineOn() != "2026-11-30" || rank.DeadlineSource != "project" || !rank.IsScheduled || rank.UnblockedTicketCount != 3 || rank.RankedAt.AsTime().Before(before) || rank.RankedAt.AsTime().After(time.Now()) {
					t.Fatalf("rank=%+v", rank)
				}
				if len(h.updated) != 1 || h.updated[0].GetTrackerRank() == nil {
					t.Fatal("missing updated notification")
				}
			}
			if len(h.resolver.calls) != 0 {
				t.Fatal("rank invoked tracker resolver")
			}
		})
	}
}

func rankString(s string) *string { return &s }

func TestSetSessionTrackerRankGitHubRepository(t *testing.T) {
	for _, tc := range []struct {
		name, request, storedURL string
		wantError                bool
	}{
		{name: "matching repository", request: "https://github.com/acme/repo/issues/12", storedURL: "https://github.com/acme/repo/issues/12"},
		{name: "repository case", request: "https://github.com/ACME/REPO/issues/12", storedURL: "https://github.com/acme/repo/issues/12"},
		{name: "other repository", request: "https://github.com/other/repo/issues/12", storedURL: "https://github.com/acme/repo/issues/12", wantError: true},
		{name: "other host", request: "https://example.com/acme/repo/issues/12", storedURL: "https://github.com/acme/repo/issues/12", wantError: true},
		{name: "missing stored URL", request: "https://github.com/acme/repo/issues/12", wantError: true},
		{name: "unscoped id", request: "GH-12", storedURL: "https://github.com/acme/repo/issues/12"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newSetTrackerHarness(t)
			ctx := context.Background()
			ticket, url := rankString("GH-12"), rankString(tc.storedURL)
			if _, err := h.sessions.Update(ctx, h.sessID, domain.UpdateSessionParams{TrackerID: &ticket, TrackerURL: &url}); err != nil {
				t.Fatal(err)
			}
			_, err := h.srv.SetSessionTrackerRank(ctx, connect.NewRequest(&pb.SetSessionTrackerRankRequest{Id: h.sessID, TrackerId: tc.request, TrackerRank: &pb.TrackerRank{Tier: 1}}))
			if tc.wantError {
				if connect.CodeOf(err) != connect.CodeFailedPrecondition {
					t.Fatalf("error = %v", err)
				}
				stored, getErr := h.sessions.Get(ctx, h.sessID)
				if getErr != nil || stored.TrackerRank != nil || len(h.updated) != 0 {
					t.Fatalf("rejected repository changed session: %+v %v", stored, getErr)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if len(h.resolver.calls) != 0 {
				t.Fatal("rank invoked tracker resolver")
			}
		})
	}
}

func TestSessionProtoTrackerRank(t *testing.T) {
	h := newSetTrackerHarness(t)
	for _, deadline := range []*string{nil, rankString("2026-11-30")} {
		rank := &models.TrackerRank{Tier: 2, TierSource: "own", DeadlineOn: deadline, DeadlineSource: "project", IsScheduled: true, UnblockedTicketCount: 4, RankedAt: time.Now().UTC()}
		p := h.srv.sessionProtoWithRepo(context.Background(), &models.Session{TrackerRank: rank})
		got := p.GetTrackerRank()
		if got == nil || got.Tier != rank.Tier || got.TierSource != rank.TierSource || got.DeadlineSource != rank.DeadlineSource || got.IsScheduled != rank.IsScheduled || got.UnblockedTicketCount != rank.UnblockedTicketCount || !got.RankedAt.AsTime().Equal(rank.RankedAt) {
			t.Fatalf("rank=%+v", got)
		}
		if deadline == nil {
			if got.DeadlineOn != nil {
				t.Fatal("undated rank has deadline")
			}
		} else if got.GetDeadlineOn() != *deadline {
			t.Fatal("deadline missing")
		}
	}
	if h.srv.sessionProtoWithRepo(context.Background(), &models.Session{}).GetTrackerRank() != nil {
		t.Fatal("unranked session has rank")
	}
}
