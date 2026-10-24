package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossd/internal/session"
	"github.com/rs/zerolog"
)

type setupWatchStarter struct {
	ready   chan struct{}
	live    chan struct{}
	release chan struct{}
	err     error
}

func (s *setupWatchStarter) StartSession(ctx context.Context, _ string, opts session.StartSessionOpts) error {
	_, _ = io.WriteString(opts.SetupOutput, "replayed\n")
	close(s.ready)
	select {
	case <-s.live:
	case <-ctx.Done():
		return ctx.Err()
	}
	_, _ = io.WriteString(opts.SetupOutput, "live\n")
	select {
	case <-s.release:
		return s.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestWatchSessionSetupReplayLiveAndOutcome(t *testing.T) {
	for _, resurrect := range []bool{false, true} {
		for _, failure := range []bool{false, true} {
			t.Run(fmt.Sprintf("resurrect=%v/failure=%v", resurrect, failure), func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				starter := &setupWatchStarter{ready: make(chan struct{}), live: make(chan struct{}), release: make(chan struct{})}
				if failure {
					starter.err = errors.New("setup failed")
				}
				runner := session.NewBootstrapRunner(ctx, starter, zerolog.Nop())
				var bs *session.Bootstrap
				if resurrect {
					var err error
					bs, err = runner.StartResurrect("session", session.ResurrectSessionOpts{}, session.ResurrectHooks{})
					if err != nil {
						t.Fatal(err)
					}
				} else {
					bs = runner.Start("session", session.StartSessionOpts{}, session.BootstrapHooks{})
				}
				defer func() { cancel(); <-bs.Done() }()
				<-starter.ready
				_, handler := bossanovav1connect.NewDaemonServiceHandler(&Server{bootstrapRunner: runner})
				server := httptest.NewServer(handler)
				defer func() { cancel(); server.Close() }()
				rpc := bossanovav1connect.NewDaemonServiceClient(server.Client(), server.URL)
				stream, err := rpc.WatchSessionSetup(ctx, connect.NewRequest(&pb.WatchSessionSetupRequest{SessionId: "session"}))
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = stream.Close() }()
				if !stream.Receive() || stream.Msg().GetSetupOutput().GetText() != "replayed" {
					t.Fatalf("replay = %v, err %v", stream.Msg(), stream.Err())
				}
				close(starter.live)
				if !stream.Receive() || stream.Msg().GetSetupOutput().GetText() != "live" {
					t.Fatalf("live = %v, err %v", stream.Msg(), stream.Err())
				}
				close(starter.release)
				if !stream.Receive() || stream.Msg().GetSetupSettled() == nil {
					t.Fatalf("settlement = %v, err %v", stream.Msg(), stream.Err())
				}
				want := ""
				if failure {
					want = starter.err.Error()
				}
				if got := stream.Msg().GetSetupSettled().SetupError; got != want {
					t.Fatalf("setup error = %q, want %q", got, want)
				}
				if stream.Receive() || stream.Err() != nil {
					t.Fatalf("expected exactly one settlement, got %v, err %v", stream.Msg(), stream.Err())
				}
			})
		}
	}

}

func TestWatchSessionSetupUnknownSession(t *testing.T) {
	runner := session.NewBootstrapRunner(t.Context(), nil, zerolog.Nop())
	_, handler := bossanovav1connect.NewDaemonServiceHandler(&Server{bootstrapRunner: runner})
	server := httptest.NewServer(handler)
	defer server.Close()
	rpc := bossanovav1connect.NewDaemonServiceClient(server.Client(), server.URL)
	stream, err := rpc.WatchSessionSetup(t.Context(), connect.NewRequest(&pb.WatchSessionSetupRequest{SessionId: "missing"}))
	if err == nil {
		defer func() { _ = stream.Close() }()
		stream.Receive()
		err = stream.Err()
	}
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown session error = %v", err)
	}
}

func (s *setupWatchStarter) ResurrectSession(ctx context.Context, id string, opts session.ResurrectSessionOpts) (*session.ResurrectSessionResult, error) {
	err := s.StartSession(ctx, id, session.StartSessionOpts{SetupOutput: opts.SetupOutput})
	return &session.ResurrectSessionResult{SetupErr: errors.New("nonfatal setup failure")}, err
}
