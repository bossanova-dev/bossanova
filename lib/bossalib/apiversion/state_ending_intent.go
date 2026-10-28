package apiversion

import (
	"github.com/recurser/bossalib/displaystatus"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"google.golang.org/protobuf/proto"
)

// StateEndingErroredIntentChange restores the red recolor served before
// V20260929 for errored sessions whose merge or archive is in flight.
type StateEndingErroredIntentChange struct{}

// Version implements VersionChange.
func (StateEndingErroredIntentChange) Version() Version { return V20260929 }

func downconvertStateEndingErroredSession(s *pb.Session) *pb.Session {
	out := displaystatus.PreStateEndingErroredOutput(s)
	if out.Intent == s.GetDisplayIntent() {
		return s
	}
	cloned, ok := proto.Clone(s).(*pb.Session)
	if !ok {
		return s
	}
	cloned.DisplayIntent = out.Intent
	return cloned
}

// TransformResponse implements VersionChange on the same session carriers as
// ErroredStatusChange, including the streaming created frame.
func (StateEndingErroredIntentChange) TransformResponse(method string, msg any) {
	if transformUnarySessionResponse(method, msg, downconvertStateEndingErroredSession) {
		return
	}
	if method == bossanovav1connect.OrchestratorServiceProxyCreateSessionProcedure {
		if m, ok := msg.(*pb.ProxyCreateSessionResponse); ok {
			if created, ok := m.Body.(*pb.ProxyCreateSessionResponse_Created); ok {
				created.Created = downconvertStateEndingErroredSession(created.Created)
			}
		}
	}
}
