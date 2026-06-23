package socketbackend

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

// fakeDaemon is a minimal DaemonService implementation served over a Unix
// socket so we can exercise the socket-backed Backend without a real bossd.
type fakeDaemon struct {
	bossanovav1connect.UnimplementedDaemonServiceHandler

	sessions []*pb.Session
	// createOutputs are emitted as SetupOutput events before the terminal
	// SessionCreated event, exercising the stream-draining behaviour.
	createOutputs []string
	created       *pb.Session
}

func (f *fakeDaemon) ListSessions(_ context.Context, _ *connect.Request[pb.ListSessionsRequest]) (*connect.Response[pb.ListSessionsResponse], error) {
	return connect.NewResponse(&pb.ListSessionsResponse{Sessions: f.sessions}), nil
}

func (f *fakeDaemon) CreateSession(_ context.Context, _ *connect.Request[pb.CreateSessionRequest], stream *connect.ServerStream[pb.CreateSessionResponse]) error {
	for _, line := range f.createOutputs {
		if err := stream.Send(&pb.CreateSessionResponse{
			Event: &pb.CreateSessionResponse_SetupOutput{SetupOutput: &pb.SetupScriptOutput{Text: line}},
		}); err != nil {
			return err
		}
	}
	return stream.Send(&pb.CreateSessionResponse{
		Event: &pb.CreateSessionResponse_SessionCreated{SessionCreated: &pb.SessionCreated{Session: f.created}},
	})
}

// serveFakeDaemon starts the fake daemon on a Unix socket and returns its path.
func serveFakeDaemon(t *testing.T, fake bossanovav1connect.DaemonServiceHandler) string {
	t.Helper()

	dir := t.TempDir()
	socketPath := filepath.Join(dir, "bossd.sock")

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}

	_, handler := bossanovav1connect.NewDaemonServiceHandler(fake)
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}

	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = os.Remove(socketPath)
	})

	return socketPath
}

func TestListSessionsRoundTrips(t *testing.T) {
	t.Parallel()

	fake := &fakeDaemon{sessions: []*pb.Session{
		{Id: "sess-1", Title: "first"},
		{Id: "sess-2", Title: "second"},
	}}
	socketPath := serveFakeDaemon(t, fake)

	backend, err := New(socketPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got, err := backend.ListSessions(context.Background(), &pb.ListSessionsRequest{})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d sessions, want 2", len(got))
	}
	if got[0].GetId() != "sess-1" || got[1].GetId() != "sess-2" {
		t.Fatalf("unexpected sessions: %+v", got)
	}
}

func TestCreateSessionDrainsStream(t *testing.T) {
	t.Parallel()

	fake := &fakeDaemon{
		createOutputs: []string{"cloning...", "installing deps...", "ready"},
		created:       &pb.Session{Id: "sess-new", Title: "drained"},
	}
	socketPath := serveFakeDaemon(t, fake)

	backend, err := New(socketPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	sess, err := backend.CreateSession(context.Background(), &pb.CreateSessionRequest{RepoId: "repo-1"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if sess.GetId() != "sess-new" {
		t.Fatalf("got session %q, want sess-new (stream must drain to the terminal Session)", sess.GetId())
	}
}

func TestGetCronJobCallsRPC(t *testing.T) {
	t.Parallel()

	socketPath := serveFakeDaemon(t, &fakeDaemonCron{cronJob: &pb.CronJob{Id: "cron-1", Name: "nightly"}})

	backend, err := New(socketPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	job, err := backend.GetCronJob(context.Background(), "cron-1")
	if err != nil {
		t.Fatalf("GetCronJob: %v", err)
	}
	if job.GetId() != "cron-1" {
		t.Fatalf("got cron %q, want cron-1", job.GetId())
	}
}

type fakeDaemonCron struct {
	bossanovav1connect.UnimplementedDaemonServiceHandler
	cronJob *pb.CronJob
}

func (f *fakeDaemonCron) GetCronJob(_ context.Context, req *connect.Request[pb.GetCronJobRequest]) (*connect.Response[pb.GetCronJobResponse], error) {
	if req.Msg.GetId() != "cron-1" {
		return nil, connect.NewError(connect.CodeNotFound, nil)
	}
	return connect.NewResponse(&pb.GetCronJobResponse{CronJob: f.cronJob}), nil
}
