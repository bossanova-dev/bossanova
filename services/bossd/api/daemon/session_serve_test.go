package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/serve"
	"github.com/recurser/bossd/internal/status"
)

type sessionServeFixture struct {
	state serve.State
	err   error
	id    string
}

func (f *sessionServeFixture) Start(_ context.Context, id string) (serve.State, error) {
	f.id = id
	return f.state, f.err
}
func (f *sessionServeFixture) Stop(_ context.Context, id string) error {
	f.id = id
	f.state.IsRunning = false
	f.state.Links = nil
	return f.err
}
func (f *sessionServeFixture) Get(_ context.Context, id string) (serve.State, error) {
	f.id = id
	return f.state, f.err
}

func TestSessionServeRPCs(t *testing.T) {
	for _, operation := range []string{"start", "stop", "get"} {
		t.Run(operation, func(t *testing.T) {
			f := &sessionServeFixture{state: serve.State{IsRunning: true, Links: []status.ServiceLink{{Name: "api", URL: "https://example.com"}}, LastError: "prior error", StartedAt: time.Unix(100, 0), TmuxName: "serve-pane"}}
			s := New(Config{SessionServeRunner: f})
			var state *pb.SessionServe
			var err error
			switch operation {
			case "start":
				var r *connect.Response[pb.StartSessionServeResponse]
				r, err = s.StartSessionServe(t.Context(), connect.NewRequest(&pb.StartSessionServeRequest{SessionId: "session"}))
				if err == nil {
					state = r.Msg.Serve
				}
			case "stop":
				var r *connect.Response[pb.StopSessionServeResponse]
				r, err = s.StopSessionServe(t.Context(), connect.NewRequest(&pb.StopSessionServeRequest{SessionId: "session"}))
				if err == nil {
					state = r.Msg.Serve
				}
			case "get":
				var r *connect.Response[pb.GetSessionServeResponse]
				r, err = s.GetSessionServe(t.Context(), connect.NewRequest(&pb.GetSessionServeRequest{SessionId: "session"}))
				if err == nil {
					state = r.Msg.Serve
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if f.id != "session" || state.LastError != "prior error" || !state.StartedAt.AsTime().Equal(time.Unix(100, 0)) || state.TmuxSessionName != "serve-pane" {
				t.Fatalf("state=%v id=%s", state, f.id)
			}
			if operation == "stop" {
				if state.IsRunning || len(state.ServiceLinks) != 0 {
					t.Fatal(state)
				}
			} else if !state.IsRunning || len(state.ServiceLinks) != 1 || state.ServiceLinks[0].Url != "https://example.com" {
				t.Fatal(state)
			}
		})
	}
}
func TestSessionServeErrors(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		code connect.Code
	}{
		{"unknown", domain.ErrNotFound, connect.CodeNotFound},
		{"archived", serve.ErrSessionArchived, connect.CodeFailedPrecondition},
		{"worktree", serve.ErrNoWorktree, connect.CodeFailedPrecondition},
		{"command", serve.ErrNoServeCommand, connect.CodeFailedPrecondition},
		{"spec", serve.ErrInvalidSpec, connect.CodeInvalidArgument},
		{"internal", errors.New("failure"), connect.CodeInternal},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := New(Config{SessionServeRunner: &sessionServeFixture{err: tt.err}})
			_, a := s.StartSessionServe(t.Context(), connect.NewRequest(&pb.StartSessionServeRequest{SessionId: "session"}))
			_, b := s.StopSessionServe(t.Context(), connect.NewRequest(&pb.StopSessionServeRequest{SessionId: "session"}))
			_, c := s.GetSessionServe(t.Context(), connect.NewRequest(&pb.GetSessionServeRequest{SessionId: "session"}))
			for _, e := range []error{a, b, c} {
				if connect.CodeOf(e) != tt.code {
					t.Fatalf("error=%v want=%v", e, tt.code)
				}
			}
		})
	}
	s := New(Config{})
	_, a := s.StartSessionServe(t.Context(), connect.NewRequest(&pb.StartSessionServeRequest{}))
	_, b := s.StopSessionServe(t.Context(), connect.NewRequest(&pb.StopSessionServeRequest{}))
	_, c := s.GetSessionServe(t.Context(), connect.NewRequest(&pb.GetSessionServeRequest{}))
	for _, e := range []error{a, b, c} {
		if connect.CodeOf(e) != connect.CodeUnimplemented {
			t.Fatal(e)
		}
	}
}
