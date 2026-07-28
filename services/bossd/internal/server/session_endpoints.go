package server

import (
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/sessionports"
)

// SessionEndpointReader supplies verified, machine-local HTTP/HTTPS endpoints
// for a session's process tree. *sessionports.Tracker (BOS-472) satisfies it;
// the daemon wires one in cmd/main.go. It is the ONLY source of the transport-
// only Session.http_endpoints, and it is read at the very end of GetSession /
// ListSessions and nowhere else — SessionToProto never touches endpoints.
//
// Reads never block and never error: the tracker returns its cached (stale-
// while-refresh) shape, and a transient scan failure surfaces as an empty list,
// so hydration can never fail or delay the parent RPC.
type SessionEndpointReader interface {
	// Get returns the cached endpoints for one session, upserting it into the
	// tracked set.
	Get(snap sessionports.SessionSnapshot) []sessionports.Endpoint
	// GetBatch returns the cached endpoints for the given sessions, treating
	// snaps as the full current set.
	GetBatch(snaps []sessionports.SessionSnapshot) map[string][]sessionports.Endpoint
}

// sessionEndpointSnapshot builds the tracker input for a session: its identity
// anchor (id + canonical worktree path) plus every tmux name whose pane roots a
// process tree to inspect — the session's own tmux session and each chat's.
func sessionEndpointSnapshot(sess *models.Session, chats []*models.AgentChat) sessionports.SessionSnapshot {
	names := make([]string, 0, len(chats)+1)
	if sess.TmuxSessionName != nil && *sess.TmuxSessionName != "" {
		names = append(names, *sess.TmuxSessionName)
	}
	for _, ch := range chats {
		if ch != nil && ch.TmuxSessionName != nil && *ch.TmuxSessionName != "" {
			names = append(names, *ch.TmuxSessionName)
		}
	}
	return sessionports.SessionSnapshot{
		ID:           sess.ID,
		WorktreePath: sess.WorktreePath,
		TmuxNames:    names,
	}
}

// endpointsToProto maps verified tracker endpoints to their wire form. Ports
// outside the valid TCP range are dropped defensively (the tracker only emits
// 1..65535, so this also keeps the uint32 conversion in range for gosec G115).
func endpointsToProto(eps []sessionports.Endpoint) []*pb.HttpEndpoint {
	if len(eps) == 0 {
		return nil
	}
	out := make([]*pb.HttpEndpoint, 0, len(eps))
	for _, e := range eps {
		if e.Port < 0 || e.Port > 65535 {
			continue
		}
		out = append(out, &pb.HttpEndpoint{Port: uint32(e.Port), Url: e.URL})
	}
	return out
}

// hydrateSessionEndpoints fills p.HttpEndpoints for a single opted-in local read
// when every gate passes: an endpoint reader is wired, and the client's network
// namespace matches the daemon's (a no-op check off Linux; see
// platformEndpointNamespaceGate). Caller must already have confirmed the request
// opted in. Never blocks, never errors — a nil reader, a namespace mismatch, or
// an empty tracker result all leave p.HttpEndpoints empty.
func (s *Server) hydrateSessionEndpoints(chats []*models.AgentChat, p *pb.Session, sess *models.Session, clientNS string) {
	if s.endpointReader == nil || sess == nil {
		return
	}
	if !s.endpointNamespaceGate(clientNS) {
		return
	}
	snap := sessionEndpointSnapshot(sess, chats)
	p.HttpEndpoints = endpointsToProto(s.endpointReader.Get(snap))
}

// hydrateSessionEndpointsBatch is the ListSessions counterpart: one GetBatch
// call over every listed session. Same gates and same never-fail contract as
// hydrateSessionEndpoints.
func (s *Server) hydrateSessionEndpointsBatch(
	pbSessions []*pb.Session,
	sessByID map[string]*models.Session,
	chatsBySession map[string][]*models.AgentChat,
	clientNS string,
) {
	if s.endpointReader == nil {
		return
	}
	if !s.endpointNamespaceGate(clientNS) {
		return
	}
	snaps := make([]sessionports.SessionSnapshot, 0, len(pbSessions))
	for _, p := range pbSessions {
		sess := sessByID[p.Id]
		if sess == nil {
			continue
		}
		snaps = append(snaps, sessionEndpointSnapshot(sess, chatsBySession[p.Id]))
	}
	byID := s.endpointReader.GetBatch(snaps)
	for _, p := range pbSessions {
		if eps := byID[p.Id]; len(eps) > 0 {
			p.HttpEndpoints = endpointsToProto(eps)
		}
	}
}
