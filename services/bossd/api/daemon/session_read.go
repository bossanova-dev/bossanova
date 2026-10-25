package server

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/displaystatus"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/safego"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/status"
)

func (s *Server) GetSession(ctx context.Context, req *connect.Request[pb.GetSessionRequest]) (*connect.Response[pb.GetSessionResponse], error) {
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}

	session, err := s.sessions.Get(ctx, req.Msg.Id)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session not found: %w", err))
	}

	p := SessionToProto(session)

	// Hydrate attention status from session state and repo flags.
	repo, repoErr := s.repos.Get(ctx, session.RepoID)
	if repoErr == nil {
		p.RepoDisplayName = repo.DisplayName
		p.RepoOriginUrl = CanonicalRepoOriginURL(repo.OriginURL)
		p.RepoShouldArchiveSessionsAfterMerge = repo.ShouldArchiveSessionsAfterMerge
		p.RepoCanAutoRepair = repo.CanAutoRepair
	}

	// Hydrate PR display status from the in-memory tracker.
	var displayEntry *status.DisplayEntry
	if s.displayTracker != nil {
		displayEntry = s.displayTracker.Get(session.ID)
		HydrateDisplayEntry(p, displayEntry)
	}
	if repoErr == nil {
		p.AttentionStatus = baseAttentionStatus(session, repo, displayEntry)
	}
	// Hydrate recent rotation audit events before the auth-failed overlay below:
	// the overlay requires a corroborating auth-invalidation audit row and fails
	// toward NOT flagging when none exists.
	s.withRotationEvents(ctx, p, session)

	// Hydrate the liveness heartbeat + auth-failed attention overlay from the
	// session's chats (needs the status tracker, which convert.SessionToProto
	// has no access to). Loaded once here and reused by the BOS-473 endpoint
	// hydration below so an opted-in read does not re-query the same chats.
	var chats []*domain.AgentChat
	if s.agentChats != nil {
		if loaded, err := s.agentChats.ListBySession(ctx, session.ID); err == nil {
			chats = loaded
			HydrateAgentObservabilityWithAuthCorroboration(s.chatStatus, p, chats, s.authInvalidationCorroborated(ctx, p, chats))
			HydrateRestartProofActivity(ctx, p, chats, s.wedgeActivity())
		}
	}
	// The verify-park attention ranks below every attention above, so it only
	// fills what they left empty (BOS-1382).
	HydrateVerifyAttention(p, displayEntry)

	// repair_active is the authoritative, lease-backed single-repairer signal;
	// repair_stalled_at is its BOS-515 stall overlay (nil-safe when the lease
	// manager is not wired). Runs after the observability hydration above so the
	// stall heartbeat can be fed the session's last chat activity without a new
	// query — a stalled lease needs BOTH no head-SHA advance AND no chat output.
	s.repairLease.HydrateSession(p, session.ID, session.LastRepairHeadSHA, protoTime(p.GetLastAgentActivityAt()))

	// Re-source provider/account from the primary chat (BOS-381 authority).
	s.withPrimaryChatIdentity(ctx, p, session)
	// Resolve the non-secret account label for display (best-effort).
	s.withAccountLabel(ctx, p, session)
	// BOS-473: hydrate verified machine-local endpoints LAST, and only for an
	// opted-in local read that clears the daemon's locality gates. Every other
	// egress path leaves p.HttpEndpoints empty (SessionToProto never sets it).
	if req.Msg.GetIncludeLocalHttpEndpoints() && s.endpointReader != nil {
		s.hydrateSessionEndpoints(chats, p, session, req.Msg.GetClientNetworkNamespace())
	}

	s.hydrateAttention(ctx, p, attentionDeps{row: session, display: displayEntry, chats: chats})

	return connect.NewResponse(&pb.GetSessionResponse{Session: p}), nil
}

