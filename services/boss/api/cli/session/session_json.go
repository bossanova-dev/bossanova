package session

import (
	"strings"

	"github.com/recurser/boss/api/cli/cliutil"

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
	Sessions            []sessionJSON                 `json:"sessions"`
	FailedOrganizations []organizationReadFailureJSON `json:"failed_organizations,omitempty"`
}

type organizationReadFailureJSON struct {
	OrganizationID string `json:"organization_id"`
	Reason         string `json:"reason"`
}

// sessionShowJSON is the `boss show --json` envelope.
type sessionShowJSON struct {
	Session sessionDetailJSON `json:"session"`
}

// sessionJSON is one row of `boss ls --json`: the fields the table shows plus
// the ones a driver needs that the table omits. Timestamps are RFC3339 (UTC),
// matching every other `--json` surface via cliutil.Rfc3339OrEmpty.
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
	PrNumber    *int32             `json:"pr_number"`
	PrURL       string             `json:"pr_url"`
	Branch      string             `json:"branch"`
	CreatedAt   string             `json:"created_at"`
	UpdatedAt   string             `json:"updated_at"`
	TrackerID   *string            `json:"tracker_id"`
	TrackerRank *trackerRankJSON   `json:"trackerRank,omitempty"`
	Labels      []sessionLabelJSON `json:"labels,omitempty"`
	// LastAgentActivityAt is empty when no agent output/activity has been
	// observed, matching the absent timestamp rendering used by CreatedAt.
	//
	// It is a FLOOR, not liveness. Any pane change advances it — a spinner
	// redraw keeps it fresh — and every session working at the moment of the
	// fetch reports it to the nanosecond, so one sample cannot tell a live
	// session from a frozen snapshot and two sessions sharing a value is not a
	// collision. There is deliberately no session-level discriminator here:
	// the fields that DO discriminate are per-chat, and a caller that needs
	// them reads `boss chats --json` (spinner_present,
	// last_substantive_output_at, last_output_seeded) — the same three the MCP
	// get_chat_statuses tool description names.
	LastAgentActivityAt string `json:"last_agent_activity_at"`
	// PolicyRef is the origin branch a trial verify session reads policy from,
	// empty for every other session. Always present, so verify can tell an
	// older CLI (no key) from a session with no trial.
	PolicyRef string `json:"policy_ref"`
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
	// Nil means repository hydration failed; false is known to be disabled.
	RepoShouldArchiveSessionsAfterMerge *bool `json:"repo_should_archive_sessions_after_merge"`
	// Nil means repository hydration failed; false is known to be disabled.
	RepoCanAutoRepair *bool  `json:"repo_can_auto_repair"`
	IsVerifyOnly      bool   `json:"is_verify_only"`
	BaseBranch        string `json:"base_branch"`
	WorktreePath      string `json:"worktree_path"`
	AccountID         string `json:"account_id"`
	AccountLabel      string `json:"account_label"`
	// DisplayStatus reuses displayStatusName so the JSON speaks the same
	// vocabulary as the TUI and `boss session checks` ("passing", "failing", …).
	DisplayStatus string `json:"display_status"`
	// DisplayLabel is the daemon's formatted status label verbatim (e.g. "✓ ready").
	DisplayLabel string `json:"display_label"`
	// LastCheckState is the ChecksOverall enum name with its prefix trimmed
	// ("PENDING" / "PASSED" / "FAILED"). UNSPECIFIED means the daemon has no
	// verdict demonstrated at the current head.
	LastCheckState         string `json:"last_check_state"`
	LastCheckStateObserved string `json:"last_check_state_observed"`
	LastCheckStateHeadSHA  string `json:"last_check_state_head_sha"`
	LastCheckStateAt       string `json:"last_check_state_at"`
	// PrMergeable is null when mergeability is unknown, otherwise the PR's mergeable flag.
	PrMergeable *bool `json:"pr_mergeable"`
	// MergeBlock is null when the daemon has no merge-gate assessment for this session.
	MergeBlock *sessionMergeBlockJSON `json:"merge_block"`
	// AttentionStatus is null when the daemon has no attention status for this session.
	AttentionStatus *sessionAttentionJSON `json:"attention_status"`
	ArchivedAt      string                `json:"archived_at"`
	SetupError      string                `json:"setup_error"`
	// BlockedReason is the daemon's raw blocked reason, untruncated. It is a
	// pointer so null ("this session is not blocked") stays distinguishable from
	// "" — the same reason PrNumber is one.
	//
	// It is deliberately NOT on sessionJSON: `boss ls --json` is the widest
	// contract in this file, and a raw multi-line transport error on every row is
	// noise for the list use case. Detail is where triage happens.
	BlockedReason *string            `json:"blocked_reason"`
	LastRepair    *sessionRepairJSON `json:"last_repair"`
	RepairActive  bool               `json:"repair_active"`
}

