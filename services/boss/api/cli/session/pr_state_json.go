package session

import (
	"fmt"
	"strings"

	"github.com/recurser/boss/api/cli/cliutil"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// prStateJSON is the stable `boss pr state --json` envelope. Field names are
// the machine contract; enum values are lowercase strings, empty when unset.
// is_mergeable is null while GitHub has not computed it,
// has_settled_workflow_runs is null unless the check verdict is green, and
// workflow_runs is empty unless is_workflow_runs_read.
type prStateJSON struct {
	Repo                   string                `json:"repo"`
	PRNumber               int32                 `json:"pr_number"`
	State                  string                `json:"state"`
	IsDraft                bool                  `json:"is_draft"`
	HeadSHA                string                `json:"head_sha"`
	HeadBranch             string                `json:"head_branch"`
	BaseBranch             string                `json:"base_branch"`
	IsMergeable            *bool                 `json:"is_mergeable"`
	MergeStateStatus       string                `json:"merge_state_status"`
	ReviewDecision         string                `json:"review_decision"`
	LatestReviewState      string                `json:"latest_review_state"`
	Checks                 []prCheckJSON         `json:"checks"`
	CheckVerdict           prCheckVerdictJSON    `json:"check_verdict"`
	VerifyPhase            prVerifyPhaseJSON     `json:"verify_phase"`
	HasBuildReceipt        bool                  `json:"has_build_receipt"`
	HasSettledWorkflowRuns *bool                 `json:"has_settled_workflow_runs"`
	HeldWorkflowRuns       []prHeldRunJSON       `json:"held_workflow_runs"`
	ReviewComments         []prReviewCommentJSON `json:"review_comments"`
	ObservedAt             string                `json:"observed_at"`
	WorkflowRuns           []prWorkflowRunJSON   `json:"workflow_runs"`
	IsWorkflowRunsRead     bool                  `json:"is_workflow_runs_read"`
}

type prCheckJSON struct {
	Name           string `json:"name"`
	Status         string `json:"status"`
	Conclusion     string `json:"conclusion"`
	Description    string `json:"description"`
	IsUnclassified bool   `json:"is_unclassified"`
	// WorkflowName lets a saved prior head's check set be matched against
	// the head's workflow runs; a commit status has none.
	WorkflowName string `json:"workflow_name,omitempty"`
}

type prCheckVerdictJSON struct {
	State             string `json:"state"`
	Reason            string `json:"reason"`
	TotalCount        int32  `json:"total_count"`
	PassedCount       int32  `json:"passed_count"`
	FailedCount       int32  `json:"failed_count"`
	PendingCount      int32  `json:"pending_count"`
	SkippedCount      int32  `json:"skipped_count"`
	UnclassifiedCount int32  `json:"unclassified_count"`
}

type prVerifyPhaseJSON struct {
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
	Label  string `json:"label"`
}

type prHeldRunJSON struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Event  string `json:"event"`
	Class  string `json:"class"`
}

type prWorkflowRunJSON struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Event      string `json:"event"`
	HeadSHA    string `json:"head_sha"`
}

type prReviewCommentJSON struct {
	Author string `json:"author"`
	Body   string `json:"body"`
	State  string `json:"state"`
	Path   string `json:"path,omitempty"`
	Line   *int32 `json:"line,omitempty"`
}

// enumName spells a proto enum value as its lowercase suffix after prefix:
// PR_STATE_OPEN is "open". The UNSPECIFIED value is empty.
func enumName(value fmt.Stringer, prefix string) string {
	name := strings.TrimPrefix(value.String(), prefix)
	if name == "UNSPECIFIED" {
		return ""
	}
	return strings.ToLower(name)
}

func newPRStateJSON(r *pb.GetPRStateResponse) prStateJSON {
	v := r.GetCheckVerdict()
	env := prStateJSON{
		Repo:              r.GetRepo(),
		PRNumber:          r.GetPrNumber(),
		State:             enumName(r.GetState(), "PR_STATE_"),
		IsDraft:           r.GetIsDraft(),
		HeadSHA:           r.GetHeadSha(),
		HeadBranch:        r.GetHeadBranch(),
		BaseBranch:        r.GetBaseBranch(),
		IsMergeable:       r.IsMergeable,
		MergeStateStatus:  r.GetMergeStateStatus(),
		ReviewDecision:    enumName(r.GetReviewDecision(), "REVIEW_STATE_"),
		LatestReviewState: enumName(r.GetLatestReviewState(), "REVIEW_STATE_"),
		Checks:            make([]prCheckJSON, 0, len(r.GetChecks())),
		CheckVerdict: prCheckVerdictJSON{
			State:             enumName(v.GetState(), "PR_CHECK_VERDICT_STATE_"),
			Reason:            v.GetReason(),
			TotalCount:        v.GetTotalCount(),
			PassedCount:       v.GetPassedCount(),
			FailedCount:       v.GetFailedCount(),
			PendingCount:      v.GetPendingCount(),
			SkippedCount:      v.GetSkippedCount(),
			UnclassifiedCount: v.GetUnclassifiedCount(),
		},
		VerifyPhase: prVerifyPhaseJSON{
			Kind:   enumName(r.GetVerifyPhase().GetKind(), "PR_VERIFY_PHASE_KIND_"),
			Reason: r.GetVerifyPhase().GetReason(),
			Label:  r.GetVerifyPhase().GetLabel(),
		},
		HasBuildReceipt:        r.GetHasBuildReceipt(),
		HasSettledWorkflowRuns: r.HasSettledWorkflowRuns,
		HeldWorkflowRuns:       make([]prHeldRunJSON, 0, len(r.GetHeldWorkflowRuns())),
		ReviewComments:         make([]prReviewCommentJSON, 0, len(r.GetReviewComments())),
		ObservedAt:             cliutil.Rfc3339OrEmpty(r.GetObservedAt()),
		WorkflowRuns:           make([]prWorkflowRunJSON, 0, len(r.GetWorkflowRuns())),
		IsWorkflowRunsRead:     r.GetIsWorkflowRunsRead(),
	}
	for _, c := range r.GetChecks() {
		check := prCheckJSON{Name: c.GetName(), Status: enumName(c.GetStatus(), "CHECK_STATUS_"), Description: c.GetDescription(), IsUnclassified: c.GetIsUnclassified(), WorkflowName: c.GetWorkflowName()}
		if c.Conclusion != nil {
			check.Conclusion = enumName(c.GetConclusion(), "CHECK_CONCLUSION_")
		}
		env.Checks = append(env.Checks, check)
	}
	for _, run := range r.GetHeldWorkflowRuns() {
		env.HeldWorkflowRuns = append(env.HeldWorkflowRuns, prHeldRunJSON{Name: run.GetName(), Status: run.GetStatus(), Event: run.GetEvent(), Class: run.GetClass()})
	}
	for _, run := range r.GetWorkflowRuns() {
		env.WorkflowRuns = append(env.WorkflowRuns, prWorkflowRunJSON{Name: run.GetName(), Status: run.GetStatus(), Conclusion: run.GetConclusion(), Event: run.GetEvent(), HeadSHA: run.GetHeadSha()})
	}
	for _, rc := range r.GetReviewComments() {
		env.ReviewComments = append(env.ReviewComments, prReviewCommentJSON{Author: rc.GetAuthor(), Body: rc.GetBody(), State: enumName(rc.GetState(), "REVIEW_STATE_"), Path: rc.GetPath(), Line: rc.Line})
	}
	return env
}