func (s *Server) ListSessions(ctx context.Context, req *connect.Request[pb.ListSessionsRequest]) (*connect.Response[pb.ListSessionsResponse], error) {
	msg := req.Msg
	repoID := msg.GetRepoId()

	rows, err := s.loadSessionsForList(ctx, repoID, msg.IncludeArchived, msg.States)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list sessions: %w", err))
	}

	sessionsNeedingPR := sessionRowsNeedingPRAssociation(rows)
	if s.prResolver != nil && len(sessionsNeedingPR) > 0 {
		updated, reconcileErr := s.reconcileListSessionPRAssociations(ctx, sessionsNeedingPR)
		if reconcileErr != nil {
			s.logger.Warn().Err(reconcileErr).Msg("ListSessions: reconcile PR associations")
		}
		if updated > 0 {
			rows, err = s.loadSessionsForList(ctx, repoID, msg.IncludeArchived, msg.States)
			if err != nil {
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list sessions: %w", err))
			}
		}
	}

	// Build repo lookup for denormalization and attention hydration, plus a
	// session-by-id index for the BOS-381 primary-chat re-source below (which runs
	// after the batch chat load to avoid an N+1 lookup).
	repoCache := make(map[string]*models.Repo)
	sessByID := make(map[string]*models.Session, len(rows))
	for _, row := range rows {
		if row == nil || row.Session == nil {
			continue
		}
		sess := row.Session
		sessByID[sess.ID] = sess
		if _, ok := repoCache[sess.RepoID]; !ok {
			if repo, err := s.repos.Get(ctx, sess.RepoID); err == nil {
				repoCache[sess.RepoID] = repo
			}
		}
	}

	pbSessions := make([]*pb.Session, 0, len(rows))
	for _, row := range rows {
		if row == nil || row.Session == nil {
			continue
		}
		sess := row.Session
		p := SessionToProto(sess)
		if repo, ok := repoCache[sess.RepoID]; ok {
			p.RepoDisplayName = repo.DisplayName
			p.RepoOriginUrl = CanonicalRepoOriginURL(repo.OriginURL)
			var display *status.DisplayEntry
			if s.displayTracker != nil {
				display = s.displayTracker.Get(sess.ID)
			}
			p.AttentionStatus = baseAttentionStatus(sess, repo, display)
		} else {
			p.RepoDisplayName = row.RepoDisplayName
			p.RepoOriginUrl = CanonicalRepoOriginURL(row.RepoOriginURL)
		}
		// Provider/account re-source (BOS-381) + account label + rotation events are
		// hydrated below, after the batch chat load, so the primary-chat re-source
		// reuses the already-loaded chats instead of an N+1 GetByAgentSessionID.
		pbSessions = append(pbSessions, p)
	}

	// Hydrate PR display statuses from the in-memory tracker.
	//
	// The composite display fields (DisplayLabel/Intent/Spinner) are persisted on
	// the session row, but the per-axis fields below (DisplayStatus,
	// DisplayHasFailures, DisplayHasChangesRequested, DisplayIsRepairing, and the
	// WorkflowDisplay* block hydrated next) are NOT in models.Session — only the
	// in-memory tracker knows them. Remaining typed-enum consumers: the TUI
	// (home.go/chatpicker.go/theme.go via GetDisplayStatus) and the repair plugin
	// (server.go via GetDisplayStatus + GetDisplayHasFailures).
	sessionIDs := make([]string, len(pbSessions))
	for i, sess := range pbSessions {
		sessionIDs[i] = sess.Id
	}
	var entries map[string]*status.DisplayEntry
	if s.displayTracker != nil {
		entries = s.displayTracker.GetBatch(sessionIDs)
		for i, sess := range pbSessions {
			if e, ok := entries[sess.Id]; ok {
				HydrateDisplayEntry(pbSessions[i], e)
			}
		}
	}
	for _, p := range pbSessions {
		suppressStaleConflictAttention(p)
	}

	// Workflow display status used to be hydrated from the workflows table,
	// which was populated by the autopilot plugin. With autopilot gone the
	// table has no writers, so the derivation has been removed. Phase 4 of
	// the autopilot removal will reintroduce a repair-driven derivation.

	chatsBySession, chatsLoaded := s.chatsBySessionForStatuses(ctx, sessionIDs)

	// BOS-381: re-source provider/account from each session's PRIMARY chat (the
	// runtime authority) using the already-loaded chats, then hydrate the account
	// label (reads the re-sourced account_id) and rotation events. Runs here so the
	// primary-chat re-source reuses the batch load above rather than an N+1 lookup.
	for _, p := range pbSessions {
		if primary := primaryChatFromSlice(chatsBySession[p.Id], p.GetAgentSessionId()); primary != nil {
			applyPrimaryChatIdentity(p, primary)
		}
		sess := sessByID[p.Id]
		// Resolve the non-secret account label for display (best-effort).
		s.withAccountLabel(ctx, p, sess)
		// Hydrate recent rotation audit events for the history block (best-effort).
		s.withRotationEvents(ctx, p, sess)
	}

	// Hydrate the liveness heartbeat + auth-failed attention overlay per session.
	// Runs after suppressStaleConflictAttention so the auth overlay sees the
	// final attention state (and only fills in where none exists). The whole
	// list shares one wait for the restart-proof reads it starts.
	wedge, wedgeCtx := s.wedgeActivity(), s.wedgeCache.ShareReadWait(ctx)
	for _, p := range pbSessions {
		chats := chatsBySession[p.Id]
		HydrateAgentObservabilityWithAuthCorroboration(s.chatStatus, p, chats, s.authInvalidationCorroborated(ctx, p, chats))
		HydrateRestartProofActivity(wedgeCtx, p, chats, wedge)
		// Lowest-ranked attention: applied after the agent overlay (BOS-1382).
		HydrateVerifyAttention(p, entries[p.Id])
	}

	// repair_active + its BOS-515 repair_stalled_at stall overlay. Deliberately
	// here rather than in the display-tracker loop above: the stall heartbeat
	// needs the last chat activity hydrated immediately above (a stalled lease
	// requires BOTH no head-SHA advance AND no chat output), and this loop also
	// covers sessions on a daemon with no display tracker wired.
	for _, p := range pbSessions {
		var headSHA string
		if sess := sessByID[p.Id]; sess != nil {
			headSHA = sess.LastRepairHeadSHA
		}
		s.repairLease.HydrateSession(p, p.Id, headSHA, protoTime(p.GetLastAgentActivityAt()))
	}

	for _, p := range pbSessions {
		_, hasDisplayEntry := entries[p.Id]
		chatStatus, _, ok, agg := s.chatStatusAndWaitingAggregate(ctx, chatsBySession[p.Id], chatsLoaded)
		if !ok || (!hasDisplayEntry && chatStatus == pb.ChatStatus_CHAT_STATUS_STOPPED) {
			continue
		}
		// This bypass leaves whatever the persisting producer last wrote,
		// including its BOS-1269 demotion decision — it does not clear the
		// composite, so the two producers stay consistent by not competing.
		if shouldKeepCheckingCompositeOverReview(p, chatStatus) {
			continue
		}
		in := displaystatus.Input{
			Session:             p,
			ChatStatus:          chatStatus,
			AllWaitingChatsIdle: agg.AllIdle(chatStatus),
			ParkCodes:           entries[p.Id].ParkCodes(),
		}
		out := displaystatus.Compute(in)
		p.DisplayLabel = out.Label
		p.DisplayIntent = out.Intent
		p.DisplaySpinner = out.Spinner
		// BOS-1269: stamp the transport-only mark from the cascade's own result
		// for this call. It must never be set where the label was not demoted —
		// the down-convert would then rewrite an ordinary green row into
		// "waiting", manufacturing a wait that never happened.
		p.IsWaitingDemoted = displaystatus.WasWaitingDemoted(in, out)
		p.IsReadyOverWaiting = displaystatus.WasReadyOverWaiting(in, out)
		// Transport-only like the two marks above: the session reads WORKING
		// only because armed callbacks no longer hide live work, so the
		// down-convert can restore the WAITING composite older pins saw.
		p.IsWorkingOverWaiting = agg.WorkingOverWaiting(chatStatus) && out.Label == displaystatus.WorkingLabel
	}

	// BOS-473: hydrate verified machine-local endpoints LAST, and only for an
	// opted-in local read that clears the daemon's locality gates. One GetBatch
	// over the listed sessions; every other egress path stays empty.
	if msg.GetIncludeLocalHttpEndpoints() && s.endpointReader != nil {
		s.hydrateSessionEndpointsBatch(pbSessions, sessByID, chatsBySession, msg.GetClientNetworkNamespace())
	}

	for _, p := range pbSessions {
		s.hydrateAttention(ctx, p, attentionDeps{row: sessByID[p.Id], display: entries[p.Id], chats: chatsBySession[p.Id]})
	}
	return connect.NewResponse(&pb.ListSessionsResponse{Sessions: pbSessions}), nil
}