// sessionMergeBlockJSON explains why a session's PR cannot currently be merged.
// BlockingReviewers is always a non-nil slice so an empty list marshals as `[]`,
// never `null`.
type sessionMergeBlockJSON struct {
	Gate              string   `json:"gate"`
	Detail            string   `json:"detail"`
	BlockingReviewers []string `json:"blocking_reviewers"`
	DisplayStatus     string   `json:"display_status"`
}

// sessionAttentionJSON reports whether and why a session needs human attention.
type sessionAttentionJSON struct {
	NeedsAttention bool   `json:"needs_attention"`
	Reason         string `json:"reason"`
	Summary        string `json:"summary"`
	Since          string `json:"since"`
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
		CreatedAt:           cliutil.Rfc3339OrEmpty(s.GetCreatedAt()),
		UpdatedAt:           cliutil.Rfc3339OrEmpty(s.GetUpdatedAt()),
		LastAgentActivityAt: cliutil.Rfc3339OrEmpty(s.GetLastAgentActivityAt()),
		PolicyRef:           s.GetPolicyRef(),
		TrackerRank:         trackerRankForJSON(s.GetTrackerRank()),
		Labels:              sessionLabelsToJSON(s.GetLabels()),
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
		IsVerifyOnly:           s.GetIsVerifyOnly(),
		BaseBranch:             s.GetBaseBranch(),
		WorktreePath:           s.GetWorktreePath(),
		AccountID:              s.GetAccountId(),
		AccountLabel:           s.GetAccountLabel(),
		DisplayStatus:          displayStatusName(s.GetDisplayStatus()),
		DisplayLabel:           s.GetDisplayLabel(),
		LastCheckState:         strings.TrimPrefix(s.GetLastCheckState().String(), "CHECKS_OVERALL_"),
		LastCheckStateObserved: strings.TrimPrefix(s.GetLastCheckStateObserved().String(), "CHECKS_OVERALL_"),
		LastCheckStateHeadSHA:  s.GetLastCheckStateHeadSha(),
		LastCheckStateAt:       cliutil.Rfc3339OrEmpty(s.GetLastCheckStateAt()),
		PrMergeable:            prMergeableJSON(s),
		MergeBlock:             mergeBlockJSON(s),
		AttentionStatus:        attentionJSON(s),
		ArchivedAt:             cliutil.Rfc3339OrEmpty(s.GetArchivedAt()),
		SetupError:             s.GetSetupError(),
		RepairActive:           s.GetRepairActive(),
	}
	if s.GetRepoDisplayName() != "" {
		archive := s.GetRepoShouldArchiveSessionsAfterMerge()
		detail.RepoShouldArchiveSessionsAfterMerge = &archive
		autoRepair := s.GetRepoCanAutoRepair()
		detail.RepoCanAutoRepair = &autoRepair
	}
	if reason := s.GetBlockedReason(); reason != "" {
		detail.BlockedReason = &reason
	}
	if s.GetLastRepairStartedAt() != nil || s.GetLastRepairRunnerError() != "" || s.GetLastRepairExitError() != "" {
		detail.LastRepair = &sessionRepairJSON{
			StartedAt:    cliutil.Rfc3339OrEmpty(s.GetLastRepairStartedAt()),
			RunnerError:  s.GetLastRepairRunnerError(),
			ExitError:    s.GetLastRepairExitError(),
			AttemptCount: s.GetLastRepairAttemptCount(),
		}
	}
	return detail
}

func prMergeableJSON(s *pb.Session) *bool {
	if s.PrMergeable == nil {
		return nil
	}
	v := s.GetPrMergeable()
	return &v
}

func mergeBlockJSON(s *pb.Session) *sessionMergeBlockJSON {
	mb := s.GetMergeBlock()
	if mb == nil {
		return nil
	}
	return &sessionMergeBlockJSON{
		Gate:              strings.TrimPrefix(mb.GetGate().String(), "GATE_"),
		Detail:            mb.GetDetail(),
		BlockingReviewers: append([]string{}, mb.GetBlockingReviewers()...),
		DisplayStatus:     displayStatusName(mb.GetDisplayStatus()),
	}
}

func attentionJSON(s *pb.Session) *sessionAttentionJSON {
	att := s.GetAttentionStatus()
	if att == nil {
		return nil
	}
	return &sessionAttentionJSON{
		NeedsAttention: att.GetNeedsAttention(),
		Reason:         strings.TrimPrefix(att.GetReason().String(), "ATTENTION_REASON_"),
		Summary:        att.GetSummary(),
		Since:          cliutil.Rfc3339OrEmpty(att.GetSince()),
	}
}
