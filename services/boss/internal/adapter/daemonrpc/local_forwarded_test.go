package daemonrpc

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/contract/daemon/callerorigin"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

type originEchoDaemon struct {
	bossanovav1connect.UnimplementedDaemonServiceHandler
	got chan callerorigin.Assertion
}

func (d originEchoDaemon) ListRepos(_ context.Context, req *connect.Request[pb.ListReposRequest]) (*connect.Response[pb.ListReposResponse], error) {
	a, _ := callerorigin.Read(req.Header())
	d.got <- a
	return connect.NewResponse(&pb.ListReposResponse{}), nil
}

func TestNewForwardedAssertsItsOriginOnEveryCall(t *testing.T) {
	socketPath := filepath.Join("/tmp", fmt.Sprintf("boss-forwarded-%d-%d.sock", os.Getpid(), time.Now().UnixNano()))
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	daemon := originEchoDaemon{got: make(chan callerorigin.Assertion, 2)}
	mux := http.NewServeMux()
	path, handler := bossanovav1connect.NewDaemonServiceHandler(daemon)
	mux.Handle(path, handler)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close(); _ = os.Remove(socketPath) })

	want := callerorigin.Assertion{Kind: callerorigin.KindAgentChat, AgentSessionID: "chat-1", Role: "build", Host: "laptop"}
	c := NewForwarded(socketPath, "token", want)
	for range 2 {
		if err := c.Ping(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := <-daemon.got; got != want {
			t.Fatalf("daemon read %+v, want %+v", got, want)
		}
	}
}
