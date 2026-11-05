package apiversion

import (
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/proto"
)

// StatusPhraseChange clears the attention's status_phrase and status_detail
// for pins older than V20261101. The fields are additive and older clients
// never read them, but a down-convert that restores an older kind or label
// would otherwise leave a phrase describing today's answer, and an older pin
// is owed exactly the bytes it was served before.
type StatusPhraseChange struct{}

// Version implements VersionChange.
func (StatusPhraseChange) Version() Version { return V20261101 }

// TransformResponse implements VersionChange for session-bearing unary responses.
func (StatusPhraseChange) TransformResponse(method string, msg any) {
	transformUnarySessionResponse(method, msg, downconvertStatusPhraseSession)
}

func downconvertStatusPhraseSession(s *pb.Session) *pb.Session {
	a := s.GetAttention()
	if a.GetStatusPhrase() == "" && a.GetStatusDetail() == "" {
		return s
	}
	cloned, ok := proto.Clone(s).(*pb.Session)
	if !ok {
		return s
	}
	cloned.Attention.StatusPhrase = ""
	cloned.Attention.StatusDetail = ""
	return cloned
}
