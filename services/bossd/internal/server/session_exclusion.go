package server

import (
	"context"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/session"
)

// activeSessionKeys holds the identifying keys of every active (non-archived)
// session for a repo. The New Session pickers use it to hide PRs and tracker
// issues that already have a session in progress, so a user can't start a
// second session for work that's already underway.
//
//	session ──┬─ PRNumber ───▶ prNumbers
//	          ├─ BranchName ─▶ branches
//	          └─ TrackerID ──▶ trackerIDs
//
// A PR is "taken" if its number OR head branch is present. A tracker issue is
// "taken" if its tracker id, its plugin-matched PR number, or that PR's branch
// is present.
type activeSessionKeys struct {
	prNumbers  map[int]string    // pr number  → session id
	branches   map[string]string // branch     → session id
	trackerIDs map[string]string // tracker id → session id
}

// newActiveSessionKeys indexes a session list by the three keys we can match a
// PR or tracker issue against. Each session contributes every key it has;
// nil sessions and empty/nil fields are skipped.
func newActiveSessionKeys(sessions []*models.Session) activeSessionKeys {
	keys := activeSessionKeys{
		prNumbers:  make(map[int]string),
		branches:   make(map[string]string),
		trackerIDs: make(map[string]string),
	}
	for _, sess := range sessions {
		if sess == nil {
			continue
		}
		if !session.StateBlocksDuplicateTarget(sess.State) {
			continue
		}
		if sess.PRNumber != nil {
			keys.prNumbers[*sess.PRNumber] = sess.ID
		}
		if sess.BranchName != "" {
			keys.branches[sess.BranchName] = sess.ID
		}
		if sess.TrackerID != nil && *sess.TrackerID != "" {
			keys.trackerIDs[*sess.TrackerID] = sess.ID
		}
	}
	return keys
}

// excludePRs returns the PRs that do NOT already have an active session,
// matched by PR number or head branch.
func (k activeSessionKeys) excludePRs(prs []vcs.PRSummary) []vcs.PRSummary {
	out := make([]vcs.PRSummary, 0, len(prs))
	for _, pr := range prs {
		if _, taken := k.prNumbers[pr.Number]; taken {
			continue
		}
		if _, taken := k.branches[pr.HeadBranch]; taken {
			continue
		}
		out = append(out, pr)
	}
	return out
}

// excludeIssues returns the tracker issues that do NOT already have an active
// session. An issue is matched by its tracker id, by the PR the tracker plugin
// associated with it (pr_number), or by that PR's branch (existing_branch) —
// the latter two catch sessions created before tracker_id was recorded.
func (k activeSessionKeys) excludeIssues(issues []*pb.TrackerIssue) []*pb.TrackerIssue {
	out := make([]*pb.TrackerIssue, 0, len(issues))
	for _, iss := range issues {
		if iss == nil {
			continue
		}
		if _, taken := k.trackerIDs[iss.ExternalId]; taken {
			continue
		}
		if iss.PrNumber > 0 {
			if _, taken := k.prNumbers[int(iss.PrNumber)]; taken {
				continue
			}
		}
		if iss.ExistingBranch != "" {
			if _, taken := k.branches[iss.ExistingBranch]; taken {
				continue
			}
		}
		if iss.BranchName != "" {
			if _, taken := k.branches[iss.BranchName]; taken {
				continue
			}
		}
		out = append(out, iss)
	}
	return out
}

// duplicateSessionID reports the id of an active (StateBlocksDuplicateTarget,
// non-archived) session that already owns this create request's target,
// matched in priority order: tracker id, then PR number, then branch (the
// legacy tracker-id-absent cases). Empty/nil signals are skipped so a create
// with no tracker/PR/branch never false-hits. The returned kind names the
// dimension that matched ("tracker issue", "PR", or "branch") so callers can
// report an honest error rather than always blaming the tracker issue.
func (k activeSessionKeys) duplicateSessionID(trackerID *string, prNumber *int, branch string) (id, kind string, ok bool) {
	if trackerID != nil && *trackerID != "" {
		if id, ok := k.trackerIDs[*trackerID]; ok {
			return id, "tracker issue", true
		}
	}
	if prNumber != nil {
		if id, ok := k.prNumbers[*prNumber]; ok {
			return id, "PR", true
		}
	}
	if branch != "" {
		if id, ok := k.branches[branch]; ok {
			return id, "branch", true
		}
	}
	return "", "", false
}

// activeSessionKeysForRepo loads the active sessions for a repo and indexes
// them. A session-store read error is returned to the caller, which surfaces
// it as an internal RPC error rather than silently showing already-taken work.
func (s *Server) activeSessionKeysForRepo(ctx context.Context, repoID string) (activeSessionKeys, error) {
	sessions, err := s.sessions.ListActive(ctx, repoID)
	if err != nil {
		return activeSessionKeys{}, err
	}
	return newActiveSessionKeys(sessions), nil
}
