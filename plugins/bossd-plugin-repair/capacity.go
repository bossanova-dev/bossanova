package main

import (
	"context"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

func (m *repairMonitor) capacityHoldersLocked(sessionID string, info sessionInfo) int {
	holders := make(map[string]struct{}, len(m.repairing))
	for id, active := range m.repairing {
		if active && id != sessionID {
			holders[id] = struct{}{}
		}
	}
	for _, s := range info.Sessions {
		id := s.GetId()
		if id == "" || id == sessionID || s.GetArchivedAt() != nil {
			continue
		}
		if s.GetRepairActive() || (s.GetHasActiveChat() && s.GetDisplayIsRepairing()) {
			holders[id] = struct{}{}
		}
	}
	return len(holders)
}

func (m *repairMonitor) deferForCapacityLocked(sessionID string, info sessionInfo) bool {
	capLimit := m.config.maxConcurrentRepairs()
	holders := m.capacityHoldersLocked(sessionID, info)
	if holders < capLimit {
		delete(m.capacityDeferrals, sessionID)
		delete(m.queued, sessionID)
		return false
	}
	if m.capacityDeferrals == nil {
		m.capacityDeferrals = make(map[string]bool)
	}
	if m.queued == nil {
		m.queued = make(map[string]bool)
	}
	event := m.logger.Debug()
	if !m.capacityDeferrals[sessionID] {
		event = m.logger.Info()
	}
	m.capacityDeferrals[sessionID] = true
	m.queued[sessionID] = true
	event.
		Str("session_id", sessionID).
		Int("holders", holders).
		Int("cap", capLimit).
		Msg("host repair capacity full, deferring repair")
	return true
}

func (m *repairMonitor) deferStartLocked(sessionID string, info sessionInfo) bool {
	if m.deferDependabotLocked(sessionID, info) {
		delete(m.capacityDeferrals, sessionID)
		delete(m.queued, sessionID)
		return true
	}
	return m.deferForCapacityLocked(sessionID, info)
}

func (m *repairMonitor) flushQueuedStatus(ctx context.Context, sessionID string) {
	m.mu.Lock()
	pending := m.queued[sessionID]
	delete(m.queued, sessionID)
	m.mu.Unlock()
	if !pending {
		return
	}
	if _, err := m.host.SetRepairStatus(ctx, &bossanovav1.SetRepairStatusRequest{
		SessionId: sessionID,
		IsQueued:  true,
	}); err != nil {
		m.logger.Warn().Err(err).Str("session_id", sessionID).Msg("failed to mark repair queued")
	}
}
