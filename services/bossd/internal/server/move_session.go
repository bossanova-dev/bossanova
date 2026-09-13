package server

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
)

// MoveSession moves one session up or down relative to its current neighbours
// in the rendered session list (BOS-1230).
//
// The arithmetic lives here rather than in each client for the reason the plan
// gives: the TUI and the web app must not each re-derive a position from the
// same list, or they will disagree. The handler reads the effective order
// through loadSessionsForList — the same chokepoint ListSessions uses — so the
// neighbours it reasons about are exactly the ones the caller is looking at.
//
// A move at the boundary of the list, or one the ordering model cannot express,
// is a SUCCESSFUL no-op with is_moved=false rather than an error: a key held
// down must not start failing. An unknown session id is the one genuine error
// (NotFound).
func (s *Server) MoveSession(ctx context.Context, req *connect.Request[pb.MoveSessionRequest]) (*connect.Response[pb.MoveSessionResponse], error) {
	msg := req.Msg
	id := msg.GetId()
	if id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("id is required"))
	}
	var up bool
	switch msg.GetDirection() {
	case pb.MoveDirection_MOVE_DIRECTION_UP:
		up = true
	case pb.MoveDirection_MOVE_DIRECTION_DOWN:
		up = false
	default:
		// UNSPECIFIED is rejected rather than defaulted: guessing a direction
		// for a client that forgot to set one would move the wrong way half
		// the time, silently.
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("direction is required"))
	}

	// Unknown session id is NotFound, decided before any list work so the error
	// does not depend on the requested scope.
	if _, err := s.sessions.Get(ctx, id); err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session %s not found: %w", id, err))
	}

	rows, err := s.loadSessionsForList(ctx, msg.GetRepoId(), false, nil)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("load sessions for move: %w", err))
	}
	sessions := make([]*models.Session, 0, len(rows))
	for _, row := range rows {
		if row == nil || row.Session == nil {
			continue
		}
		sessions = append(sessions, row.Session)
	}
	ordered := db.SessionListRankRowsFrom(sessions)
	// The store already returns rendered order, but sorting here keeps the
	// handler correct against any caller that hands it an unsorted list and
	// pins the Go comparator and the SQL clause to the same rule.
	db.SortSessionListRankRows(ordered)

	index := -1
	for i, row := range ordered {
		if row.ID == id {
			index = i
			break
		}
	}
	if index < 0 {
		// The session exists but is not in the list being reordered (archived,
		// or a different repo than the requested scope). Reporting NotFound
		// rather than a silent no-op tells the caller its scope was wrong.
		return nil, connect.NewError(connect.CodeNotFound,
			fmt.Errorf("session %s is not in the session list for this scope", id))
	}

	move := db.ComputeListRankMove(ordered, index, up)
	if move.IsNoop() {
		sess, err := s.sessions.Get(ctx, id)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("reload session: %w", err))
		}
		return connect.NewResponse(&pb.MoveSessionResponse{
			Session: s.hydrateMovedSession(ctx, sess),
			IsMoved: false,
		}), nil
	}

	if _, err := s.sessions.SetListRanks(ctx, move.Ranks); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("write session list ranks: %w", err))
	}
	if move.IsRespaced {
		s.logger.Info().Str("session", id).Int("rows", len(move.Ranks)).
			Msg("session list rank gap exhausted; re-spaced the ranked block")
	}

	sess, err := s.sessions.Get(ctx, id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("reload moved session: %w", err))
	}
	p := s.hydrateMovedSession(ctx, sess)
	// Propagate to the cloud/web via the upstream stream, matching
	// UpdateSession/LinkSessionPR — otherwise the local list reorders and bosso
	// keeps the old order until the next full daemon snapshot.
	//
	// EVERY row the move wrote is published, not just the requested one. An
	// ordinary move writes exactly one row and those are the same thing, but a
	// re-space renumbers the whole ranked block: publishing only the requested
	// session would leave the read model holding the siblings' OLD ranks
	// alongside the moved row's new one, which is an order neither the daemon
	// nor the user ever asked for.
	if s.onSessionUpdated != nil {
		for _, siblingID := range sortedRankIDs(move.Ranks) {
			if siblingID == id {
				continue // published last, below, as the response payload
			}
			sibling, err := s.sessions.Get(ctx, siblingID)
			if err != nil {
				// The rank is already committed; a sibling that vanished
				// between the write and the read must not fail the move.
				s.logger.Warn().Err(err).Str("session", siblingID).
					Msg("re-spaced session could not be published upstream")
				continue
			}
			s.onSessionUpdated(ctx, s.hydrateMovedSession(ctx, sibling))
		}
		s.onSessionUpdated(ctx, p)
	}
	return connect.NewResponse(&pb.MoveSessionResponse{Session: p, IsMoved: true}), nil
}

// hydrateMovedSession projects a session for a MoveSession response, filling
// the denormalized repo fields the list view renders.
func (s *Server) hydrateMovedSession(ctx context.Context, sess *models.Session) *pb.Session {
	p := SessionToProto(sess)
	if repo, err := s.repos.Get(ctx, sess.RepoID); err == nil {
		p.RepoDisplayName = repo.DisplayName
		p.RepoOriginUrl = CanonicalRepoOriginURL(repo.OriginURL)
	}
	return p
}

// sortedRankIDs returns the session ids a move wrote, in a stable order so the
// upstream publishes are deterministic run to run.
func sortedRankIDs(ranks map[string]*int64) []string {
	ids := make([]string, 0, len(ranks))
	for id := range ranks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
