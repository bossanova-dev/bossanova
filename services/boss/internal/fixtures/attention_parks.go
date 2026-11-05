package fixtures

import (
	"fmt"
	"time"

	"github.com/recurser/bossalib/attention"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// attentionSession is row i of the AttentionSessions board.
func attentionSession(i int, a *pb.SessionAttention, label string, intent pb.DisplayIntent, status pb.DisplayStatus) *pb.Session {
	prURL := fmt.Sprintf("https://github.com/demo/my-app/pull/%d", 2980+i)
	return &pb.Session{Id: fmt.Sprintf("attention-%d", i), RepoId: "repo-1", RepoDisplayName: "my-app", Title: fmt.Sprintf("[BOS-%d] Improve the unattended session workflow", 1500+i), BranchName: fmt.Sprintf("attention-%d", i), PrNumber: i32(int32(2980 + i)), PrUrl: &prURL, Attention: a, DisplayLabel: label, DisplayIntent: intent, DisplayStatus: status}
}

// withConfidenceParks gives the AttentionSessions board its verify parks: the
// first review becomes a Moderate park on two review areas with a proof page
// in place of its video, the second a Low park with reasons and a legacy video
// only, the third a park on a CJK area under a CJK title, the fix a repair
// that exhausted its attempts on one P2 finding, and a blocked park whose
// repair is running joins IN PROGRESS. The levels stay served so the board
// proves it never draws them.
func withConfidenceParks(out []*pb.Session) []*pb.Session {
	moderate := out[0].GetAttention()
	moderate.ParkCodes, moderate.ConfidenceLevel, moderate.Asks = []string{"human-review:database"}, pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE, []string{"API schema", "API compatibility"}
	moderate.ProofUrl, moderate.EvidenceUrl = "https://example.com/proof/2980", ""
	moderate.Actions = []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_GITHUB, pb.AttentionAction_ATTENTION_ACTION_PROOF, pb.AttentionAction_ATTENTION_ACTION_MERGE}

	low := out[4].GetAttention()
	low.ParkCodes, low.ConfidenceLevel, low.Asks = []string{"human-review:security"}, pb.ConfidenceLevel_CONFIDENCE_LEVEL_LOW, []string{"Infrastructure"}
	low.LoweredBy = []string{"large diff", "touches the auth path", "no end-to-end test"}

	out[6].Title = "[BOS-1506] 権限ルールの見直し"
	out[6].GetAttention().ParkCodes, out[6].GetAttention().Asks = []string{"human-review:security"}, []string{"セキュリティ"}

	exhausted := out[2].GetAttention()
	exhausted.BlockerTallies = []*pb.BlockerTally{{Severity: "P2", Kind: attention.BlockerKindReviewFinding, BlockerCount: 1}}
	exhausted.BlockerFiles = []string{"internal/api/schema.go"}

	i := len(out)
	repairing := &pb.SessionAttention{
		Kind: pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS, Sentence: "Verify is blocked: open checklist items", Since: ts(time.Duration(i) * time.Minute),
		ProgressLabel: attention.RepairingLabel, ParkCodes: []string{"ledger-open"},
		BlockerTallies: []*pb.BlockerTally{{Severity: "P2", Kind: attention.BlockerKindReviewFinding, BlockerCount: 1}},
		BlockerFiles:   []string{"services/web/src/App.tsx"},
	}
	s := attentionSession(i, repairing, "repairing", pb.DisplayIntent_DISPLAY_INTENT_WARNING, pb.DisplayStatus_DISPLAY_STATUS_UNSPECIFIED)
	s.DisplayIsRepairing = true
	return append(out, s)
}

// withServedStatus serves each row's status phrase and detail as bossd does,
// from the shared producer over the row's served fields.
func withServedStatus(out []*pb.Session) []*pb.Session {
	for _, s := range out {
		a := s.GetAttention()
		a.StatusPhrase, a.StatusDetail = attention.RowStatus(attention.Input{RepairOwned: s.GetDisplayIsRepairing()}, a)
	}
	return out
}
