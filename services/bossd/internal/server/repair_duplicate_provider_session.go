package server

import (
	"context"
	"fmt"
	"sort"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
)

// codexProviderSessionIDDuplicatesToClear detects codex chats within one
// session that share a provider_session_id (the BOS-290 collision) and returns
// the AgentSessionIDs whose id should be cleared so process-fd re-resolution
// runs on next wake. For each colliding group it KEEPS the earliest-created
// chat (tie → lexicographically-first AgentSessionID) and clears the rest.
//
// Only codex chats with a non-nil provider id participate: a claude chat's
// provider id is its agent id, which is always unique, so claude rows are left
// untouched.
func codexProviderSessionIDDuplicatesToClear(chats []*models.AgentChat) []string {
	groups := map[string][]*models.AgentChat{}
	for _, c := range chats {
		if c == nil || c.AgentName != "codex" || c.ProviderSessionID == nil || *c.ProviderSessionID == "" {
			continue
		}
		id := *c.ProviderSessionID
		groups[id] = append(groups[id], c)
	}

	var toClear []string
	for _, group := range groups {
		if len(group) < 2 {
			continue
		}
		// Keeper = earliest CreatedAt; tie → lexicographically-first
		// AgentSessionID (deterministic, independent of input order).
		sort.SliceStable(group, func(i, j int) bool {
			if !group[i].CreatedAt.Equal(group[j].CreatedAt) {
				return group[i].CreatedAt.Before(group[j].CreatedAt)
			}
			return group[i].AgentSessionID < group[j].AgentSessionID
		})
		for _, c := range group[1:] {
			toClear = append(toClear, c.AgentSessionID)
		}
	}
	return toClear
}

// repairDuplicateCodexProviderSessionIDs scans all active sessions for codex
// chats that collide on provider_session_id and clears the duplicates (keeping
// each group's earliest chat) so fd resolution rebinds them on next wake. It is
// self-healing and non-destructive: a cleared id is re-derivable from the
// rollout the chat's process holds open; nothing is lost. Returns the count of
// sessions inspected and the AgentSessionIDs cleared.
//
// Placement: this runs from RepairDoctor rather than a standalone RPC because
// RepairDoctor is the operator's "make the automation pipeline healthy again"
// entry point and already enumerates active sessions. The clear is logged.
func (s *Server) repairDuplicateCodexProviderSessionIDs(ctx context.Context) (sessionsScanned int, cleared []string, unreadable int, err error) {
	if s.repos == nil || s.sessions == nil || s.agentChats == nil {
		return 0, nil, 0, fmt.Errorf("session deps not configured")
	}
	repos, err := s.repos.List(ctx)
	if err != nil {
		return 0, nil, 0, fmt.Errorf("list repos: %w", err)
	}
	for _, repo := range repos {
		list, listErr := s.sessions.ListActive(ctx, repo.ID)
		if listErr != nil {
			// A repo whose sessions can't be listed may hide a collision; count
			// it so the check never reports a clean bill of health it can't back.
			unreadable++
			s.logger.Warn().Err(listErr).Str("repo_id", repo.ID).
				Msg("list active sessions failed during duplicate codex provider id scan")
			continue
		}
		for _, sess := range list {
			chats, chatsErr := s.agentChats.ListBySession(ctx, sess.ID)
			if chatsErr != nil {
				unreadable++
				s.logger.Warn().Err(chatsErr).Str("session_id", sess.ID).
					Msg("list chats failed during duplicate codex provider id scan")
				continue
			}
			sessionsScanned++
			for _, agentSessionID := range codexProviderSessionIDDuplicatesToClear(chats) {
				if clearErr := s.agentChats.UpdateProviderSessionID(ctx, agentSessionID, nil); clearErr != nil {
					s.logger.Warn().Err(clearErr).
						Str("agent_session_id", agentSessionID).
						Msg("failed to clear duplicate codex provider session id")
					continue
				}
				s.logger.Info().
					Str("agent_session_id", agentSessionID).
					Str("session_id", sess.ID).
					Msg("cleared duplicate codex provider session id (BOS-290); will re-resolve via process fd on next wake")
				cleared = append(cleared, agentSessionID)
			}
		}
	}
	return sessionsScanned, cleared, unreadable, nil
}

// duplicateCodexProviderSessionCheck runs the repair and reports what it did as
// a RepairDoctor check. Ok=true when nothing needed clearing and every session
// was readable; Ok=false surfaces the ids it repaired (so the operator sees the
// self-heal happened) or unreadable sessions (so a swallowed list error is
// never rendered as a clean bill of health).
func (s *Server) duplicateCodexProviderSessionCheck(ctx context.Context) *bossanovav1.RepairDoctorCheck {
	scanned, cleared, unreadable, err := s.repairDuplicateCodexProviderSessionIDs(ctx)
	if err != nil {
		return &bossanovav1.RepairDoctorCheck{
			Name:   "duplicate codex provider session ids",
			Ok:     false,
			Detail: fmt.Sprintf("could not scan: %v", err),
		}
	}
	unreadableNote := ""
	if unreadable > 0 {
		unreadableNote = fmt.Sprintf("; %d session(s)/repo(s) unreadable (not inspected)", unreadable)
	}
	if len(cleared) == 0 {
		return &bossanovav1.RepairDoctorCheck{
			Name:   "duplicate codex provider session ids",
			Ok:     unreadable == 0,
			Detail: fmt.Sprintf("scanned %d active session(s); no colliding codex chats%s", scanned, unreadableNote),
		}
	}
	return &bossanovav1.RepairDoctorCheck{
		Name: "duplicate codex provider session ids",
		Ok:   false,
		Detail: fmt.Sprintf("cleared %d duplicate codex provider session id(s) across %d session(s) — they re-resolve via process fd on next wake: %v%s",
			len(cleared), scanned, cleared, unreadableNote),
	}
}
