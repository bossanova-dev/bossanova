package realstackrpc

import (
	"context"
	"time"

	"github.com/recurser/boss/internal/adapter/rpcerror"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Operations adapts scenario inputs to the authenticated daemon transport.
type Operations struct{ Client Client }

func (o Operations) MergeSession(ctx context.Context, id string) error {
	_, err := o.Client.MergeSession(ctx, Request(&pb.MergeSessionRequest{Id: id}))
	return rpcerror.WrapError(err)
}
func (o Operations) ArchiveSession(ctx context.Context, id string) (bool, error) {
	response, err := o.Client.ArchiveSession(ctx, Request(&pb.ArchiveSessionRequest{Id: id}))
	if err != nil {
		return false, rpcerror.WrapError(err)
	}
	return response.Msg.IsDeferred, nil
}
func (o Operations) ReportChatStatus(ctx context.Context, id string, status pb.ChatStatus, lastOutputAt time.Time) error {
	_, err := o.Client.ReportChatStatus(ctx, Request(&pb.ReportChatStatusRequest{Reports: []*pb.ChatStatusReport{{AgentSessionId: id, Status: status, LastOutputAt: timestamppb.New(lastOutputAt)}}}))
	return rpcerror.WrapError(err)
}
