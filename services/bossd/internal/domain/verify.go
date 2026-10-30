package domain

import (
	"errors"
	"time"
)

var (
	ErrVerifyToolboxMissing = errors.New("boss-verify toolbox not found")
	ErrVerifyNodeMissing    = errors.New("node runtime not found")
	ErrVerifyNodeExit       = errors.New("verify-gate exited non-zero")
	ErrVerifyNodeTimeout    = errors.New("verify-gate timed out")
	ErrVerifyEmptyOutput    = errors.New("verify-gate produced no output")
	ErrVerifyBadJSON        = errors.New("verify-gate produced unparseable output")
	ErrVerifyShape          = errors.New("verify-gate returned an unexpected shape")
	ErrVerifyInvalidTarget  = errors.New("invalid verify target")
)

// VerifyTarget names the PR a verb acts on and the session worktree it runs in.
type VerifyTarget struct {
	PR     int
	Repo   string // owner/repo; empty lets gh infer it from Dir
	Dir    string // the PR session's worktree, used as the verb's cwd
	DryRun bool
}

// VerifyMergeApproval pins an approval to what the operator saw before merging: the
// head and the exact park codes waived (empty when nothing is parked). approve
// runs with --for-merge and refuses before writing anything when either moved.
type VerifyMergeApproval struct {
	Head   string
	Waives []string
}

// VerifyApproveResult is `verify-gate.mjs approve`'s outcome. Approved is true only
// when the head-bound approval record (ApprovalURL) took effect on HeadSHA:
// it waives Waives there; with Lifted "sent-back" it waives nothing and lifts a
// legacy send-back of a head verify already passed; with SignOff "merge" it
// waives nothing and signs off an operator merge of a head with nothing
// parked. TrackerWrites reports the
// needs-human label removal that follows, which is housekeeping. A refusal
// carries Reason, plus ApprovalURL when a record was posted that waives
// nothing (or whose effect could not be confirmed), and Detail when a failed
// read is behind it.
type VerifyApproveResult struct {
	Approved      bool   `json:"approved"`
	Planned       bool   `json:"planned,omitempty"`
	DryRun        bool   `json:"dryRun,omitempty"`
	Reason        string `json:"reason,omitempty"`
	Waives        string `json:"waives,omitempty"`
	Lifted        string `json:"lifted,omitempty"`
	HeadSHA       string `json:"headSha,omitempty"`
	TrackerWrites string `json:"trackerWrites,omitempty"`
	ApprovalURL   string `json:"approvalUrl,omitempty"`
	Why           string `json:"why,omitempty"`
	Park          string `json:"park,omitempty"`
	State         string `json:"state,omitempty"`
	SignOff       string `json:"signOff,omitempty"`
	Detail        string `json:"detail,omitempty"`
}

// VerifyMarkMergedResult is `verify-gate.mjs mark-merged`'s outcome: for a merged PR,
// the post-merge tracker write the verify stage's merge makes (TrackerWrites
// ok, none, unavailable, failed, permanent, ..., with Why when it did not
// happen). A PR that is not merged carries Reason instead. Writes lists the
// writes the verb attempted.
type VerifyMarkMergedResult struct {
	Merged        bool                    `json:"merged"`
	MergeSHA      string                  `json:"mergeSha,omitempty"`
	TrackerWrites string                  `json:"trackerWrites,omitempty"`
	Why           string                  `json:"why,omitempty"`
	Reason        string                  `json:"reason,omitempty"`
	State         string                  `json:"state,omitempty"`
	Writes        []VerifyMarkMergedWrite `json:"writes,omitempty"`
}

// VerifyMarkMergedWrite is one write mark-merged attempted; a tracker write names
// the ticket it moves in IssueID.
type VerifyMarkMergedWrite struct {
	Kind    string `json:"kind"`
	IssueID string `json:"issueId,omitempty"`
}

// Ticket is the ticket the tracker write tried to move, or "" when there was
// none.
func (r VerifyMarkMergedResult) Ticket() string {
	for _, w := range r.Writes {
		if w.Kind == "tracker" && w.IssueID != "" {
			return w.IssueID
		}
	}
	return ""
}

// VerifySendBack pins an operator's quoted note to the head they reviewed.
type VerifySendBack struct {
	Head string
	Note string
}

// VerifySendBackResult is the send-back verb's answer; refusal carries Reason.
type VerifySendBackResult struct {
	SentBack   bool   `json:"sentBack"`
	Reason     string `json:"reason,omitempty"`
	Head       string `json:"head,omitempty"`
	CommentURL string `json:"commentUrl,omitempty"`
	AutoRepair bool   `json:"autoRepair,omitempty"`
}

// VerifyVerbTimeout bounds a detached operator verb.
const VerifyVerbTimeout = 100 * time.Second