func (s *Server) reconcileListSessionPRAssociations(ctx context.Context, sessions []*models.Session) (int64, error) {
	reconcileCtx, cancel := context.WithTimeout(ctx, listSessionsPRAssociationTimeout)
	defer cancel()

	// The result channel is buffered so the goroutine can always send and exit
	// even if we have already returned on the timeout below. safego.Go gives the
	// goroutine panic recovery so a misbehaving provider can't crash the daemon
	// on the ListSessions hot path.
	resultCh := make(chan listSessionsPRAssociationResult, 1)
	safego.Go(s.logger, func() {
		updated, err := s.prResolver.ReconcileSessions(reconcileCtx, sessions)
		resultCh <- listSessionsPRAssociationResult{updated: updated, err: err}
	})

	select {
	case result := <-resultCh:
		return result.updated, result.err
	case <-reconcileCtx.Done():
		return 0, reconcileCtx.Err()
	}
}

// loadSessionsForList loads the session rows for a ListSessions request and
// applies the optional state filter. It is called twice: once up front, and
// again after PR-association reconciliation when associations changed. Using
// the *WithRepo store methods means the rows carry denormalized repo fields,
// which back the display-name/origin fallback used when the repo cache misses.
func (s *Server) loadSessionsForList(ctx context.Context, repoID string, includeArchived bool, states []pb.SessionState) ([]*domain.SessionWithRepo, error) {
	var rows []*domain.SessionWithRepo
	var err error
	if includeArchived {
		rows, err = s.sessions.ListWithRepo(ctx, repoID)
	} else {
		rows, err = s.sessions.ListActiveWithRepo(ctx, repoID)
	}
	if err != nil {
		return nil, err
	}
	if len(states) > 0 {
		rows = filterSessionRowsByStates(rows, states)
	}
	return rows, nil
}

