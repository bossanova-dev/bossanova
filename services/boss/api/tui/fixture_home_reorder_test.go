package tui

import (
	"context"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// moveStubClient records every reorder the chords issue. It layers on
// stubClient, whose other methods panic, so an unexpected RPC
// from Home fails loudly rather than being quietly absorbed.
type moveStubClient struct {
	*stubClient
	reqs  []*pb.MoveSessionRequest
	Moved bool
	Err   error
}

var _ Client = (*moveStubClient)(nil)

func (c *moveStubClient) MoveSession(_ context.Context, req *pb.MoveSessionRequest) (*pb.Session, bool, error) {
	c.reqs = append(c.reqs, req)
	if c.Err != nil {
		return nil, false, c.Err
	}
	return &pb.Session{Id: req.GetId()}, c.Moved, nil
}

// DismissSessionAlert satisfies the BossClient seam (BOS-1537). This stub is
// not part of any dismiss test.
func (*moveStubClient) DismissSessionAlert(context.Context, string, string) (*pb.Session, error) {
	return nil, nil
}
