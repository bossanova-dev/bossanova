package main

import (
	"context"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

const repairStartBlockReprobeAfter = 15 * time.Minute

func (m *repairMonitor) releaseStartBlock(ctx context.Context, sessionID string, info sessionInfo, displayStatus bossanovav1.DisplayStatus) bool {
	leg := ""
	base := ""
	if info.StartBlock.HeadMoved(info.HeadSHA) {
		leg = "head-moved"
	} else {
		if info.StartBlock.BaseSHA != "" {
			base, _ = m.currentBaseSHA(ctx, info, displayStatus)
		}
		if info.StartBlock.BaseMoved(base) {
			leg = "base-moved"
		} else if !info.LastRepairBlockedAt.IsZero() && time.Since(info.LastRepairBlockedAt) >= repairStartBlockReprobeAfter {
			leg = "contention-reprobe"
		}
	}
	if leg == "" {
		return false
	}
	if _, err := m.host.FireSessionEvent(ctx, &bossanovav1.FireSessionEventRequest{SessionId: sessionID, Event: bossanovav1.SessionEvent_SESSION_EVENT_UNBLOCK}); err != nil {
		m.mu.Lock()
		m.cooldowns[sessionID] = time.Now()
		m.mu.Unlock()
		m.logger.Warn().Err(err).Str("session_id", sessionID).Str("leg", leg).Msg("could not release repair start block")
		return false
	}
	m.clearRefusals(sessionID)
	if leg == "contention-reprobe" {
		m.mu.Lock()
		m.refusals[sessionID] = refusalStreak{headSHA: info.HeadSHA, first: info.LastRepairBlockedAt, count: maxRepairLoopAttempts, escalated: true}
		m.mu.Unlock()
	}
	m.logger.Info().Str("session_id", sessionID).Str("leg", leg).Str("head", info.HeadSHA).Str("base", base).Msg("released repair start block")
	return true
}

func (m *repairMonitor) evaluateRepairSession(ctx context.Context, sessionID string, displayStatus bossanovav1.DisplayStatus) sessionInfo {
	info := m.lookupSession(ctx, sessionID, displayStatus)
	if info.StartBlock != nil && m.releaseStartBlock(ctx, sessionID, info, displayStatus) {
		info = m.lookupSession(ctx, sessionID, displayStatus)
	} else if info.Repairable {
		m.clearReleasedStartBlock(sessionID)
	}
	if !info.Repairable && info.StartBlock == nil {
		m.clearRefusals(sessionID)
	}
	return info
}

func (m *repairMonitor) clearReleasedStartBlock(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.refusals[sessionID].blocked {
		delete(m.refusals, sessionID)
	}
}

func (m *repairMonitor) markRefusalBlocked(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	streak := m.refusals[sessionID]
	streak.blocked = true
	m.refusals[sessionID] = streak
}