func filterSessionRowsByStates(rows []*domain.SessionWithRepo, states []pb.SessionState) []*domain.SessionWithRepo {
	stateSet := make(map[pb.SessionState]bool, len(states))
	for _, st := range states {
		stateSet[st] = true
	}

	filtered := make([]*domain.SessionWithRepo, 0, len(rows))
	for _, row := range rows {
		if row == nil || row.Session == nil {
			continue
		}
		if stateSet[pb.SessionState(clampInt32(int(row.State)))] {
			filtered = append(filtered, row)
		}
	}
	return filtered
}

func sessionRowsNeedingPRAssociation(rows []*domain.SessionWithRepo) []*models.Session {
	sessions := make([]*models.Session, 0, len(rows))
	for _, row := range rows {
		if row == nil || row.Session == nil {
			continue
		}
		sess := row.Session
		if session.NeedsPRAssociation(sess) {
			sessions = append(sessions, sess)
		}
	}
	return sessions
}

func shouldKeepCheckingCompositeOverReview(sess *pb.Session, chatStatus pb.ChatStatus) bool {
	if sess == nil {
		return false
	}
	if chatStatus == pb.ChatStatus_CHAT_STATUS_QUESTION ||
		chatStatus == pb.ChatStatus_CHAT_STATUS_WORKING ||
		chatStatus == pb.ChatStatus_CHAT_STATUS_LIMITED {
		// LIMITED ranks above the PR-derived "checking"/review labels in
		// displaystatus.Compute (branch 1b), so it must be allowed to win here
		// just like QUESTION/WORKING — otherwise ListSessions would keep
		// rendering "checking" while the Recompute path (no keep-checking guard)
		// renders "limited", making the two display paths disagree.
		return false
	}
	// Keep the visible list/status-view label consistent when a review-required
	// PR axis arrives while the persisted composite is still actively checking.
	return sess.GetDisplayStatus() == pb.DisplayStatus_DISPLAY_STATUS_REVIEW &&
		!sess.GetDisplayIsRepairing() &&
		sess.GetDisplayLabel() == "checking" &&
		sess.GetDisplaySpinner()
}
