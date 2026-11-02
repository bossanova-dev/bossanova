package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// prStateGoldenPath is the `boss pr state --json` output the skills toolbox
// tests read (skills-toolbox/ci/pr-state-cli-golden.test.mjs), so the toolbox
// is proven against the CLI's real bytes rather than a hand-built envelope.
var prStateGoldenPath = filepath.Join("testdata", "pr_state_workflow.golden.json")

// TestPRStateJSONMatchesToolboxGolden pins the CLI's JSON for a head whose
// checks belong to two workflows, and a commit status that belongs to none.
// Regenerate with UPDATE_GOLDEN=1.
func TestPRStateJSONMatchesToolboxGolden(t *testing.T) {
	success := pb.CheckConclusion_CHECK_CONCLUSION_SUCCESS
	mergeable := true
	resp := &pb.GetPRStateResponse{
		Repo: "acme/widgets", PrNumber: 7, State: pb.PRState_PR_STATE_OPEN,
		HeadSha: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", HeadBranch: "feat", BaseBranch: "main",
		IsMergeable: &mergeable, MergeStateStatus: "clean",
		Checks: []*pb.PRCheck{
			{Name: "build", Status: pb.CheckStatus_CHECK_STATUS_COMPLETED, Conclusion: &success, WorkflowName: "CI"},
			{Name: "docs-lint", Status: pb.CheckStatus_CHECK_STATUS_COMPLETED, Conclusion: &success, WorkflowName: "Docs"},
			{Name: "Terraform Cloud", Status: pb.CheckStatus_CHECK_STATUS_COMPLETED, Conclusion: &success},
		},
		CheckVerdict:       &pb.PRCheckVerdict{State: pb.PRCheckVerdictState_PR_CHECK_VERDICT_STATE_GREEN, Reason: "ok", TotalCount: 3, PassedCount: 3},
		VerifyPhase:        &pb.PRVerifyPhase{Kind: pb.PRVerifyPhaseKind_PR_VERIFY_PHASE_KIND_NONE},
		IsWorkflowRunsRead: true,
		WorkflowRuns: []*pb.PRWorkflowRun{
			{Name: "CI", Status: "completed", Conclusion: "success", Event: "pull_request", HeadSha: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			{Name: "Docs", Status: "completed", Conclusion: "success", Event: "pull_request", HeadSha: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		},
		ObservedAt: timestamppb.New(time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)),
	}
	out, err := runPRStateCmd(t, &prStateFake{resp: resp}, "7", "--repo", "acme/widgets", "--json")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(prStateGoldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(prStateGoldenPath, []byte(out), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(prStateGoldenPath)
	if err != nil {
		t.Fatalf("read golden (regenerate with UPDATE_GOLDEN=1): %v", err)
	}
	if out != string(want) {
		t.Fatalf("boss pr state --json drifted from %s (regenerate with UPDATE_GOLDEN=1 and rerun the toolbox golden test)\ngot:\n%s\nwant:\n%s", prStateGoldenPath, out, want)
	}
}
