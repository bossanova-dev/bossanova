package main

import (
	"context"
	"time"

	"github.com/recurser/bossd/internal/domain"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog"

	server "github.com/recurser/bossd/api/daemon"
	"github.com/recurser/bossd/api/upstream"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/status"
)

// sessionListerAdapter adapts SessionStore to upstream.SessionLister.
type sessionListerAdapter struct {
	sessions db.SessionStore
}

// ListSessions returns every session (active and archived) as protobuf,
// populated with each session's repo display name via a single JOIN query.
// Archived sessions are included so the orchestrator sees the archive
// transition — filtering to active only would make an archived session
// look indistinguishable from a deleted one at the receiver.
func (a *sessionListerAdapter) ListSessions(ctx context.Context) ([]*bossanovav1.Session, error) {
	rows, err := a.sessions.ListWithRepo(ctx, "")
	if err != nil {
		return nil, err
	}
	pbSessions := make([]*bossanovav1.Session, 0, len(rows))
	for _, r := range rows {
		pbSess := server.SessionToProto(r.Session)
		pbSess.RepoDisplayName = r.RepoDisplayName
		pbSess.RepoOriginUrl = server.CanonicalRepoOriginURL(r.RepoOriginURL)
		pbSessions = append(pbSessions, pbSess)
	}
	return pbSessions, nil
}

// protoSessionListerFunc adapts a bare function to upstream.ProtoSessionLister
// (the snapshot reader's input) so the snapshot path can post-process the
// projected sessions — applying the reverse-stream observability overlay —
// without a dedicated struct.
type protoSessionListerFunc func(ctx context.Context) ([]*bossanovav1.Session, error)

func (f protoSessionListerFunc) ListSessions(ctx context.Context) ([]*bossanovav1.Session, error) {
	return f(ctx)
}

type streamAccountLabeler interface {
	Label(context.Context, string) (string, error)
}

type usageSnapshotProber interface {
	ProbeUsageSnapshot(context.Context, string) (domain.UsageSnapshot, error)
}
type streamSessionHydrator struct {
	agentChats        db.AgentChatStore
	rawSessions       db.SessionStore
	repos             db.RepoStore
	chatStatusTracker *status.Tracker
	displayTracker    *status.DisplayTracker
	rotationEvents    db.RotationEventStore
	// wedgeActivity makes last_agent_activity_at restart-proof, as the local
	// GetSession/ListSessions RPCs do.
	wedgeActivity  server.WedgeActivity
	accountLabeler streamAccountLabeler
	attention      func(context.Context, *bossanovav1.Session)
	logger         zerolog.Logger
}

func (h *streamSessionHydrator) Hydrate(ctx context.Context, pbSess *bossanovav1.Session) {
	if h == nil || pbSess == nil {
		return
	}
	// Re-run the shared attention resolver after every legacy hydration path.
	// Register first so the deferred verify overlay below runs before it.
	defer func() {
		if h.attention != nil {
			h.attention(ctx, pbSess)
		}
	}()
	// The verify-park attention (BOS-1382) is the lowest-ranked attention, so it
	// is applied last — after the base attention and the agent-observability
	// overlay — on EVERY return path, including the no-chats early returns
	// below. displayEntry is filled by the display-tracker hydration.
	var displayEntry *status.DisplayEntry
	defer func() { server.HydrateVerifyAttention(pbSess, displayEntry) }()
	// Compute base attention before the auth overlay, and hydrate the
	// stream-only fields that local GetSession/ListSessions also provide.
	if h.rawSessions != nil {
		if row, err := h.rawSessions.Get(ctx, pbSess.Id); err == nil && row != nil {
			if h.repos != nil {
				if repo, err := h.repos.Get(ctx, row.RepoID); err == nil && repo != nil {
					var display *status.DisplayEntry
					if h.displayTracker != nil {
						display = h.displayTracker.Get(pbSess.Id)
					}
					server.HydrateBaseAttention(pbSess, row, repo, display)
				}
			}
			if h.accountLabeler != nil {
				accountID := ""
				if row.AccountID != nil {
					accountID = *row.AccountID
				}
				label, _ := h.accountLabeler.Label(ctx, accountID)
				pbSess.AccountLabel = &label
			}
		}
	}
	// Stamp the per-axis display-tracker fields (DisplayStatus, PR mergeability,
	// merge-block, transient merge/setup flags) that live only in the in-memory
	// tracker — the same fields the local GetSession/ListSessions RPCs add. The
	// cloud/web read model is fed solely by the reverse stream, so without this
	// the web sees DisplayStatus=UNSPECIFIED and anything gating on PASSING (e.g.
	// the web Merge button) never lights up. Applied on every delta because bosso
	// treats deltas as full replacements.
	if h.displayTracker != nil {
		displayEntry = h.displayTracker.Get(pbSess.Id)
		server.HydrateDisplayEntry(pbSess, displayEntry)
	}
	server.HydrateRotationEvents(ctx, h.rotationEvents, h.logger, pbSess, pbSess.Id)
	if h.agentChats == nil {
		return
	}
	chats, err := h.agentChats.ListBySession(ctx, pbSess.Id)
	if err != nil {
		return
	}
	server.HydrateAgentObservabilityWithAuthCorroboration(
		h.chatStatusTracker,
		pbSess,
		chats,
		server.AuthInvalidationCorroboratedFromStore(ctx, h.rotationEvents, h.chatStatusTracker, h.logger, pbSess, chats),
	)
	server.HydrateRestartProofActivity(ctx, pbSess, chats, h.wedgeActivity)
}

