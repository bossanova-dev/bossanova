package server

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/vcs"
)

// ListCheckSnapshots returns the daemon's per-poll record of what it
// saw for a session's CI checks. The daemon's DisplayPoller persists a
// snapshot every poll cycle; this RPC just hands the rows back to the
// CLI for `boss session checks <id>` rendering.
func (s *Server) ListCheckSnapshots(ctx context.Context, req *connect.Request[bossanovav1.ListCheckSnapshotsRequest]) (*connect.Response[bossanovav1.ListCheckSnapshotsResponse], error) {
	if req.Msg.GetSessionId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("session_id is required"))
	}
	if s.checkSnapshots == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("check snapshot store not configured"))
	}
	limit := int(req.Msg.GetLimit())
	if limit <= 0 {
		limit = 10
	}
	snaps, err := s.checkSnapshots.RecentBySession(ctx, req.Msg.GetSessionId(), limit)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list check snapshots: %w", err))
	}
	resp := &bossanovav1.ListCheckSnapshotsResponse{}
	for _, snap := range snaps {
		resp.Snapshots = append(resp.Snapshots, &bossanovav1.CheckSnapshot{
			PolledAt:       timestamppb.New(snap.PolledAt),
			HeadSha:        snap.HeadSHA,
			RawJson:        snap.RawJSON,
			ComputedStatus: bossanovav1.DisplayStatus(vcs.DisplayStatus(snap.ComputedStatus)),
		})
	}
	return connect.NewResponse(resp), nil
}
