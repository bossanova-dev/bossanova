package main

import (
	"strings"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// The session read surfaces (`boss ls --json`, `boss show --json`) exist so a
// driver never has to scrape the ANSI-styled table the human rendering emits.
// The field names below are therefore a machine contract: rename one and every
// caller breaks. States are emitted as the trimmed enum *name* rather than the
// numeric value, so a caller is never coupled to proto field ordering — the
// symmetric inverse of the `--state` filter parsing in runLS.

// sessionListJSON is the `boss ls --json` envelope. Sessions is always a
// non-nil slice so an empty result marshals as `[]`, never `null`.
type sessionListJSON struct {
	Sessions []sessionJSON `json:"sessions"`
}

// sessionShowJSON is the `boss show --json` envelope.
type sessionShowJSON struct {
	Session sessionDetailJSON `json:"session"`
}

// sessionJSON is one row of `boss ls --json`: the fields the table shows plus
// the ones a driver needs that the table omits. Timestamps are RFC3339 (UTC),
// matching every other `--json` surface via rfc3339OrEmpty.
type sessionJSON struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// State is the enum name with its SESSION_STATE_ prefix trimmed
	// (e.g. "READY_FOR_REVIEW"), never the numeric value.
	State  string `json:"state"`
	RepoID string `json:"repo_id"`
	Agent  string `json:"agent"`
	Model  string `json:"model"`
	Effort string `json:"effort"`
	// PrNumber is null rather than 0 for a session with no PR, so a caller can
	// tell "no PR yet" from a hypothetical PR #0.
	PrNumber  *int32  `json:"pr_number"`
	PrURL     string  `json:"pr_url"`
	Branch    string  `json:"branch"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
	TrackerID *string `json:"tracker_id"`
	// LastAgentActivityAt is empty when no agent output/activity has been
	// observed, matching the absent timestamp rendering used by CreatedAt.
	LastAgentActivityAt string `json:"last_agent_activity_at"`
}

// sessionDetailJSON is `boss show --json`: an ls row plus the detail runShow
// renders. The chats table runShow prints below the header is deliberately
// absent — `boss chats` owns that shape, because it has to join in per-chat
// status. Account is carried as the raw id/label pair rather than the human
// line's resolved label, which substitutes a display string ("unmanaged local
// credentials") for the absent value a driver wants to see as empty.
type sessionDetailJSON struct {
	sessionJSON
	RepoDisplayName string `json:"repo_display_name"`
	BaseBranch      string `json:"base_branch"`
	WorktreePath    string `json:"worktree_path"`
	AccountID       string `json:"account_id"`
	AccountLabel    string `json:"account_label"`
	// DisplayStatus reuses displayStatusName so the JSON speaks the same
	// vocabulary as the TUI and `boss session checks` ("passing", "failing", …).
	DisplayStatus string `json:"display_status"`
	// LastCheckState is the ChecksOverall enum name with its prefix trimmed
	// ("PENDING" / "PASSED" / "FAILED"). UNSPECIFIED means the daemon has no
	// verdict demonstrated at the current head.
	LastCheckState         string `json:"last_check_state"`
	LastCheckStateObserved string `json:"last_check_state_observed"`
	LastCheckStateHeadSHA  string `json:"last_check_state_head_sha"`
	LastCheckStateAt       string `json:"last_check_state_at"`
	ArchivedAt             string `json:"archived_at"`
	SetupError             string `json:"setup_error"`
	// BlockedReason is the daemon's raw blocked reason, untruncated. It is a
	// pointer so null ("this session is not blocked") stays distinguishable from
	// "" — the same reason PrNumber is one.
	//
	// It is deliberately NOT on sessionJSON: `boss ls --json` is the widest
	// contract in this file, and a raw multi-line transport error on every row is
	// noise for the list use case. Detail is where triage happens.
	BlockedReason *string            `json:"blocked_reason"`
	LastRepair    *sessionRepairJSON `json:"last_repair"`
}

// sessionRepairJSON mirrors the "Last repair:" line runShow prints. It is nil
// (JSON null) when the repair plugin has never run for this session — the same
// last_repair_started_at sentinel the human rendering suppresses on.
type sessionRepairJSON struct {
	StartedAt    string `json:"started_at"`
	RunnerError  string `json:"runner_error"`
	ExitError    string `json:"exit_error"`
	AttemptCount int32  `json:"attempt_count"`
}

// newSessionRowJSON builds one `boss ls --json` row. The name is deliberately
// not newSessionJSON: that belongs to `boss new --json`, whose envelope is a
// different, much narrower shape.
func newSessionRowJSON(s *pb.Session) sessionJSON {
	row := sessionJSON{
		ID:                  s.GetId(),
		Title:               s.GetTitle(),
		State:               strings.TrimPrefix(s.GetState().String(), "SESSION_STATE_"),
		RepoID:              s.GetRepoId(),
		Agent:               s.GetAgentName(),
		Model:               s.GetEffectiveModel(),
		Effort:              s.GetEffectiveEffort(),
		PrURL:               s.GetPrUrl(),
		Branch:              s.GetBranchName(),
		CreatedAt:           rfc3339OrEmpty(s.GetCreatedAt()),
		UpdatedAt:           rfc3339OrEmpty(s.GetUpdatedAt()),
		LastAgentActivityAt: rfc3339OrEmpty(s.GetLastAgentActivityAt()),
	}
	if s.PrNumber != nil {
		pr := s.GetPrNumber()
		row.PrNumber = &pr
	}
	if s.TrackerId != nil {
		trackerID := s.GetTrackerId()
		row.TrackerID = &trackerID
	}
	return row
}

func newSessionDetailJSON(s *pb.Session) sessionDetailJSON {
	detail := sessionDetailJSON{
		sessionJSON:            newSessionRowJSON(s),
		RepoDisplayName:        s.GetRepoDisplayName(),
		BaseBranch:             s.GetBaseBranch(),
		WorktreePath:           s.GetWorktreePath(),
		AccountID:              s.GetAccountId(),
		AccountLabel:           s.GetAccountLabel(),
		DisplayStatus:          displayStatusName(s.GetDisplayStatus()),
		LastCheckState:         strings.TrimPrefix(s.GetLastCheckState().String(), "CHECKS_OVERALL_"),
		LastCheckStateObserved: strings.TrimPrefix(s.GetLastCheckStateObserved().String(), "CHECKS_OVERALL_"),
		LastCheckStateHeadSHA:  s.GetLastCheckStateHeadSha(),
		LastCheckStateAt:       rfc3339OrEmpty(s.GetLastCheckStateAt()),
		ArchivedAt:             rfc3339OrEmpty(s.GetArchivedAt()),
		SetupError:             s.GetSetupError(),
	}
	if reason := s.GetBlockedReason(); reason != "" {
		detail.BlockedReason = &reason
	}
	if s.GetLastRepairStartedAt() != nil || s.GetLastRepairRunnerError() != "" || s.GetLastRepairExitError() != "" {
		detail.LastRepair = &sessionRepairJSON{
			StartedAt:    rfc3339OrEmpty(s.GetLastRepairStartedAt()),
			RunnerError:  s.GetLastRepairRunnerError(),
			ExitError:    s.GetLastRepairExitError(),
			AttemptCount: s.GetLastRepairAttemptCount(),
		}
	}
	return detail
}