// publishAgentMarkerSessionDelta re-hydrates the session owning agentSessionID
// and publishes it as a SessionDelta{UPDATED}. It is marker-agnostic — the
// hydrator re-runs HydrateAgentObservability, which recomputes every agent
// overlay (auth-failed and, since BOS-667, agent-stalled) from the tracker — so
// both the auth and stalled change hooks share it.
func publishAgentMarkerSessionDelta(
	ctx context.Context,
	agentSessionID string,
	hydrator *streamSessionHydrator,
	streamBus *upstream.StreamBus,
	logger zerolog.Logger,
) {
	if hydrator == nil || hydrator.agentChats == nil || hydrator.rawSessions == nil || streamBus == nil {
		return
	}
	chat, err := hydrator.agentChats.GetByAgentSessionID(ctx, agentSessionID)
	if err != nil || chat == nil {
		return
	}
	row, err := hydrator.rawSessions.Get(ctx, chat.SessionID)
	if err != nil {
		logger.Debug().Err(err).Str("session_id", chat.SessionID).Msg("agent-marker change: session lookup failed")
		return
	}
	pbSess := server.SessionToProto(row)
	if row.RepoID != "" && hydrator.repos != nil {
		if r, err := hydrator.repos.Get(ctx, row.RepoID); err == nil && r != nil {
			pbSess.RepoDisplayName = r.DisplayName
			pbSess.RepoOriginUrl = server.CanonicalRepoOriginURL(r.OriginURL)
		}
	}
	hydrator.Hydrate(ctx, pbSess)
	streamBus.Publish(upstream.StreamEvent{
		Session: &upstream.SessionEvent{
			Kind:    bossanovav1.SessionDelta_KIND_UPDATED,
			Session: pbSess,
		},
	})
}

type repoPRSessionStore interface {
	ListByRepoAndPR(ctx context.Context, repoID string, prNumber int) ([]*db.SessionWithRepo, error)
}

type displayPollerSessionLookup struct {
	sessions db.SessionStore
	repos    db.RepoStore
}

func (a *displayPollerSessionLookup) SessionsForPR(ctx context.Context, repoOriginURL string, prNumber int) ([]session.ForPR, error) {
	repo, err := a.repos.GetByOriginURL(ctx, repoOriginURL)
	if err != nil || repo == nil {
		return nil, err
	}

	var rows []*db.SessionWithRepo
	if lister, ok := a.sessions.(repoPRSessionStore); ok {
		rows, err = lister.ListByRepoAndPR(ctx, repo.ID, prNumber)
	} else {
		rows, err = a.sessions.ListWithRepo(ctx, repo.ID)
		if err == nil {
			rows = filterSessionsByPR(rows, prNumber)
		}
	}
	if err != nil {
		return nil, err
	}

	out := make([]session.ForPR, 0, len(rows))
	for _, row := range rows {
		out = append(out, session.ForPR{ID: row.ID})
	}
	return out, nil
}

func filterSessionsByPR(rows []*db.SessionWithRepo, prNumber int) []*db.SessionWithRepo {
	out := make([]*db.SessionWithRepo, 0, len(rows))
	for _, row := range rows {
		if row.PRNumber == nil || *row.PRNumber != prNumber {
			continue
		}
		out = append(out, row)
	}
	return out
}

// snapshotFallbackEnabled reports whether the unary PublishDaemonSnapshot
// publisher should run in break-glass FULL-FALLBACK mode — i.e. as the sole
// feed for transports that cannot carry the DaemonStream bidi stream. In this
// mode it publishes aggressively and reclaims idle connections. It is opt-in.
func snapshotFallbackEnabled(getenv func(string) string) bool {
	return getenv("BOSSD_SNAPSHOT_FALLBACK") == "true"
}

// snapshotReconcileDisabled reports whether the steady-state read-model
// reconciliation publisher is turned off. It runs by default (alongside the
// stream) as a safety net against drift from lost/never-published deltas; set
// BOSSD_SNAPSHOT_RECONCILE=false to disable.
func snapshotReconcileDisabled(getenv func(string) string) bool {
	return getenv("BOSSD_SNAPSHOT_RECONCILE") == "false"
}

const (
	// steadyStateSnapshotInterval is how often the read model is reconciled
	// against the daemon's authoritative session set while the bidi stream is
	// the primary feed. Long enough to be cheap, short enough that phantom
	// rows self-heal promptly.
	steadyStateSnapshotInterval = 5 * time.Minute
	// snapshotFallbackInterval is the aggressive cadence used only in
	// break-glass full-fallback mode, where the publisher is the sole feed.
	snapshotFallbackInterval = 30 * time.Second
)
