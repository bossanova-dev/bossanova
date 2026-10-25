package apiversion

import (
	"strings"

	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

// WaitingForVerifyLabelChange restores the resting chat label served before
// V20260926 on green factory PRs waiting for verify, including factory MERGE.
//
// Session carries no served chat status. The display composite identifies idle,
// waiting and usage-limited chats; an IDLE chat showing the PR's own passing or
// ready label instead gets stopped. This inexact leg affects the version-skewed
// boss TUI through a remote daemon and the web through bosso. An exact inverse
// would require a new transport mark solely for two muted resting labels. See
// docs/structure/contracts.md#skipping-a-bump-justify-it-in-the-diff.
type WaitingForVerifyLabelChange struct{}

// Version implements VersionChange.
func (WaitingForVerifyLabelChange) Version() Version { return V20260926 }

// downconvertWaitingForVerifyLabelSession clones before writing because bosso
// caches session pointers. Only a row bearing the new progress label allocates.
func downconvertWaitingForVerifyLabelSession(s *pb.Session) *pb.Session {
	if s.GetAttention().GetProgressLabel() != shortWaitLabelPrevious[attention.WaitingForVerifyLabel] {
		return s
	}
	cloned, ok := proto.Clone(s).(*pb.Session)
	if !ok {
		return s
	}
	label := s.GetDisplayLabel()
	previous := "stopped"
	if label == "idle" || label == "waiting" || strings.HasPrefix(label, "usage-limited") {
		previous = "idle"
	}
	cloned.Attention.ProgressLabel = previous
	return cloned
}

// TransformResponse implements VersionChange for session-bearing unary responses.
func (WaitingForVerifyLabelChange) TransformResponse(method string, msg any) {
	transformUnarySessionResponse(method, msg, downconvertWaitingForVerifyLabelSession)
}

// shortWaitLabelPrevious maps each in-progress wait label to the words served
// before V20260928.
var shortWaitLabelPrevious = map[string]string{
	attention.WaitingForVerifyLabel: "waiting for verify",
	"main red":                      "waiting for main CI",
	"tests busy":                    "waiting for the test stack",
}

// ShortWaitLabelChange restores the long wait labels served before V20260928.
// Clients of that era colour a label as a wait only when it starts with
// "waiting", so the short words would read as live work there.
type ShortWaitLabelChange struct{}

// Version implements VersionChange.
func (ShortWaitLabelChange) Version() Version { return V20260928 }

// downconvertShortWaitLabelSession clones before writing because bosso caches
// session pointers. Only a row bearing a short wait label allocates.
func downconvertShortWaitLabelSession(s *pb.Session) *pb.Session {
	previous, ok := shortWaitLabelPrevious[s.GetAttention().GetProgressLabel()]
	if !ok {
		return s
	}
	cloned, ok := proto.Clone(s).(*pb.Session)
	if !ok {
		return s
	}
	cloned.Attention.ProgressLabel = previous
	return cloned
}

// TransformResponse implements VersionChange for session-bearing unary responses.
func (ShortWaitLabelChange) TransformResponse(method string, msg any) {
	transformUnarySessionResponse(method, msg, downconvertShortWaitLabelSession)
}

// VerifyAdmissionLabelChange restores the queued label for clients built before
// verify admission distinguished pending and unknown heads.
type VerifyAdmissionLabelChange struct{}

// Version implements VersionChange.
func (VerifyAdmissionLabelChange) Version() Version { return V20260928 }

func downconvertVerifyAdmissionLabelSession(s *pb.Session) *pb.Session {
	label := s.GetAttention().GetProgressLabel()
	if label != attention.VerifyUnknownLabel && label != attention.OutsideVerifyLabelPrefix && !strings.HasPrefix(label, attention.OutsideVerifyLabelPrefix+":") {
		return s
	}
	cloned, ok := proto.Clone(s).(*pb.Session)
	if !ok {
		return s
	}
	cloned.Attention.ProgressLabel = attention.WaitingForVerifyLabel
	return cloned
}

// TransformResponse implements VersionChange for session-bearing unary responses.
func (VerifyAdmissionLabelChange) TransformResponse(method string, msg any) {
	transformUnarySessionResponse(method, msg, downconvertVerifyAdmissionLabelSession)
}
