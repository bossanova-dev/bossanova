package main

import (
	"strings"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func isDependabotBranch(branch string) bool { return strings.HasPrefix(branch, "dependabot/") }

// dependabotHolderLocked includes reservations made after the snapshot was read.
// A persisted repair flag without a live chat cannot pin a repository forever.
func (m *repairMonitor) dependabotHolderLocked(sessionID string, info sessionInfo) string {
	if !isDependabotBranch(info.BranchName) {
		return ""
	}
	for _, s := range info.Sessions {
		if s.GetId() == sessionID || s.GetRepoId() != info.RepoID || s.GetArchivedAt() != nil || !isDependabotBranch(s.GetBranchName()) {
			continue
		}
		if m.repairing[s.GetId()] || s.GetHasActiveChat() && (s.GetDisplayIsRepairing() || s.GetState() == pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN) {
			return s.GetId()
		}
	}
	return ""
}

func (m *repairMonitor) deferDependabotLocked(sessionID string, info sessionInfo) bool {
	holder := m.dependabotHolderLocked(sessionID, info)
	if holder == "" {
		return false
	}
	if m.dependabotDeferrals == nil {
		m.dependabotDeferrals = make(map[string]bool)
	}
	pair := sessionID + "\x00" + holder
	event := m.logger.Debug()
	if !m.dependabotDeferrals[pair] {
		event = m.logger.Info()
	}
	m.dependabotDeferrals[pair] = true
	event.Str("session_id", sessionID).Str("holder", holder).Msg("Dependabot repo busy, deferring repair")
	return true
}
