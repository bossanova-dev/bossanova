package apiversion_test

import (
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

// sessionResponseCase is one Session-carrying response a transform table
// builds and reads back.
type sessionResponseCase = struct {
	name   string
	method string
	build  func() any
	get    func(any) *pb.Session
}

// reviewQueueResponseCase carries session as the only review queue row.
func reviewQueueResponseCase(session func() *pb.Session) sessionResponseCase {
	return sessionResponseCase{
		name:   "ProxyListReviewQueue",
		method: bossanovav1connect.OrchestratorServiceProxyListReviewQueueProcedure,
		build: func() any {
			return &pb.ProxyListReviewQueueResponse{Rows: []*pb.ReviewQueueRow{{Session: session()}}}
		},
		get: func(m any) *pb.Session { return m.(*pb.ProxyListReviewQueueResponse).GetRows()[0].GetSession() },
	}
}
