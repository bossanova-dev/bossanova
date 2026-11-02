package daemonrpc

import (
	"context"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/adapter/rpcerror"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func (c *LocalClient) FollowUpSessionPRNotes(ctx context.Context, id string, ids []string, all, dryRun bool) (*pb.VerifyFollowUpResult, error) {
	resp, err := c.rpc.FollowUpSessionPRNotes(ctx, connect.NewRequest(&pb.FollowUpSessionPRNotesRequest{Id: id, VerifyNoteIds: ids, ShouldFollowUpAll: all, IsDryRun: dryRun}))
	if err != nil {
		return nil, rpcerror.WrapError(err)
	}
	return resp.Msg.GetResult(), nil
}
