package fixtures

import (
	"fmt"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// attentionSession is row i of the AttentionSessions board.
func attentionSession(i int, a *pb.SessionAttention, label string, intent pb.DisplayIntent, status pb.DisplayStatus) *pb.Session {
	prURL := fmt.Sprintf("https://github.com/demo/my-app/pull/%d", 2980+i)
	return &pb.Session{Id: fmt.Sprintf("attention-%d", i), RepoId: "repo-1", RepoDisplayName: "my-app", Title: fmt.Sprintf("[BOS-%d] Improve the unattended session workflow", 1500+i), BranchName: fmt.Sprintf("attention-%d", i), PrNumber: i32(int32(2980 + i)), PrUrl: &prURL, Attention: a, DisplayLabel: label, DisplayIntent: intent, DisplayStatus: status}
}

// withConfidenceParks gives the AttentionSessions board its verify park
// confidence rows: the first review becomes a Moderate park with a proof page
// in place of its video, the second a Low park with reasons and a legacy video
// only, and a blocked park whose session is repairing joins the board.
func withConfidenceParks(out []*pb.Session) []*pb.Session {
	moderate := out[0].GetAttention()
	moderate.ParkCodes, moderate.ConfidenceLevel, moderate.Asks = []string{"human-review:database"}, pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE, []string{"schema", "api"}
	moderate.ProofUrl, moderate.EvidenceUrl = "https://example.com/proof/2980", ""
	moderate.Actions = []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_GITHUB, pb.AttentionAction_ATTENTION_ACTION_PROOF, pb.AttentionAction_ATTENTION_ACTION_MERGE}

	low := out[4].GetAttention()
	low.ParkCodes, low.ConfidenceLevel, low.Asks = []string{"human-review:security"}, pb.ConfidenceLevel_CONFIDENCE_LEVEL_LOW, []string{"large", "auth"}
	low.LoweredBy = []string{"large diff", "touches the auth path", "no end-to-end test"}

	i := len(out)
	blocked := &pb.SessionAttention{
		Kind: pb.AttentionKind_ATTENTION_KIND_FIX, Sentence: "Verify is blocked: open checklist items", Since: ts(time.Duration(i) * time.Minute),
		ParkCodes: []string{"ledger-open"}, Actions: []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_GITHUB},
	}
	s := attentionSession(i, blocked, "repairing", pb.DisplayIntent_DISPLAY_INTENT_WARNING, pb.DisplayStatus_DISPLAY_STATUS_UNSPECIFIED)
	s.DisplayIsRepairing = true
	return append(out, s)
}
