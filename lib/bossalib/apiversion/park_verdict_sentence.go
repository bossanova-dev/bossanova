package apiversion

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// ParkVerdictSentenceChange restores pre-verdict park sentences and hides the
// additive label from older clients. It runs before the older copy transforms.
type ParkVerdictSentenceChange struct{}

// Version implements VersionChange.
func (ParkVerdictSentenceChange) Version() Version { return V20261031 }

// TransformResponse implements VersionChange on session-bearing unary responses.
func (ParkVerdictSentenceChange) TransformResponse(method string, msg any) {
	transformUnarySessionResponse(method, msg, downconvertParkVerdictSentence)
}

func downconvertParkVerdictSentence(s *pb.Session) *pb.Session {
	a := s.GetAttention()
	if len(a.GetParkCodes()) == 0 || (!strings.HasPrefix(a.GetSentence(), "Not verified · ") && !strings.HasPrefix(a.GetSentence(), "Verified · waiting for you · ")) {
		return s
	}
	code, ok := attention.DecidingParkCode(a.GetParkCodes())
	if !ok {
		return s
	}
	cloned, ok := proto.Clone(s).(*pb.Session)
	if !ok {
		return s
	}
	cloned.Attention.Sentence = preVerdictParkSentence(code, a)
	cloned.Attention.ParkLabel = ""
	return cloned
}

// These formats are frozen compatibility copy, independent of current verdicts.
func preVerdictParkSentence(code string, a *pb.SessionAttention) string {
	switch code {
	case "human-review:ui":
		if a.GetEvidenceUrl() == "" {
			return "UI change"
		}
		duration := ""
		const evidenceSegment = " · evidence video"
		if i := strings.LastIndex(a.GetSentence(), evidenceSegment); i >= 0 {
			duration = a.GetSentence()[i+len(evidenceSegment):]
		}
		return fmt.Sprintf("UI change · evidence video%s", duration)
	case "always-human-path", "human-review:security":
		label := ""
		if a.GetParkLabel() != "" {
			label = ": " + a.GetParkLabel()
		}
		return fmt.Sprintf("Security change%s", label)
	case "human-review:database":
		return "Database migration"
	case "human-review:unproven":
		return "Some checks have no evidence"
	case "partial-build":
		return "Partial build"
	case "ledger-open":
		switch len(a.GetOpenItems()) {
		case 0:
			return "The PR checklist has open items"
		case 1:
			return "The PR checklist has 1 open item"
		default:
			return fmt.Sprintf("The PR checklist has %d open items", len(a.GetOpenItems()))
		}
	case "no-receipt":
		return "No review of the latest commit"
	case "human-merge":
		return "Verified · a person merges"
	default:
		if attention.IsSignOffPark([]string{code}) {
			return "Needs a sign-off"
		}
		return "Verify is blocked"
	}
}
