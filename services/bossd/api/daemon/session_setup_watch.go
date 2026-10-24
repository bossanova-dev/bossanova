package server

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// WatchSessionSetup attaches to daemon-owned setup without extending or cancelling it.
func (s *Server) WatchSessionSetup(ctx context.Context, req *connect.Request[pb.WatchSessionSetupRequest], stream *connect.ServerStream[pb.WatchSessionSetupResponse]) error {
	if s.bootstrapRunner == nil {
		return connect.NewError(connect.CodeNotFound, errors.New("no in-flight session setup"))
	}
	bootstrap, ok := s.bootstrapRunner.Lookup(req.Msg.SessionId)
	if !ok {
		return connect.NewError(connect.CodeNotFound, errors.New("no in-flight session setup"))
	}
	lines, cancel := bootstrap.Subscribe()
	defer cancel()
	if err := relaySetupOutput(ctx, lines, bootstrap.Done(), func(line string) error {
		return stream.Send(&pb.WatchSessionSetupResponse{Event: &pb.WatchSessionSetupResponse_SetupOutput{SetupOutput: &pb.SetupScriptOutput{Text: line}}})
	}); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-bootstrap.Done():
	}
	settled := &pb.SessionSetupSettled{}
	if err := bootstrap.Err(); err != nil {
		settled.SetupError = err.Error()
	}
	return stream.Send(&pb.WatchSessionSetupResponse{Event: &pb.WatchSessionSetupResponse_SetupSettled{SetupSettled: settled}})
}
