package server

import (
	"context"
	"fmt"
	"time"

	"github.com/recurser/bossd/internal/domain"
)

func chatResumeSessionIDForServer(chat *domain.AgentChat) string {
	if chat.ProviderSessionID != nil && *chat.ProviderSessionID != "" {
		return *chat.ProviderSessionID
	}
	return chat.AgentSessionID
}

// panePIDForChat resolves the live pane pid of a chat's tmux session, or 0 when
// there is no pane to ask about. Best-effort by design: pane pid 0 is the signal
// that fd resolution is unavailable and the time-window scan should run, which
// is exactly right for a chat whose process is gone.
func panePIDForChat(ctx context.Context, tmuxer tmuxSpawner, tmuxName string) int {
	if tmuxer == nil || tmuxName == "" {
		return 0
	}
	pid, err := tmuxer.PanePID(ctx, tmuxName)
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// backfillCodexProviderSessionID binds a codex chat that has no provider id yet.
//
// panePID is the chat's LIVE pane pid where one exists, and 0 otherwise. It used
// to be hard-coded to 0, which — combined with the allowLegacyBackfill flag
// below — is precisely the request shape the codex plugin's fd branch declines.
// So a chat with a live codex process holding its rollout open could never be
// bound by the one mechanism that can read its id; the whole-corpus time-window
// scan was the only thing ever tried (BOS-1298). Passing the real pid re-enables
// fd resolution on the case most likely to succeed: the process is alive and has
// long since opened its rollout. allowLegacyBackfill stays true because the scan
// remains the fallback for chats with no live pane.
func (s *Server) backfillCodexProviderSessionID(ctx context.Context, chat *domain.AgentChat, worktreePath string, panePID int, resolver interactiveSessionResolver) (bool, string, error) {
	if chat == nil || chat.AgentName != "codex" || chat.ProviderSessionID != nil || chat.CreatedAt.IsZero() || resolver == nil {
		return false, "", nil
	}
	legacyLaunchedAfter := chat.CreatedAt.Add(-5 * time.Minute)
	resolution, err := resolver.ResolveInteractiveSessionID(ctx, chat.AgentName, worktreePath, chat.AgentSessionID, legacyLaunchedAfter, chat.CreatedAt, true, panePID)
	if err != nil {
		return false, "", err
	}
	if resolution.SessionID == "" {
		if resolution.Ambiguous {
			return false, resolution.Reason, nil
		}
		return false, "", nil
	}
	providerID := resolution.SessionID
	// Fresh context for the persist: ctx here is the scan budget, which the
	// resolution above may have all but spent. Dropping a found id because the
	// budget that bounded the search also bounded the write is exactly the
	// failure this avoids.
	persistCtx, cancelPersist := context.WithTimeout(context.WithoutCancel(ctx), providerSessionIDBackfillPersistTimeout)
	defer cancelPersist()
	if err := s.agentChats.UpdateProviderSessionID(persistCtx, chat.AgentSessionID, &providerID); err != nil {
		return false, "", fmt.Errorf("persist provider session id: %w", err)
	}
	chat.ProviderSessionID = &providerID
	return true, "", nil
}

func (s *Server) discoverProviderSessionIDInBackground(chat *domain.AgentChat, worktreePath, tmuxName string, launchedAt time.Time, resolver interactiveSessionResolver, tmuxer tmuxSpawner) {
	if chat == nil || resolver == nil || launchedAt.IsZero() {
		return
	}
	agentSessionID := chat.AgentSessionID
	agentName := chat.AgentName
	if agentName == "" {
		agentName = defaultLegacyAgent
	}
	timeout := providerSessionIDBackgroundDiscoveryTimeout
	interval := providerSessionIDBackgroundDiscoveryPollInterval
	if timeout <= 0 || interval <= 0 {
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		// The foreground fd resolution missed only because codex was slow to
		// open its rollout — the pane and its process tree still exist. So keep
		// resolving with THIS chat's pane pid: fd binding stays authoritative
		// across the full background window, which is exactly the slow-startup
		// case most prone to the sibling-collision race (BOS-290). A tmux
		// failure or empty name leaves panePID 0 and falls back to the
		// time-window scan, as before.
		panePID := panePIDForChat(ctx, tmuxer, tmuxName)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		// lastReason carries the plugin's own account of the most recent miss
		// ("no rollout fd open yet" vs "no matching rollout found") onto the
		// timeout log below. Without it a timed-out discovery is indistinguishable
		// from a chat the resolver never had an opinion about (BOS-1144).
		lastReason := ""
		for {
			resolution, err := resolver.ResolveInteractiveSessionID(ctx, agentName, worktreePath, agentSessionID, launchedAt, time.Time{}, false, panePID)
			if err != nil {
				if ctx.Err() == nil {
					s.logger.Warn().Err(err).
						Str("agent_session_id", agentSessionID).
						Str("agent", agentName).
						Msg("background provider session id discovery failed")
				}
				return
			}
			if resolution.SessionID != "" {
				// Never overwrite a non-nil id that another path (foreground fd
				// resolution, wake, backfill) may have bound while this
				// goroutine polled — the same BOS-290 invariant the synchronous
				// write sites enforce. Re-read current state from the store
				// rather than the captured chat pointer, whose ProviderSessionID
				// is mutated by the spawn path under no shared lock (data race).
				if current, getErr := s.agentChats.GetByAgentSessionID(ctx, agentSessionID); getErr == nil &&
					current != nil && current.ProviderSessionID != nil && *current.ProviderSessionID != "" {
					return
				}
				providerID := resolution.SessionID
				if err := s.agentChats.UpdateProviderSessionID(ctx, agentSessionID, &providerID); err != nil {
					s.logger.Warn().Err(err).
						Str("agent_session_id", agentSessionID).
						Str("provider_session_id", providerID).
						Msg("failed to persist background provider session id")
				}
				return
			}
			if resolution.Ambiguous {
				s.logger.Warn().
					Str("agent_session_id", agentSessionID).
					Str("agent", agentName).
					Str("reason", resolution.Reason).
					Msg("background provider session id discovery ambiguous")
				return
			}

			if resolution.Reason != "" {
				lastReason = resolution.Reason
			}

			select {
			case <-ctx.Done():
				s.logger.Warn().
					Str("agent_session_id", agentSessionID).
					Str("agent", agentName).
					Str("reason", lastReason).
					Msg("background provider session id discovery timed out")
				return
			case <-ticker.C:
			}
		}
	}()
}
