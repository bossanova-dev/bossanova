package realstackrpc

import (
	"context"
	"errors"
	"time"

	"github.com/recurser/boss/internal/adapter/rpcerror"
	"github.com/recurser/boss/internal/domain/rpcerr"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Operations adapts scenario inputs to the authenticated daemon transport.
type Operations struct{ Client Client }

func (o Operations) MergeSession(ctx context.Context, id string) error {
	session, err := o.Client.GetSession(ctx, Request(&pb.GetSessionRequest{Id: id}))
	if err != nil {
		return rpcerror.WrapError(err)
	}
	head := session.Msg.GetSession().GetPrDisplayHeadSha()
	if head == "" {
		return rpcerr.New(rpcerr.FailedPrecondition, errors.New("merge requires an observed PR head"))
	}
	_, err = o.Client.MergeSession(ctx, Request(&pb.MergeSessionRequest{Id: id, ExpectedHeadSha: head}))
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
