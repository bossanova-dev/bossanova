package server

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/serve"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// SessionServeRunner is the local serve lifecycle consumed by the daemon door.
type SessionServeRunner interface {
	Start(context.Context, string) (serve.State, error)
	Stop(context.Context, string) error
	Get(context.Context, string) (serve.State, error)
}

func (s *Server) StartSessionServe(ctx context.Context, req *connect.Request[pb.StartSessionServeRequest]) (*connect.Response[pb.StartSessionServeResponse], error) {
	if s.sessionServeRunner == nil {
		return nil, serveUnavailable()
	}
	state, err := s.sessionServeRunner.Start(ctx, req.Msg.SessionId)
	if err != nil {
		return nil, serveError(err)
	}
	return connect.NewResponse(&pb.StartSessionServeResponse{Serve: serveProto(state)}), nil
}
func (s *Server) StopSessionServe(ctx context.Context, req *connect.Request[pb.StopSessionServeRequest]) (*connect.Response[pb.StopSessionServeResponse], error) {
	if s.sessionServeRunner == nil {
		return nil, serveUnavailable()
	}
	if err := s.sessionServeRunner.Stop(ctx, req.Msg.SessionId); err != nil {
		return nil, serveError(err)
	}
	state, err := s.sessionServeRunner.Get(ctx, req.Msg.SessionId)
	if err != nil {
		return nil, serveError(err)
	}
	return connect.NewResponse(&pb.StopSessionServeResponse{Serve: serveProto(state)}), nil
}
func (s *Server) GetSessionServe(ctx context.Context, req *connect.Request[pb.GetSessionServeRequest]) (*connect.Response[pb.GetSessionServeResponse], error) {
	if s.sessionServeRunner == nil {
		return nil, serveUnavailable()
	}
	state, err := s.sessionServeRunner.Get(ctx, req.Msg.SessionId)
	if err != nil {
		return nil, serveError(err)
	}
	return connect.NewResponse(&pb.GetSessionServeResponse{Serve: serveProto(state)}), nil
}
func serveUnavailable() error {
	return connect.NewError(connect.CodeUnimplemented, errors.New("serve runner is not configured"))
}
func serveError(err error) error {
	code := connect.CodeInternal
	switch {
	case errors.Is(err, domain.ErrNotFound):
		code = connect.CodeNotFound
	case errors.Is(err, serve.ErrSessionArchived), errors.Is(err, serve.ErrNoWorktree), errors.Is(err, serve.ErrNoServeCommand):
		return connect.NewError(connect.CodeFailedPrecondition, errors.New(err.Error()+"; configure the repo with boss repo update --serve-script"))
	case errors.Is(err, serve.ErrInvalidSpec):
		code = connect.CodeInvalidArgument
	}
	return connect.NewError(code, err)
}
func serveProto(state serve.State) *pb.SessionServe {
	out := &pb.SessionServe{IsRunning: state.IsRunning, LastError: state.LastError, TmuxSessionName: state.TmuxName}
	if !state.StartedAt.IsZero() {
		out.StartedAt = timestamppb.New(state.StartedAt)
	}
	for _, link := range state.Links {
		out.ServiceLinks = append(out.ServiceLinks, &pb.ServiceLink{Name: link.Name, Url: link.URL})
	}
	return out
}
